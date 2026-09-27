package main

// pay.go is the a2a-x402 same-task flow as a local client runs it
// (A2A-DESIGN §8.3, §8.7): a capability call answered with
// payment-required, then, on the same task, the client's payment message —
// x402.payment.status: payment-submitted with no payload, the chosen option
// copied into anet.payment.accept — which the node signs within its agent
// tier and carries to the provider. And the three ways the node must say
// no without signing anything: an option the quote did not offer, a
// payload the client made itself, an amount above the agent tier.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// Reasons and codes the refusals carry (§8.5 table, §8.7).
const (
	codeSettlementFailed        = "SETTLEMENT_FAILED"
	reasonOptionNotOffered      = "option_not_offered"
	reasonClientPayload         = "client_payload_unsupported"
	reasonNeedsOperatorApproval = "needs_operator_approval"
)

// payFlow runs the flow on paid (a price within the agent tier) and on
// pricey (a price above it), with a2a-x402 activated the way a2a-go does
// it: a2aext's activator, which asks for the extension when the card
// declares it.
func (p *probe) payFlow(ctx context.Context, card *a2a.AgentCard, paid, pricey string) {
	cl, ctx, err := newClient(ctx, card, p.token, a2a.TransportProtocolJSONRPC, true)
	if !p.rep.check(err == nil, "pay-client", "a2a-go client with the a2a-x402 activator (%v)", errText(err)) {
		return
	}
	text := "joint-a2a paid " + p.nonce
	p.st.PaidText = text
	t, opt := p.quote(ctx, cl, "pay-quote", paid, text, "capability-payment-required")
	if t == nil {
		return
	}
	p.st.Paid = string(t.ID)

	// An option the quote did not offer: refused here, nothing signed; the
	// quote stands.
	bad := cloneJSON(opt)
	bad["amount"] = otherAmount(opt["amount"])
	res, err := p.payMessage(ctx, cl, t, map[string]any{keyX402Status: paySubmitted, keyAccept: bad}, "")
	p.refused("pay-option-not-offered", res, err, reasonOptionNotOffered)

	// A payload of the client's own: its payer is not this node, so it
	// could not settle even if carried; refused, nothing forwarded.
	own := map[string]any{"x402Version": 2, "accepted": opt,
		"payload": map[string]any{"authorization": "made-by-the-client", "signature": "AAAA"}}
	res, err = p.payMessage(ctx, cl, t, map[string]any{keyX402Status: paySubmitted, keyX402Payload: own}, "")
	p.refused("pay-client-payload", res, err, reasonClientPayload)

	gctx, cancel := opCtx(ctx, 30*time.Second)
	g, err := cl.GetTask(gctx, &a2a.GetTaskRequest{ID: t.ID})
	cancel()
	p.rep.check(err == nil && g.Status.State == a2a.TaskStateInputRequired && str(taskMeta(g, keyX402Status)) == payRequired,
		"pay-quote-stands", "after both refusals the task is %s, %s=%v (%v)", stateOf(g), keyX402Status, metaOf(g, keyX402Status), errText(err))

	// The payment: signed by the node at the agent tier, settled by the
	// provider, the capability run, the result on the same task.
	res, err = p.payMessage(ctx, cl, t, map[string]any{keyX402Status: paySubmitted, keyAccept: opt}, "capability-paid")
	if err != nil {
		p.rep.fail("pay", "payment-submitted: %v", err)
	} else {
		p.checkPaid(res, text)
	}

	if pricey == "" {
		p.rep.note("pay-over-limit", "no --pricey skill: the agent-tier limit was not tried")
		return
	}
	// Above the agent tier: nothing signed, the task waits for an
	// operator (anet pay), and the client is told so.
	t2, opt2 := p.quote(ctx, cl, "over-quote", pricey, "joint-a2a pricey "+p.nonce, "")
	if t2 == nil {
		return
	}
	p.st.Over = string(t2.ID)
	res, err = p.payMessage(ctx, cl, t2, map[string]any{keyX402Status: paySubmitted, keyAccept: opt2}, "")
	switch {
	case err != nil:
		p.rep.check(errIs(err, a2a.ErrUnsupportedOperation, ""), "pay-over-limit", "refused: %v", errText(err))
	default:
		p.rep.check(res.Status.State == a2a.TaskStateInputRequired && str(taskMeta(res, keyReason)) == reasonNeedsOperatorApproval &&
			str(taskMeta(res, keyX402Status)) != paySubmitted, "pay-over-limit",
			"answered %s, %s=%v, %s=%v", res.Status.State, keyReason, metaOf(res, keyReason), keyX402Status, metaOf(res, keyX402Status))
	}
	gctx, cancel = opCtx(ctx, 30*time.Second)
	g, err = cl.GetTask(gctx, &a2a.GetTaskRequest{ID: t2.ID})
	cancel()
	p.rep.check(err == nil && g.Status.State == a2a.TaskStateInputRequired && str(taskMeta(g, keyX402Status)) == payRequired &&
		str(taskMeta(g, keyReason)) == reasonNeedsOperatorApproval, "pay-over-limit-task",
		"the task waits: %s, %s=%v, %s=%v (%v)", stateOf(g), keyX402Status, metaOf(g, keyX402Status), keyReason, metaOf(g, keyReason), errText(err))

	// Declined: payment-rejected ends the task here and at the provider.
	res, err = p.payMessage(ctx, cl, t2, map[string]any{keyX402Status: payRejected}, "")
	p.rep.check(err == nil && res.Status.State == a2a.TaskStateCanceled, "pay-reject",
		"payment-rejected: %s, %s=%v (%v)", stateOf(res), keyX402Status, metaOf(res, keyX402Status), errText(err))
}

// quote calls skill with {text} and expects payment-required: the task at
// input-required, x402.payment.required with at least one option. It
// returns the task and the first option.
func (p *probe) quote(ctx context.Context, cl *a2aclient.Client, id, skill, text, record string) (*a2a.Task, map[string]any) {
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{"skill": skill, "args": map[string]any{"text": text}}))
	var body []byte
	sctx, cancel := opCtx(ctx, p.timeout)
	defer cancel()
	if record != "" {
		sctx = recordInto(sctx, &body)
	}
	res, err := cl.SendMessage(sctx, &a2a.SendMessageRequest{Message: m})
	t, terr := asTask(res)
	if err != nil || terr != nil {
		p.rep.fail(id, "%s: %v", skill, errText(errors.Join(err, terr)))
		return nil, nil
	}
	opts := accepts(paymentRequired(t))
	if !p.rep.check(t.Status.State == a2a.TaskStateInputRequired && str(taskMeta(t, keyX402Status)) == payRequired && len(opts) > 0,
		id, "%s: %s, %s=%v, %d option(s)", skill, t.Status.State, keyX402Status, metaOf(t, keyX402Status), len(opts)) {
		return nil, nil
	}
	if record != "" && len(body) > 0 {
		// Hermes does not speak x402; what it can do is show its model
		// the quote in words (0017 Q21 P3), which the projection may not
		// do yet.
		p.rec.add(hermesCase{Name: record, Agent: p.agent, ContextID: t.ContextID, Response: string(body),
			WantState: short(t.Status.State), WantReplyContains: str(opts[0]["amount"]), Soft: true,
			Why: "0017 Q21 P3: a payment status message names the amount"})
	}
	return t, opts[0]
}

// payMessage sends a payment message on task t: the given x402 metadata,
// and a short text part (the kernel keeps no text of it).
func (p *probe) payMessage(ctx context.Context, cl *a2aclient.Client, t *a2a.Task, meta map[string]any, record string) (*a2a.Task, error) {
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("payment"))
	m.TaskID, m.ContextID, m.Metadata = t.ID, t.ContextID, meta
	var body []byte
	sctx, cancel := opCtx(ctx, p.timeout)
	defer cancel()
	if record != "" {
		sctx = recordInto(sctx, &body)
	}
	res, err := cl.SendMessage(sctx, &a2a.SendMessageRequest{Message: m})
	if err != nil {
		return nil, err
	}
	out, err := asTask(res)
	if err == nil && record != "" && len(body) > 0 {
		want := ""
		if v, ok := meta[keyAccept]; ok && v != nil {
			want = digestHex(p.st.PaidText)
		}
		p.rec.add(hermesCase{Name: record, Agent: p.agent, ContextID: t.ContextID, Response: string(body),
			WantState: short(out.Status.State), WantReplyContains: want})
	}
	return out, err
}

// refused checks a refusal of §8.7: the task as it was, at input-required,
// with a status message saying payment-failed, SETTLEMENT_FAILED and the
// reason. The kernel's own refusal, an InvalidParams error naming the
// reason, is accepted too: nothing was signed either way.
func (p *probe) refused(id string, t *a2a.Task, err error, reason string) {
	if err != nil {
		p.rep.check(strings.Contains(err.Error(), reason), id, "refused as an error: %v", err)
		return
	}
	p.rep.check(t.Status.State == a2a.TaskStateInputRequired && str(statusMeta(t, keyX402Status)) == payFailed &&
		str(statusMeta(t, keyX402Error)) == codeSettlementFailed && str(statusMeta(t, keyReason)) == reason, id,
		"%s, %s=%v, %s=%v, %s=%v", t.Status.State, keyX402Status, statusMeta(t, keyX402Status),
		keyX402Error, statusMeta(t, keyX402Error), keyReason, statusMeta(t, keyReason))
}

// checkPaid checks a paid capability task at its end: completed,
// payment-completed with one successful receipt, the effect status (SI-6),
// and the deliverable the backend computed from the text.
func (p *probe) checkPaid(t *a2a.Task, text string) {
	p.rep.check(t.Status.State == a2a.TaskStateCompleted && str(taskMeta(t, keyX402Status)) == payCompleted && successReceipts(t) == 1,
		"pay", "%s, %s=%v, %d successful receipt(s)", t.Status.State, keyX402Status, metaOf(t, keyX402Status), successReceipts(t))
	es, has := t.Metadata[keyEffect]
	switch {
	case !has:
		p.rep.fail("pay-effect", "a terminal capability task without %s (SI-6)", keyEffect)
	case es == "OK":
		p.rep.pass("pay-effect", "%s=%v", keyEffect, es)
	default:
		p.rep.note("pay-effect", "%s=%v", keyEffect, es)
	}
	arts, _ := json.Marshal(t.Artifacts)
	want := digestHex(text)
	p.rep.check(strings.Contains(string(arts), want), "pay-deliverable", "the deliverable carries sha256(text) %s…", want[:12])
}

// digestHex is what backend answers for text.
func digestHex(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

// otherAmount is an amount that is not a.
func otherAmount(a any) string {
	if fmt.Sprint(a) == "1" {
		return "2"
	}
	return "1"
}

func metaOf(t *a2a.Task, key string) any {
	if t == nil {
		return nil
	}
	return taskMeta(t, key)
}

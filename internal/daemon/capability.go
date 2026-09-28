// Capability delegation — C1 over the wire (docs/CONTRACTS-zh.md).
//
// Convention (uses only signed, CID-significant TaskDoc fields):
//   - Tasks[0].Requires carries {ID: <capability-id>, Type: "capability",
//     Necessity: "must"} — the capability being invoked;
//   - Tasks[0].Contexts carries {Key: "args", Value: <JSON>, Format: "json"}
//     — the invocation arguments.
//
// A daemon whose provider registry resolves the capability executes it
// deterministically and answers with the effect as the deliverable plus the
// usual signed receipt — no LLM in the loop. Unresolvable capability calls
// fall through to the normal auto-reply path untouched.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/provider"
)

// RequireTypeCapability marks a TaskDoc requirement as a C1 capability call.
const RequireTypeCapability = "capability"

// capabilityInvokeTimeout bounds one provider invocation that does not
// say how long it needs.
//
// The original comment read "providers are local: in-process or a UDS hop
// to anetlinkd", and for the device, storage and blackboard providers it
// was written for, sixty seconds is generous. It stopped being true the
// moment a provider runs an operator's own command: the shell module
// validates timeout_s up to its own ceiling, an operator sets twenty
// minutes, and this constant killed the command at one — two layers each
// holding a timeout, only the shorter one ever visible.
//
// A provider that knows better now says so; see provider.LongRunning.
const capabilityInvokeTimeout = 60 * time.Second

// maxConcurrentLongCalls bounds how many long-running invocations this
// node will have in flight at once.
//
// It exists because running them off the poll loop is what makes
// concurrency possible here at all: while invocations were synchronous
// the loop was the limit, one at a time. Removing that without putting
// something in its place would trade "one long task freezes the node"
// for "twenty long tasks exhaust it", which on the small boards this is
// aimed at is the worse of the two.
//
// Over the limit the caller is told so, rather than queued behind work
// it cannot see: a requester that gets UNAVAILABLE with a reason can
// retry or go elsewhere, and one left waiting cannot tell a busy node
// from a dead one.
const maxConcurrentLongCalls = 4

// invokeBound reports how long p may take for capID, and whether that is
// long enough to run off the poll loop.
func invokeBound(p provider.CapabilityProvider, capID string) (d time.Duration, long bool) {
	lr, ok := p.(provider.LongRunning)
	if !ok {
		return capabilityInvokeTimeout, false
	}
	want, declared := lr.InvokeTimeout(capID)
	if !declared || want <= capabilityInvokeTimeout {
		return capabilityInvokeTimeout, false
	}
	return want, true
}

// Providers exposes the daemon's capability registry (config wires anetlink;
// tests and embedders register their own).
func (d *Daemon) Providers() *provider.Registry { return d.providers }

// capabilityCall detects the capability-delegation convention on a verified
// TaskDoc. Malformed args JSON is NOT a capability call (falls through to
// auto-reply rather than failing a possibly-humane task).
func capabilityCall(td *tsir.TaskDoc) (capID string, args map[string]any, ok bool) {
	if td == nil || len(td.Tasks) == 0 {
		return "", nil, false
	}
	t := td.Tasks[0]
	for _, r := range t.Requires {
		if r.Type == RequireTypeCapability && r.ID != "" {
			capID = r.ID
			break
		}
	}
	if capID == "" {
		return "", nil, false
	}
	args = map[string]any{}
	for _, c := range t.Contexts {
		if c.Key == "args" && c.Value != "" {
			if err := json.Unmarshal([]byte(c.Value), &args); err != nil {
				return "", nil, false
			}
			break
		}
	}
	return capID, args, true
}

// DelegateCapability delegates a capability invocation to providerAID. The
// TaskDoc carries the capability in Requires and args in Contexts — both
// inside the signed canonical preimage.
// DelegateCapability delegates a capability call, optionally paying for it.
func (d *Daemon) DelegateCapability(ctx context.Context, providerAID, capID string, args map[string]any) (string, error) {
	return d.delegateCapabilityPaid(ctx, providerAID, capID, args, nil)
}

// DelegateCapabilityIn is DelegateCapability under a given A2A context id
// (empty mints one).
func (d *Daemon) DelegateCapabilityIn(ctx context.Context, providerAID, capID string, args map[string]any, contextID string) (string, error) {
	id, err := newInteractionID()
	if err != nil {
		return "", err
	}
	return d.delegateCapabilityCtx(ctx, id, "", providerAID, capID, args, nil, contextID)
}

func (d *Daemon) delegateCapabilityPaid(ctx context.Context, providerAID, capID string,
	args map[string]any, paymentJSON []byte) (string, error) {
	id, err := newInteractionID()
	if err != nil {
		return "", err
	}
	return d.delegateCapabilityWithID(ctx, id, providerAID, capID, args, paymentJSON)
}

// delegateCapabilityWithID takes the interaction id from the caller,
// because a paid delegation must name the id its authorization was signed
// over — minting a fresh one here would leave the payment pointing at
// work that never happened.
func (d *Daemon) delegateCapabilityWithID(ctx context.Context, id, providerAID, capID string,
	args map[string]any, paymentJSON []byte) (string, error) {
	return d.delegateCapabilityCtx(ctx, id, "", providerAID, capID, args, paymentJSON, "")
}

// delegateCapabilityCtx sends a capability call as interaction id. nonce is
// the task nonce, minted here when empty; a prepaid call mints it first,
// because its payment is bound to it (pay_bind).
func (d *Daemon) delegateCapabilityCtx(ctx context.Context, id, nonce, providerAID, capID string,
	args map[string]any, paymentJSON []byte, contextID string) (string, error) {
	hub := d.config().HubURL
	if hub == "" {
		return "", fmt.Errorf("anet: no hub configured (run `anet hub-register` first)")
	}
	if providerAID == d.AID() {
		return "", fmt.Errorf("anet: cannot delegate to yourself")
	}
	argsJSON := "{}"
	if len(args) > 0 {
		b, err := json.Marshal(args)
		if err != nil {
			return "", err
		}
		argsJSON = string(b)
	}
	if contextID == "" {
		c, err := newContextID()
		if err != nil {
			return "", err
		}
		contextID = c
	}
	if nonce == "" {
		var err error
		if nonce, err = newTaskNonce(); err != nil {
			return "", err
		}
	}
	goal := "invoke capability " + capID
	td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{{
		Intent:   tsir.Intent{Summary: goal, Body: goal},
		Requires: []tsir.Require{{ID: capID, Type: RequireTypeCapability, Necessity: "must"}},
		Contexts: []tsir.Context{{Key: "args", Value: argsJSON, Format: "json"}, nonceContext(nonce)},
	}}}
	if err := td.Sign(d.self); err != nil {
		return "", err
	}
	doc, err := coredet.Marshal(td)
	if err != nil {
		return "", err
	}
	requestCID, err := anetcid.Sum(doc)
	if err != nil {
		return "", err
	}
	kelB, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		return "", err
	}
	dr := &delegation.DelegateReq{TaskDoc: doc, Envelope: td.Envelope, KEL: kelB, InteractionID: id, ContextID: contextID}
	if len(paymentJSON) > 0 {
		dr.Payment = paymentJSON
	}
	payload, err := dr.Marshal()
	if err != nil {
		return "", err
	}
	// Recorded and queued together, under the envelope's message id (0017
	// Q5, Q9); see delegateInWithID.
	mid, msgID := newWireMID()
	var seq int64
	qid, err := d.queueSendAs(ctx, wireSend{to: providerAID, typ: seal.TypeDelegate, ix: id, body: payload, mid: mid,
		pin: interactions.PinOutbound, strict: true}, func(tx *interactions.Tx) error {
		if err := tx.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: providerAID,
			Goal: goal, RequestCID: requestCID, RequestDoc: doc, ContextID: contextID, IsCapability: true,
			TaskNonce: nonce}); err != nil {
			return err
		}
		if len(paymentJSON) > 0 {
			// The prepaid authorization is this task's, so its receipt can
			// be checked against it when the result comes back (§8.3).
			authID := ""
			if _, _, aid, err := payloadAuth(paymentJSON); err == nil {
				authID = aid
			}
			if _, err := tx.SetPayment(id, interactions.PayUpdate{
				State: interactions.PayState(interactions.PaySubmitted), AddAuthID: authID, Payload: paymentJSON}); err != nil {
				return err
			}
		}
		var err error
		seq, _, err = tx.AddMessageRecord(interactions.MessageRecord{InteractionID: id, SenderAID: d.AID(),
			Kind: interactions.MsgText, Body: goal + " args=" + argsJSON, MsgID: msgID})
		return err
	})
	if err != nil {
		return "", err
	}
	d.publishMessage(id, seq, interactions.MsgText)
	d.publishState(id)
	// C5: a requester's chain should show what it asked for, not only what
	// it received.
	//
	// The prose path has recorded this since it existed (relay.go); this
	// one never did. The consequence was not limited to requests still in
	// flight: after a capability call completed successfully, the
	// requester's chain held only anet.result.accepted — interaction id,
	// result CID, receipt, receipt_verified — with no provider, no request
	// CID and no capability id. So a node could prove what it accepted and
	// could not prove, from its own chain, what it had asked whom to do.
	//
	// The capability id is carried in addition to what the prose path
	// records, because "which capability" is the question this path exists
	// to answer. Found by the release matrix.
	if _, lerr := d.ledger.Append(EvDelegationSent, map[string]any{
		"interaction_id": id,
		"provider_aid":   providerAID,
		"request_cid":    requestCID,
		"capability":     capID,
	}); lerr != nil {
		log.Printf("anet: capability delegation evidence ledger: %v", lerr)
	}
	if err := d.firstDelivery(ctx, id, qid); err != nil {
		return "", err
	}
	return id, nil
}

// capabilityResult is the deterministic deliverable of a capability call.
//
// Evidence travels with it. It did not, and that made every read in the
// system write-only across the wire: a capability reports what it read in
// Evidence.ObservedState — the CID a store wrote, the bytes it read back,
// the position a camera reported, the org a node serves — and the only
// other channel is Metrics, which is map[string]float64 and cannot hold a
// CID, a blob or a list. So `cas.put` told the caller how many bytes it
// stored but not where, and `cas.get` could not be called at all.
//
// The provenance belongs here for a second reason, which is the one that
// generalises. A provider that corrected a vendor deviation says so in
// Evidence.Quirk, and a corrected reading is not the value the device put
// on the wire. That correction was reaching this node's own chain and
// stopping there — so the peer consuming the result, the one party who
// cannot check for itself, was the only party not told. A correction
// reaches every surface or none.
//
// The deliverable is what the receipt's ResultCID covers, so a provider
// that ships provenance is signing it: claiming V3 for a value it took on
// the device's word is now a signed claim rather than a private note.
type capabilityResult struct {
	Capability string `json:"capability"`
	Status     string `json:"status"`
	// Nonce is the task's anet.nonce (A2A-DESIGN §2 X4): 16 random bytes
	// in the deliverable, so its CID, which the receipt carries, does not
	// identify the result to a party that holds only the receipt.
	Nonce      string             `json:"nonce,omitempty"`
	Verifiable bool               `json:"verifiable"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
	Message    string             `json:"message,omitempty"`
	Evidence   map[string]any     `json:"evidence,omitempty"`
	// Payment is the x402 402 body when the provider wants paying, so a
	// caller learns the price from the answer rather than from a document.
	Payment *payment.PaymentRequired `json:"payment_required,omitempty"`
	// Paid names the settlement that let this run, so the caller holds a
	// transaction id it can point at.
	Paid *paidView `json:"paid,omitempty"`
}

// paidView is the settlement, as the payer sees it in the result.
//
// Receipt is the hub's own signed statement that the credit moved, passed
// through untouched. Without it the payer has a transaction string it
// cannot check — it would be taking the provider's word that the provider
// was paid, which is precisely the word least worth taking. With it, the
// payer can verify against the hub's key history, and so can a stranger
// handed nothing but the receipt.
type paidView struct {
	Transaction string `json:"transaction"`
	Amount      string `json:"amount"`
	Network     string `json:"network"`
	Receipt     string `json:"receipt,omitempty"` // base64 CoreDet-CBOR payment.Receipt
}

// provenanceOf renders an effect's evidence for every surface that
// carries it. It lives in the provider package so the voucher door in
// module/x402 uses the SAME function: evidence that differs depending on
// which door a caller came through is worse than no evidence, and two
// field lists that must stay identical are two field lists that will not.
func provenanceOf(e *effect.Evidence) map[string]any { return provider.Provenance(e) }

// tryCapability resolves and executes a capability call, answering with the
// effect + signed receipt.
func (d *Daemon) tryCapability(ctx context.Context, interactionID, capID string, args map[string]any) bool {
	return d.tryCapabilityPaid(ctx, interactionID, capID, args, nil)
}

// tryCapabilityPaid is tryCapability with the delegation's payment, if it
// carried one.
//
// It returns false, having recorded nothing, when the daemon is stopping:
// a call not started, or one whose failure is the stop's rather than its
// own, is left open for the next process (SI-10; see cutOffByStop).
func (d *Daemon) tryCapabilityPaid(ctx context.Context, interactionID, capID string, args map[string]any, paymentRaw []byte) bool {
	if d.providers == nil {
		return false
	}
	if d.ctx.Err() != nil {
		return false
	}
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		log.Printf("anet: capability %s: load interaction: %v", capID, err)
		return false
	}
	if ix.IsTerminal() {
		// Canceled (or otherwise ended) before execution started.
		return true
	}
	p, ok := d.providers.Resolve(capID)
	if !ok {
		// A capability this node has no provider for is answered, not
		// handed to auto-reply. Capability interactions never reach the
		// auto-reply loop (A2A-DESIGN §6), so without an answer the task
		// would stay open with no error and no effect status, and the
		// requester could not tell "does not serve it" from "is down".
		res := capabilityResult{
			Capability: capID,
			Status:     string(effect.Unavailable),
			Message:    "this node does not serve " + capID,
		}
		d.deliverCapabilityResult(ctx, interactionID, capID, ix, res, nil, resultOpts{reason: "capability_not_served"})
		return true
	}
	res := capabilityResult{Capability: capID}

	// Priced work is answered with a price, not attempted and refused
	// (A2A-DESIGN §8.3): the task waits in input-required with the quote,
	// and the payment arrives on the same task.
	//
	// The settle happens before the work. Settling after would mean doing
	// the work and then finding out we cannot be paid; settling before
	// means a payer whose work then fails has paid for a failure, and is
	// shown the receipt with the failure. The second is the one the
	// evidence model can speak about.
	if price, priced := priceOfCapability(p, capID); priced {
		payer := d.payer()
		if payer == nil {
			// Priced work on a build that cannot charge. Refused, not
			// done: "I cannot take your money, so I will not do it" is
			// true, and doing it anyway is a decision nobody made.
			res.Status = string(effect.Unavailable)
			res.Message = errNoPayments().Error()
			d.deliverCapabilityResult(ctx, interactionID, capID, ix, res, nil, resultOpts{reason: "payments_unavailable"})
			return true
		}
		if d.refuseUnboundPriced(ctx, ix, capID) {
			return true
		}
		switch {
		case ix.PayState == interactions.PayCompleted:
			// Settled: by the retry loop, or before a redelivery.
		case len(paymentRaw) == 0:
			return d.quoteCapability(ctx, ix, capID, price)
		default:
			if !d.takePayment(ctx, ix, capID, price, paymentRaw) {
				return true
			}
			cur, err := d.ix.Get(interactionID)
			if err != nil || cur.IsTerminal() {
				return true
			}
			ix = cur
		}
		res.Paid = paidViewOf(ix)
	}

	eff, err := p.Invoke(ctx, provider.Call{Capability: capID, Args: args, CallID: interactionID,
		CallerAID: ix.PeerAID, Via: provider.ViaRelay})
	if d.cutOffByStop(err, eff) {
		log.Printf("anet: %s: %s was cut off by the daemon stopping; no result recorded", interactionID, capID)
		return false
	}
	if err != nil {
		res.Status, res.Message = "FAILED", err.Error()
	} else {
		res.Status, res.Verifiable, res.Message = string(eff.Status), eff.Verifiable(), eff.Message
		res.Evidence = provenanceOf(eff.Evidence)
		if eff.Record != nil {
			res.Metrics = eff.Record.Metrics
		}
	}
	return d.deliverCapabilityResult(ctx, interactionID, capID, ix, res, eff.Evidence, resultOpts{})
}

// refuseUnboundPriced answers a priced call whose TaskDoc carries no task
// nonce, and reports whether it did. The payment's binding is
// PayBind(ix, anet.nonce) (§2 X4); such a call is rejected with
// task_nonce_required rather than quoted, so the two sides never disagree
// over what an empty nonce binds (0017 Q19). A task whose payment flow
// already began (an older row) is left alone.
func (d *Daemon) refuseUnboundPriced(ctx context.Context, ix *interactions.Interaction, capID string) bool {
	if ix.TaskNonce != "" || ix.PayState != interactions.PayNone {
		return false
	}
	res := capabilityResult{Capability: capID, Status: string(effect.Unavailable),
		Message: "a priced call must carry the task nonce (TaskDoc context " + NonceContextKey +
			") its payment is bound to"}
	d.deliverCapabilityResult(ctx, ix.ID, capID, ix, res, nil,
		resultOpts{state: interactions.StateRejected, reason: x402a2a.ReasonTaskNonceRequired})
	return true
}

// stateForEffect maps a capability effect status to the task state and
// the metadata the result carries (A2A-DESIGN §4.3). The effect status is
// carried as anet.effect_status in every case: a completed task does not
// mean the effect was verified (SI-6).
//
//	OK, UNVERIFIED       completed
//	FAILED               failed
//	UNAVAILABLE          rejected (failed once a payment has settled)
//	PAYMENT_REQUIRED     input-required
func stateForEffect(status string, paid bool) (interactions.State, map[string]any) {
	meta := map[string]any{"anet.effect_status": status}
	var st interactions.State
	switch effect.Status(status) {
	case effect.OK, effect.Unverified:
		st = interactions.StateCompleted
	case effect.Unavailable:
		st = interactions.StateRejected
		if paid {
			st = interactions.StateFailed
		}
	case effect.PaymentRequired:
		st = interactions.StateInputRequired
	default:
		st = interactions.StateFailed
	}
	meta["anet.state"] = string(st)
	return st, meta
}

// resultOpts adjusts how a capability result is recorded.
type resultOpts struct {
	// state overrides the state stateForEffect derives.
	state interactions.State
	// reason is carried as anet.reason.
	reason string
	// retryAfterMS is carried as anet.retry_after_ms (a temporary refusal).
	retryAfterMS int64
}

// deliverCapabilityResult signs, stores, records and delivers one answer.
//
// Not used for a quote: a price is a status on the open task
// (quoteCapability), never a result, because a receipted result ends the
// task and the payment arrives on the same task (A2A-DESIGN §8.3). The
// result, its receipt, the state it maps to and the queued delivery commit
// together; the result goes through the retry queue, so a relay failure is
// retried rather than lost. The write is guarded: a task that reached a
// terminal state first (a cancel) is not given a result or a receipt.
func (d *Daemon) deliverCapabilityResult(_ context.Context, interactionID, capID string,
	ix *interactions.Interaction, res capabilityResult, prov *effect.Evidence, opts resultOpts) bool {
	// Sealing and sending run under the daemon's context, not the
	// invocation's: a call stopped by a cancel or by its deadline still has
	// a result to deliver, and its own context is already done.
	ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
	defer cancel()
	if res.Capability == "" {
		res.Capability = capID
	}
	res.Nonce = ix.TaskNonce
	if res.Nonce == "" {
		res.Nonce, _ = newTaskNonce()
	}
	// The payment columns as they stand now: a settlement may have landed
	// since the caller read the row.
	if cur, err := d.ix.Get(interactionID); err == nil {
		ix = cur
	}
	paid := ix.PayState == interactions.PayCompleted
	state, meta := stateForEffect(res.Status, paid)
	paymentResultMeta(ix, meta)
	if opts.state != "" {
		state = opts.state
		meta["anet.state"] = string(state)
	}
	if opts.reason != "" {
		meta["anet.reason"] = opts.reason
	}
	if opts.retryAfterMS > 0 {
		meta["anet.retry_after_ms"] = opts.retryAfterMS
	}
	deliverable, err := json.Marshal(res)
	if err != nil {
		return false
	}
	resultCID, err := anetcid.Sum(deliverable)
	if err != nil {
		return false
	}
	rc := &evidence.Receipt{
		InteractionID: interactionID,
		RequesterAID:  ix.PeerAID,
		ProviderAID:   d.AID(),
		RequestCID:    ix.RequestCID,
		ResultCID:     resultCID,
		CompletedAt:   uint64(nowMillis()),
	}
	if err := rc.Sign(d.self); err != nil {
		log.Printf("anet: capability %s: sign receipt: %v", capID, err)
		return false
	}
	receiptBytes, err := rc.Marshal()
	if err != nil {
		return false
	}
	// The receipt travels with the key that signed it, so the requester can
	// check what it accepts.
	selfKEL, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		log.Printf("anet: capability %s: marshal KEL: %v", capID, err)
		return false
	}
	status := delegation.StatusDone
	if state == interactions.StateFailed || state == interactions.StateRejected {
		status = delegation.StatusFailed
	}
	metaBytes, _ := json.Marshal(meta)
	payload, err := (&delegation.ResultResp{Status: status, Deliverable: deliverable,
		Receipt: receiptBytes, KEL: selfKEL, Metadata: metaBytes}).Marshal()
	if err != nil {
		return false
	}
	var seq int64
	id, err := d.queueSend(ctx, ix.PeerAID, seal.TypeResult, interactionID, payload, func(tx *interactions.Tx) error {
		// Signed with this node's own key a few lines up, so there is
		// nothing to take on trust.
		if err := tx.Finish(interactionID, interactions.Finish{State: state, Result: deliverable,
			ResultCID: resultCID, Receipt: receiptBytes, Verified: interactions.VerificationVerified,
			Meta: metaBytes}); err != nil {
			return err
		}
		var err error
		seq, err = tx.AddMessage(interactionID, d.AID(), interactions.MsgText, string(deliverable))
		return err
	})
	if errors.Is(err, interactions.ErrTerminal) {
		log.Printf("anet: capability %s: %s ended before its result; no result or receipt recorded", capID, interactionID)
		return true
	}
	if err != nil {
		log.Printf("anet: capability %s: store result: %v", capID, err)
		return false
	}
	// The chain records the provenance, not just the outcome — the same
	// provenance the requester was sent, from the same value, so the two
	// accounts of one effect cannot disagree.
	ev := map[string]any{
		"interaction_id": interactionID, "capability": capID, "caller_aid": ix.PeerAID,
		"status": res.Status, "verifiable": res.Verifiable, "metrics": res.Metrics,
		"result_cid": resultCID, "state": string(state),
	}
	// A public_cap call in the cid mode keeps only how the effect was
	// checked, not what it produced (evidence_mode.go).
	if prov := d.effectEvidence(ix, capID, res.Evidence); prov != nil {
		ev["evidence"] = prov
	}
	if _, lerr := d.ledger.Append(EvCapabilityEffect, ev); lerr != nil {
		log.Printf("anet: capability %s: evidence ledger: %v", capID, lerr)
	}
	d.publishMessage(interactionID, seq, interactions.MsgText)
	d.publishResult(interactionID)
	if err := d.deliverQueued(ctx, id); err != nil {
		log.Printf("anet: capability %s: result for %s queued for delivery: %v", capID, interactionID, err)
	}
	return true
}

// recoverInterrupted runs at start (A2A-DESIGN §3.6, startup recovery): an
// inbound capability call the previous process left open with no result is
// dealt with before any mail is read (leftoverAction).
//
//   - A long call, working or only recorded (the process stopped between
//     committing the delegation and marking it working), is set failed,
//     with effect UNVERIFIED and anet.reason=interrupted, and the
//     requester is told through the retry queue. Whether the effect
//     happened is not known; the result says so rather than guessing
//     either way. Long calls are not run twice (at-most-once).
//   - A short call that was working is run again (at-least-once, §3.6
//     step 10). One whose payment was taken has no redelivery to bring it
//     back: its delegation and its payment were acknowledged.
//   - A short call only recorded is left for the redelivery of its
//     delegation, which was not acknowledged and runs it again; an
//     approved one, whose delegation was acknowledged when it was held,
//     is run again here.
//   - A call waiting on a payment (quoted, submitted, or received and not
//     taken) is the payment flow's (startPayments, §8.3).
//   - Unpaid work for a peer now on the deny list is not run again: the
//     revocation sweep cancels it (§5.1). Paid work is (0017 Q10).
//
// Short or long is the provider's answer, so it is read from the registry,
// which the modules have filled by now. A recorded call whose provider is
// not there cannot be told apart and is left (a redelivery decides); a
// working one is reported interrupted.
func (d *Daemon) recoverInterrupted() {
	list, err := d.ix.ListAll(interactions.ListFilter{Role: interactions.RoleInbound,
		States: []interactions.State{interactions.StateSubmitted, interactions.StateWorking}})
	if err != nil {
		log.Printf("anet: startup recovery: %v", err)
		return
	}
	ps := d.readPeers()
	for _, ix := range list {
		if !ix.IsCapability || len(ix.Receipt) > 0 {
			continue
		}
		if _, running := d.running.Load(ix.ID); running {
			// Running in this process, so not a leftover. Nothing is
			// delivered before recovery (awaitReady), but a call already
			// running is never the previous process's to report
			// ([redteam:F30]).
			continue
		}
		capID, args := storedCall(ix)
		switch d.leftoverAction(ix, capID) {
		case leftoverRerun:
			if ps.denied(ix.PeerAID) && ix.PayState != interactions.PayCompleted {
				log.Printf("anet: %s: %s was cut off by a restart; its peer is denied, so it is not run again", ix.ID, capID)
				continue
			}
			ix, capID, args := ix, capID, args
			log.Printf("anet: %s: %s was cut off by a restart; running it again", ix.ID, capID)
			d.goBackground(func() { d.runCapabilityCall(ix.ID, capID, args, nil, nil) })
		case leftoverInterrupted:
			d.reportInterrupted(ix, capID)
		}
	}
}

// reportInterrupted answers a long capability call that stopped, or never
// started, in an earlier process: failed, effect UNVERIFIED,
// anet.reason=interrupted, through the retry queue.
func (d *Daemon) reportInterrupted(ix *interactions.Interaction, capID string) {
	ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
	defer cancel()
	d.deliverCapabilityResult(ctx, ix.ID, capID, ix, capabilityResult{
		Capability: capID,
		Status:     string(effect.Unverified),
		Message:    "the provider stopped while this call was running; whether its effect happened is not known",
	}, nil, resultOpts{state: interactions.StateFailed, reason: "interrupted"})
	log.Printf("anet: %s: %s was interrupted by a restart; reported as failed, effect unverified", ix.ID, capID)
}

// decodeTaskDoc decodes stored TaskDoc bytes (the request of an
// interaction). The signature is not checked here; it was checked when the
// delegation was accepted.
func decodeTaskDoc(b []byte) (*tsir.TaskDoc, error) {
	var td tsir.TaskDoc
	if err := coredet.Unmarshal(b, &td); err != nil {
		return nil, err
	}
	return &td, nil
}

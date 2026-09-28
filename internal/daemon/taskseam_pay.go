package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// The payment half of the task surface (A2A-DESIGN §8.3, §8.6, §8.7). The
// flow itself is PayTask (x402task.go), the one implementation behind the
// control plane's /tasks/pay and /tasks/pay-manual as well: comparing the
// chosen option with the stored requirements, the spending policy,
// signing, and forwarding the PaymentPayload. What is here only reads the
// A2A shape of a decision and says PayTask's answers in A2A terms. The
// purpose is always task-agent: a local A2A client is an agent, whatever
// it says about itself (§8.6).
//
// Two answers are tasks rather than errors: a refusal with an a2a-x402
// outcome (a payload of the client's own, an option not offered: the task
// with a payment-failed status message, §8.7), and a payment above the
// agent tier (the task still input-required, now with anet.reason
// needs_operator_approval and a status message naming `anet pay`, §8.3).
// Nothing was signed, sent or stored in either case.

// payAnswer is a decision PayTask did not carry out, to be said as the
// task: exactly one of refusal and hold is set.
type payAnswer struct {
	refusal *PayRefusal
	hold    *PayHold
}

// payAnswerOf sorts PayTask's error: an answer said as the task, or an
// error (nil for neither).
func payAnswerOf(err error) (*payAnswer, error) {
	var refusal *PayRefusal
	var hold *PayHold
	switch {
	case errors.As(err, &refusal):
		return &payAnswer{refusal: refusal}, nil
	case errors.As(err, &hold):
		return &payAnswer{hold: hold}, nil
	}
	return nil, err
}

// apply is t answered with a.
func (a *payAnswer) apply(t a2ashape.Task) a2ashape.Task {
	id, err := newMessageID()
	if err != nil {
		id = t.ID + ".payment." + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	if a.hold != nil {
		return a2ashape.PaymentHold(t, id, a.hold.Outcome.Reason, a.hold.Outcome.Message)
	}
	pr := a.refusal
	detail := pr.Outcome.Message
	if detail == "" {
		detail = a2ashape.PaymentRefusalDetail(pr.Outcome.Reason)
	}
	return a2ashape.PaymentRefusal(t, id, pr.Outcome.Error, pr.Outcome.Reason, detail)
}

// taskPaymentMessage handles a client message that carries x402.* metadata
// on an existing task: payment-submitted with anet.payment.accept (the
// option copied from x402.payment.required.accepts, optional when there is
// one), or payment-rejected (§8.7). It returns the state_seq the task had
// before the decision: the blocking wait that follows then ends on the
// first terminal or interrupted state after it, which a submitted payment
// (the task goes to working) is not.
//
// A decision PayTask answers without carrying it out — refused with an
// a2a-x402 outcome, or held for the operator above the agent tier — is not
// an error: answer is set and the caller returns the task with it
// (payAnswer.apply).
func (d *Daemon) taskPaymentMessage(ctx context.Context, ix *interactions.Interaction, msg a2ashape.Message) (after int64, answer *payAnswer, err error) {
	var decision string
	switch msg.Metadata[x402a2a.KeyStatus] {
	case x402a2a.StatusSubmitted:
		decision = PayDecisionSubmit
	case x402a2a.StatusRejected:
		decision = PayDecisionReject
	default:
		return 0, nil, a2ashape.Errorf(a2ashape.ErrInvalidParams,
			"task %s: a payment message sets %s to %s or %s", ix.ID, x402a2a.KeyStatus,
			x402a2a.StatusSubmitted, x402a2a.StatusRejected)
	}
	accept, err := rawMeta(msg.Metadata, x402a2a.KeyAccept)
	if err != nil {
		return 0, nil, a2ashape.Errorf(a2ashape.ErrInvalidParams, "task %s: %s: %v", ix.ID, x402a2a.KeyAccept, err)
	}
	payload, err := rawMeta(msg.Metadata, x402a2a.KeyPayload)
	if err != nil {
		return 0, nil, a2ashape.Errorf(a2ashape.ErrInvalidParams, "task %s: %s: %v", ix.ID, x402a2a.KeyPayload, err)
	}
	if decision == PayDecisionSubmit && d.payer() == nil && payload == nil {
		return 0, nil, a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s: %v", ix.ID, errNoPayments())
	}
	_, err = d.PayTask(ctx, PayRequest{TaskID: ix.ID, Decision: decision, Accept: accept,
		Payload: payload, Purpose: module.PurposeTaskAgent, ClientMsgID: msg.ID})
	if answer, err = payAnswerOf(err); err != nil {
		return 0, nil, payTaskError(ix.ID, err)
	}
	return ix.StateSeq, answer, nil
}

// taskPay is TaskSeam.Pay: submit or reject at the agent tier, purpose
// task-agent. A decision answered without being carried out (a refusal, a
// hold for the operator) is returned as answer, not as an error, as
// taskPaymentMessage does.
func (d *Daemon) taskPay(ctx context.Context, ix *interactions.Interaction, decision module.PayDecision) (answer *payAnswer, err error) {
	var dec string
	switch decision.Decision {
	case module.PaySubmit:
		dec = PayDecisionSubmit
	case module.PayReject:
		dec = PayDecisionReject
	default:
		return nil, a2ashape.Errorf(a2ashape.ErrInvalidParams, "task %s: decision must be %q or %q",
			ix.ID, module.PaySubmit, module.PayReject)
	}
	if dec == PayDecisionSubmit && d.payer() == nil {
		return nil, a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s: %v", ix.ID, errNoPayments())
	}
	_, err = d.PayTask(ctx, PayRequest{TaskID: ix.ID, Decision: dec, Accept: decision.Accept,
		Purpose: module.PurposeTaskAgent})
	if answer, err = payAnswerOf(err); err != nil {
		return nil, payTaskError(ix.ID, err)
	}
	return answer, nil
}

// rawMeta is metadata value k as JSON, nil when absent or null.
func rawMeta(m map[string]any, k string) (json.RawMessage, error) {
	v, ok := m[k]
	if !ok || v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil, nil
	}
	return b, nil
}

// payTaskError says a PayTask error as an A2A error, the way the control
// plane's /tasks/pay says it in HTTP (writePayError): a decision on a task
// with nothing to pay, a payment already outstanding or a quote no tier
// pays (zero_amount) is not an operation this task takes now. Nothing was
// signed or sent in any of these cases. (A refusal with an a2a-x402
// outcome and a payment held for the operator are answered as the task by
// the callers above, never as an error.)
func payTaskError(id string, err error) error {
	if err == nil {
		return nil
	}
	var sr *SpendRefusal
	switch {
	case errors.As(err, &sr):
		return a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s: %v", id, sr)
	case errors.Is(err, interactions.ErrNotFound):
		return a2ashape.Errorf(a2ashape.ErrTaskNotFound, "%s", id)
	case errors.Is(err, ErrNotRequester):
		return a2ashape.Errorf(a2ashape.ErrInvalidParams, "task %s: %v", id, err)
	case errors.Is(err, ErrTaskTerminal), errors.Is(err, ErrNoQuote), errors.Is(err, ErrPaymentPending),
		errors.Is(err, ErrQuoteExpired), errors.Is(err, ErrPayeeNotPeer), errors.Is(err, errNoResend),
		errors.Is(err, errAutoPaidOnce):
		return a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s: %v", id, err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return err
	}
	var ae *a2ashape.Error
	if errors.As(err, &ae) {
		return err
	}
	return fmt.Errorf("task %s: %w", id, err)
}

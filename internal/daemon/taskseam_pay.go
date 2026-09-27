package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
// A2A shape of a decision and says PayTask's refusals in A2A terms. The
// purpose is always task-agent: a local A2A client is an agent, whatever
// it says about itself (§8.6).

// taskPaymentMessage handles a client message that carries x402.* metadata
// on an existing task: payment-submitted with anet.payment.accept (the
// option copied from x402.payment.required.accepts, optional when there is
// one), or payment-rejected (§8.7). It returns the state_seq the task had
// before the decision: the blocking wait that follows then ends on the
// first terminal or interrupted state after it, which a submitted payment
// (the task goes to working) is not.
func (d *Daemon) taskPaymentMessage(ctx context.Context, ix *interactions.Interaction, msg a2ashape.Message) (int64, error) {
	var decision string
	switch msg.Metadata[x402a2a.KeyStatus] {
	case x402a2a.StatusSubmitted:
		decision = PayDecisionSubmit
	case x402a2a.StatusRejected:
		decision = PayDecisionReject
	default:
		return 0, a2ashape.Errorf(a2ashape.ErrInvalidParams,
			"task %s: a payment message sets %s to %s or %s", ix.ID, x402a2a.KeyStatus,
			x402a2a.StatusSubmitted, x402a2a.StatusRejected)
	}
	accept, err := rawMeta(msg.Metadata, x402a2a.KeyAccept)
	if err != nil {
		return 0, a2ashape.Errorf(a2ashape.ErrInvalidParams, "task %s: %s: %v", ix.ID, x402a2a.KeyAccept, err)
	}
	payload, err := rawMeta(msg.Metadata, x402a2a.KeyPayload)
	if err != nil {
		return 0, a2ashape.Errorf(a2ashape.ErrInvalidParams, "task %s: %s: %v", ix.ID, x402a2a.KeyPayload, err)
	}
	if decision == PayDecisionSubmit && d.payer() == nil {
		return 0, a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s: %v", ix.ID, errNoPayments())
	}
	if _, err := d.PayTask(ctx, PayRequest{TaskID: ix.ID, Decision: decision, Accept: accept,
		Payload: payload, Purpose: module.PurposeTaskAgent}); err != nil {
		return 0, payTaskError(ix.ID, err)
	}
	return ix.StateSeq, nil
}

// taskPay is TaskSeam.Pay: submit or reject at the agent tier, purpose
// task-agent.
func (d *Daemon) taskPay(ctx context.Context, ix *interactions.Interaction, decision module.PayDecision) error {
	var dec string
	switch decision.Decision {
	case module.PaySubmit:
		dec = PayDecisionSubmit
	case module.PayReject:
		dec = PayDecisionReject
	default:
		return a2ashape.Errorf(a2ashape.ErrInvalidParams, "task %s: decision must be %q or %q",
			ix.ID, module.PaySubmit, module.PayReject)
	}
	if dec == PayDecisionSubmit && d.payer() == nil {
		return a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s: %v", ix.ID, errNoPayments())
	}
	_, err := d.PayTask(ctx, PayRequest{TaskID: ix.ID, Decision: dec, Accept: decision.Accept,
		Purpose: module.PurposeTaskAgent})
	return payTaskError(ix.ID, err)
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
// plane's /tasks/pay says it in HTTP (writePayError): a refusal with an
// a2a-x402 outcome (client payload, option not offered) is the caller's
// to fix; a decision on a task with nothing to pay, a payment already
// outstanding or a spending-policy refusal is not an operation this task
// takes now. Nothing was signed or sent in any of these cases.
func payTaskError(id string, err error) error {
	if err == nil {
		return nil
	}
	var pr *PayRefusal
	var sr *SpendRefusal
	switch {
	case errors.As(err, &pr):
		return a2ashape.Errorf(a2ashape.ErrInvalidParams, "task %s: %s: %s (%s=%s)", id,
			x402a2a.StatusFailed, pr.Outcome.Error, x402a2a.KeyReason, pr.Outcome.Reason)
	case errors.As(err, &sr):
		return a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s: %v; an operator can pay it with `anet pay %s`", id, sr, id)
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

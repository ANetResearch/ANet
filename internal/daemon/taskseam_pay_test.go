//go:build !no_x402

package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// payMsg is a local client's payment message on a task (A2A-DESIGN §8.7).
func payMsg(taskID string, meta map[string]any) a2ashape.Message {
	return a2ashape.Message{TaskID: taskID, Role: a2ashape.RoleUser, Metadata: meta,
		Parts: []a2ashape.Part{a2ashape.TextPart("")}}
}

// The task surface and the payment flow are one: a quote in the same-task
// flow reads as A2A through the payment flow's own derivation
// (PaymentStatusMeta), and a local client's payment message or Pay goes to
// PayTask, the implementation behind /tasks/pay (A2A-DESIGN §8.2, §8.7,
// §11.5).
func TestTheTaskSeamPaysThroughTheSameTaskFlow(t *testing.T) {
	work := &meteredWork{price: 5}
	_, req, prov := paidPair(t, work)
	ctx := context.Background()
	seam := req.TaskSeam()

	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)

	// Waiting above the automatic tier: input-required, the quote in the
	// status message, the payment keys and the reason in the metadata.
	task, err := seam.Get(ctx, prov.AID(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != a2ashape.TaskStateInputRequired || task.Status.Message == nil {
		t.Fatalf("waiting task: %+v", task.Status)
	}
	sm := task.Status.Message.Metadata
	if sm[a2ashape.KeyX402Status] != a2ashape.PaymentRequired || sm[a2ashape.KeyX402Required] == nil {
		t.Errorf("status message metadata while waiting: %v", sm)
	}
	md := task.Metadata
	if md[a2ashape.KeyX402Status] != a2ashape.PaymentRequired || md[a2ashape.KeyX402Required] == nil ||
		md[a2ashape.KeyQuoteExpiresAt] == nil || md[a2ashape.KeyReason] != x402a2a.ReasonNeedsOperatorApproval {
		t.Errorf("task metadata while waiting: %v", md)
	}

	// §8.7: a client's own payload and an option that was not offered are
	// refused, and nothing is signed.
	_, err = seam.Send(ctx, prov.AID(), module.TaskSend{ReturnImmediately: true, Message: payMsg(id, map[string]any{
		x402a2a.KeyStatus: x402a2a.StatusSubmitted, x402a2a.KeyPayload: map[string]any{"x402Version": 2}})})
	if !errors.Is(err, a2ashape.ErrInvalidParams) || !strings.Contains(err.Error(), x402a2a.ReasonClientPayloadUnsupported) {
		t.Fatalf("a client's own payload: %v", err)
	}
	_, err = seam.Send(ctx, prov.AID(), module.TaskSend{ReturnImmediately: true, Message: payMsg(id, map[string]any{
		x402a2a.KeyStatus: x402a2a.StatusSubmitted, x402a2a.KeyAccept: map[string]any{"scheme": "exact"}})})
	if !errors.Is(err, a2ashape.ErrInvalidParams) || !strings.Contains(err.Error(), x402a2a.ReasonOptionNotOffered) {
		t.Fatalf("an option not offered: %v", err)
	}
	// Above agent_max (0 by default) the agent tier is refused; the task
	// still waits for an operator.
	_, err = seam.Send(ctx, prov.AID(), module.TaskSend{ReturnImmediately: true, Message: payMsg(id, map[string]any{
		x402a2a.KeyStatus: x402a2a.StatusSubmitted})})
	if !errors.Is(err, a2ashape.ErrUnsupportedOperation) {
		t.Fatalf("over the agent tier: %v", err)
	}
	if n := chainEvents(t, req, EvPaymentAuthorized); n != 0 {
		t.Fatalf("%d authorizations signed by refused decisions", n)
	}

	// Within the agent tier the message pays, as task-agent.
	payPolicy(t, req, PaymentsConfig{AgentMax: 10, AgentDailyMax: 10}, prov.AID())
	task, err = seam.Send(ctx, prov.AID(), module.TaskSend{ReturnImmediately: true, Message: payMsg(id, map[string]any{
		x402a2a.KeyStatus: x402a2a.StatusSubmitted})})
	if err != nil {
		t.Fatal(err)
	}
	if task.Metadata[a2ashape.KeyX402Status] != a2ashape.PaymentSubmitted {
		t.Errorf("after the payment message: %v", task.Metadata)
	}
	if ev := lastLedgerPayload(t, req, EvPaymentAuthorized); ev["purpose"] != module.PurposeTaskAgent || ev["interaction_id"] != id {
		t.Errorf("anet.payment.authorized = %v", ev)
	}
	poll(t, prov, req, req)
	task, err = seam.Get(ctx, prov.AID(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := task.Metadata[a2ashape.KeyX402Receipts].([]any)
	if task.Status.State != a2ashape.TaskStateCompleted || task.Metadata[a2ashape.KeyX402Status] != a2ashape.PaymentCompleted ||
		len(rc) != 1 || work.invoked.Load() != 1 {
		t.Fatalf("after settling: %s %v (ran %d)", task.Status.State, task.Metadata, work.invoked.Load())
	}
	if task.Metadata[a2ashape.KeyReason] != nil {
		t.Errorf("a paid, completed task still gives a reason: %v", task.Metadata[a2ashape.KeyReason])
	}

	// Pay with reject declines a second quote: canceled here, nothing paid.
	id2, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	task, err = seam.Pay(ctx, prov.AID(), id2, module.PayDecision{Decision: module.PayReject})
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != a2ashape.TaskStateCanceled || task.Metadata[a2ashape.KeyX402Status] != a2ashape.PaymentRejected {
		t.Fatalf("after rejecting: %s %v", task.Status.State, task.Metadata)
	}
	if rix := getIX(t, req, id2); rix.PayState != interactions.PayRejected {
		t.Errorf("pay_state after rejecting: %q", rix.PayState)
	}
	// A decision on a task with nothing to pay is not an operation it takes.
	if _, err := seam.Pay(ctx, prov.AID(), id, module.PayDecision{Decision: module.PaySubmit}); !errors.Is(err, a2ashape.ErrUnsupportedOperation) {
		t.Errorf("paying a settled task: %v", err)
	}
	if work.invoked.Load() != 1 {
		t.Errorf("the work ran %d times", work.invoked.Load())
	}
}

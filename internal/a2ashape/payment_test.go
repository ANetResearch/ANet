package a2ashape_test

import (
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/x402a2a"
)

// A hold keeps the quote and the task's state, gives the reason in the
// message and the task, and says in text what happens now (§8.3).
func TestPaymentHold(t *testing.T) {
	quote := map[string]any{"accepts": []any{map[string]any{"amount": "5"}}}
	task := a2ashape.Task{ID: "ix_1", ContextID: "c", Status: a2ashape.TaskStatus{State: a2ashape.TaskStateInputRequired},
		Metadata: map[string]any{a2ashape.KeyX402Status: a2ashape.PaymentRequired, a2ashape.KeyX402Required: quote}}
	held := a2ashape.PaymentHold(task, "m1", x402a2a.ReasonNeedsOperatorApproval, "run `anet pay ix_1`")
	m := held.Status.Message
	if m == nil || m.Role != a2ashape.RoleAgent || m.TaskID != "ix_1" || len(m.Parts) != 1 || m.Parts[0].Text != "run `anet pay ix_1`" ||
		m.Metadata[a2ashape.KeyX402Status] != a2ashape.PaymentRequired || m.Metadata[a2ashape.KeyX402Required] == nil ||
		m.Metadata[a2ashape.KeyReason] != x402a2a.ReasonNeedsOperatorApproval {
		t.Fatalf("hold message: %+v", m)
	}
	if held.Metadata[a2ashape.KeyReason] != x402a2a.ReasonNeedsOperatorApproval || !a2ashape.IsPaymentHold(held) {
		t.Fatalf("hold: %+v", held.Metadata)
	}
	if _, ok := task.Metadata[a2ashape.KeyReason]; ok {
		t.Fatal("PaymentHold changed the task it was given")
	}
	if a2ashape.IsPaymentHold(task) || a2ashape.IsPaymentRefusal(held) {
		t.Fatal("a waiting task is not a hold, and a hold is not a refusal")
	}
}

// §8.7: needs_operator_approval, and only that, is said as
// payment_extension_not_activated, in the task and its status message, on
// copies.
func TestWithoutX402Extension(t *testing.T) {
	md := map[string]any{a2ashape.KeyReason: x402a2a.ReasonNeedsOperatorApproval, "k": 1}
	msg := &a2ashape.Message{ID: "m", Metadata: map[string]any{a2ashape.KeyReason: x402a2a.ReasonNeedsOperatorApproval}}
	task := a2ashape.Task{ID: "ix_1", Status: a2ashape.TaskStatus{State: a2ashape.TaskStateInputRequired, Message: msg}, Metadata: md}
	out := a2ashape.WithoutX402Extension(task)
	if out.Metadata[a2ashape.KeyReason] != x402a2a.ReasonExtensionNotActivated || out.Metadata["k"] != 1 ||
		out.Status.Message.Metadata[a2ashape.KeyReason] != x402a2a.ReasonExtensionNotActivated {
		t.Fatalf("rewritten: %v %v", out.Metadata, out.Status.Message.Metadata)
	}
	if md[a2ashape.KeyReason] != x402a2a.ReasonNeedsOperatorApproval || msg.Metadata[a2ashape.KeyReason] != x402a2a.ReasonNeedsOperatorApproval {
		t.Fatal("the task given was changed")
	}
	su := a2ashape.StatusWithoutX402Extension(a2ashape.StatusUpdate(task))
	if su.Metadata[a2ashape.KeyReason] != x402a2a.ReasonExtensionNotActivated {
		t.Fatalf("status update: %v", su.Metadata)
	}
	other := a2ashape.Task{Metadata: map[string]any{a2ashape.KeyReason: "quote_expired"}}
	if a2ashape.WithoutX402Extension(other).Metadata[a2ashape.KeyReason] != "quote_expired" {
		t.Fatal("another reason was rewritten")
	}
}

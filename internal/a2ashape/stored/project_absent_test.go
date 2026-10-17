package stored_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// A quote that reaches a node with no payment module is a D1 decision whose sentence key is
// module_absent.payment and whose anet.reason is module_absent: a variant changes the words, never the
// reason (ux B-5), and the code is the key without its variant. The task projection says both, with the
// command that cancels the task.
func TestQuoteWithoutPaymentModuleHasTheBaseReason(t *testing.T) {
	t.Setenv("ANET_LANG", "C")
	if got := a2ashape.ReasonCode("module_absent.payment"); got != "module_absent" {
		t.Fatalf("ReasonCode(module_absent.payment) = %q, want module_absent", got)
	}
	note := a2ashape.PayNote{Code: "module_absent.payment", Amount: 12, Peer: notePeer, At: time.Now().UTC(), Why: "not_compiled"}
	task := waitingOn(t, note, a2ashape.ReasonCode(note.Code))
	m := task.Status.Message
	if m == nil || m.Metadata[a2ashape.KeyReason] != "module_absent" {
		t.Fatalf("status.message %+v, want anet.reason module_absent", m)
	}
	if got := task.Metadata[a2ashape.KeyReason]; got != nil && got != "module_absent" {
		t.Errorf("task anet.reason %v, want module_absent", got)
	}
	if next := task.Metadata[a2ashape.KeyNext]; next != "anet task cancel ix_9f3c" {
		t.Errorf("anet.next %v, want anet task cancel ix_9f3c", next)
	}
	if s, _ := task.Metadata[a2ashape.KeyPaymentNote].(string); strings.Contains(s, "anet pay ") && !strings.Contains(s, "to pay") {
		t.Errorf("the sentence tells the person to run anet pay: %q", s)
	}
}

// No waiting D1 decision puts a variant in anet.reason.
func TestWaitingReasonsCarryNoVariant(t *testing.T) {
	for _, r := range a2ashape.Rows() {
		if key := r.Key(); a2ashape.PayNoteWaits(key) && strings.Contains(a2ashape.ReasonCode(key), ".") {
			t.Errorf("%s: anet.reason %q has a variant", key, a2ashape.ReasonCode(key))
		}
	}
}

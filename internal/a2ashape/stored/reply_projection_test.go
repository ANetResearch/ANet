package stored_test

import (
	"github.com/ANetResearch/ANet/internal/a2ashape/stored"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// A projection for a local backend never carries anet.identity_note; one
// for the operator's clients does (correctness-ux §8.1 UX-3).
func TestProjectionForBackendHasNoIdentityNote(t *testing.T) {
	src := stored.Source{Interaction: &interactions.Interaction{ID: "ix_1", Role: interactions.RoleInbound,
		PeerAID: "peer", State: interactions.StateWorking}}
	const note = "the reminder"
	if got := stored.Project(src, stored.Options{IdentityNote: note}); got.Metadata[a2ashape.KeyIdentityNote] != note {
		t.Fatalf("the operator's projection: %v", got.Metadata)
	}
	got := stored.Project(src, stored.Options{IdentityNote: note, ForBackend: true})
	if _, ok := got.Metadata[a2ashape.KeyIdentityNote]; ok {
		t.Fatalf("a backend's projection carries the note: %v", got.Metadata)
	}
}

// A stored mismatch is anet.receipt_verified = mismatch (CUX-R6 item 4).
func TestReceiptMismatchIsProjected(t *testing.T) {
	src := stored.Source{Interaction: &interactions.Interaction{ID: "ix_1", Role: interactions.RoleOutbound,
		PeerAID: "peer", State: interactions.StateCompleted, Receipt: []byte{1},
		ReceiptVerified: interactions.VerificationMismatch}}
	if got := stored.Project(src, stored.Options{}); got.Metadata[a2ashape.KeyReceiptVerified] != a2ashape.ReceiptMismatch {
		t.Fatalf("%v", got.Metadata)
	}
}

// A message carrying anet.state is a control message, whatever else it
// carries: there is no final-reply exception any more (CUX-R4).
func TestAStateMarkIsAlwaysControl(t *testing.T) {
	for _, meta := range []map[string]any{
		{a2ashape.KeyState: "working"},
		{a2ashape.KeyState: "working", "anet." + "final": true},
	} {
		if !a2ashape.ControlMetadata(meta) {
			t.Errorf("%v is not control", meta)
		}
	}
	if a2ashape.ControlMetadata(map[string]any{"a2a.messageId": "m"}) {
		t.Error("a message id made a message control")
	}
}

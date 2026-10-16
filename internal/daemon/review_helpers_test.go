package daemon

import (
	"testing"

	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// receiptOver is signer's receipt (as it travels) for the outbound task ix: the commitments and the
// review key are the TaskDoc ix holds carries, and the receipt names requestCID and resultCID.
func receiptOver(t testing.TB, signer identity.Signer, ix *interactions.Interaction, requestCID, resultCID string) []byte {
	t.Helper()
	b, err := taskBindingOf(ix.RequestDoc)
	if err != nil {
		t.Fatalf("the TaskDoc of %s carries no binding: %v", ix.ID, err)
	}
	ixc, err := b.IXCommit(ix.ID)
	if err != nil {
		t.Fatal(err)
	}
	return receiptBody(t, signer, evidence.ReceiptBody{IXCommit: ixc, RequesterCommit: b.RequesterCommit,
		ReviewKey: b.ReviewKey, RequestCID: requestCID, ResultCID: resultCID})
}

// receiptBody signs body (its provider is signer's AID, its day today when left 0) and returns the
// receipt as it travels. Unset commitments are filled with fixed bytes.
func receiptBody(t testing.TB, signer identity.Signer, body evidence.ReceiptBody) []byte {
	t.Helper()
	fill := func(b []byte, v byte) []byte {
		if len(b) == 0 {
			b = make([]byte, 32)
			for i := range b {
				b[i] = v
			}
		}
		return b
	}
	body.IXCommit, body.RequesterCommit, body.ReviewKey = fill(body.IXCommit, 1), fill(body.RequesterCommit, 2), fill(body.ReviewKey, 3)
	if body.RequestCID == "" {
		body.RequestCID = "req"
	}
	if body.ResultCID == "" {
		body.ResultCID = "res"
	}
	if body.CompletedDay == 0 {
		body.CompletedDay = dayOf(uint64(nowMillis()))
	}
	w, err := body.Sign(signer)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := w.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// bindingContexts is the task binding of a new delegation from requester, as contexts for a TaskDoc a
// test builds by hand.
func bindingContexts(t testing.TB, requester string) []tsir.Context {
	t.Helper()
	b, err := makeTaskBinding(requester)
	if err != nil {
		t.Fatal(err)
	}
	return b.contexts
}

// evidenceIXCommit is the ix_commit a receipt for the outbound task ix must carry.
func evidenceIXCommit(ix *interactions.Interaction) ([]byte, error) {
	b, err := taskBindingOf(ix.RequestDoc)
	if err != nil {
		return nil, err
	}
	return b.IXCommit(ix.ID)
}

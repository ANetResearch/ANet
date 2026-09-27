package interactions_test

import (
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The payment columns (A2A-DESIGN §4.1): a write guarded by the current
// pay_state applies once, receipts append in order and are kept to the
// newest, and a terminal task still takes a late receipt without its
// state changing.
func TestPaymentColumns(t *testing.T) {
	s, err := interactions.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Create(interactions.New{ID: "ix1", Role: interactions.RoleInbound, PeerAID: "did:anet:p",
		Goal: "g", IsCapability: true}); err != nil {
		t.Fatal(err)
	}
	exp := int64(12345)
	ok, err := s.SetPayment("ix1", interactions.PayUpdate{From: []string{interactions.PayNone},
		State: interactions.PayState(interactions.PayRequired), Required: []byte(`{"accepts":[{}]}`), QuoteExpiresAt: &exp})
	if err != nil || !ok {
		t.Fatalf("quote: %v %v", ok, err)
	}
	// Two submissions from the quoted state: the second finds submitted
	// and does not apply.
	sub := interactions.PayUpdate{From: []string{interactions.PayRequired, interactions.PayFailed},
		State: interactions.PayState(interactions.PaySubmitted), AuthIDs: []string{"a1"}, Payload: []byte(`{"p":1}`)}
	if ok, _ := s.SetPayment("ix1", sub); !ok {
		t.Fatal("the first submission did not apply")
	}
	sub.AuthIDs, sub.Payload = []string{"a2"}, []byte(`{"p":2}`)
	if ok, _ := s.SetPayment("ix1", sub); ok {
		t.Fatal("a second submission applied over the first")
	}
	ix, _ := s.Get("ix1")
	if ix.PayState != interactions.PaySubmitted || len(ix.PayAuthIDs) != 1 || ix.PayAuthIDs[0] != "a1" ||
		string(ix.PayPayload) != `{"p":1}` || ix.QuoteExpiresAt != exp || string(ix.PayRequired) != `{"accepts":[{}]}` {
		t.Fatalf("row = %+v", ix)
	}
	if _, err := s.SetPayment("ix1", interactions.PayUpdate{AddAuthID: "a1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPayment("ix1", interactions.PayUpdate{AddAuthID: "a3"}); err != nil {
		t.Fatal(err)
	}
	if ix, _ = s.Get("ix1"); len(ix.PayAuthIDs) != 2 || ix.PayAuthIDs[1] != "a3" {
		t.Fatalf("auth ids = %v", ix.PayAuthIDs)
	}
	// Receipts append, and a terminal row still takes one.
	if _, err := s.SetState("ix1", interactions.StateCanceled); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 70; i++ {
		b, _ := json.Marshal(map[string]int{"n": i})
		if _, err := s.SetPayment("ix1", interactions.PayUpdate{AddReceipt: b}); err != nil {
			t.Fatal(err)
		}
	}
	ix, _ = s.Get("ix1")
	var list []map[string]int
	if err := json.Unmarshal(ix.PayReceipts, &list); err != nil || len(list) != 64 || list[0]["n"] != 6 || list[63]["n"] != 69 {
		t.Fatalf("receipts: %d, first %v, last %v (%v)", len(list), list[0], list[len(list)-1], err)
	}
	if ix.State != interactions.StateCanceled {
		t.Errorf("a receipt changed the state: %s", ix.State)
	}
	got, err := s.ListPayState(interactions.RoleInbound, []string{interactions.PaySubmitted}, false)
	if err != nil || len(got) != 1 {
		t.Fatalf("listed %d (%v)", len(got), err)
	}
	if got, _ := s.ListPayState(interactions.RoleInbound, []string{interactions.PaySubmitted}, true); len(got) != 0 {
		t.Errorf("an ended task is listed as active")
	}
}

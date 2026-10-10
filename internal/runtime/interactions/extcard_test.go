package interactions_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// HPP §3.5, PAY-C14: one kept extended card per agent, replaced by the next;
// a row more than a day older than the one written goes; the public card's
// high-water mark is not touched.
func TestPeerExtCardKeepsOnePerAgentForADay(t *testing.T) {
	s := open(t)
	const day = interactions.ExtCardLife
	now := uint64(100 * day)
	a := interactions.PeerExtCard{AID: "did:anet:ba", Card: []byte(`{"a":1}`), PublicSeq: 4, KeyStateSeq: 1, Variant: "list",
		Prices: map[string]uint64{"x": 2}, FetchedAt: now - day - 1}
	if err := s.PutPeerExtCard(a); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.PeerExtCard(a.AID)
	if err != nil || !ok || got.PublicSeq != 4 || got.KeyStateSeq != 1 || got.Variant != "list" || got.Prices["x"] != 2 ||
		got.FetchedAt != a.FetchedAt || !bytes.Equal(got.Card, a.Card) {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	// Replaced whole.
	a2 := a
	a2.Card, a2.PublicSeq, a2.Variant, a2.Prices, a2.FetchedAt = []byte(`{"a":2}`), 5, "general", nil, now-day-1
	if err := s.PutPeerExtCard(a2); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.PeerExtCard(a.AID); got.PublicSeq != 5 || len(got.Prices) != 0 || got.Variant != "general" {
		t.Fatalf("not replaced: %+v", got)
	}
	// Another agent's card a day and a moment later: the first goes.
	b := interactions.PeerExtCard{AID: "did:anet:bb", Card: []byte(`{"b":1}`), Variant: "general", FetchedAt: now}
	if err := s.PutPeerExtCard(b); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.PeerExtCard(a.AID); ok {
		t.Fatal("a card more than a day old was kept")
	}
	if _, ok, _ := s.PeerExtCard(b.AID); !ok {
		t.Fatal("the new card is gone")
	}
	if err := s.DeletePeerExtCard(b.AID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.PeerExtCard(b.AID); ok || err != nil {
		t.Fatalf("deleted: %v %v", ok, err)
	}
	// What a row must have.
	for _, bad := range []interactions.PeerExtCard{
		{Card: []byte("x"), Variant: "general"},
		{AID: "did:anet:bc", Variant: "general"},
		{AID: "did:anet:bc", Card: []byte("x"), Variant: "public"},
	} {
		if err := s.PutPeerExtCard(bad); !errors.Is(err, interactions.ErrBadInput) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

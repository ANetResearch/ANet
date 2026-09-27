package daemon

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// 0017 Q6: a node that published a card and lost its last public skill
// withdraws the card (a2a_card: null) once; the hub's confirmation is kept
// across a restart, so it is not sent again; a card published again is
// withdrawn again when the skill goes. A fresh install withdraws nothing.
func TestLosingTheLastPublicSkillWithdrawsTheCard(t *testing.T) {
	h := newFakeHub(t)
	fh := fakeHubAt(t, h.URL)
	ctx := context.Background()
	state := func(aid string) (withdrawals int, status string, held bool) {
		fh.mu.Lock()
		defer fh.mu.Unlock()
		_, held = fh.a2aCards[aid]
		return fh.a2aWithdrawals, fh.registerCards[aid], held
	}

	fresh := newCardDaemon(t, "Nobody", "")
	if err := fresh.RegisterWithHub(ctx, h.URL, "Nobody", nil, ""); err != nil {
		t.Fatal(err)
	}
	if n, status, _ := state(fresh.AID()); n != 0 || status != hubapi.CardStatusAbsent {
		t.Fatalf("a fresh install: %d withdrawals, card_status %q", n, status)
	}

	d := lampNode(t)
	if err := d.RegisterWithHub(ctx, h.URL, "Lamp Agent", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, status, held := state(d.AID()); status != hubapi.CardStatusOK || !held {
		t.Fatalf("published: card_status %q, held %v", status, held)
	}

	setPublic(d)
	if err := d.RegisterWithHub(ctx, h.URL, "Lamp Agent", nil, ""); err != nil {
		t.Fatal(err)
	}
	if n, status, held := state(d.AID()); n != 1 || status != hubapi.CardStatusWithdrawn || held {
		t.Fatalf("after losing the skill: %d withdrawals, card_status %q, held %v", n, status, held)
	}
	if pub := d.CardPublicationStatus(); pub.Sent || pub.Status != hubapi.CardStatusWithdrawn {
		t.Fatalf("publication status %+v", pub)
	}
	if err := d.RegisterWithHub(ctx, h.URL, "Lamp Agent", nil, ""); err != nil {
		t.Fatal(err)
	}
	if n, status, _ := state(d.AID()); n != 1 || status != hubapi.CardStatusAbsent {
		t.Fatalf("registering again: %d withdrawals, card_status %q", n, status)
	}

	// A restart remembers the hub confirmed it.
	d2, err := New(d.layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d2.Close() })
	if err := d2.Providers().Register(ctx, &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	setPublic(d2)
	if err := d2.RegisterWithHub(ctx, h.URL, "Lamp Agent", nil, ""); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := state(d.AID()); n != 1 {
		t.Fatalf("a restart withdrew again: %d withdrawals", n)
	}

	// Published again, then withdrawn again. The capability list and the
	// ADP card name only the public capability.
	setPublic(d2, lampCap)
	if err := d2.RegisterWithHub(ctx, h.URL, "Lamp Agent", []string{"private.cap", lampCap}, ""); err != nil {
		t.Fatal(err)
	}
	if _, status, held := state(d.AID()); status != hubapi.CardStatusOK || !held {
		t.Fatalf("published again: card_status %q, held %v", status, held)
	}
	fh.mu.Lock()
	adp := fh.adpCaps[d.AID()]
	fh.mu.Unlock()
	if len(adp) != 1 || adp[0] != lampCap {
		t.Fatalf("the ADP card lists %v, want only the public %s (0017 Q14)", adp, lampCap)
	}
	setPublic(d2)
	if err := d2.RegisterWithHub(ctx, h.URL, "Lamp Agent", nil, ""); err != nil {
		t.Fatal(err)
	}
	if n, status, held := state(d.AID()); n != 2 || status != hubapi.CardStatusWithdrawn || held {
		t.Fatalf("withdrawn again: %d withdrawals, card_status %q, held %v", n, status, held)
	}
}

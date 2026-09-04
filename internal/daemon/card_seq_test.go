package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANetCore/adp"
)

// A node that registers twice in a row must be admitted twice.
//
// The hub admits a card only if its sequence exceeds the highest it has
// seen for that subject. The sequence used to be the clock in seconds, so
// two registrations inside the same second carried the same number and
// the hub refused the second one with STALE_SEQ — which is what
// `anet hub-register` twice, or a node re-registering because its
// capability list changed, actually does.
func TestTwoRegistrationsInTheSameSecondBothSucceed(t *testing.T) {
	h := newFakeHub(t)
	defer h.Close()
	d := newTestDaemon(t, h.URL, true)

	for i, name := range []string{"First", "Second", "Third"} {
		if err := d.RegisterWithHub(context.Background(), h.URL, name, []string{"work.do"}, 5, ""); err != nil {
			t.Fatalf("registration %d (%s) was refused: %v", i+1, name, err)
		}
	}
}

// The guarantee the clock cannot give on its own.
func TestCardSequenceStrictlyIncreases(t *testing.T) {
	h := newFakeHub(t)
	defer h.Close()
	d := newTestDaemon(t, h.URL, true)

	var prev uint64
	for i := 0; i < 50; i++ {
		raw, err := d.signedCard("n", []string{"work.do"})
		if err != nil {
			t.Fatal(err)
		}
		var card adp.AgentCard
		if err := json.Unmarshal(raw, &card); err != nil {
			t.Fatal(err)
		}
		if card.Seq <= prev {
			t.Fatalf("card %d: seq %d did not exceed the previous %d", i, card.Seq, prev)
		}
		prev = card.Seq
	}
}

// A restart inside the same second must not collide with the hub's high
// water. The in-memory counter starts fresh in a new process, so the value
// has to survive on disk — which matters more now that a daemon
// re-publishes its capabilities at startup, making a quick restart a
// registration rather than a no-op.
func TestTheSequenceSurvivesARestart(t *testing.T) {
	h := newFakeHub(t)
	defer h.Close()
	d := newTestDaemon(t, h.URL, true)
	if err := d.RegisterWithHub(context.Background(), h.URL, "a", nil, 5, ""); err != nil {
		t.Fatal(err)
	}
	first := d.lastCardSeq.Load()
	if first == 0 {
		t.Fatal("nothing was minted")
	}

	// A fresh Daemon over the same data dir is what a restart is.
	d2, err := New(d.layout)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if got := d2.lastCardSeq.Load(); got < first {
		t.Fatalf("the restarted node forgot where it had got to: %d < %d", got, first)
	}
	if err := d2.RegisterWithHub(context.Background(), h.URL, "a", nil, 5, ""); err != nil {
		t.Fatalf("registering immediately after a restart was refused: %v", err)
	}
}

// Seconds stay the unit. Milliseconds would also break the tie, and would
// put every sequence three orders of magnitude above where an older build
// mints them — after which a node that downgraded could never update its
// card again.
func TestSequenceStaysInSecondsSoADowngradeIsNotLockedOut(t *testing.T) {
	h := newFakeHub(t)
	defer h.Close()
	d := newTestDaemon(t, h.URL, true)

	raw, err := d.signedCard("n", nil)
	if err != nil {
		t.Fatal(err)
	}
	var card adp.AgentCard
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	// Within a day of IssuedAt, which is the clock in seconds. A
	// millisecond sequence would be about 55,000 years past it.
	if diff := int64(card.Seq) - card.IssuedAt; diff < 0 || diff > 86400 {
		t.Fatalf("seq %d is not a second-resolution value beside issued_at %d (diff %d)",
			card.Seq, card.IssuedAt, diff)
	}
}

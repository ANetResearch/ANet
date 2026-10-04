package interactions_test

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// putTomb records one tombstone in its own transaction and returns how
// many the write evicted.
func putTomb(t *testing.T, s *interactions.Store, from, ix string, cancelExp uint64) int {
	t.Helper()
	var evicted int
	if err := s.Update(func(tx *interactions.Tx) error {
		var err error
		evicted, err = tx.PutCancelTomb(from, ix, cancelExp)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return evicted
}

func hasTomb(t *testing.T, s *interactions.Store, from, ix string, now uint64) bool {
	t.Helper()
	ok, err := s.HasCancelTomb(from, ix, now)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// T47 (DLV-ST-6, M39): one sender's 1030 tombstones keep the 1024 that
// last longest, and the six that expire first are evicted and counted;
// another sender's tombstones are not touched by it.
func TestCancelTombsAreBoundedPerSender(t *testing.T) {
	s := open(t)
	if interactions.CancelTombPerSender != 1024 || interactions.CancelTombTotal != 100_000 {
		t.Fatalf("bounds %d per sender, %d in all; want 1024 and 100000 (DLV-ST-6)",
			interactions.CancelTombPerSender, interactions.CancelTombTotal)
	}
	putTomb(t, s, "did:anet:other", "ix-other", 1)
	const n = 1030
	evicted := 0
	// Written out of exp order: the eviction goes by exp, not by arrival.
	for i := 0; i < n; i++ {
		k := (i * 7) % n // a permutation of 0..n-1, since gcd(7, 1030) = 1
		evicted += putTomb(t, s, "did:anet:a", fmt.Sprintf("ix-%04d", k), uint64(1_000_000+k))
	}
	if evicted != 6 {
		t.Fatalf("cancel-tombstone-evicted = %d, want 6", evicted)
	}
	for k := 0; k < n; k++ {
		if got, want := hasTomb(t, s, "did:anet:a", fmt.Sprintf("ix-%04d", k), 0), k >= 6; got != want {
			t.Fatalf("tombstone %d (exp %d): present %v, want %v", k, 1_000_000+k, got, want)
		}
	}
	if !hasTomb(t, s, "did:anet:other", "ix-other", 0) {
		t.Fatal("another sender's tombstone went with this sender's eviction")
	}
}

// The table as a whole is bounded the same way: past the total, the
// tombstones that expire first go, whoever left them.
func TestCancelTombsAreBoundedInAll(t *testing.T) {
	defer interactions.SetCancelTombCaps(1024, 5)()
	s := open(t)
	evicted := 0
	for i := 0; i < 8; i++ {
		evicted += putTomb(t, s, fmt.Sprintf("did:anet:s%d", i), "ix", uint64(10_000-i)) // later senders expire sooner
	}
	if evicted != 3 {
		t.Fatalf("evicted %d, want 3", evicted)
	}
	if n, err := s.CancelTombCount(); err != nil || n != 5 {
		t.Fatalf("%d tombstones (%v), want 5", n, err)
	}
	for i := 0; i < 8; i++ {
		if got, want := hasTomb(t, s, fmt.Sprintf("did:anet:s%d", i), "ix", 0), i < 5; got != want {
			t.Errorf("sender %d: present %v, want %v", i, got, want)
		}
	}
}

// A tombstone lasts until the cancel's exp plus five minutes (DLV-ST-7):
// HasCancelTomb is false from then on, and the sweep deletes it.
func TestACancelTombExpires(t *testing.T) {
	s := open(t)
	const cancelExp = 1_000_000
	putTomb(t, s, "did:anet:a", "ix-1", cancelExp)
	end := uint64(cancelExp + interactions.CancelTombMarginMS)
	if interactions.CancelTombMarginMS != 5*60*1000 {
		t.Fatalf("margin %d ms, want five minutes", interactions.CancelTombMarginMS)
	}
	if !hasTomb(t, s, "did:anet:a", "ix-1", end-1) {
		t.Fatal("the tombstone is gone before its end")
	}
	if hasTomb(t, s, "did:anet:a", "ix-1", end) {
		t.Fatal("the tombstone is still in force at its end")
	}
	if hasTomb(t, s, "did:anet:b", "ix-1", 0) || hasTomb(t, s, "did:anet:a", "ix-2", 0) {
		t.Fatal("a tombstone counts for another sender or another task")
	}
	if n, err := s.PurgeCancelTombs(end - 1); err != nil || n != 0 {
		t.Fatalf("sweep before the end: %d (%v)", n, err)
	}
	if n, err := s.PurgeCancelTombs(end); err != nil || n != 1 {
		t.Fatalf("sweep at the end: %d (%v)", n, err)
	}
	if c, _ := s.CancelTombCount(); c != 0 {
		t.Fatalf("%d tombstones after the sweep", c)
	}
}

// A second cancel for the same task keeps whichever tombstone lasts
// longer; an earlier one does not shorten it.
func TestACancelTombKeepsTheLaterExp(t *testing.T) {
	s := open(t)
	putTomb(t, s, "did:anet:a", "ix-1", 2_000_000)
	putTomb(t, s, "did:anet:a", "ix-1", 1_000_000)
	if !hasTomb(t, s, "did:anet:a", "ix-1", 2_000_000) {
		t.Fatal("an earlier cancel shortened the tombstone")
	}
	putTomb(t, s, "did:anet:a", "ix-1", 3_000_000)
	if !hasTomb(t, s, "did:anet:a", "ix-1", 3_000_000) {
		t.Fatal("a later cancel did not lengthen the tombstone")
	}
	if c, _ := s.CancelTombCount(); c != 1 {
		t.Fatalf("%d rows for one (sender, ix)", c)
	}
}

// A tombstone commits with the transaction it is written in, or not at
// all; it needs a sender and a task; and an exp at the end of time does
// not wrap around.
func TestPutCancelTombEdges(t *testing.T) {
	s := open(t)
	boom := errors.New("the rest of step 10 failed")
	err := s.Update(func(tx *interactions.Tx) error {
		if _, err := tx.PutCancelTomb("did:anet:a", "ix-1", 1_000_000); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || hasTomb(t, s, "did:anet:a", "ix-1", 0) {
		t.Fatalf("a rolled-back transaction left its tombstone (%v)", err)
	}
	for _, c := range []struct{ from, ix string }{{"", "ix"}, {"did:anet:a", ""}} {
		err := s.Update(func(tx *interactions.Tx) error {
			_, err := tx.PutCancelTomb(c.from, c.ix, 1)
			return err
		})
		if !errors.Is(err, interactions.ErrBadInput) {
			t.Errorf("from %q ix %q: %v, want ErrBadInput", c.from, c.ix, err)
		}
	}
	putTomb(t, s, "did:anet:a", "ix-max", math.MaxUint64)
	if !hasTomb(t, s, "did:anet:a", "ix-max", math.MaxInt64-1) {
		t.Fatal("a tombstone at the end of time expired early")
	}
}

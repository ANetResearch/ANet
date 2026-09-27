package interactions_test

import (
	"errors"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// A write transaction is all or nothing: an error from the function rolls
// back the business rows and the replay row together (§3.6 step 10).
func TestUpdateRollsBackEverythingOnError(t *testing.T) {
	s := open(t)
	boom := errors.New("boom")
	err := s.Update(func(tx *interactions.Tx) error {
		if err := tx.Put("ix_1", interactions.RoleInbound, "peer", "g", "", nil); err != nil {
			return err
		}
		if ok, err := tx.ClaimReplay("peer", []byte("mid-0000000000001"), 1<<40); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update returned %v", err)
	}
	if _, err := s.Get("ix_1"); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("a rolled-back interaction is visible")
	}
	if seen, _ := s.ReplaySeen("peer", []byte("mid-0000000000001")); seen {
		t.Fatal("a rolled-back replay row is visible")
	}
}

// A replay row is claimed once; the second claim reports the duplicate.
// Expired rows are purged.
func TestReplayRowsClaimOnceAndExpire(t *testing.T) {
	s := open(t)
	claim := func(mid string, exp uint64) bool {
		var ok bool
		if err := s.Update(func(tx *interactions.Tx) error {
			var err error
			ok, err = tx.ClaimReplay("peer", []byte(mid), exp)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !claim("a", 100) || claim("a", 100) {
		t.Fatal("a replay row was claimable twice or not at all")
	}
	if !claim("b", 1000) {
		t.Fatal("a second message id was refused")
	}
	if n, err := s.PurgeReplay(500); err != nil || n != 1 {
		t.Fatalf("purge removed %d (%v), want 1", n, err)
	}
	if seen, _ := s.ReplaySeen("peer", []byte("a")); seen {
		t.Fatal("an expired row survived the purge")
	}
	if seen, _ := s.ReplaySeen("peer", []byte("b")); !seen {
		t.Fatal("an unexpired row was purged")
	}
}

// A write never clears a pinned reason, and eviction takes only unpinned
// rows that no active interaction refers to, oldest first.
func TestPeerIdentityPinsAndEviction(t *testing.T) {
	s := open(t)
	s.SetPeerIdentityCap(2)
	put := func(aid, pin string) {
		if err := s.UpdatePeerIdentity(aid, func(cur *interactions.PeerIdentity) (*interactions.PeerIdentity, error) {
			return &interactions.PeerIdentity{KEL: []byte("kel-" + aid), KELLen: 1, PinnedReason: pin}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("pinned", interactions.PinOutbound)
	put("pinned", "") // a later unpinned write keeps the pin
	if row, _ := s.PeerIdentity("pinned"); row.PinnedReason != interactions.PinOutbound {
		t.Fatalf("pin cleared: %q", row.PinnedReason)
	}
	if err := s.Put("ix_active", interactions.RoleInbound, "active", "g", "", nil); err != nil {
		t.Fatal(err)
	}
	put("active", "")
	put("old", "")
	put("new1", "")
	put("new2", "")
	for aid, want := range map[string]bool{"pinned": true, "active": true, "old": false, "new1": false, "new2": true} {
		_, err := s.PeerIdentity(aid)
		if got := err == nil; got != want {
			t.Errorf("%s present=%v, want %v", aid, got, want)
		}
	}
}

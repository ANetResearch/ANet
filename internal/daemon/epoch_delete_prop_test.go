package daemon

// FS-5: the early deletion branch of epochKeyDeletable (FS-K7), its gate and
// the properties that hold it (P-1, P-2).

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"
)

func earlyKey(removedAt uint64) *ringKey {
	const na = uint64(1_800_000_000_000)
	return &ringKey{Pair: seal.KeyPair{Public: seal.EncKey{KID: []byte("kid-0123456789ab"), NotBefore: na - 24*hourMS, NotAfter: na}},
		RemovedAt: removedAt}
}

// TestHeldRowKeepsItsKey (mutation 5): a mailbox row sealed to the key that
// the node holds back keeps the key; a row sealed to another key does not.
func TestHeldRowKeepsItsKey(t *testing.T) {
	k := earlyKey(0)
	now := lastAcceptance(k) + drainMarginMS
	if ok, why := epochKeyDeletable(k, now, clockFull, now, kidSet{"kid-0123456789ab": {}}); ok || why != keyHeldMessages {
		t.Fatalf("held row: (%v, %q)", ok, why)
	}
	if ok, why := epochKeyDeletable(k, now, clockFull, now, kidSet{"another-kid": {}}); !ok || why != keyDeletable {
		t.Fatalf("another key's row: (%v, %q)", ok, why)
	}
	if ok, why := epochKeyDeletable(k, now, clockFull, now-1, nil); ok || why != keyMailboxNotDrained {
		t.Fatalf("mailbox read to a moment before A(k)+M: (%v, %q)", ok, why)
	}
}

// TestEarlyDeleteNeedsHubGate (mutation 44): only a full gate lets a key go
// before its hard bound.
func TestEarlyDeleteNeedsHubGate(t *testing.T) {
	k := earlyKey(0)
	now := lastAcceptance(k) + drainMarginMS
	for _, c := range []struct {
		gate clockLevel
		ok   bool
		why  string
	}{{clockFull, true, keyDeletable}, {clockLocal, false, keyClock}, {clockClosed, false, keyClock}} {
		if ok, why := epochKeyDeletable(k, now, c.gate, math.MaxUint64, nil); ok != c.ok || why != c.why {
			t.Errorf("gate %v: (%v, %q), want (%v, %q)", c.gate, ok, why, c.ok, c.why)
		}
	}
	// The boundary: a millisecond before A(k)+M nothing goes early.
	if ok, _ := epochKeyDeletable(k, now-1, clockFull, math.MaxUint64, nil); ok {
		t.Fatal("deleted before A(k) + M")
	}
}

// TestOldHubDrainIgnoresRemovedAt (FS-K6a): while an old hub is drained,
// a removed key waits for not_after + D, since that hub's set still lists it.
func TestOldHubDrainIgnoresRemovedAt(t *testing.T) {
	for _, draining := range []bool{false, true} {
		d := newTestDaemon(t, "", true)
		now := d.nowMS()
		clock := atClock(d, now)
		d.enc.mu.Lock()
		kid := string(d.enc.keys[0].Pair.Public.KID)
		d.enc.keys[0].RemovedAt = now
		d.enc.mu.Unlock()
		clock.Store(now + drainMarginMS + 1)
		if _, err := d.enc.maintain(ringInputs{now: clock.Load(), sign: d.self.Sign, keySeq: d.self.CurrentSeq(),
			gate: clockFull, drainMark: math.MaxUint64, draining: draining}); err != nil {
			t.Fatal(err)
		}
		_, kept := ringKids(d)[kid]
		if kept != draining {
			t.Errorf("draining=%v: key kept=%v", draining, kept)
		}
	}
}

// P-1: the engine's world (engine_release_test.go) with the early branch on
// many seeds, and with a poll that reads the mailbox very often or very
// rarely. No accepted, unexpired envelope loses its key.
func TestP1_EarlyBranchNeverDeletesWhatTheHubHolds(t *testing.T) {
	for seed := uint64(1); seed <= 8; seed++ {
		rng := rand.New(rand.NewPCG(seed, 11))
		for run := range 200 {
			bad := pEng3Run(rng, func(k *ringKey, now, drain uint64, held kidSet) bool {
				ok, _ := epochKeyDeletable(k, now, clockFull, drain, held)
				return ok
			})
			if bad != 0 {
				t.Fatalf("seed %d run %d: key deleted at %d with an accepted, unexpired envelope at the hub", seed, run, bad)
			}
		}
	}
}

// P-2: closed never; local never before the hard bound; before A(k)+M
// never early; full deletes early exactly when drained and nothing is held.
func TestP2_DeletionFunction(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 5))
	kid := "kid-0123456789ab"
	for range 50000 {
		k := earlyKey(0)
		if rng.IntN(2) == 0 {
			k.RemovedAt = k.Pair.Public.NotBefore + rng.Uint64N(3*24*hourMS)
		}
		now := k.Pair.Public.NotBefore + rng.Uint64N(seal.MaxMessageLifetimeMS+3*24*hourMS)
		gate := clockLevel(rng.IntN(3))
		due := satAdd(lastAcceptance(k), drainMarginMS)
		drain := due - 3*hourMS + rng.Uint64N(6*hourMS)
		var held kidSet
		if rng.IntN(3) == 0 {
			held = kidSet{kid: {}}
		}
		ok, _ := epochKeyDeletable(k, now, gate, drain, held)
		hard := now >= k.Pair.Public.NotAfter+seal.MaxMessageLifetimeMS
		want := gate != clockClosed && (hard || (gate == clockFull && now >= due && drain >= due && held == nil))
		if ok != want {
			t.Fatalf("gate %v now %d due %d drain %d held %v: %v, want %v", gate, now, due, drain, held != nil, ok, want)
		}
	}
}

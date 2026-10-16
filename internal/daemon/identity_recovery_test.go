package daemon

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	mrand "math/rand/v2"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
)

// KEL D-15: whether the identity has a recovery passphrase, read from
// local_commit and the KEL alone.

// digestOf is a 32-byte digest made of b.
func digestOf(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// The rules of D-15, one row each, and the rows that must not hold.
func TestRecoveryStateTable(t *testing.T) {
	local, other, next := digestOf(1), digestOf(2), digestOf(3)
	key := digestOf(4)
	for _, c := range []struct {
		name string
		f    recoveryFacts
		rot  *WitnessRot
		want RecoveryState
	}{
		{"not joining yet", recoveryFacts{WitnessPending: true}, nil, RecoveryState{State: RecoveryNone}},
		{"not joining yet, a local_commit left by a join it left", recoveryFacts{WitnessPending: true, LocalCommit: local}, nil,
			RecoveryState{State: RecoveryNone}},
		{"joining without a terminal", recoveryFacts{WitnessPending: true, LocalCommit: local}, &WitnessRot{NextDigest: local},
			RecoveryState{State: RecoveryUnprotected}},
		{"joining with a terminal", recoveryFacts{WitnessPending: true}, &WitnessRot{NextDigest: other},
			RecoveryState{State: RecoveryProtected}},
		{"joined without a terminal, then protected before admission (D-14)", recoveryFacts{WitnessPending: true, LocalCommit: local},
			&WitnessRot{NextDigest: other}, RecoveryState{State: RecoveryProtected}},
		{"witnessed, committed by the seed in the data directory", recoveryFacts{LocalCommit: local, TipNext: local, TipKeyDigest: key},
			nil, RecoveryState{State: RecoveryUnprotected}},
		{"witnessed, its witness_rot still in the record", recoveryFacts{LocalCommit: local, TipNext: local, TipKeyDigest: key},
			&WitnessRot{NextDigest: other}, RecoveryState{State: RecoveryUnprotected}},
		{"the first of protect's rotations done", recoveryFacts{LocalCommit: local, TipNext: next, TipKeyDigest: local}, nil,
			RecoveryState{State: RecoveryUnprotected, Resume: true}},
		{"both done", recoveryFacts{LocalCommit: local, TipNext: next, TipKeyDigest: other}, nil,
			RecoveryState{State: RecoveryProtected}},
		{"joined with a terminal", recoveryFacts{TipNext: next, TipKeyDigest: key}, nil, RecoveryState{State: RecoveryProtected}},
		{"a dip revealing the key the seed in the data directory made", recoveryFacts{LocalCommit: local, TipKeyDigest: local}, nil,
			RecoveryState{State: RecoveryUnprotected, Resume: true}},
		{"witness pending: the icp's commitment is the hot key's, never local_commit", recoveryFacts{WitnessPending: true,
			LocalCommit: local, TipNext: next, TipKeyDigest: key}, &WitnessRot{NextDigest: local}, RecoveryState{State: RecoveryUnprotected}},
	} {
		if got := recoveryStateOf(c.f, c.rot); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// The reminder follows the state: none while protected or not joining, and
// none once the identity is stopped or deactivated (ux §1.4 E3).
func TestTheReminderFollowsTheRecoveryState(t *testing.T) {
	for _, c := range []struct {
		r     RecoveryState
		state string
		want  string
	}{
		{RecoveryState{State: RecoveryUnprotected}, IdentityOK, reasonUnprotected},
		{RecoveryState{State: RecoveryUnprotected}, IdentityNoHub, reasonUnprotected},
		{RecoveryState{State: RecoveryUnprotected}, IdentityPublishing, reasonUnprotected},
		{RecoveryState{State: RecoveryUnprotected}, IdentityNoticeStale, reasonUnprotected},
		{RecoveryState{State: RecoveryUnprotected, Resume: true}, IdentityOK, reasonUnprotectedResume},
		{RecoveryState{State: RecoveryUnprotected}, IdentityStopped, ""},
		{RecoveryState{State: RecoveryUnprotected, Resume: true}, IdentityDeactivated, ""},
		{RecoveryState{State: RecoveryProtected}, IdentityOK, ""},
		{RecoveryState{State: RecoveryNone}, IdentityNoHub, ""},
	} {
		if got := c.r.reminderKey(c.state); got != c.want {
			t.Errorf("%+v in %s: %q, want %q", c.r, c.state, got, c.want)
		}
	}
}

// recoveryRun is one identity's history as KEL D-11, D-10, D-14 and D-16
// let it happen, for P10: who made each commitment, so that what the state
// must be is known without the rules recoveryStateOf applies.
type recoveryRun struct {
	c        *identity.Controller
	local    [32]byte // the seed kept without a passphrase in the data directory
	localSet bool     // local_commit was written (a join without a terminal)
	rot      *WitnessRot
	// seedOf[j] is the seed whose key j is (the seq that reveals it); local
	// marks the seeds of the data directory.
	seedOf  map[uint64][32]byte
	isLocal map[[32]byte]bool
	ts      uint64
}

// newSeed is a fresh recovery seed.
func newSeed(t testing.TB) [32]byte {
	var rs [32]byte
	if _, err := rand.Read(rs[:]); err != nil {
		t.Fatal(err)
	}
	return rs
}

// commit is the next_digest of seed rs for seq j, and records whose it is.
func (r *recoveryRun) commit(t testing.TB, rs [32]byte, j uint64) []byte {
	t.Helper()
	d, err := identity.NextDigest(rs, r.c.AID(), j)
	if err != nil {
		t.Fatal(err)
	}
	r.seedOf[j] = rs
	return d[:]
}

// runRecoveryProgram plays a history chosen by next: a join without a
// terminal or with one, a passphrase set before admission or not, the
// witness designation or not, then up to three rotations, each committing
// to a passphrase seed: the one the last committed key came from, or a new
// one — never the data directory's again, which `anet identity protect`
// and every rotation of an unprotected identity leave behind (KEL D-16,
// D-31a). It returns the run and whether the current key (fromLocalKey)
// or the next one (fromLocalNext) comes from the data directory's seed.
func runRecoveryProgram(t testing.TB, next func(n int) int) (r *recoveryRun, fromLocalNext, fromLocalKey bool) {
	t.Helper()
	c, err := identity.InceptAgent(1)
	if err != nil {
		t.Fatal(err)
	}
	r = &recoveryRun{c: c, local: newSeed(t), seedOf: map[uint64][32]byte{}, isLocal: map[[32]byte]bool{}, ts: 1}
	r.isLocal[r.local] = true
	pass := newSeed(t)
	switch next(4) {
	case 0: // not joining yet
		if next(2) == 0 {
			// a join that was left again: local_commit stays, the record goes
			if err := c.SetLocalCommit(r.commit(t, r.local, 2)); err != nil {
				t.Fatal(err)
			}
			r.localSet = true
		}
		return r, false, false
	case 1: // a join with a terminal (D-10)
		r.rot = &WitnessRot{NextDigest: r.commit(t, pass, 2), HubAID: identitytest.Witness}
	default: // a join without a terminal (D-11)
		d := r.commit(t, r.local, 2)
		if err := c.SetLocalCommit(d); err != nil {
			t.Fatal(err)
		}
		r.localSet = true
		r.rot = &WitnessRot{NextDigest: d, HubAID: identitytest.Witness}
		if next(3) == 0 {
			// protect before admission: the commitment is replaced (D-14)
			r.rot = &WitnessRot{NextDigest: r.commit(t, pass, 2), HubAID: identitytest.Witness}
		}
	}
	committed := r.seedOf[2]
	if next(4) == 0 {
		// not admitted yet: the witness is pending
		return r, r.isLocal[committed], false
	}
	r.ts++
	ev, err := c.BuildWitnessDesignation(identitytest.Witness, r.rot.NextDigest, r.ts)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	keyLocal := false // the hot key of the icp
	for j := uint64(2); j < 2+uint64(next(4)); j++ {
		revealed, err := identity.RecoverySeed(r.seedOf[j], c.AID(), j)
		if err != nil {
			t.Fatal(err)
		}
		if next(3) == 0 {
			pass = newSeed(t)
		}
		r.ts++
		ev, err := c.BuildRotation(revealed, r.commit(t, pass, j+1), identitytest.Witness, next(2) == 0, r.ts)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Adopt(ev); err != nil {
			t.Fatal(err)
		}
		keyLocal = r.isLocal[r.seedOf[j]]
		committed = r.seedOf[j+1]
	}
	return r, r.isLocal[committed], keyLocal
}

// checkRecoveryRun is P10 on one history: unprotected exactly when the next
// key or the current one comes from the seed in the data directory, resume
// exactly when only the current one does; none before the join begins.
func checkRecoveryRun(t testing.TB, r *recoveryRun, fromLocalNext, fromLocalKey bool) {
	t.Helper()
	got := recoveryStateOf(recoveryFactsOf(r.c), r.rot)
	facts := recoveryFactsOf(r.c)
	var want RecoveryState
	switch {
	case facts.WitnessPending && r.rot == nil:
		want = RecoveryState{State: RecoveryNone}
	case fromLocalNext:
		want = RecoveryState{State: RecoveryUnprotected}
	case fromLocalKey:
		want = RecoveryState{State: RecoveryUnprotected, Resume: true}
	default:
		want = RecoveryState{State: RecoveryProtected}
	}
	if got != want {
		t.Fatalf("recoveryStateOf = %+v, want %+v (KEL of %d events, local_commit %v, witness_rot %v, next from the local seed %v, key from it %v)",
			got, want, len(r.c.Accepted()), r.localSet, r.rot != nil, fromLocalNext, fromLocalKey)
	}
}

// P10 (KEL §7.2): over random histories of joins and rotations, the state
// is unprotected exactly while the data directory's seed makes the current
// or the next key; after the two rotations of protect it is protected, and
// after only the first it is unprotected with Resume.
func TestRecoveryStateFollowsTheKEL(t *testing.T) {
	rng := mrand.New(mrand.NewPCG(10, 15))
	for i := 0; i < 300; i++ {
		r, nextLocal, keyLocal := runRecoveryProgram(t, func(n int) int { return rng.IntN(n) })
		checkRecoveryRun(t, r, nextLocal, keyLocal)
	}
}

// The two rotations of `anet identity protect` (KEL D-16), step by step:
// unprotected, then unprotected with Resume after the first (the reminder
// becomes identity.unprotected.resume), then protected — each read from the
// identity file written to disk.
func TestProtectsTwoRotationsStepByStep(t *testing.T) {
	l := NewLayout(t.TempDir())
	c, err := identity.InceptAgent(1)
	if err != nil {
		t.Fatal(err)
	}
	local := newSeed(t)
	d2, err := identity.NextDigest(local, c.AID(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetLocalCommit(d2[:]); err != nil {
		t.Fatal(err)
	}
	ev, err := c.BuildWitnessDesignation(identitytest.Witness, d2[:], 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	read := func(step string, want RecoveryState) {
		t.Helper()
		if err := saveIdentity(l, c); err != nil {
			t.Fatal(err)
		}
		back, err := LoadOrGenerateIdentity(l)
		if err != nil {
			t.Fatal(err)
		}
		if got := recoveryStateOf(recoveryFactsOf(back), nil); got != want {
			t.Fatalf("%s: %+v, want %+v", step, got, want)
		}
	}
	read("joined without a terminal", RecoveryState{State: RecoveryUnprotected})
	pass := newSeed(t)
	k2, err := identity.RecoverySeed(local, c.AID(), 2)
	if err != nil {
		t.Fatal(err)
	}
	d3, err := identity.NextDigest(pass, c.AID(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if ev, err = c.BuildRotation(k2, d3[:], identitytest.Witness, false, 3); err != nil {
		t.Fatal(err)
	}
	read("the first rotation built, not accepted", RecoveryState{State: RecoveryUnprotected})
	if err := c.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	read("the first rotation accepted", RecoveryState{State: RecoveryUnprotected, Resume: true})
	k3, err := identity.RecoverySeed(pass, c.AID(), 3)
	if err != nil {
		t.Fatal(err)
	}
	d4, err := identity.NextDigest(pass, c.AID(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if ev, err = c.BuildRotation(k3, d4[:], identitytest.Witness, false, 4); err != nil {
		t.Fatal(err)
	}
	if err := c.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	read("both rotations accepted", RecoveryState{State: RecoveryProtected})
	if !bytes.Equal(c.LocalCommit(), d2[:]) {
		t.Fatal("local_commit was removed; it stays (KEL §2.9)")
	}
	h := sha256.Sum256(c.Accepted()[3].Event.Keys[0])
	if bytes.Equal(h[:], d2[:]) || bytes.Equal(c.Accepted()[3].Event.NextDigest, d2[:]) {
		t.Fatal("the test's history is wrong")
	}
}

// FuzzRecoveryState is P10 driven by the fuzzer's bytes: each byte chooses
// the next step of a history (runRecoveryProgram).
func FuzzRecoveryState(f *testing.F) {
	for _, s := range [][]byte{{}, {0}, {1, 1}, {2, 1, 1, 0, 1, 0}, {3, 2, 1, 3, 0, 1, 2, 1, 0}, {2, 0, 1, 2, 0, 0, 0, 1}} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, prog []byte) {
		i := 0
		next := func(n int) int {
			if i >= len(prog) {
				return 0
			}
			v := int(prog[i]) % n
			i++
			return v
		}
		r, nextLocal, keyLocal := runRecoveryProgram(t, next)
		checkRecoveryRun(t, r, nextLocal, keyLocal)
	})
}

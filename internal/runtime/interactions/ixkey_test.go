package interactions_test

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

const ixPeer = "did:anet:peer"

// lifetimeL is L, how long a local end keeps the requester's key (FS §0).
const lifetimeL = int64(seal.MaxMessageLifetimeMS)

// newIXKey makes a fresh suite-1 key pair for task ix.
func newIXKey(t *testing.T, ix string) interactions.IXKey {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return interactions.IXKey{IX: ix, PeerAID: ixPeer, Suite: seal.SuiteX25519,
		Pub: priv.PublicKey().Bytes(), Priv: priv.Bytes()}
}

// keyedTask records task id in role with this node's key for it, and returns the key.
func keyedTask(t *testing.T, s *interactions.Store, id string, role interactions.Role) interactions.IXKey {
	t.Helper()
	k := newIXKey(t, id)
	if err := s.Update(func(tx *interactions.Tx) error {
		if err := tx.Create(interactions.New{ID: id, Role: role, PeerAID: ixPeer, Goal: "g"}); err != nil {
			return err
		}
		return tx.PutIXKey(k)
	}); err != nil {
		t.Fatalf("record %s with its key: %v", id, err)
	}
	k.KID = seal.KID(k.Suite, k.Pub)
	return k
}

// stored reads the key k back by its KID, nil when it is gone.
func stored(t *testing.T, s *interactions.Store, k interactions.IXKey) *interactions.IXKey {
	t.Helper()
	got, err := s.IXKeyByKID(k.KID)
	if err != nil {
		t.Fatalf("IXKeyByKID(%s): %v", k.IX, err)
	}
	return got
}

func state(t *testing.T, s *interactions.Store, id string) (interactions.State, int64) {
	t.Helper()
	ix, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return ix.State, ix.StateSeq
}

var errRollback = errors.New("the rest of the transaction failed")

// FS-X7, mutation 13: the key goes with the commit of the write that ends the task, not before. A
// transaction that ends the task and then fails leaves the key in place, so the redelivered message
// that the failure left unacknowledged can still be opened; the one that commits deletes it.
func TestIXKeyDeletedOnlyWithCommit(t *testing.T) {
	s := open(t)

	// The provider stores its result; the rest of the result's transaction (the outbox row, the
	// replay row) fails.
	prov := keyedTask(t, s, "ix_prov", interactions.RoleInbound)
	err := s.Update(func(tx *interactions.Tx) error {
		if err := tx.Finish("ix_prov", interactions.Finish{State: interactions.StateCompleted, Result: []byte("r"),
			Verified: interactions.VerificationVerified}, interactions.CauseProviderEnd); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("Update = %v", err)
	}
	if got := stored(t, s, prov); got == nil || !bytes.Equal(got.Priv, prov.Priv) {
		t.Fatal("a result transaction that rolled back took the provider's key with it")
	}
	if st, _ := state(t, s, "ix_prov"); st != interactions.StateSubmitted {
		t.Fatalf("state after the rollback = %s", st)
	}

	// The requester canceled; the provider's late result lands in a transaction that fails.
	req := keyedTask(t, s, "ix_req", interactions.RoleOutbound)
	if _, err := s.SetState("ix_req", interactions.StateCanceled, interactions.CauseLocalEnded); err != nil {
		t.Fatal(err)
	}
	err = s.Update(func(tx *interactions.Tx) error {
		if _, err := tx.SetLateResult("ix_req", []byte("r"), "cid", []byte("rc"), interactions.VerificationVerified,
			interactions.CausePeerEnded); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("Update = %v", err)
	}
	if stored(t, s, req) == nil {
		t.Fatal("a late result that rolled back took the requester's key with it")
	}

	// Committed, the same writes delete the keys.
	if err := s.Finish("ix_prov", interactions.Finish{State: interactions.StateCompleted, Result: []byte("r"),
		Verified: interactions.VerificationVerified}, interactions.CauseProviderEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLateResult("ix_req", []byte("r"), "cid", []byte("rc"), interactions.VerificationVerified,
		interactions.CausePeerEnded); err != nil {
		t.Fatal(err)
	}
	for _, k := range []interactions.IXKey{prov, req} {
		if got := stored(t, s, k); got != nil {
			t.Errorf("%s: key still there after the committed end", k.IX)
		}
		if rk, err := s.OwnRK(k.IX); err != nil || rk != nil {
			t.Errorf("%s: OwnRK = %v, %v after the key went", k.IX, rk, err)
		}
	}
}

// FS-X7–X9: each terminal write settles the key as the table says, in its own transaction; a write to a
// state that is not terminal leaves it.
func TestIXKeyFollowsEachTerminalWrite(t *testing.T) {
	const now = int64(1_700_000_000_000)
	type step func(s *interactions.Store, id string) error
	finish := func(st interactions.State, c interactions.Cause) step {
		return func(s *interactions.Store, id string) error {
			return s.Finish(id, interactions.Finish{State: st, Verified: interactions.VerificationUnknown}, c)
		}
	}
	set := func(st interactions.State, c interactions.Cause) step {
		return func(s *interactions.Store, id string) error {
			_, err := s.SetState(id, st, c)
			return err
		}
	}
	cases := []struct {
		name       string
		role       interactions.Role
		steps      []step
		kept       bool
		closeAfter int64
	}{
		{"provider completes", interactions.RoleInbound, []step{finish(interactions.StateCompleted, interactions.CauseProviderEnd)}, false, 0},
		{"provider fails", interactions.RoleInbound, []step{set(interactions.StateFailed, interactions.CauseProviderEnd)}, false, 0},
		{"provider is canceled", interactions.RoleInbound, []step{set(interactions.StateCanceled, interactions.CauseProviderEnd)}, false, 0},
		{"provider rejects", interactions.RoleInbound, []step{set(interactions.StateRejected, interactions.CauseProviderEnd)}, false, 0},
		{"provider works", interactions.RoleInbound, []step{set(interactions.StateWorking, interactions.CauseNotTerminal)}, true, 0},
		{"provider quotes", interactions.RoleInbound, []step{finish(interactions.StateInputRequired, interactions.CauseNotTerminal)}, true, 0},
		{"result arrives", interactions.RoleOutbound, []step{finish(interactions.StateCompleted, interactions.CausePeerEnded)}, false, 0},
		{"terminal status arrives", interactions.RoleOutbound, []step{set(interactions.StateFailed, interactions.CausePeerEnded)}, false, 0},
		{"refused (outcome 2)", interactions.RoleOutbound, []step{set(interactions.StateRejected, interactions.CausePeerEnded)}, false, 0},
		{"canceled here", interactions.RoleOutbound, []step{set(interactions.StateCanceled, interactions.CauseLocalEnded)}, true, now + lifetimeL},
		{"no response", interactions.RoleOutbound, []step{finish(interactions.StateFailed, interactions.CauseLocalEnded)}, true, now + lifetimeL},
		{"never sent", interactions.RoleOutbound, []step{set(interactions.StateCanceled, interactions.CauseNeverSent)}, false, 0},
		{"closed", interactions.RoleOutbound, []step{set(interactions.StateFailed, interactions.CauseClosed)}, false, 0},
		{"closed provider", interactions.RoleInbound, []step{set(interactions.StateFailed, interactions.CauseClosed)}, false, 0},
		{"quote received", interactions.RoleOutbound, []step{finish(interactions.StateInputRequired, interactions.CauseNotTerminal)}, true, 0},
		// J8: a local cancel keeps the key for the late result, which then takes it.
		{"cancel then late result", interactions.RoleOutbound, []step{
			set(interactions.StateCanceled, interactions.CauseLocalEnded),
			func(s *interactions.Store, id string) error {
				_, err := s.SetLateResult(id, []byte("r"), "cid", []byte("rc"), interactions.VerificationVerified, interactions.CausePeerEnded)
				return err
			}}, false, 0},
		// The provider's terminal status on a task canceled here: the state stays, the key goes.
		{"cancel then terminal status", interactions.RoleOutbound, []step{
			set(interactions.StateCanceled, interactions.CauseLocalEnded),
			set(interactions.StateFailed, interactions.CausePeerEnded)}, false, 0},
		// A result on a task that ended here is ErrTerminal, and is still the provider's answer.
		{"cancel then result", interactions.RoleOutbound, []step{
			set(interactions.StateCanceled, interactions.CauseLocalEnded),
			func(s *interactions.Store, id string) error {
				err := finish(interactions.StateCompleted, interactions.CausePeerEnded)(s, id)
				if errors.Is(err, interactions.ErrTerminal) {
					return nil
				}
				return errors.New("a result on a canceled task was not ErrTerminal")
			}}, false, 0},
		// A second local end does not lengthen the first.
		{"two local ends", interactions.RoleOutbound, []step{
			set(interactions.StateCanceled, interactions.CauseLocalEnded),
			func(s *interactions.Store, id string) error {
				s.SetClock(func() int64 { return now + 1000 })
				defer s.SetClock(func() int64 { return now })
				return finish(interactions.StateFailed, interactions.CauseLocalEnded)(s, id)
			}}, true, now + lifetimeL},
		// The key outlives a late result in a task still open: SetLateResult leaves an open task alone.
		{"late result on an open task", interactions.RoleOutbound, []step{
			func(s *interactions.Store, id string) error {
				wrote, err := s.SetLateResult(id, []byte("r"), "cid", []byte("rc"), interactions.VerificationVerified, interactions.CausePeerEnded)
				if wrote {
					return errors.New("a late result was written on an open task")
				}
				return err
			}}, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := open(t)
			s.SetClock(func() int64 { return now })
			k := keyedTask(t, s, "ix", c.role)
			for i, st := range c.steps {
				if err := st(s, "ix"); err != nil && !errors.Is(err, interactions.ErrTerminal) {
					t.Fatalf("step %d: %v", i, err)
				}
			}
			got := stored(t, s, k)
			if (got != nil) != c.kept {
				t.Fatalf("key kept = %v, want %v", got != nil, c.kept)
			}
			if got != nil && got.CloseAfter != c.closeAfter {
				t.Fatalf("close_after = %d, want %d", got.CloseAfter, c.closeAfter)
			}
		})
	}
}

// Every state write takes a Cause; the zero value, an unknown or misplaced one and one on the wrong side
// of the task are ErrBadInput, and nothing is written — not the state, not the result, not the key.
func TestTerminalWritesRequireCause(t *testing.T) {
	s := open(t)
	prov := keyedTask(t, s, "ix_prov", interactions.RoleInbound)
	req := keyedTask(t, s, "ix_req", interactions.RoleOutbound)
	if _, err := s.SetState("ix_req", interactions.StateCanceled, interactions.CauseLocalEnded); err != nil {
		t.Fatal(err)
	}
	type write struct {
		name string
		do   func(id string, c interactions.Cause) error
	}
	viaTx := func(fn func(tx *interactions.Tx) error) error { return s.Update(fn) }
	done := interactions.Finish{State: interactions.StateCompleted, Result: []byte("r"), Verified: interactions.VerificationVerified}
	terminal := []write{
		{"SetState", func(id string, c interactions.Cause) error {
			_, err := s.SetState(id, interactions.StateFailed, c)
			return err
		}},
		{"Finish", func(id string, c interactions.Cause) error { return s.Finish(id, done, c) }},
		{"SetResult", func(id string, c interactions.Cause) error {
			return s.SetResult(id, []byte("r"), "cid", []byte("rc"), interactions.VerificationVerified, c)
		}},
		{"SetFailed", func(id string, c interactions.Cause) error { return s.SetFailed(id, []byte("d"), c) }},
		{"SetLateResult", func(id string, c interactions.Cause) error {
			_, err := s.SetLateResult(id, []byte("r"), "cid", []byte("rc"), interactions.VerificationVerified, c)
			return err
		}},
		{"Tx.SetState", func(id string, c interactions.Cause) error {
			return viaTx(func(tx *interactions.Tx) error { _, err := tx.SetState(id, interactions.StateRejected, c); return err })
		}},
		{"Tx.Finish", func(id string, c interactions.Cause) error {
			return viaTx(func(tx *interactions.Tx) error { return tx.Finish(id, done, c) })
		}},
		{"Tx.SetResult", func(id string, c interactions.Cause) error {
			return viaTx(func(tx *interactions.Tx) error {
				return tx.SetResult(id, []byte("r"), "cid", []byte("rc"), interactions.VerificationVerified, c)
			})
		}},
		{"Tx.SetFailed", func(id string, c interactions.Cause) error {
			return viaTx(func(tx *interactions.Tx) error { return tx.SetFailed(id, []byte("d"), c) })
		}},
		{"Tx.SetLateResult", func(id string, c interactions.Cause) error {
			return viaTx(func(tx *interactions.Tx) error {
				_, err := tx.SetLateResult(id, []byte("r"), "cid", []byte("rc"), interactions.VerificationVerified, c)
				return err
			})
		}},
	}
	open := []write{
		{"SetState", func(id string, c interactions.Cause) error {
			_, err := s.SetState(id, interactions.StateWorking, c)
			return err
		}},
		{"Finish", func(id string, c interactions.Cause) error {
			return s.Finish(id, interactions.Finish{State: interactions.StateInputRequired, Verified: interactions.VerificationUnknown}, c)
		}},
		{"Tx.SetState", func(id string, c interactions.Cause) error {
			return viaTx(func(tx *interactions.Tx) error {
				_, err := tx.SetState(id, interactions.StateInputRequired, c)
				return err
			})
		}},
	}
	refusedTerminal := map[string][]interactions.Cause{
		// the provider's task: nothing a requester would say, and nothing that is not an end
		"ix_prov": {"", "bogus", interactions.CauseNotTerminal, interactions.CauseRetention, interactions.CausePeerEnded,
			interactions.CauseLocalEnded, interactions.CauseNeverSent},
		"ix_req": {"", "bogus", interactions.CauseNotTerminal, interactions.CauseRetention, interactions.CauseProviderEnd},
	}
	refusedOpen := []interactions.Cause{"", "bogus", interactions.CauseRetention, interactions.CauseProviderEnd,
		interactions.CausePeerEnded, interactions.CauseLocalEnded, interactions.CauseNeverSent, interactions.CauseClosed}
	_, provSeq := state(t, s, "ix_prov")
	_, reqSeq := state(t, s, "ix_req")
	check := func(what string) {
		t.Helper()
		if st, seq := state(t, s, "ix_prov"); st != interactions.StateSubmitted || seq != provSeq {
			t.Fatalf("%s: the provider's task moved to %s (seq %d)", what, st, seq)
		}
		if st, seq := state(t, s, "ix_req"); st != interactions.StateCanceled || seq != reqSeq {
			t.Fatalf("%s: the requester's task moved to %s (seq %d)", what, st, seq)
		}
		if ix, _ := s.Get("ix_req"); len(ix.Receipt) != 0 {
			t.Fatalf("%s: a late result was stored", what)
		}
		if stored(t, s, prov) == nil || stored(t, s, req) == nil {
			t.Fatalf("%s: a refused write took a key", what)
		}
	}
	for _, w := range terminal {
		for id, causes := range refusedTerminal {
			for _, c := range causes {
				if err := w.do(id, c); !errors.Is(err, interactions.ErrBadInput) {
					t.Errorf("%s(%s, %q) = %v, want ErrBadInput", w.name, id, c, err)
				}
				check(w.name + "/" + string(c))
			}
		}
	}
	for _, w := range open {
		for _, c := range refusedOpen {
			if err := w.do("ix_prov", c); !errors.Is(err, interactions.ErrBadInput) {
				t.Errorf("%s(ix_prov, %q) to an open state = %v, want ErrBadInput", w.name, c, err)
			}
			check(w.name + "/" + string(c))
		}
	}
	// An absent task is ErrNotFound with any cause that could apply.
	if _, err := s.SetState("ix_absent", interactions.StateFailed, interactions.CauseClosed); !errors.Is(err, interactions.ErrNotFound) {
		t.Errorf("SetState on an absent task = %v", err)
	}
	if _, err := s.SetLateResult("ix_absent", nil, "", nil, interactions.VerificationUnknown, interactions.CausePeerEnded); !errors.Is(err, interactions.ErrNotFound) {
		t.Errorf("SetLateResult on an absent task = %v", err)
	}
}

// FS-X9: the close_after sweep deletes the keys whose time has come, and only while the clock gate is
// open (ClockFull or ClockLocal); at ClockClosed — the zero value — it waits.
func TestCloseAfterSweepGated(t *testing.T) {
	s := open(t)
	t0 := int64(1_700_000_000_000)
	s.SetClock(func() int64 { return t0 })
	early := keyedTask(t, s, "ix_early", interactions.RoleOutbound)
	if _, err := s.SetState("ix_early", interactions.StateCanceled, interactions.CauseLocalEnded); err != nil {
		t.Fatal(err)
	}
	s.SetClock(func() int64 { return t0 + 3_600_000 })
	late := keyedTask(t, s, "ix_late", interactions.RoleOutbound)
	if _, err := s.SetState("ix_late", interactions.StateCanceled, interactions.CauseLocalEnded); err != nil {
		t.Fatal(err)
	}
	live := keyedTask(t, s, "ix_live", interactions.RoleOutbound)

	sweep := func(now int64, level interactions.ClockLevel, want int64) {
		t.Helper()
		n, err := s.SweepCloseAfter(now, level)
		if err != nil || n != want {
			t.Fatalf("SweepCloseAfter(%d, %d) = %d, %v; want %d", now, level, n, err, want)
		}
	}
	due := t0 + lifetimeL
	sweep(due-1, interactions.ClockFull, 0) // not yet
	var unknown interactions.ClockLevel
	sweep(due, unknown, 0) // a caller that does not know the level deletes nothing
	sweep(due+lifetimeL, interactions.ClockClosed, 0)
	if stored(t, s, early) == nil {
		t.Fatal("the closed gate let a key go")
	}
	sweep(due, interactions.ClockLocal, 1)
	if stored(t, s, early) != nil || stored(t, s, late) == nil {
		t.Fatal("the local-clock sweep took the wrong key")
	}
	sweep(due+3_600_000, interactions.ClockFull, 1)
	if stored(t, s, late) != nil {
		t.Fatal("the full-gate sweep left a key whose time had come")
	}
	sweep(due+100*lifetimeL, interactions.ClockFull, 0)
	if stored(t, s, live) == nil {
		t.Fatal("the sweep took the key of an open task")
	}
}

// FS-X10: closing the open tasks (emergency rotation, rollback, deactivation, stopped) ends them with
// CauseClosed and deletes every key, those a local end keeps included, in that one transaction.
func TestCloseInteractionsDeletesAllIXKeys(t *testing.T) {
	s := open(t)
	keys := []interactions.IXKey{
		keyedTask(t, s, "ix_prov", interactions.RoleInbound),
		keyedTask(t, s, "ix_req", interactions.RoleOutbound),
		keyedTask(t, s, "ix_kept", interactions.RoleOutbound),
	}
	if _, err := s.SetState("ix_kept", interactions.StateCanceled, interactions.CauseLocalEnded); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(interactions.New{ID: "ix_nokey", Role: interactions.RoleOutbound, PeerAID: ixPeer, Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	closeAll := func(tx *interactions.Tx) (int64, error) {
		for _, id := range []string{"ix_prov", "ix_req", "ix_nokey"} {
			if _, err := tx.SetState(id, interactions.StateFailed, interactions.CauseClosed); err != nil {
				return 0, err
			}
		}
		return tx.DeleteAllIXKeys()
	}
	err := s.Update(func(tx *interactions.Tx) error {
		if _, err := closeAll(tx); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	for _, k := range keys {
		if stored(t, s, k) == nil {
			t.Fatalf("%s: a closing that rolled back took the key", k.IX)
		}
	}
	var n int64
	if err := s.Update(func(tx *interactions.Tx) error {
		var err error
		n, err = closeAll(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The two open tasks' keys went with their CauseClosed write; DeleteAllIXKeys took the one kept on
	// close_after.
	if n != 1 {
		t.Fatalf("DeleteAllIXKeys deleted %d, want the 1 left after the closing writes", n)
	}
	for _, k := range keys {
		if stored(t, s, k) != nil {
			t.Fatalf("%s: key left after the tasks were closed", k.IX)
		}
	}
	for _, id := range []string{"ix_prov", "ix_req", "ix_nokey"} {
		if st, _ := state(t, s, id); st != interactions.StateFailed {
			t.Fatalf("%s: %s after closing", id, st)
		}
	}
}

// CloseIXKey settles the key where no state write does: the provider's terminal message on a task this
// node has ended. It refuses an open task and a non-terminal cause.
func TestCloseIXKey(t *testing.T) {
	s := open(t)
	req := keyedTask(t, s, "ix_req", interactions.RoleOutbound)
	call := func(id string, c interactions.Cause) error {
		return s.Update(func(tx *interactions.Tx) error { return tx.CloseIXKey(id, c) })
	}
	for _, c := range []interactions.Cause{"", interactions.CauseNotTerminal, interactions.CausePeerEnded} {
		if err := call("ix_req", c); !errors.Is(err, interactions.ErrBadInput) {
			t.Fatalf("CloseIXKey on an open task, %q: %v", c, err)
		}
	}
	if stored(t, s, req) == nil {
		t.Fatal("CloseIXKey took the key of an open task")
	}
	if _, err := s.SetState("ix_req", interactions.StateCanceled, interactions.CauseLocalEnded); err != nil {
		t.Fatal(err)
	}
	if err := call("ix_req", interactions.CauseProviderEnd); !errors.Is(err, interactions.ErrBadInput) {
		t.Fatalf("the provider's cause on the requester's task: %v", err)
	}
	if err := call("ix_req", interactions.CausePeerEnded); err != nil {
		t.Fatal(err)
	}
	if stored(t, s, req) != nil {
		t.Fatal("the provider's answer left the requester's key")
	}
	if err := call("ix_absent", interactions.CausePeerEnded); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("CloseIXKey on an absent task: %v", err)
	}
	if err := call("ix_absent", interactions.CauseRetention); err != nil {
		t.Fatalf("CloseIXKey(retention) on a task already deleted: %v", err)
	}
}

// X7e: the retention sweep takes a key still kept for a task (a local end waiting on close_after) with
// the task, and leaves the keys of the tasks it does not delete.
func TestRetentionDeletesResidualIXKey(t *testing.T) {
	s := open(t)
	t0 := int64(1_700_000_000_000)
	s.SetClock(func() int64 { return t0 })
	keyed := func(id, trust string) interactions.IXKey {
		k := newIXKey(t, id)
		if err := s.Update(func(tx *interactions.Tx) error {
			if err := tx.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: ixPeer, Goal: "g",
				Trust: trust}); err != nil {
				return err
			}
			return tx.PutIXKey(k)
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetState(id, interactions.StateCanceled, interactions.CauseLocalEnded); err != nil {
			t.Fatal(err)
		}
		k.KID = seal.KID(k.Suite, k.Pub)
		return k
	}
	pruned, kept := keyed("ix_pruned", "t"), keyed("ix_kept", "u")
	if stored(t, s, pruned) == nil {
		t.Fatal("the local end did not keep the key")
	}
	n, err := s.PruneTerminal("t", t0+1)
	if err != nil || n.Interactions != 1 {
		t.Fatalf("PruneTerminal = %+v, %v", n, err)
	}
	if stored(t, s, pruned) != nil {
		t.Fatal("the pruned task's key was left behind")
	}
	if stored(t, s, kept) == nil {
		t.Fatal("pruning one class took another task's key")
	}
}

// PutIXKey stores a key once, for an open task, with the task's peer, a suite this build implements and
// the KID derived from the key; anything else is refused and nothing is stored.
func TestPutIXKey(t *testing.T) {
	s := open(t)
	if err := s.Create(interactions.New{ID: "ix", Role: interactions.RoleInbound, PeerAID: ixPeer, Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(interactions.New{ID: "ix_done", Role: interactions.RoleInbound, PeerAID: ixPeer, Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetState("ix_done", interactions.StateCompleted, interactions.CauseProviderEnd); err != nil {
		t.Fatal(err)
	}
	put := func(k interactions.IXKey) error {
		return s.Update(func(tx *interactions.Tx) error { return tx.PutIXKey(k) })
	}
	good := newIXKey(t, "ix")
	with := func(f func(k *interactions.IXKey)) interactions.IXKey {
		k := good
		f(&k)
		return k
	}
	for _, c := range []struct {
		name string
		k    interactions.IXKey
		want error
	}{
		{"no task id", with(func(k *interactions.IXKey) { k.IX = "" }), interactions.ErrBadInput},
		{"no peer", with(func(k *interactions.IXKey) { k.PeerAID = "" }), interactions.ErrBadInput},
		{"another peer", with(func(k *interactions.IXKey) { k.PeerAID = "did:anet:other" }), interactions.ErrBadInput},
		{"reserved suite", with(func(k *interactions.IXKey) { k.Suite = seal.SuiteXWing }), interactions.ErrBadInput},
		{"short public key", with(func(k *interactions.IXKey) { k.Pub = k.Pub[:31] }), interactions.ErrBadInput},
		{"no private key", with(func(k *interactions.IXKey) { k.Priv = nil }), interactions.ErrBadInput},
		{"wrong kid", with(func(k *interactions.IXKey) { k.KID = make([]byte, seal.KIDLen) }), interactions.ErrBadInput},
		{"absent task", with(func(k *interactions.IXKey) { k.IX = "ix_absent" }), interactions.ErrNotFound},
		{"ended task", with(func(k *interactions.IXKey) { k.IX = "ix_done" }), interactions.ErrTerminal},
	} {
		if err := put(c.k); !errors.Is(err, c.want) {
			t.Errorf("%s: PutIXKey = %v, want %v", c.name, err, c.want)
		}
	}
	if rk, err := s.OwnRK("ix"); err != nil || rk != nil {
		t.Fatalf("a refused key was stored: %v %v", rk, err)
	}
	// The right KID may be given.
	if err := put(with(func(k *interactions.IXKey) { k.KID = seal.KID(k.Suite, k.Pub) })); err != nil {
		t.Fatal(err)
	}
	if err := put(newIXKey(t, "ix")); !errors.Is(err, interactions.ErrIXKeyExists) {
		t.Fatalf("a second key for the task: %v", err)
	}
	got, err := s.IXKeyByKID(seal.KID(good.Suite, good.Pub))
	if err != nil || got == nil {
		t.Fatalf("IXKeyByKID = %v, %v", got, err)
	}
	if got.IX != "ix" || got.PeerAID != ixPeer || got.Suite != seal.SuiteX25519 || !bytes.Equal(got.Pub, good.Pub) ||
		!bytes.Equal(got.Priv, good.Priv) || got.CloseAfter != 0 || !bytes.Equal(got.KID, seal.KID(good.Suite, good.Pub)) {
		t.Fatalf("stored key = %+v", got)
	}
	rk, err := s.OwnRK("ix")
	if err != nil || rk == nil || rk.Suite != seal.SuiteX25519 || !bytes.Equal(rk.Pub, good.Pub) {
		t.Fatalf("OwnRK = %+v, %v", rk, err)
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		rk, err := tx.OwnRK("ix")
		if err != nil || rk == nil || !bytes.Equal(rk.Pub, good.Pub) {
			t.Fatalf("Tx.OwnRK = %+v, %v", rk, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got.Wipe()
	if !bytes.Equal(got.Priv, make([]byte, len(got.Priv))) {
		t.Fatal("Wipe left the private key")
	}
}

// FS-R1: IXKeyByKID answers (nil, nil) for a kid that is not this node's, and an error — not "no key" —
// when the store cannot be read, so the receive path treats it as temporary.
func TestIXKeyByKIDLookup(t *testing.T) {
	s, err := interactions.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k := keyedTask(t, s, "ix", interactions.RoleOutbound)
	for _, kid := range [][]byte{nil, make([]byte, seal.KIDLen), k.KID[:seal.KIDLen-1], append(append([]byte{}, k.KID...), 0)} {
		if got, err := s.IXKeyByKID(kid); got != nil || err != nil {
			t.Fatalf("IXKeyByKID(%x) = %v, %v; want nil, nil", kid, got, err)
		}
	}
	if got := stored(t, s, k); got == nil {
		t.Fatal("the stored key was not found")
	}
	s.Close()
	if got, err := s.IXKeyByKID(k.KID); err == nil || got != nil {
		t.Fatalf("IXKeyByKID on a store that cannot be read = %v, %v; want an error", got, err)
	}
}

// FS-R4: the peer's first rk is recorded, the same one again is fine, a different one is ErrRKMismatch
// and the recorded one stays. RKConflict is the comparison all three receive points use.
func TestRecordPeerRK(t *testing.T) {
	s := open(t)
	rk1, rk2 := []byte("\xa2\x01\x01\x02\x58\x20rk-one-32-bytes-of-public-key..."), []byte("\xa2\x01\x01\x02\x58\x20rk-two-32-bytes-of-public-key...")
	if err := s.Create(interactions.New{ID: "ix", Role: interactions.RoleOutbound, PeerAID: ixPeer, Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	record := func(id string, rk []byte) error {
		return s.Update(func(tx *interactions.Tx) error { return tx.RecordPeerRK(id, rk) })
	}
	peerRK := func() []byte {
		ix, err := s.Get("ix")
		if err != nil {
			t.Fatal(err)
		}
		return ix.PeerRK
	}
	if peerRK() != nil {
		t.Fatal("a new task has a peer rk")
	}
	if err := record("ix", nil); !errors.Is(err, interactions.ErrBadInput) {
		t.Fatalf("an empty rk: %v", err)
	}
	if err := record("ix", make([]byte, 4096)); !errors.Is(err, interactions.ErrBadInput) {
		t.Fatalf("an oversized rk: %v", err)
	}
	if err := record("ix_absent", rk1); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("an absent task: %v", err)
	}
	// A recording rolled back with its transaction is not kept.
	if err := s.Update(func(tx *interactions.Tx) error {
		if err := tx.RecordPeerRK("ix", rk2); err != nil {
			return err
		}
		return errRollback
	}); !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	if peerRK() != nil {
		t.Fatal("a rolled-back rk was kept")
	}
	if err := record("ix", rk1); err != nil {
		t.Fatal(err)
	}
	if err := record("ix", rk1); err != nil {
		t.Fatalf("the same rk again: %v", err)
	}
	if err := record("ix", rk2); !errors.Is(err, interactions.ErrRKMismatch) {
		t.Fatalf("another rk: %v", err)
	}
	if !bytes.Equal(peerRK(), rk1) {
		t.Fatalf("recorded rk = %x", peerRK())
	}
	for _, c := range []struct {
		recorded, carried []byte
		conflict          bool
	}{
		{nil, nil, false}, {nil, rk1, false}, {rk1, nil, false}, {rk1, rk1, false}, {rk1, rk2, true},
		{rk1, rk1[:len(rk1)-1], true},
	} {
		if got := interactions.RKConflict(c.recorded, c.carried); got != c.conflict {
			t.Errorf("RKConflict(%x, %x) = %v", c.recorded, c.carried, got)
		}
	}
	// The provider records the rk its delegation carried with the task.
	if err := s.Create(interactions.New{ID: "ix_in", Role: interactions.RoleInbound, PeerAID: ixPeer, Goal: "g", PeerRK: rk2}); err != nil {
		t.Fatal(err)
	}
	if ix, _ := s.Get("ix_in"); !bytes.Equal(ix.PeerRK, rk2) {
		t.Fatalf("New.PeerRK not stored: %x", ix.PeerRK)
	}
}

// FS-R4: a held delegation keeps its rk, and the approval carries it to the task in one transaction.
func TestPendingCarriesPeerRK(t *testing.T) {
	s := open(t)
	rk := []byte("\xa2\x01\x01\x02\x58\x20held-delegation-rk-32-bytes-long")
	if err := s.Update(func(tx *interactions.Tx) error {
		if err := tx.PutPending(interactions.PendingItem{IX: "ix_h", FromAID: ixPeer, Delegate: []byte("d"), PeerRK: rk}, 10, 10); err != nil {
			return err
		}
		return tx.PutPending(interactions.PendingItem{IX: "ix_none", FromAID: ixPeer, Delegate: []byte("d")}, 10, 10)
	}); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPending("ix_h")
	if err != nil || !bytes.Equal(p.PeerRK, rk) {
		t.Fatalf("held rk = %x, %v", p.PeerRK, err)
	}
	if p, _ := s.GetPending("ix_none"); p.PeerRK != nil {
		t.Fatalf("a delegation without rk holds %x", p.PeerRK)
	}
	list, err := s.ListPending()
	if err != nil || len(list) != 2 {
		t.Fatalf("ListPending = %d, %v", len(list), err)
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		p, err := tx.GetPending("ix_h")
		if err != nil {
			return err
		}
		if _, err := tx.DeletePending("ix_h"); err != nil {
			return err
		}
		return tx.Create(interactions.New{ID: p.IX, Role: interactions.RoleInbound, PeerAID: p.FromAID, Goal: "g",
			Trust: interactions.TrustApproved, PeerRK: p.PeerRK})
	}); err != nil {
		t.Fatal(err)
	}
	if ix, _ := s.Get("ix_h"); !bytes.Equal(ix.PeerRK, rk) {
		t.Fatalf("approved task rk = %x", ix.PeerRK)
	}
}

// FS-X8 (mutation 16, the secure_delete half): once a key is deleted and the WAL checkpointed, its
// private bytes are in none of the database's files.
func TestDeletedKeyBytesAbsentFromDBFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	k := keyedTask(t, s, "ix", interactions.RoleInbound)
	// Other rows on the same page, as in a running node.
	for _, id := range []string{"ix_a", "ix_b"} {
		keyedTask(t, s, id, interactions.RoleInbound)
	}
	scan := func() []string {
		t.Helper()
		var hits []string
		files, _ := filepath.Glob(filepath.Join(dir, "interactions.db*"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, k.Priv) {
				hits = append(hits, filepath.Base(f))
			}
		}
		return hits
	}
	if err := s.CheckpointTruncate(); err != nil {
		t.Fatal(err)
	}
	if hits := scan(); len(hits) == 0 {
		t.Fatal("the key is not in the database file: the test would prove nothing")
	}
	if err := s.Finish("ix", interactions.Finish{State: interactions.StateCompleted, Verified: interactions.VerificationVerified},
		interactions.CauseProviderEnd); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckpointTruncate(); err != nil {
		t.Fatal(err)
	}
	if hits := scan(); len(hits) != 0 {
		t.Fatalf("the deleted key's private bytes are still in %v", hits)
	}
}

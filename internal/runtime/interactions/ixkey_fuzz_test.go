package interactions_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// FuzzIXKeyLifecycle runs a random sequence of state writes, key settlements, sweeps, rollbacks and clock
// steps over two provider and two requester tasks, and checks after every step what forward secrecy
// needs of the store (FS-X7–X10, P-5 at the store):
//
//   - a key never leaves a task that is still open;
//   - a provider's task that has ended has no key;
//   - a requester's task that has ended keeps its key only on close_after (a local end), and an open
//     task's key has no close_after;
//   - a write that is refused, or whose transaction rolls back, changes nothing;
//   - a sweep with the gate open leaves no key whose close_after has come, and with the gate closed
//     deletes nothing.
func FuzzIXKeyLifecycle(f *testing.F) {
	f.Add([]byte{0, 3, 1, 7, 2, 9, 4, 5, 5, 200, 4, 6})
	f.Add([]byte{6, 0, 3, 1, 0, 1, 0, 4, 5, 255, 4, 2, 3, 8, 1, 9, 2, 0, 7, 3})
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23})
	f.Fuzz(ixKeyLifecycle)
}

// TestIXKeyLifecycleRandom runs FuzzIXKeyLifecycle's checks over seeded random sequences at test speed:
// fuzzing instruments the SQLite driver too and manages a few runs a second. IXKEY_LIFECYCLE_RUNS sets
// the number of sequences (default 20); the seed of a failing one is in the subtest name.
func TestIXKeyLifecycleRandom(t *testing.T) {
	runs := 20
	if v, err := strconv.Atoi(os.Getenv("IXKEY_LIFECYCLE_RUNS")); err == nil && v > 0 {
		runs = v
	}
	base := time.Now().UnixNano()
	if v, err := strconv.ParseInt(os.Getenv("IXKEY_LIFECYCLE_SEED"), 10, 64); err == nil {
		base, runs = v, 1
	}
	for n := 0; n < runs; n++ {
		seed := base + int64(n)
		r := mrand.New(mrand.NewPCG(uint64(seed), 0))
		ops := make([]byte, 40+r.IntN(160))
		for i := range ops {
			ops[i] = byte(r.Uint32())
		}
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) { ixKeyLifecycle(t, ops) })
	}
}

func ixKeyLifecycle(t *testing.T, ops []byte) {
	if len(ops) > 200 {
		ops = ops[:200]
	}
	s := open(t)
	now := int64(1_700_000_000_000)
	s.SetClock(func() int64 { return now })
	ids := []string{"p0", "p1", "r0", "r1"}
	role := map[string]interactions.Role{"p0": interactions.RoleInbound, "p1": interactions.RoleInbound,
		"r0": interactions.RoleOutbound, "r1": interactions.RoleOutbound}
	keys := map[string]interactions.IXKey{}
	for _, id := range ids {
		keys[id] = keyedTask(t, s, id, role[id])
	}
	states := []interactions.State{interactions.StateWorking, interactions.StateInputRequired,
		interactions.StateCompleted, interactions.StateFailed, interactions.StateCanceled, interactions.StateRejected}
	causes := []interactions.Cause{interactions.CauseProviderEnd, interactions.CausePeerEnded,
		interactions.CauseLocalEnded, interactions.CauseNeverSent, interactions.CauseRetention,
		interactions.CauseClosed, interactions.CauseNotTerminal, "", "x"}
	levels := []interactions.ClockLevel{interactions.ClockClosed, interactions.ClockLocal, interactions.ClockFull, 7}

	type row struct {
		state      interactions.State
		seq        int64
		key        bool
		closeAfter int64
		peerRK     string
	}
	snap := func() map[string]row {
		out := map[string]row{}
		for _, id := range ids {
			ix, err := s.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			r := row{state: ix.State, seq: ix.StateSeq, peerRK: string(ix.PeerRK)}
			if k := stored(t, s, keys[id]); k != nil {
				r.key, r.closeAfter = true, k.CloseAfter
			}
			out[id] = r
		}
		return out
	}
	same := func(a, b map[string]row) bool {
		for _, id := range ids {
			if a[id] != b[id] {
				return false
			}
		}
		return true
	}
	next := func(i *int) byte {
		if *i >= len(ops) {
			return 0
		}
		b := ops[*i]
		*i++
		return b
	}
	// write applies one state write, key settlement or rk record in tx.
	write := func(tx *interactions.Tx, i *int) (string, error) {
		id := ids[int(next(i))%len(ids)]
		st := states[int(next(i))%len(states)]
		c := causes[int(next(i))%len(causes)]
		switch next(i) % 5 {
		case 0:
			_, err := tx.SetState(id, st, c)
			return fmt.Sprintf("SetState(%s, %s, %q)", id, st, c), err
		case 1:
			return fmt.Sprintf("Finish(%s, %s, %q)", id, st, c), tx.Finish(id,
				interactions.Finish{State: st, Result: []byte("r"), Verified: interactions.VerificationUnknown}, c)
		case 2:
			_, err := tx.SetLateResult(id, []byte("r"), "cid", []byte("rc"), interactions.VerificationVerified, c)
			return fmt.Sprintf("SetLateResult(%s, %q)", id, c), err
		case 3:
			return fmt.Sprintf("CloseIXKey(%s, %q)", id, c), tx.CloseIXKey(id, c)
		default:
			rk := sha256.Sum256([]byte{next(i) % 2})
			return fmt.Sprintf("RecordPeerRK(%s, %x)", id, rk[:2]), tx.RecordPeerRK(id, rk[:])
		}
	}
	expected := func(err error) bool {
		return err == nil || errors.Is(err, interactions.ErrBadInput) || errors.Is(err, interactions.ErrTerminal) ||
			errors.Is(err, interactions.ErrRKMismatch)
	}
	var history []string
	for i := 0; i < len(ops); {
		before := snap()
		var what string
		switch next(&i) % 5 {
		case 0, 1: // a write, committed unless it failed
			var werr error
			err := s.Update(func(tx *interactions.Tx) error {
				what, werr = write(tx, &i)
				return werr
			})
			if !expected(err) {
				t.Fatalf("%s: %v", what, err)
			}
			if err != nil && !same(before, snap()) {
				t.Fatalf("%s failed (%v) and still changed the store", what, err)
			}
		case 2: // a write whose transaction then fails
			err := s.Update(func(tx *interactions.Tx) error {
				var werr error
				what, werr = write(tx, &i)
				if !expected(werr) {
					t.Fatalf("%s: %v", what, werr)
				}
				return errRollback
			})
			what = "rolled back " + what
			if !errors.Is(err, errRollback) {
				t.Fatalf("%s: Update = %v", what, err)
			}
			if !same(before, snap()) {
				t.Fatalf("%s changed the store", what)
			}
		case 3: // the close_after sweep
			level := levels[int(next(&i))%len(levels)]
			what = fmt.Sprintf("SweepCloseAfter(level %d)", level)
			if _, err := s.SweepCloseAfter(now, level); err != nil {
				t.Fatal(err)
			}
			after := snap()
			open := level == interactions.ClockFull || level == interactions.ClockLocal
			if !open && !same(before, after) {
				t.Fatalf("%s with the gate closed deleted a key", what)
			}
			for _, id := range ids {
				if r := after[id]; open && r.key && r.closeAfter != 0 && r.closeAfter <= now {
					t.Fatalf("%s left %s's key, due at %d (now %d)", what, id, r.closeAfter, now)
				}
			}
		default: // the clock moves on, up to two L
			step := int64(next(&i)) * int64(seal.MaxMessageLifetimeMS) / 128
			now += step
			what = fmt.Sprintf("clock +%d", step)
		}
		history = append(history, what)
		after := snap()
		for _, id := range ids {
			b, a := before[id], after[id]
			switch {
			case b.key && !a.state.IsTerminal() && !a.key:
				t.Fatalf("after %v: the key of open task %s went", history, id)
			case role[id] == interactions.RoleInbound && a.state.IsTerminal() && a.key:
				t.Fatalf("after %v: provider task %s ended (%s) and kept its key", history, id, a.state)
			case role[id] == interactions.RoleOutbound && a.state.IsTerminal() && a.key && a.closeAfter == 0:
				t.Fatalf("after %v: requester task %s ended (%s) and kept its key with no close_after", history, id, a.state)
			case !a.state.IsTerminal() && a.closeAfter != 0:
				t.Fatalf("after %v: open task %s has close_after %d", history, id, a.closeAfter)
			case a.key && b.key && b.closeAfter != 0 && a.closeAfter != b.closeAfter:
				t.Fatalf("after %v: %s's close_after moved from %d to %d", history, id, b.closeAfter, a.closeAfter)
			case a.peerRK != b.peerRK && b.peerRK != "":
				t.Fatalf("after %v: %s's recorded rk changed", history, id)
			case a.key && !bytes.Equal(stored(t, s, keys[id]).Priv, keys[id].Priv):
				t.Fatalf("after %v: %s's key changed", history, id)
			}
		}
	}
}

package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/dirbox"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The tests of a local state rollback (forward-secrecy §4.7, FS-10): what a
// hub's 409 keys_rollback makes this node do, and the publish lock (FS-K5a).

// rollbackPair is a requester and a provider registered at one fake hub,
// the provider serving one task in flight for the requester.
func rollbackPair(t *testing.T) (h *fakeHub, srv *httptest.Server, req, prov *Daemon, task string) {
	t.Helper()
	srv, req, prov = registeredPair(t)
	h = fakeHubAt(t, srv.URL)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "a task in flight", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if ix, err := prov.ix.Get(id); err != nil || ix.IsTerminal() {
		t.Fatalf("setup: the provider's task: %v %v", ix, err)
	}
	return h, srv, req, prov, id
}

// otherCopySet is a key set d's identity signs at seq with keys of its own,
// as a copy of d's data directory running elsewhere, or d before its data
// directory was restored from a backup, signs one.
func otherCopySet(t testing.TB, d *Daemon, seq uint64) []byte {
	t.Helper()
	set := decodedSet(t, d.enc.SignedSet())
	for i := range set.Keys {
		k := &set.Keys[i]
		kp, err := seal.GenerateKeyPair(k.Suite, k.NotBefore, k.NotAfter)
		if err != nil {
			t.Fatal(err)
		}
		*k = kp.Public
	}
	set.Seq, set.IssuedAt = seq, d.nowMS()
	signed, err := seal.SignEncKeySet(&set, d.self.Sign)
	if err != nil {
		t.Fatal(err)
	}
	b, err := signed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// hubHolds makes the fake hub hold signed as aid's key set, in its key
// store and its directory alike: another copy published it.
func hubHolds(h *fakeHub, aid string, signed []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agents[aid].keyset = append([]byte(nil), signed...)
}

// hubSet is the key set the fake hub holds for aid.
func hubSet(h *fakeHub, aid string) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]byte(nil), h.agents[aid].keyset...)
}

// localStateEvents are the anet.local_state.lost records of d's evidence
// log, oldest first.
func localStateEvents(d *Daemon) []map[string]any {
	var out []map[string]any
	d.ledger.scan(EvLocalStateLost, 0, func(_ int64, p map[string]any) { out = append(out, p) })
	return out
}

// regenTagOf is d's ring's regen_tag.
func regenTagOf(d *Daemon) uint64 {
	d.enc.mu.RLock()
	defer d.enc.mu.RUnlock()
	return d.enc.regenTag
}

// failedWith fails the test unless d's task id failed with reason.
func failedWith(t *testing.T, d *Daemon, id, reason string) {
	t.Helper()
	if st, _, got := taskViewMap(t, d, id); st != string(a2ashape.TaskStateFailed) || got != reason {
		t.Fatalf("task %s: %s %q, want failed %q", id, st, got, reason)
	}
}

// stillOpen fails the test unless d's task id is in flight.
func stillOpen(t *testing.T, d *Daemon, id string) {
	t.Helper()
	if ix, err := d.ix.Get(id); err != nil || ix.IsTerminal() {
		t.Fatalf("task %s ended: %v %v", id, ix, err)
	}
}

// TestRollbackClosesAndRegenerates (FS-B2 item 2, FS-B3; SI-33; J5): the hub
// holds a key set this identity signed and this node did not — its data
// went back. The task in flight fails with local_state_lost and its
// requester is told; the interaction keys go; the ring is made anew and its
// set, above the hub's and following it, is what the hub then holds; the
// evidence log records it once with the payload core §2.14 registers; the
// mark says so and is shown.
func TestRollbackClosesAndRegenerates(t *testing.T) {
	h, srv, req, prov, id := rollbackPair(t)
	ctx := context.Background()
	before := prov.enc.Seq()
	stored := before + 1000
	hubHolds(h, prov.AID(), otherCopySet(t, prov, stored))

	if err := prov.publishKeys(ctx, srv.URL); err != nil {
		t.Fatalf("the publication after the rollback: %v", err)
	}
	failedWith(t, prov, id, string(closeLocalStateLost))
	if rk, err := prov.ix.OwnRK(id); err != nil || rk != nil {
		t.Fatalf("the task's interaction key after the rollback: %v %v", rk, err)
	}
	if got := regenTagOf(prov); got != stored {
		t.Fatalf("regen_tag %d, want the hub's seq %d", got, stored)
	}
	ours, seq, prev := prov.enc.published()
	if seq <= stored || prev != stored {
		t.Fatalf("the new set: seq %d prev_seq %d; want above and following %d", seq, prev, stored)
	}
	if !bytes.Equal(hubSet(h, prov.AID()), ours) {
		t.Fatal("the hub does not hold the node's new set")
	}
	ev := localStateEvents(prov)
	if len(ev) != 1 {
		t.Fatalf("%d anet.local_state.lost records, want 1", len(ev))
	}
	shape, _ := evtypes.PayloadOf(evtypes.LocalStateLost)
	if why := shape.Check(ev[0]); why != "" {
		t.Fatalf("the record's payload %v: %s", ev[0], why)
	}
	if fmt.Sprint(ev[0]["hub_seq"]) != fmt.Sprint(stored) || fmt.Sprint(ev[0]["local_seq"]) != fmt.Sprint(before) ||
		fmt.Sprint(ev[0]["failed_tasks"]) != "1" {
		t.Fatalf("the record says %v; want hub_seq %d, local_seq %d, failed_tasks 1", ev[0], stored, before)
	}
	m := prov.enc.rollbackMark()
	if m == nil || m.Silent || !m.Logged || m.StoredSeq != stored || m.Tasks != 1 || m.PrevAt != 0 || m.At == 0 || m.At > prov.nowMS() {
		t.Fatalf("the rollback mark: %+v", m)
	}
	if n := prov.localStateLost(); n == nil || n.Tasks != 1 || n.Repeated {
		t.Fatalf("the notice: %+v, want 1 task, not repeated", n)
	}
	if n := counter(prov, counterKeysRollbackUnverified); n != 0 {
		t.Fatalf("a rollback this node found counted as unverified (%d)", n)
	}
	// The requester is told: its task fails with the provider's reason.
	waitUntil(t, "the requester to learn of the rollback", func() bool {
		_ = req.pollOnce(ctx)
		ix, err := req.ix.Get(id)
		return err == nil && ix.State == interactions.StateFailed
	})
	if _, _, reason := taskViewMap(t, req, id); reason != string(closeLocalStateLost) {
		t.Fatalf("the requester's task failed with %q, want %s", reason, closeLocalStateLost)
	}
	// The mark survives a restart (the ring file holds it).
	layout := prov.layout
	if err := prov.Close(); err != nil {
		t.Fatal(err)
	}
	p2, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p2.Close() })
	p2.stopRelayLoop()
	if got := p2.enc.rollbackMark(); got == nil || *got != *m {
		t.Fatalf("after a restart the mark is %+v, want %+v", got, m)
	}
	if n := len(localStateEvents(p2)); n != 1 {
		t.Fatalf("after a restart %d records, want the 1", n)
	}
}

// TestRollbackRegeneratesKeys (FS-B3 step 3; mutation 29): the set
// published after a rollback holds none of the keys the ring held before
// it; those are retired and kept, so the mail already sealed to them still
// opens.
func TestRollbackRegeneratesKeys(t *testing.T) {
	h, srv, _, prov, _ := rollbackPair(t)
	old := ringKids(prov)
	hubHolds(h, prov.AID(), otherCopySet(t, prov, prov.enc.Seq()+1000))
	if err := prov.publishKeys(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	kids := setKIDList(t, prov.enc.SignedSet())
	if len(kids) != int(ringWindowEpochs)+1 {
		t.Fatalf("the new set lists %d keys", len(kids))
	}
	for _, kid := range kids {
		if old[string(kid)] != nil {
			t.Fatal("the set after the rollback lists a key the ring held before it")
		}
	}
	now := ringKids(prov)
	for kid := range old {
		if k := now[kid]; k == nil || !k.Retired {
			t.Fatalf("a key from before the rollback: %+v, want kept and retired", k)
		}
	}
}

// TestForgedRollbackEndsNothing (FS-B2; SI-33; mutation 27; J10, red team
// A6): a hub that answers keys_rollback naming a seq it shows no set of this
// identity at — or a directory entry holding a set this identity did not
// sign — ends nothing: the ring follows the seq, the count goes up, the set
// is sent again and taken, no mark is written, nothing is shown. Twice in a
// row does not make it a repeated rollback.
func TestForgedRollbackEndsNothing(t *testing.T) {
	h, srv, _, prov, id := rollbackPair(t)
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		forged := prov.enc.Seq() + 5000
		h.mu.Lock()
		h.forgedRollback = []uint64{forged}
		h.mu.Unlock()
		if err := prov.publishKeys(ctx, srv.URL); err != nil {
			t.Fatalf("forgery %d: the set was not sent again and taken: %v", i, err)
		}
		if _, _, prev := prov.enc.published(); prev != forged {
			t.Fatalf("forgery %d: prev_seq %d, want the forged %d followed", i, prev, forged)
		}
		if n := counter(prov, counterKeysRollbackUnverified); n != uint64(i) {
			t.Fatalf("forgery %d: keys-rollback-unverified %d", i, n)
		}
		stillOpen(t, prov, id)
		if m := prov.enc.rollbackMark(); m != nil {
			t.Fatalf("forgery %d wrote a rollback mark: %+v", i, m)
		}
		if n := prov.localStateLost(); n != nil {
			t.Fatalf("forgery %d is shown: %+v", i, n)
		}
	}
	// The directory holds a set at the seq the hub names, signed by another
	// identity under this AID's entry: not this identity's.
	other := newTestDaemon(t, "", false)
	forged := prov.enc.Seq() + 5000
	set := decodedSet(t, other.enc.SignedSet())
	set.AID, set.Seq = prov.AID(), forged
	signed, err := seal.SignEncKeySet(&set, other.self.Sign)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := signed.Marshal()
	h.mu.Lock()
	kel := h.agents[prov.AID()].kel
	h.mu.Unlock()
	h.setDir(func(h *fakeHub) {
		h.dirOverride[prov.AID()] = dirbox.DirEntry{AID: prov.AID(), Home: h.self.AID(), KEL: kel, Keys: b}
		h.forgedRollback = []uint64{forged}
	})
	if err := prov.publishKeys(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	stillOpen(t, prov, id)
	if m := prov.enc.rollbackMark(); m != nil || counter(prov, counterKeysRollbackUnverified) != 3 {
		t.Fatalf("a set another identity signed: mark %+v, count %d", m, counter(prov, counterKeysRollbackUnverified))
	}
	if n := len(localStateEvents(prov)); n != 0 {
		t.Fatalf("%d anet.local_state.lost records after forgeries", n)
	}
}

// TestHubReplayingThisNodesOwnSetEndsNothing (FS-B2): a hub that names one
// of this node's own sets — an earlier one, or the current one — and shows
// it in the directory has shown nothing this identity signed elsewhere.
func TestHubReplayingThisNodesOwnSetEndsNothing(t *testing.T) {
	h, srv, _, prov, id := rollbackPair(t)
	ctx := context.Background()
	earlier, q := append([]byte(nil), prov.enc.SignedSet()...), prov.enc.Seq()
	clock := atClock(prov, epochOf(prov.nowMS())+seal.EpochMS+60_000)
	h.mu.Lock()
	h.keysAt = clock.Load
	h.mu.Unlock()
	if err := prov.resignKeyRing(); err != nil || prov.enc.Seq() <= q {
		t.Fatalf("no new set: %v", err)
	}
	// An earlier set of this node's.
	hubHolds(h, prov.AID(), earlier)
	h.mu.Lock()
	h.forgedRollback = []uint64{q}
	h.mu.Unlock()
	if err := prov.publishKeys(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	stillOpen(t, prov, id)
	if m := prov.enc.rollbackMark(); m != nil || counter(prov, counterKeysRollbackUnverified) != 1 {
		t.Fatalf("this node's earlier set named: mark %+v, count %d", m, counter(prov, counterKeysRollbackUnverified))
	}
	// Its current set: the one the hub now holds.
	if !bytes.Equal(hubSet(h, prov.AID()), prov.enc.SignedSet()) {
		t.Fatal("setup: the hub does not hold the node's current set")
	}
	h.mu.Lock()
	h.forgedRollback = []uint64{prov.enc.Seq()}
	h.mu.Unlock()
	if err := prov.publishKeys(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	stillOpen(t, prov, id)
	if m := prov.enc.rollbackMark(); m != nil || counter(prov, counterKeysRollbackUnverified) != 2 {
		t.Fatalf("this node's current set named: mark %+v, count %d", m, counter(prov, counterKeysRollbackUnverified))
	}
}

// TestACopyThatSignedFirstIsFound (FS-B2, FS-B7): a set this identity
// signed elsewhere a moment before this node signed its own — two copies
// both sign at a new epoch, the backup's node signs at its start — has a
// seq below this node's last_seq. It is still a set this node did not
// sign, and the rollback is found.
func TestACopyThatSignedFirstIsFound(t *testing.T) {
	h, srv, _, prov, id := rollbackPair(t)
	stored := prov.enc.Seq() + 1000
	hubHolds(h, prov.AID(), otherCopySet(t, prov, stored))
	clock := atClock(prov, epochOf(prov.nowMS())+seal.EpochMS+60_000)
	h.mu.Lock()
	h.keysAt = clock.Load
	h.mu.Unlock()
	if err := prov.resignKeyRing(); err != nil {
		t.Fatal(err)
	}
	if prov.enc.Seq() <= stored {
		t.Fatalf("setup: this node's set (seq %d) is not above the other copy's (%d)", prov.enc.Seq(), stored)
	}
	if err := prov.publishKeys(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	failedWith(t, prov, id, string(closeLocalStateLost))
	if m := prov.enc.rollbackMark(); m == nil || m.StoredSeq != stored || m.Silent {
		t.Fatalf("the mark: %+v", m)
	}
}

// TestRecoverOnFreshDirIsSilent (core G25, FS-B2 item 1; mutation 28; J9):
// the identity alone on a new machine — no task ever — follows the hub's
// set without a word: no evidence, nothing shown, nothing counted; a silent
// mark that only a second rollback reads. A stored_seq past 2^53 is not
// followed, and writes no mark either.
func TestRecoverOnFreshDirIsSilent(t *testing.T) {
	srv := newFakeHub(t)
	h := fakeHubAt(t, srv.URL)
	ctx := context.Background()
	a := newTestDaemon(t, srv.URL, false)
	if err := a.RegisterWithHub(ctx, srv.URL, "A", nil); err != nil {
		t.Fatal(err)
	}
	stored := decodedSet(t, hubSet(h, a.AID())).Seq
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := startAt(t, rootWithIdentityOf(t, a, srv.URL))
	holdGateOpen(b)
	if err := b.RegisterWithHub(ctx, srv.URL, "B", nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hubSet(h, b.AID()), b.enc.SignedSet()) {
		t.Fatal("the hub does not hold the new machine's set")
	}
	m := b.enc.rollbackMark()
	if m == nil || !m.Silent || !m.Logged || m.StoredSeq != stored || m.Tasks != 0 || m.PrevAt != 0 {
		t.Fatalf("the mark: %+v, want silent at the hub's seq %d", m, stored)
	}
	if n := len(localStateEvents(b)); n != 0 {
		t.Fatalf("%d anet.local_state.lost records on a fresh data directory", n)
	}
	if n := b.localStateLost(); n != nil {
		t.Fatalf("a fresh data directory shows %+v", n)
	}
	if n := counter(b, counterKeysRollbackUnverified); n != 0 {
		t.Fatalf("keys-rollback-unverified %d", n)
	}
	h.mu.Lock()
	h.forgedRollback = []uint64{maxFollowedSeq}
	h.mu.Unlock()
	if err := b.publishKeys(ctx, srv.URL); err == nil {
		t.Fatal("a stored_seq of 2^53 was followed")
	}
	if got := b.enc.rollbackMark(); got == nil || *got != *m {
		t.Fatalf("a stored_seq of 2^53 changed the mark to %+v", got)
	}
}

// TestSecondFreshDirRollbackIsVerified (FS-B2 item 1, FS-B4a; mutation 47):
// a fresh data directory copied and run in two places: the first rollback
// is silent, the second within 7 days is checked — and found, though no
// task ever ran: it is shown, and as repeated.
func TestSecondFreshDirRollbackIsVerified(t *testing.T) {
	srv := newFakeHub(t)
	h := fakeHubAt(t, srv.URL)
	ctx := context.Background()
	a := newTestDaemon(t, srv.URL, false)
	if err := a.RegisterWithHub(ctx, srv.URL, "A", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := startAt(t, rootWithIdentityOf(t, a, srv.URL))
	holdGateOpen(b)
	if err := b.RegisterWithHub(ctx, srv.URL, "B", nil); err != nil {
		t.Fatal(err)
	}
	first := b.enc.rollbackMark()
	if first == nil || !first.Silent {
		t.Fatalf("setup: the first rollback's mark %+v", first)
	}
	// The other copy publishes again.
	stored := b.enc.Seq() + 1000
	hubHolds(h, b.AID(), otherCopySet(t, b, stored))
	if err := b.publishKeys(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	m := b.enc.rollbackMark()
	if m == nil || m.Silent || m.StoredSeq != stored || m.PrevAt != first.At || m.Tasks != 0 || !m.Logged {
		t.Fatalf("the second rollback's mark: %+v (the first at %d)", m, first.At)
	}
	if n := b.localStateLost(); n == nil || !n.Repeated {
		t.Fatalf("the notice: %+v, want repeated", n)
	}
	if ev := localStateEvents(b); len(ev) != 1 || fmt.Sprint(ev[0]["failed_tasks"]) != "0" {
		t.Fatalf("the records: %v", ev)
	}
}

// TestLocalStateNoticeSevenDays (FS-B4, FS-U7 row 1; ux E4): a rollback this
// node found is shown from its time until 7 days after it, the last
// millisecond included, as local_state.detected with the tasks it ended
// and the emergency rotation as the next step; then no more.
func TestLocalStateNoticeSevenDays(t *testing.T) {
	h, srv, _, prov, _ := rollbackPair(t)
	at := prov.nowMS()
	clock := atClock(prov, at)
	h.mu.Lock()
	h.keysAt = clock.Load
	h.mu.Unlock()
	hubHolds(h, prov.AID(), otherCopySet(t, prov, prov.enc.Seq()+1000))
	if err := prov.publishKeys(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	want := time.UnixMilli(int64(at)).UTC().Format(time.RFC3339)
	for _, c := range []struct {
		now   uint64
		shown bool
	}{{at, true}, {at + localStateNoticeFor/2, true}, {at + localStateNoticeFor, true}, {at + localStateNoticeFor + 1, false}} {
		clock.Store(c.now)
		n := prov.localStateLost()
		if (n != nil) != c.shown {
			t.Fatalf("%d ms after the rollback: notice %+v, shown %v", c.now-at, n, c.shown)
		}
		if n == nil {
			continue
		}
		if n.At != want || n.Tasks != 1 || n.Repeated {
			t.Fatalf("the notice %+v, want at %s, 1 task, not repeated", n, want)
		}
		s := n.said()
		row, _ := a2ashape.Lookup(s.Key)
		if s.Key != "local_state.detected" || row.Level != a2ashape.LevelWarn ||
			a2ashape.Next(s.Key, a2ashape.English, s.Args) != "anet identity rotate --emergency" {
			t.Fatalf("the notice says %s (%s), next %q", s.Key, row.Level, a2ashape.Next(s.Key, a2ashape.English, s.Args))
		}
	}
}

// TestRollbackNoticeEvenWithoutTasks (FS-B4; mutation 46): a rollback that
// ended no task — none was in flight — is shown all the same: another copy
// of the identity may be running.
func TestRollbackNoticeEvenWithoutTasks(t *testing.T) {
	srv, req, prov := registeredPair(t)
	h := fakeHubAt(t, srv.URL)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "a task that ends", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n, err := req.ix.Count(interactions.ListFilter{Active: true}); err != nil || n != 0 {
		t.Fatalf("setup: %d tasks in flight (%v)", n, err)
	}
	hubHolds(h, req.AID(), otherCopySet(t, req, req.enc.Seq()+1000))
	if err := req.publishKeys(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	if m := req.enc.rollbackMark(); m == nil || m.Silent || m.Tasks != 0 {
		t.Fatalf("the mark: %+v", m)
	}
	n := req.localStateLost()
	if n == nil || n.Tasks != 0 || n.Repeated || n.said().Key != "local_state.detected" {
		t.Fatalf("a rollback that ended no task: notice %+v", n)
	}
}

// verifiedRollbackAt makes the hub hold a set prov's identity signed
// elsewhere and has prov publish at clock time at: a rollback prov finds.
func verifiedRollbackAt(t *testing.T, h *fakeHub, srvURL string, prov *Daemon, clock interface {
	Store(uint64)
	Load() uint64
}, at uint64) {
	t.Helper()
	clock.Store(at)
	if err := prov.resignKeyRing(); err != nil {
		t.Fatal(err)
	}
	stored := prov.enc.Seq() + 1000
	hubHolds(h, prov.AID(), otherCopySet(t, prov, stored))
	if err := prov.publishKeys(context.Background(), srvURL); err != nil {
		t.Fatal(err)
	}
	if m := prov.enc.rollbackMark(); m == nil || m.StoredSeq != stored || m.Silent {
		t.Fatalf("the rollback at %d was not found: mark %+v", at, m)
	}
}

// TestRepeatedRollbackFailsDoctor (FS-B4a, FS-U7 row 0; mutation 45; J12):
// a second rollback found at most 7 days after the first says the identity
// runs somewhere else: local_state.repeated, which fails doctor's
// local_state; 7 days and a millisecond apart, each is only detected (a
// warning). Forged keys_rollback answers in between change nothing.
func TestRepeatedRollbackFailsDoctor(t *testing.T) {
	for _, c := range []struct {
		gap      uint64
		repeated bool
	}{{localStateNoticeFor, true}, {localStateNoticeFor + 1, false}, {60_000, true}} {
		t.Run(fmt.Sprint(c.gap), func(t *testing.T) {
			h, srv, _, prov, _ := rollbackPair(t)
			base := prov.nowMS()
			clock := atClock(prov, base)
			h.mu.Lock()
			h.keysAt = clock.Load
			h.mu.Unlock()
			verifiedRollbackAt(t, h, srv.URL, prov, clock, base)
			first := prov.enc.rollbackMark()
			// Two forgeries: no mark, nothing repeated.
			for range 2 {
				h.mu.Lock()
				h.forgedRollback = []uint64{prov.enc.Seq() + 5000}
				h.mu.Unlock()
				if err := prov.publishKeys(context.Background(), srv.URL); err != nil {
					t.Fatal(err)
				}
			}
			if m := prov.enc.rollbackMark(); m == nil || *m != *first {
				t.Fatalf("forgeries changed the mark: %+v", m)
			}
			if n := prov.localStateLost(); n == nil || n.Repeated {
				t.Fatalf("after two forgeries: %+v, want detected only", n)
			}
			verifiedRollbackAt(t, h, srv.URL, prov, clock, first.At+c.gap)
			n := prov.localStateLost()
			if n == nil || n.Repeated != c.repeated {
				t.Fatalf("%d ms apart: notice %+v, want repeated %v", c.gap, n, c.repeated)
			}
			s := n.said()
			row, _ := a2ashape.Lookup(s.Key)
			wantKey, wantLevel := "local_state.detected", a2ashape.LevelWarn
			if c.repeated {
				wantKey, wantLevel = "local_state.repeated", a2ashape.LevelFail
			}
			if s.Key != wantKey || row.Level != wantLevel ||
				a2ashape.Next(s.Key, a2ashape.English, s.Args) != "anet identity rotate --emergency" {
				t.Fatalf("%d ms apart: %s (%s), want %s (%s)", c.gap, s.Key, row.Level, wantKey, wantLevel)
			}
		})
	}
	// The rule on its own: the bounds, a mark of the past, a silent one.
	for _, c := range []struct {
		m    rollbackMark
		now  uint64
		want *localStateNotice
	}{
		{rollbackMark{At: 10 * dayMS, PrevAt: 3 * dayMS}, 10 * dayMS, &localStateNotice{Repeated: true}},
		{rollbackMark{At: 10*dayMS + 1, PrevAt: 3 * dayMS}, 10 * dayMS, &localStateNotice{}},
		{rollbackMark{At: 10 * dayMS, PrevAt: 11 * dayMS}, 10 * dayMS, &localStateNotice{Repeated: true}},
		{rollbackMark{At: 10 * dayMS}, 17 * dayMS, &localStateNotice{}},
		{rollbackMark{At: 10 * dayMS}, 17*dayMS + 1, nil},
		{rollbackMark{At: 10 * dayMS, Silent: true}, 10 * dayMS, nil},
		{rollbackMark{At: 10 * dayMS}, 9 * dayMS, &localStateNotice{}},
	} {
		got := localStateNoticeAt(&c.m, c.now)
		if (got == nil) != (c.want == nil) || got != nil && got.Repeated != c.want.Repeated {
			t.Errorf("mark %+v at %d: %+v, want %+v", c.m, c.now, got, c.want)
		}
	}
}

// errCrash stops a rollback the way a crash would (setRollbackStep).
var errCrash = errors.New("crash (test)")

// TestRollbackStepsIdempotent (FS-B3; engine CR11): a rollback cut short
// after any of its steps — the mark, the close, the new keys, the evidence
// before the mark records it, the mark — is finished by the next start, or,
// in a process that went on, by the next publication; and every step
// happens once: the task fails once, one notice is queued for its
// requester, the keys are made anew once, one record is written, and the
// rollback is not taken for a second one.
func TestRollbackStepsIdempotent(t *testing.T) {
	for _, stop := range []int{rbMarked, rbClosed, rbRegenerated, rbRecorded, rbLogged} {
		for _, restart := range []bool{true, false} {
			t.Run(fmt.Sprintf("%d/restart=%v", stop, restart), func(t *testing.T) {
				h, srv, req, prov, id := rollbackPair(t)
				ctx := context.Background()
				req.stopRelayLoop() // the requester reads nothing: the notice stays queued
				stored := prov.enc.Seq() + 1000
				hubHolds(h, prov.AID(), otherCopySet(t, prov, stored))
				hits := 0
				prov.setRollbackStep(func(n int) error {
					if n == stop {
						if hits++; hits <= 2 {
							return errCrash
						}
					}
					return nil
				})
				if err := prov.publishKeys(ctx, srv.URL); err == nil {
					t.Fatal("a rollback cut short published its set")
				}
				d := prov
				if restart {
					layout := prov.layout
					if err := prov.Close(); err != nil {
						t.Fatal(err)
					}
					p2, err := New(layout)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { p2.Close() })
					p2.stopRelayLoop()
					d = p2
					failedWith(t, d, id, string(closeLocalStateLost))
				} else {
					_ = d.publishKeys(ctx, srv.URL) // cut short again, or finished
				}
				if err := d.publishKeys(ctx, srv.URL); err != nil {
					t.Fatalf("the publication after it: %v", err)
				}
				failedWith(t, d, id, string(closeLocalStateLost))
				notices := 0
				for _, r := range outboxRows(t, d, id) {
					if r.Type == seal.TypeStatus {
						notices++
					}
				}
				if notices != 1 {
					t.Fatalf("%d status rows queued for the requester, want the one notice", notices)
				}
				if got := regenTagOf(d); got != stored {
					t.Fatalf("regen_tag %d, want %d", got, stored)
				}
				ev := localStateEvents(d)
				if len(ev) != 1 || fmt.Sprint(ev[0]["hub_seq"]) != fmt.Sprint(stored) || fmt.Sprint(ev[0]["failed_tasks"]) != "1" {
					t.Fatalf("the records: %v", ev)
				}
				if m := d.enc.rollbackMark(); m == nil || !m.Logged || m.Silent || m.StoredSeq != stored || m.Tasks != 1 || m.PrevAt != 0 {
					t.Fatalf("the mark: %+v", m)
				}
				if n := d.localStateLost(); n == nil || n.Repeated {
					t.Fatalf("the notice: %+v, want one rollback", n)
				}
				if !bytes.Equal(hubSet(h, d.AID()), d.enc.SignedSet()) {
					t.Fatal("the hub does not hold the new set")
				}
				if n, want := len(ringKids(d)), 2*(int(ringWindowEpochs)+1); n != want {
					t.Fatalf("the ring holds %d keys, want %d: the old window retired and one new one", n, want)
				}
				if n := counter(d, counterKeysRollbackUnverified); n != 0 {
					t.Fatalf("keys-rollback-unverified %d", n)
				}
			})
		}
	}
}

// TestRollbackWaitsForTheDirectory (FS-B2: a network failure decides
// nothing): while the hub's directory cannot be read, a keys_rollback is
// neither followed nor taken for a rollback, and a registration is retried
// rather than refused; once it can be read, the rollback is found.
func TestRollbackWaitsForTheDirectory(t *testing.T) {
	h, srv, _, prov, id := rollbackPair(t)
	ctx := context.Background()
	seq := prov.enc.Seq()
	hubHolds(h, prov.AID(), otherCopySet(t, prov, seq+1000))
	h.setDir(func(h *fakeHub) { h.dirDown = true })
	if err := prov.publishKeys(ctx, srv.URL); err == nil {
		t.Fatal("the publication succeeded with the directory down")
	}
	// The registration is an attempt the hub could not complete: the node
	// stays registered (AD-5) and tries again, rather than being refused.
	_ = prov.RegisterWithHub(ctx, srv.URL, "Prov", nil)
	prov.join.mu.Lock()
	state := prov.join.rec.State
	prov.join.mu.Unlock()
	if state != JoinRegistered {
		t.Fatalf("the join after a keys_rollback it could not check: %s, want it kept %s", state, JoinRegistered)
	}
	stillOpen(t, prov, id)
	if m, s := prov.enc.rollbackMark(), prov.enc.Seq(); m != nil || s != seq || counter(prov, counterKeysRollbackUnverified) != 0 {
		t.Fatalf("with the directory down: mark %+v, seq %d (was %d), count %d", m, s, seq, counter(prov, counterKeysRollbackUnverified))
	}
	h.setDir(func(h *fakeHub) { h.dirDown = false })
	if err := prov.publishKeys(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	failedWith(t, prov, id, string(closeLocalStateLost))
}

// TestKeySetPublishesAreSerialized (FS-K5a): two publications of one node
// never interleave. A publication that read the set it sends, and waits to
// send it while the ring signs a newer one, is the only one until the
// hub's answer: a second publication — or a registration, which carries
// the set — waits for it and then sends the newer set. So the hub gets them
// in the order they were signed: no keys_rollback, nothing counted, no
// mark, and the hub ends with the newer set.
func TestKeySetPublishesAreSerialized(t *testing.T) {
	for _, second := range []string{"publication", "registration"} {
		t.Run(second, func(t *testing.T) {
			srv, req, prov := registeredPair(t)
			h := fakeHubAt(t, srv.URL)
			ctx := context.Background()
			// A task: a keys_rollback would be checked, and counted.
			if _, err := req.Delegate(ctx, prov.AID(), "a task", nil); err != nil {
				t.Fatal(err)
			}
			if err := prov.pollOnce(ctx); err != nil {
				t.Fatal(err)
			}
			clock := atClock(prov, epochOf(prov.nowMS())+seal.EpochMS+60_000)
			h.mu.Lock()
			h.keysAt = clock.Load
			posts := h.keyPosts
			h.mu.Unlock()
			if err := prov.resignKeyRing(); err != nil {
				t.Fatal(err)
			}
			paused, release := make(chan struct{}), make(chan struct{})
			var pause sync.Once
			prov.setPublishSent(func() { pause.Do(func() { close(paused); <-release }) })
			done1 := make(chan error, 1)
			go func() { done1 <- prov.publishKeys(ctx, srv.URL) }()
			<-paused
			// While the first waits to send, the ring signs the next day's set.
			clock.Store(clock.Load() + seal.EpochMS)
			if err := prov.resignKeyRing(); err != nil {
				t.Fatal(err)
			}
			waiting := make(chan struct{})
			var wait sync.Once
			prov.setPublishWait(func() { wait.Do(func() { close(waiting) }) })
			done2 := make(chan error, 1)
			go func() {
				if second == "publication" {
					done2 <- prov.publishKeys(ctx, srv.URL)
				} else {
					done2 <- prov.RegisterWithHub(ctx, srv.URL, "Prov", nil)
				}
			}()
			var err2 error
			got2 := false
			select {
			case <-waiting: // it waits for the first, as FS-K5a asks
			case err2 = <-done2: // it did not wait
				got2 = true
			case <-time.After(time.Minute):
				t.Fatal("the second publication neither waited nor finished")
			}
			close(release)
			if err := <-done1; err != nil {
				t.Fatalf("the first publication: %v", err)
			}
			if !got2 {
				err2 = <-done2
			}
			if err2 != nil {
				t.Fatalf("the second %s: %v", second, err2)
			}
			if n := counter(prov, counterKeysRollbackUnverified); n != 0 {
				t.Fatalf("keys-rollback-unverified %d: the publications interleaved", n)
			}
			if m := prov.enc.rollbackMark(); m != nil {
				t.Fatalf("a rollback mark %+v: the publications interleaved", m)
			}
			if !bytes.Equal(hubSet(h, prov.AID()), prov.enc.SignedSet()) {
				t.Fatal("the hub does not end with the newer set")
			}
			h.mu.Lock()
			sent := h.keyPosts - posts
			h.mu.Unlock()
			if want := map[string]int{"publication": 2, "registration": 1}[second]; sent != want {
				t.Fatalf("%d key set publications, want %d (no second attempt after a keys_rollback)", sent, want)
			}
		})
	}
}

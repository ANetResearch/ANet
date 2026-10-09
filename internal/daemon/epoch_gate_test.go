package daemon

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"math"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// silentHub is a hub URL nothing answers on: the node is configured with a
// hub that never answers (connection refused), so no answer checks its
// clock unless a test says what the hub's Date was (noteHubDate).
const silentHub = "http://127.0.0.1:1"

// gateClocks drives a daemon's two clocks: the wall clock (wire clock) and
// the boot clock, moving together unless a test steps the wall.
type gateClocks struct {
	wall, mono atomic.Uint64
}

func newGateClocks(t *testing.T, d *Daemon, wall uint64) *gateClocks {
	t.Helper()
	c := &gateClocks{}
	c.wall.Store(wall)
	c.mono.Store(1_000 * hourMS)
	d.setClock(c.wall.Load)
	d.setMonoClock(c.mono.Load)
	return c
}

func (c *gateClocks) run(ms uint64) { c.wall.Add(ms); c.mono.Add(ms) }

// hubSays records an answer of d's configured hub whose Date is at.
func hubSays(d *Daemon, at uint64) {
	d.noteHubDate(d.config().HubURL, time.UnixMilli(int64(at)).UTC().Format(http.TimeFormat))
}

// heldKeys is the kids of d's ring, by kid, with their hard bound.
func heldKeys(d *Daemon) map[string]uint64 {
	d.enc.mu.RLock()
	defer d.enc.mu.RUnlock()
	out := map[string]uint64{}
	for _, k := range d.enc.keys {
		out[string(k.Pair.Public.KID)] = k.Pair.Public.NotAfter + seal.MaxMessageLifetimeMS
	}
	return out
}

// reloadRing is a restart as far as the clock gate is concerned: the ring's
// state comes back from its file, and the process's memory — the previous
// period and the last check — is gone.
func reloadRing(t *testing.T, d *Daemon) {
	t.Helper()
	fresh, err := openKeyRing(d.layout.EncKeysPath(), d.AID(), d.nowMS)
	if err != nil {
		t.Fatal(err)
	}
	r := d.enc
	r.maintMu.Lock()
	r.mu.Lock()
	r.keys, r.lastSeq, r.prevSeq, r.signed, r.mbox = fresh.keys, fresh.lastSeq, fresh.prevSeq, fresh.signed, fresh.mbox
	r.lastWall, r.run, r.rollback, r.regenTag = fresh.lastWall, fresh.run, fresh.rollback, fresh.regenTag
	r.mu.Unlock()
	r.maintMu.Unlock()
	d.forgetCycle()
	d.clk.mu.Lock()
	d.clk.hub, d.clk.obs = "", clockObs{}
	d.clk.mu.Unlock()
}

// TestHubAnswersCheckTheClock (FS-K14): every answer of the configured hub
// that carries a Date is a check of this node's clock — a poll here — and
// an answer of another hub is not.
func TestHubAnswersCheckTheClock(t *testing.T) {
	srv, _, prov := registeredPair(t)
	h := fakeHubAt(t, srv.URL)
	clocks := newGateClocks(t, prov, prov.nowMS())
	h.mu.Lock()
	h.dateFn = func() time.Time { return time.UnixMilli(int64(clocks.wall.Load()) + 3000) }
	h.mu.Unlock()
	if err := prov.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	obs := prov.clockObservation()
	if !obs.hub || !obs.checked || obs.offset > -2000 || obs.offset < -3000 || obs.mono != clocks.mono.Load() {
		t.Fatalf("after a poll: %+v, want a check finding the clock 2 to 3 seconds behind", obs)
	}
	if level, state := clockGate(obs, 0, clocks.wall.Load(), clocks.mono.Load()); level != clockFull || state != clockOK {
		t.Fatalf("right after the check: %v %s", level, state)
	}
	// Another hub's answer says nothing about this one.
	prov.noteHubDate("http://elsewhere.invalid", time.UnixMilli(int64(clocks.wall.Load())+int64(hourMS)).UTC().Format(http.TimeFormat))
	if got := prov.clockObservation(); got != obs {
		t.Fatalf("another hub's Date changed the check: %+v", got)
	}
	// A Date that does not parse is no check.
	prov.noteHubDate(srv.URL, "yesterday")
	if got := prov.clockObservation(); got != obs {
		t.Fatal("a Date that does not parse changed the check")
	}
	// No hub: nothing to check against, and the gate is full.
	d := newTestDaemon(t, "", true)
	if obs := d.clockObservation(); obs.hub {
		t.Fatalf("a node without a hub has %+v", obs)
	}
}

// keyedOutboundTask records an outbound task with an interaction key whose
// close_after comes at closeAfter, at the store clock.
func keyedOutboundTask(t *testing.T, d *Daemon, id string, endedAt uint64) []byte {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d.ix.SetClock(func() int64 { return int64(endedAt) })
	defer d.ix.SetClock(nil)
	k := interactions.IXKey{IX: id, PeerAID: "did:anet:peer", Suite: seal.SuiteX25519, Pub: priv.PublicKey().Bytes(), Priv: priv.Bytes()}
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		if err := tx.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: k.PeerAID, Goal: "g"}); err != nil {
			return err
		}
		return tx.PutIXKey(k)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ix.SetState(id, interactions.StateCanceled, interactions.CauseLocalEnded); err != nil {
		t.Fatal(err)
	}
	return seal.KID(k.Suite, k.Pub)
}

// endedPublicCapTask records a public_cap call that ended at endedAt.
func endedPublicCapTask(t *testing.T, d *Daemon, id string, endedAt uint64) {
	t.Helper()
	d.ix.SetClock(func() int64 { return int64(endedAt) })
	defer d.ix.SetClock(nil)
	if err := d.ix.Create(interactions.New{ID: id, Role: interactions.RoleInbound, PeerAID: "did:anet:x",
		Goal: "c", Trust: interactions.TrustPublicCap, IsCapability: true}); err != nil {
		t.Fatal(err)
	}
	if err := d.ix.SetResult(id, []byte("r"), "cid", nil, interactions.VerificationVerified, interactions.CauseProviderEnd); err != nil {
		t.Fatal(err)
	}
}

// TestNoDeletionAfterClockJump (mutations 7 and 19): the wall clock jumps
// 20 days ahead, and the hub keeps telling the truth about the time. For a
// day of running time after that nothing goes — no epoch key, though half
// of them are past their hard bound by the jumped clock, no interaction key
// past close_after, no task past its retention — and nothing goes early;
// then the local gate opens and the hard-bound deletions run.
func TestNoDeletionAfterClockJump(t *testing.T) {
	d := newTestDaemon(t, silentHub, true)
	start := epochOf(d.nowMS()) + 2*hourMS
	clocks := newGateClocks(t, d, start)
	kid := keyedOutboundTask(t, d, "ix_closing", start-14*dayMS) // close_after = start + 1 day
	endedPublicCapTask(t, d, "ix_public", start-3*dayMS)         // past its retention four days on
	hubSays(d, start)
	d.keyMaintenance()
	keys := heldKeys(d)
	if len(keys) != 15 {
		t.Fatalf("%d keys at the start", len(keys))
	}
	privs := map[string][]byte{} // the private halves the ring holds, as it holds them
	for kid := range keys {
		kp, _ := d.enc.Key([]byte(kid))
		privs[kid] = kp.Private
	}
	jumped := start + 20*dayMS
	clocks.wall.Store(jumped)
	clocks.mono.Add(keyMaintainEveryMS)
	trueTime := start + keyMaintainEveryMS
	var ran uint64
	const step = 30 * 60 * 1000
	for ran+step < clockFallbackAfter {
		hubSays(d, trueTime) // an honest hub: the clock is 20 days off
		d.keyMaintenance()
		d.maintainKeyRing() // the mailbox loop's maintenance between periods deletes nothing either
		if level, state := clockGate(d.clockObservation(), d.enc.unverifiedRun(), clocks.wall.Load(), clocks.mono.Load()); level != clockClosed || state != clockSkewed {
			t.Fatalf("%d ms after the jump: %v %s, want closed and skewed", ran, level, state)
		}
		got := heldKeys(d)
		for k := range keys {
			if _, ok := got[k]; !ok {
				t.Fatalf("%d ms of running time after the jump an epoch key went", ran)
			}
		}
		if k, err := d.ix.IXKeyByKID(kid); err != nil || k == nil {
			t.Fatalf("%d ms after the jump the interaction key went: %v", ran, err)
		}
		if _, err := d.ix.Get("ix_public"); err != nil {
			t.Fatalf("%d ms after the jump the public_cap task went: %v", ran, err)
		}
		clocks.run(step)
		trueTime += step
		ran += step
	}
	clocks.run(2 * step)
	trueTime += 2 * step
	hubSays(d, trueTime)
	d.keyMaintenance()
	now := clocks.wall.Load()
	gone := 0
	for k, bound := range keys {
		_, held := heldKeys(d)[k]
		switch {
		case bound <= now && held:
			t.Fatalf("a day of running time on, a key past its hard bound is still held")
		case bound > now && !held:
			t.Fatal("a key went before its hard bound")
		case !held:
			gone++
			if p := privs[k]; bytes.Count(p, []byte{0}) != len(p) {
				t.Fatal("a deleted private key was not cleared in memory (FS-K12)")
			}
		}
	}
	if gone == 0 {
		t.Fatal("no key went once the local gate opened")
	}
	if k, _ := d.ix.IXKeyByKID(kid); k != nil {
		t.Fatal("the interaction key past close_after stayed")
	}
	if _, err := d.ix.Get("ix_public"); err == nil {
		t.Fatal("the public_cap task past its retention stayed")
	}
}

// TestStartSweepFollowsTheGate (FS-K15, mutation 19 at the start): the
// sweeps a process runs as it starts, before its first period, run at the
// level of the period New ran. A node whose hub has not answered yet starts
// closed and deletes nothing — not an interaction key past close_after, not
// a public call past its retention; one that starts with a day of running
// time behind it starts at local and deletes both.
func TestStartSweepFollowsTheGate(t *testing.T) {
	if n := newTestDaemon(t, "", true); n.startLevel != clockFull {
		t.Fatalf("a node without a hub started at level %v, want full", n.startLevel)
	}
	d := newTestDaemon(t, silentHub, true)
	if d.startLevel != clockClosed {
		t.Fatalf("a node whose hub never answered started at level %v, want closed", d.startLevel)
	}
	now := d.nowMS()
	kid := keyedOutboundTask(t, d, "ix_closing", now-20*dayMS) // close_after five days ago
	endedPublicCapTask(t, d, "ix_public", now-20*dayMS)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.keyMaintenanceLoop(ctx) // its start sweep, then it stops
	if k, err := d.ix.IXKeyByKID(kid); err != nil || k == nil {
		t.Fatalf("the start sweep at closed deleted the interaction key: %v", err)
	}
	if _, err := d.ix.Get("ix_public"); err != nil {
		t.Fatalf("the start sweep at closed deleted the public call: %v", err)
	}
	// A day of running time with the hub silent, on disk: the next process
	// starts at local, and its start sweep deletes what is due.
	d.enc.mu.Lock()
	d.enc.run = clockFallbackAfter
	d.enc.mu.Unlock()
	if err := d.enc.persist(); err != nil {
		t.Fatal(err)
	}
	layout := d.layout
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2 := startAt(t, layout.Root)
	if d2.startLevel != clockLocal {
		t.Fatalf("after a day of running time on disk the start level is %v, want local", d2.startLevel)
	}
	waitUntil(t, "the start sweep at local", func() bool {
		k, _ := d2.ix.IXKeyByKID(kid)
		_, err := d2.ix.Get("ix_public")
		return k == nil && err != nil
	})
}

// stallWorld runs a node whose hub never confirms its clock — its Date six
// minutes off, or no answer at all — for days, one period an hour, and
// checks each epoch key goes no earlier than its hard bound and no later
// than a day of running time after it (TestHardBoundDeletesDespiteHubStall).
func stallWorld(t *testing.T, skewed bool, days int, restartEvery uint64) {
	d := newTestDaemon(t, silentHub, true)
	start := epochOf(d.nowMS()) + 30*60*1000
	clocks := newGateClocks(t, d, start)
	const step = hourMS
	bounds := map[string]uint64{}
	deleted := map[string]uint64{}
	var sinceRestart uint64
	for clocks.wall.Load() < start+uint64(days)*dayMS {
		if skewed {
			hubSays(d, clocks.wall.Load()-6*minuteMS)
		}
		d.keyMaintenance()
		now := clocks.wall.Load()
		held := heldKeys(d)
		for k, b := range held {
			if _, seen := bounds[k]; !seen {
				bounds[k] = b
			}
		}
		for k, b := range bounds {
			if _, ok := held[k]; ok || deleted[k] != 0 {
				continue
			}
			deleted[k] = now
			if now < b {
				t.Fatalf("a key went %d ms before its hard bound", b-now)
			}
		}
		for k, b := range bounds {
			if _, ok := held[k]; ok && now > b+clockFallbackAfter+step {
				t.Fatalf("a key is still held %d ms past its hard bound (skewed %v)", now-b, skewed)
			}
		}
		clocks.run(step)
		sinceRestart += step
		if restartEvery != 0 && sinceRestart >= restartEvery {
			clocks.run(hourMS) // stopped for an hour
			reloadRing(t, d)
			sinceRestart = 0
		}
	}
	if len(deleted) < 15 {
		t.Fatalf("only %d keys went in %d days", len(deleted), days)
	}
}

// TestHardBoundDeletesDespiteHubStall (FS-K14a, mutation 41; the epoch key
// part, P-6 item 3): a hub that answers with a Date six minutes off, or does
// not answer at all, for a month keeps the hub gate closed the whole time;
// every epoch key still goes, no earlier than not_after + L and no later
// than a day of running time after it. So do an interaction key past
// close_after and the tasks past their retention (stallContentWorld).
func TestHardBoundDeletesDespiteHubStall(t *testing.T) {
	t.Run("Date six minutes off", func(t *testing.T) { stallWorld(t, true, 33, 0) })
	t.Run("no answer", func(t *testing.T) { stallWorld(t, false, 33, 0) })
	t.Run("close_after and retention, Date six minutes off", func(t *testing.T) { stallContentWorld(t, true) })
	t.Run("close_after and retention, no answer", func(t *testing.T) { stallContentWorld(t, false) })
}

// TestStallCounterSurvivesRestart (FS-K14a, mutation 42): unverified_run is
// on disk and a restart keeps it, so a daemon restarted every 12 hours
// still reaches a day of running time; the time it was stopped is not
// counted.
func TestStallCounterSurvivesRestart(t *testing.T) {
	d := newTestDaemon(t, silentHub, true)
	clocks := newGateClocks(t, d, epochOf(d.nowMS())+hourMS)
	const step = 30 * 60 * 1000
	var ran uint64
	for restarts := 0; restarts < 2; restarts++ {
		for i := uint64(0); i < 12*hourMS/step; i++ {
			d.keyMaintenance()
			clocks.run(step)
			ran += step
		}
		before := d.enc.unverifiedRun()
		clocks.run(5 * hourMS) // stopped for five hours: not running time
		reloadRing(t, d)
		if got := d.enc.unverifiedRun(); got != before {
			t.Fatalf("after restart %d unverified_run is %d, was %d", restarts+1, got, before)
		}
	}
	// Each process counted all but its first period: 2 × 23 half hours.
	if run, want := d.enc.unverifiedRun(), 2*(12*hourMS/step-1)*step; run != want {
		t.Fatalf("after %d ms of running time across restarts unverified_run is %d, want %d", ran, run, want)
	}
	d.keyMaintenance() // the first period of the third process: adds nothing
	for i := 0; i < 2; i++ {
		clocks.run(step)
		d.keyMaintenance()
	}
	if level, _ := clockGate(d.clockObservation(), d.enc.unverifiedRun(), clocks.wall.Load(), clocks.mono.Load()); level != clockLocal {
		t.Fatalf("a day of running time across two restarts: level %v, want local", level)
	}
	// And a whole daemon restart reads it back from the ring file.
	want := d.enc.unverifiedRun()
	layout := d.layout
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if got := d2.enc.unverifiedRun(); got != want {
		t.Fatalf("after a daemon restart unverified_run is %d, want %d", got, want)
	}
	// Restarted every 12 hours with the hub silent, keys still go
	// (TestHardBoundDeletesDespiteHubStall's world with restarts).
	stallWorld(t, false, 33, 12*hourMS)
}

// TestStallCounterResetsOnWallJump (FS-K14a step 3, mutation 43; P-6 item
// 4): a wall clock that steps by more than a minute while the daemon runs
// starts the day of running time again.
func TestStallCounterResetsOnWallJump(t *testing.T) {
	d := newTestDaemon(t, silentHub, true)
	clocks := newGateClocks(t, d, epochOf(d.nowMS())+hourMS)
	const step = hourMS
	for i := 0; i < 21; i++ {
		d.keyMaintenance()
		clocks.run(step)
	}
	if run := d.enc.unverifiedRun(); run < 20*hourMS {
		t.Fatalf("unverified_run %d after 21 hours", run)
	}
	clocks.wall.Add(clockJumpTolerance + 1) // the wall steps while running
	d.keyMaintenance()
	if run := d.enc.unverifiedRun(); run != 0 {
		t.Fatalf("after a step unverified_run is %d, want 0", run)
	}
	// A minute of drift between two periods is no step.
	clocks.run(step)
	clocks.wall.Add(clockJumpTolerance)
	d.keyMaintenance()
	if run := d.enc.unverifiedRun(); run != step {
		t.Fatalf("after a period with a minute of drift unverified_run is %d, want %d", run, step)
	}
	for i := 0; i < 22; i++ {
		clocks.run(step)
		d.keyMaintenance()
		if level, _ := clockGate(d.clockObservation(), d.enc.unverifiedRun(), clocks.wall.Load(), clocks.mono.Load()); level != clockClosed {
			t.Fatalf("%d hours after the step: level %v", i+2, level)
		}
	}
	clocks.run(step)
	d.keyMaintenance()
	if level, _ := clockGate(d.clockObservation(), d.enc.unverifiedRun(), clocks.wall.Load(), clocks.mono.Load()); level != clockLocal {
		t.Fatalf("a day after the step: level %v, want local", level)
	}
}

// TestMaintenanceRunsEveryPeriod (FS-K4): the receive maintenance loop runs
// a maintenance period every S, which counts running time.
func TestMaintenanceRunsEveryPeriod(t *testing.T) {
	old := keyMaintainEvery.get()
	keyMaintainEvery.ns.Store(int64(20 * time.Millisecond))
	t.Cleanup(func() { keyMaintainEvery.ns.Store(int64(old)) })
	d := newTestDaemon(t, silentHub, true)
	clocks := newGateClocks(t, d, epochOf(d.nowMS())+hourMS)
	go func() {
		for i := 0; i < 200; i++ {
			clocks.run(minuteMS)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	waitUntil(t, "unverified_run to grow", func() bool { return d.enc.unverifiedRun() > 0 })
}

// TestDrainMarkNeedsAck (FS-K9 to FS-K11): a poll round sets the hub's
// drain mark to when it asked, only when the hub said nothing more is
// queued and every row it read was acknowledged; a held envelope's kid is
// kept until a round acknowledges it or finds it gone; pollFresh counts as
// a round. A node without a hub has nothing to drain.
func TestDrainMarkNeedsAck(t *testing.T) {
	srv, req, prov := registeredPair(t)
	h := fakeHubAt(t, srv.URL)
	ctx := context.Background()
	if mark, held := (&Daemon{}).drainInputs(); mark != math.MaxUint64 || len(held) != 0 {
		t.Fatalf("no hub: drain mark %d, %d held", mark, len(held))
	}
	if mark, _ := prov.drainInputs(); mark != 0 {
		t.Fatalf("before any round: drain mark %d", mark)
	}
	clock := atClock(prov, prov.nowMS())
	if _, err := req.Delegate(ctx, prov.AID(), "a task", nil); err != nil {
		t.Fatal(err)
	}
	// The ack fails: no mark.
	h.mu.Lock()
	h.ackFail = len(h.ackRequests) + 1
	h.mu.Unlock()
	if err := prov.pollOnce(ctx); err == nil {
		t.Fatal("the ack did not fail")
	}
	if mark, _ := prov.drainInputs(); mark != 0 {
		t.Fatalf("a round whose ack failed set the drain mark to %d", mark)
	}
	// The ack succeeds and nothing more is queued: the mark is when the
	// round asked.
	// The hub takes two seconds to answer: rows it accepted meanwhile may
	// be in no page, so the mark is the asking, not the answer.
	h.mu.Lock()
	h.whilePolling = func() { clock.Add(2000) }
	h.mu.Unlock()
	asked := clock.Add(minuteMS)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if mark, _ := prov.drainInputs(); mark != asked {
		t.Fatalf("drain mark %d, want %d (when the round asked)", mark, asked)
	}
	// The hub says more is queued: the mark stays.
	more := true
	h.mu.Lock()
	h.pollMore = &more
	h.mu.Unlock()
	clock.Add(minuteMS)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if mark, _ := prov.drainInputs(); mark != asked {
		t.Fatalf("a round with more set moved the drain mark to %d", mark)
	}
	h.mu.Lock()
	h.pollMore = nil
	h.mu.Unlock()
	// An envelope held back (a message for a task the provider does not
	// know yet): its kid is held, and goes with the row.
	env := sealFrom(t, req, prov, seal.TypeMessage, "ix_0123456789abcdef0123456789abcdef", chatBody(t, "early", ""))
	injectEnvelope(t, srv, prov.AID(), env)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	o, err := seal.ParseOuter(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := prov.drainInputs(); !held.has(o.KID) {
		t.Fatal("the held envelope's kid is not held")
	}
	// The rounds after it — one reading on past the row, one going back
	// over it — keep it held while the hub holds it.
	for i := 0; i < 2; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if _, held := prov.drainInputs(); !held.has(o.KID) {
			t.Fatalf("round %d after it: a row the hub still holds is no longer held", i+1)
		}
	}
	h.mu.Lock()
	h.mailbox = nil
	h.mu.Unlock()
	for i := 0; i < 3; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, held := prov.drainInputs(); held.has(o.KID) {
		t.Fatal("a row the hub no longer holds is still held")
	}
	// pollFresh is a round too.
	fresh := clock.Add(minuteMS)
	prov.pollFresh(ctx)
	if mark, _ := prov.drainInputs(); mark != fresh {
		t.Fatalf("after pollFresh the drain mark is %d, want %d", mark, fresh)
	}
}

// TestHeldRowsFollowTheMailbox (FS-K10): a held row is forgotten only when
// the hub no longer holds it — a round acknowledged it, or read the span it
// was in without finding it — and never by a round that did not read that
// span: one reading on from after it, or one whose page the hub cut before
// it. The hub URL is one hub with or without its trailing slash.
func TestHeldRowsFollowTheMailbox(t *testing.T) {
	const hub = "https://hub.example"
	k5, k7, k9 := []byte("kid-5"), []byte("kid-7"), []byte("kid-9")
	var s drainState
	rows := func() map[int64]bool {
		out := map[int64]bool{}
		for r := range s.held {
			out[r.id] = true
		}
		return out
	}
	want := func(step string, ids ...int64) {
		t.Helper()
		got := rows()
		if len(got) != len(ids) {
			t.Fatalf("%s: held %v, want %v", step, got, ids)
		}
		for _, id := range ids {
			if !got[id] {
				t.Fatalf("%s: held %v, want %v", step, got, ids)
			}
		}
	}
	// Rows 5, 7 and 9 held back.
	s.noteRound(pollRound{hub: hub, read: []int64{5, 7, 9}, kids: map[int64][]byte{5: k5, 7: k7, 9: k9}, ackOK: true})
	want("held", 5, 7, 9)
	// Reading on from after them (nothing new) says nothing of them.
	s.noteRound(pollRound{hub: hub + "/", from: 9, kids: map[int64][]byte{}, ackOK: true})
	want("a round reading on past them", 5, 7, 9)
	// Going back with a page cut after 5: 7 and 9 were not read.
	s.noteRound(pollRound{hub: hub, read: []int64{5}, more: true, kids: map[int64][]byte{5: k5}, ackOK: true})
	want("a page cut after 5", 5, 7, 9)
	// Going back to the end without finding 7: the hub holds it no more.
	s.noteRound(pollRound{hub: hub, read: []int64{5, 9}, kids: map[int64][]byte{5: k5, 9: k9}, ackOK: true})
	want("7 not found", 5, 9)
	// An acknowledgement that failed keeps 5; one that succeeded does not.
	s.noteRound(pollRound{hub: hub, from: 4, read: []int64{5}, more: true, acked: []int64{5}, kids: map[int64][]byte{}})
	want("5's ack failed", 5, 9)
	s.noteRound(pollRound{hub: hub, from: 4, read: []int64{5}, more: true, acked: []int64{5}, kids: map[int64][]byte{}, ackOK: true})
	want("5 acknowledged", 9)
	// Another hub's rounds touch none of this hub's rows.
	s.noteRound(pollRound{hub: "https://other.example", kids: map[int64][]byte{}, ackOK: true})
	want("another hub drained", 9)
}

// TestNoPublishBetweenE1AndE2 (core §4.7; SS-7's handoff): between the
// emergency rotation's E1, which put the new mailbox key in place, and E2,
// no key set is signed or published, by the maintenance period, the
// mailbox key's retries or a registration: a set signed then would publish
// the old epoch keys under the new mailbox key. E2 regenerates the ring and
// publishes the set of the new keys.
func TestNoPublishBetweenE1AndE2(t *testing.T) {
	srv, _, prov := registeredPair(t)
	h := fakeHubAt(t, srv.URL)
	ctx := context.Background()
	hubSet := func() []byte {
		h.mu.Lock()
		defer h.mu.Unlock()
		return append([]byte(nil), h.agents[prov.AID()].keyset...)
	}
	before, signed := hubSet(), append([]byte(nil), prov.enc.SignedSet()...)
	posts := func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.keyPosts
	}
	posts0 := posts()
	held := true
	prov.setKeyHold(func() bool { return held })
	// E1: the new mailbox key, in its transaction.
	if err := prov.ix.Update(prov.capsEmergencyTx); err != nil {
		t.Fatal(err)
	}
	clock := atClock(prov, epochOf(prov.nowMS())+seal.EpochMS+60_000) // a new epoch too
	h.mu.Lock()
	h.keysAt = clock.Load // the hub's clock goes with the node's
	h.mu.Unlock()
	prov.keyMaintenance()
	prov.maintainKeyRing()
	if err := prov.republishKeys(ctx, srv.URL); err == nil {
		t.Fatal("a republish between E1 and E2 went through")
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hubSet(), before) || posts() != posts0 {
		t.Fatal("a key set was published between E1 and E2")
	}
	if !bytes.Equal(prov.enc.SignedSet(), signed) {
		t.Fatal("a key set was signed between E1 and E2")
	}
	// E2.
	held = false
	if done, err := prov.regenerateKeys(42); err != nil || !done {
		t.Fatalf("regenerate: %v %v", done, err)
	}
	if err := prov.publishKeys(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	set := decodedSet(t, hubSet())
	cur, _ := prov.mbox.Current()
	if set.Mbox.Epoch != cur.Epoch || !bytes.Equal(set.Mbox.Pub, cur.Pub) {
		t.Fatal("E2 did not publish the new mailbox key")
	}
}

// TestEmergencyPublishesOnlyNewKeys (FS-K18, mutation 37; red team A4): the
// set an emergency rotation publishes holds none of the keys before it,
// whose private halves the old data directory has; those stay held until
// they may go, so the mail already queued for them still opens, and their
// A(k) is the moment the new set was registered.
func TestEmergencyPublishesOnlyNewKeys(t *testing.T) {
	srv, _, prov := registeredPair(t)
	h := fakeHubAt(t, srv.URL)
	ctx := context.Background()
	old := ringKids(prov)
	if err := prov.ix.Update(prov.capsEmergencyTx); err != nil {
		t.Fatal(err)
	}
	clock := atClock(prov, prov.nowMS())
	if _, err := prov.regenerateKeys(3); err != nil {
		t.Fatal(err)
	}
	registered := clock.Add(1000)
	if err := prov.publishKeys(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	stored := append([]byte(nil), h.agents[prov.AID()].keyset...)
	h.mu.Unlock()
	for _, kid := range setKIDList(t, stored) {
		if old[string(kid)] != nil {
			t.Fatal("the emergency rotation published a key from before it")
		}
	}
	for kid := range old {
		if _, ok := prov.enc.Key([]byte(kid)); !ok {
			t.Fatal("an old key went at once: mail already queued for it would not open")
		}
		k := ringKids(prov)[kid]
		if !k.Retired || k.RemovedAt != registered {
			t.Fatalf("an old key: retired %v, removed_at %d, want %d", k.Retired, k.RemovedAt, registered)
		}
		if a := lastAcceptance(k); a != registered {
			t.Fatalf("A(k) of an old key is %d, want %d", a, registered)
		}
	}
	cur, _ := prov.mbox.Current()
	if set := decodedSet(t, stored); set.Mbox.Epoch != cur.Epoch {
		t.Fatal("the new set does not carry the new mailbox key")
	}
}

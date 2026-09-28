package daemon

// Red-team PoCs for SI-10 (at-least-once delivery, exactly-once
// processing). Each test asserts that the ATTACK SUCCEEDS: a passing test
// means the defect is present. See the finding named in each comment.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/provider"
)

// misrouteTransport is a direct transport whose address for the recipient
// leads to another node's peer process (a stale rendezvous entry, a reused
// host:port, an on-path box): Send hands the envelope to that node's
// Inbound, exactly as tools/anetpeer's handOff does, and reports what its
// ack says.
type misrouteTransport struct{ wrong *Daemon }

func (m misrouteTransport) Name() string                           { return "p2p-misroute" }
func (m misrouteTransport) Reachable(context.Context, string) bool { return true }
func (m misrouteTransport) Send(ctx context.Context, _ string, env []byte) error {
	return m.wrong.Inbound().Receive(ctx, env)
}

// [redteam:F22] regression (was TestRedteamSI10_P2PMisrouteIsAckedAndTheMessageIsLost).
// SI-10 / §3.10: an envelope that reaches the wrong node over p2p (a stale
// rendezvous entry, a reused host:port) cannot be opened there. Over a
// direct transport that is a temporary refusal: the wrong node does not
// acknowledge it, the sender falls through to the hub, and the real
// recipient gets the message from its mailbox.
func TestRedteamSI10_P2PMisrouteFallsBackToTheHub(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	wrong := newTestDaemon(t, srv.URL, false) // an honest, unrelated node
	req.RegisterTransport(misrouteTransport{wrong: wrong})

	id, err := req.Delegate(ctx, prov.AID(), "please do X", nil)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	// The wrong node refused it as one it cannot open, temporarily.
	if n := counter(wrong, seal.ReasonWrongRecipient); n != 0 {
		t.Fatalf("wrong node counted a permanent wrong-recipient (%v)", wrong.ReceiveStats())
	}
	if n := counter(wrong, transientDirectUnopened); n != 1 {
		t.Fatalf("wrong node t-direct-unopened = %d, want 1 (%v)", n, wrong.ReceiveStats())
	}
	// The sender fell through to the hub: the provider's mailbox holds it
	// and nothing is left to retry.
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("hub holds %d envelopes for the provider, want the delegation", n)
	}
	if rows, err := req.ix.Outbox(id); err != nil || len(rows) != 0 {
		t.Fatalf("requester outbox = %d rows (%v)", len(rows), err)
	}
	// The intended provider gets the task.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.Get(id); err != nil {
		t.Fatalf("provider does not have the task: %v", err)
	}
	// From the hub mailbox the same refusal stays permanent: that mailbox
	// is the node's own.
	env := craft(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "for prov", ""), nil)
	if r := receive(t, wrong, env); r.class != rxDropped || r.reason != seal.ReasonWrongRecipient {
		t.Fatalf("hub path: %+v, want a permanent %s", r, seal.ReasonWrongRecipient)
	}
}

// sharedClock is one wire clock for several daemons, advanced by the test.
type sharedClock struct{ now uint64 }

func (c *sharedClock) fn() func() uint64 { return func() uint64 { return c.now } }

// driveOutage keeps the hub's relay refusing sends (503) while the
// requester's retry loop runs on its own backoff, until the clock passes
// until. It returns the delegation row as the outage leaves it.
func driveOutage(t *testing.T, ctx context.Context, d *Daemon, clk *sharedClock, rowID int64, until uint64) *interactions.OutboxItem {
	t.Helper()
	for {
		it, err := d.ix.GetOutbox(rowID)
		if err != nil {
			t.Fatalf("outbox row: %v", err)
		}
		if uint64(it.NextAt) > until {
			return it
		}
		if uint64(it.NextAt) > clk.now {
			clk.now = uint64(it.NextAt)
		}
		d.flushOutbox(ctx)
	}
}

// SI-10 / §3.6 step 9 / §4.2: the retry queue delivers rows in next_at
// order, not per-task FIFO. After a hub outage the delegation sits in a
// long backoff while a cancel queued later goes out at once. The provider
// holds the cancel for the 10-minute unknown-ix window, then answers
// TaskNotFound and acknowledges it; the delegation arrives afterwards and
// the capability EXECUTES although the requester canceled it (its local
// state says canceled).
func TestRedteamSI10_OutboxReorderLosesCancelAndCanceledCallExecutes(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	clk := &sharedClock{now: uint64(time.Now().UnixMilli())}
	req.setClock(clk.fn())
	prov.setClock(clk.fn())
	start := clk.now

	// The hub's relay is down (503): the delegation is recorded and queued.
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	rows, err := req.ix.Outbox(id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("outbox = %+v (%v)", rows, err)
	}
	dRow := rows[0].ID
	// A 25-minute outage, the retry loop backing off as it does.
	d := driveOutage(t, ctx, req, clk, dRow, start+uint64((25*time.Minute).Milliseconds()))
	t.Logf("after the outage the delegation is next tried %s after the start (%d attempts)",
		time.Duration(uint64(d.NextAt)-start)*time.Millisecond, d.Attempts)

	// The hub is back. The requester cancels the call it no longer wants.
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = false })
	clk.now = start + uint64((25*time.Minute + 30*time.Second).Milliseconds())
	if uint64(d.NextAt) <= clk.now+uint64((11*time.Minute).Milliseconds()) {
		t.Fatalf("setup: the delegation is due too soon (%d)", d.NextAt)
	}
	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cur, _ := req.ix.Get(id); cur.State != interactions.StateCanceled {
		t.Fatalf("requester state = %s, want canceled", cur.State)
	}
	// The cancel went out at once; the delegation is still backing off.
	if q := queuedFor(t, srv, prov.AID()); len(q) != 1 {
		t.Fatalf("hub holds %d envelopes for the provider, want only the cancel", len(q))
	}
	if _, err := req.ix.GetOutbox(dRow); err != nil {
		t.Fatalf("the delegation row is gone: %v", err)
	}

	// The provider sees a cancel for a task it does not know: held.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if counter(prov, transientUnknownIX) == 0 {
		t.Fatalf("cancel not held: %v", prov.ReceiveStats())
	}
	// Eleven minutes later the delegation still has not come; the cancel is
	// answered TaskNotFound and acknowledged.
	clk.now += uint64((11 * time.Minute).Milliseconds())
	for i := 0; i < 3; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if counter(prov, dropUnknownIX) != 1 {
		t.Fatalf("cancel not dropped as unknown-ix: %v", prov.ReceiveStats())
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("%d envelopes still in the provider's mailbox", n)
	}

	// The delegation's backoff ends; it is delivered and executed.
	clk.now = uint64(d.NextAt)
	req.flushOutbox(ctx)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("capability ran %d times after the cancel; attack failed", n)
	}
	pix, err := prov.ix.Get(id)
	if err != nil || len(pix.Receipt) == 0 {
		t.Fatalf("provider did not complete the canceled call: %+v (%v)", pix, err)
	}
	if cur, _ := req.ix.Get(id); cur.State != interactions.StateCanceled {
		t.Fatalf("requester state = %s", cur.State)
	}
}

// Decision Q1 / SI-10: the relay cursor returns to the head of the mailbox
// only on a round whose page after the cursor is empty. A stranger that
// sends one message per poll round for an interaction the node does not
// hold (class T: held for the unknown-ix window) keeps every page non-empty
// and "held", so the cursor never returns. A legitimate envelope held back
// once (here a single store error) is never tried again for as long as the
// stranger keeps going, and the stranger's own held envelopes are never
// re-read, never turn permanent (TaskNotFound) and pile up in the mailbox
// toward its 5000-message quota.
func TestRedteamSI10_StrangerPinsTheRelayCursorAndStarvesHeldMail(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "text task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	clk := &sharedClock{now: uint64(time.Now().UnixMilli())}
	prov.setClock(clk.fn())

	// A legitimate follow-up hits one store error: held (not acked), as
	// SI-10 requires.
	if err := req.SendMessage(ctx, id, "legit follow-up", nil); err != nil {
		t.Fatal(err)
	}
	failOnce := true
	prov.setRxFault(func(typ string) error {
		if typ == seal.TypeMessage && failOnce {
			failOnce = false
			return errors.New("injected store error")
		}
		return nil
	})
	before := countMsgs(t, prov, id)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	prov.setRxFault(nil)
	if countMsgs(t, prov, id) != before || len(queuedFor(t, srv, prov.AID())) != 1 {
		t.Fatal("setup: the follow-up was not held back")
	}

	// A registered stranger sends one message per round (1/s is 5% of the
	// hub's per-sender budget) for interactions this node does not hold.
	mallory := newStranger(t)
	const rounds = 40 // 40 rounds, 30 s of wire time each: 20 minutes
	for i := 0; i < rounds; i++ {
		clk.now += 30_000
		now := clk.now
		ix := "ix_mallory_" + time.Duration(i).String()
		injectEnvelope(t, srv, prov.AID(), craft(t, mallory, prov, seal.TypeMessage, ix,
			chatBody(t, "x", ""), func(in *seal.SealedInner) { in.TS, in.Exp = now, now+messageLifetimeMS }))
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Twenty minutes on: the legitimate follow-up (which would succeed
	// now) was never tried again.
	if got := countMsgs(t, prov, id); got != before {
		t.Fatalf("the held follow-up was processed during the flood (%d → %d); attack failed", before, got)
	}
	// Every stranger message is still queued: none was re-read after its
	// window, so none turned into TaskNotFound and none was acknowledged.
	if n := len(queuedFor(t, srv, prov.AID())); n != rounds+1 {
		t.Fatalf("%d envelopes queued, want %d (held follow-up + every stranger message)", n, rounds+1)
	}
	if counter(prov, dropUnknownIX) != 0 {
		t.Fatalf("stranger messages turned permanent: %v", prov.ReceiveStats())
	}

	// Control: the moment the stranger stops, the head is read again and
	// the follow-up is processed.
	for i := 0; i < 2; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := countMsgs(t, prov, id); got != before+1 {
		t.Fatalf("control: follow-up still not processed (%d → %d)", before, got)
	}
}

// §3.6 [C15c] / SI-3 / SI-10: a delegation refused at step 9 is kept only in
// an in-memory LRU, never in the persistent replay table. The requester is
// told `rejected` (terminal). A hub that kept the envelope replays it after
// the provider restarted (or after 4096 other refusals evicted the entry);
// the policy has since admitted the requester, so the replay is ACCEPTED
// and the capability executes, at a time the hub chose, for a task the
// requester was told was rejected. (The code comment's claim that such a
// replay "reaches only side effects that are themselves rate limited"
// does not hold when the policy decision has changed.)
func TestRedteamSI10_ReplayOfARefusedDelegationExecutesAfterPolicyChange(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, false) // closed: refuses everyone
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.Providers().Register(ctx, &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID()) // what a malicious hub keeps
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if counter(prov, dropNotAccepting) != 1 || len(queuedFor(t, srv, prov.AID())) != 0 {
		t.Fatalf("setup: not refused and acked: %v", prov.ReceiveStats())
	}
	waitUntil(t, "the rejected notice", func() bool { return len(queuedFor(t, srv, req.AID())) == 1 })
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, _ := req.ix.Get(id); cur.State != interactions.StateRejected {
		t.Fatalf("requester state = %s, want rejected", cur.State)
	}

	// Later the operator allows the requester, and the provider restarts
	// (an upgrade, a reboot).
	allowPeers(t, prov, req.AID())
	prov2 := restartDaemon(t, prov)
	lamp := &lampProvider{}
	if err := prov2.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	// The hub replays the refused envelope (still within its 14-day exp).
	injectEnvelope(t, srv, prov2.AID(), env)
	if err := prov2.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("replayed refused delegation ran %d times; attack failed (%v)", n, prov2.ReceiveStats())
	}
	if pix, err := prov2.ix.Get(id); err != nil || len(pix.Receipt) == 0 {
		t.Fatalf("provider did not complete the replayed call: %v", err)
	}
	if cur, _ := req.ix.Get(id); cur.State != interactions.StateRejected {
		t.Fatalf("requester state = %s", cur.State)
	}
}

// SI-10 "the same envelope over p2p and the hub at once is processed
// once": step 9 (authorization, including the public-capability admission
// quota) runs BEFORE the (from, mid) lock and the replay check of step 10.
// Two copies of one delegation in flight together (the p2p copy is slow to
// commit, the sender's 3 s p2p timeout falls back to the hub) both take
// admission; with the quota at its edge the second copy is REFUSED and the
// provider sends the requester a signed `rejected` for the very task the
// first copy is executing. The requester ends in `rejected` while the call
// ran and was answered.
func TestRedteamSI10_ConcurrentCopiesOfAPublicCallGetBothExecutedAndRejected(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	prov.mu.Lock()
	in := prov.cfg.inbound()
	in.PublicCapabilities = []PublicCapability{{ID: lampCap, MaxInflight: 1}}
	prov.cfg.Inbound = &in
	prov.mu.Unlock()

	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	clearMailbox(t, srv, prov.AID())

	// The p2p copy is inside its step-10 transaction (a slow disk, a busy
	// write lock) when the hub copy arrives.
	entered, release := make(chan struct{}), make(chan struct{})
	var once bool
	prov.setRxFault(func(typ string) error {
		if typ == seal.TypeDelegate && !once {
			once = true
			close(entered)
			<-release
		}
		return nil
	})
	p2pDone := make(chan error, 1)
	go func() { p2pDone <- prov.Inbound().Receive(ctx, env) }()
	<-entered
	hubCopy := receive(t, prov, env)
	if hubCopy.class != rxDropped || hubCopy.reason != dropRefusedPrefix+admitInflightLimit {
		t.Fatalf("hub copy: %+v; attack failed", hubCopy)
	}
	waitUntil(t, "the rejected notice", func() bool { return len(queuedFor(t, srv, req.AID())) == 1 })
	close(release)
	if err := <-p2pDone; err != nil {
		t.Fatalf("p2p copy: %v", err)
	}
	prov.setRxFault(nil)
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("capability ran %d times", n)
	}
	if n := len(queuedFor(t, srv, req.AID())); n != 2 {
		t.Fatalf("%d envelopes for the requester, want the rejected notice and the result", n)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	cur, err := req.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if cur.State != interactions.StateRejected || len(cur.Receipt) == 0 {
		t.Fatalf("requester: %s receipt=%d; attack failed", cur.State, len(cur.Receipt))
	}
}

// slowLamp is a long-running capability (runs off the poll loop) that
// waits for its gate.
type slowLamp struct {
	gate    chan struct{}
	started chan struct{}
	runs    atomic.Int32
}

func (p *slowLamp) ID() string { return "slowlamp" }
func (p *slowLamp) Capabilities(context.Context) ([]string, error) {
	return []string{"flash.firmware@sim/board-1"}, nil
}
func (p *slowLamp) Describe(context.Context) (string, error) { return "", nil }
func (p *slowLamp) Health(context.Context) error             { return nil }
func (p *slowLamp) InvokeTimeout(string) (time.Duration, bool) {
	return time.Hour, true
}
func (p *slowLamp) Invoke(ctx context.Context, _ provider.Call) (effect.Effect, error) {
	p.runs.Add(1)
	close(p.started)
	select {
	case <-p.gate:
	case <-ctx.Done():
	}
	return effect.Effect{Status: effect.OK, Record: &tsir.EffectRecord{Metrics: map[string]float64{"flashed": 1}}}, nil
}

// C1 / SI-10 / SI-6: New starts the modules — the p2p transport among them,
// which begins handing envelopes to the receive pipeline at once — BEFORE
// recoverInterrupted, whose contract is "dealt with before any mail is
// read". recoverInterrupted does not check d.running or the creation time,
// so a long call delivered over p2p in that window and already running is
// reported failed / effect UNVERIFIED / interrupted to the requester, and
// when it finishes its real result and receipt are discarded ("ended
// before its result"). The test runs recoverInterrupted exactly as New
// would after such a delivery.
func TestRedteamSI10_StartupRecoveryReportsAP2PCallRunningNowAsInterrupted(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	slow := &slowLamp{gate: make(chan struct{}), started: make(chan struct{})}
	if err := prov.Providers().Register(ctx, slow); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "flash.firmware@sim/board-1", map[string]any{"image": "v2"})
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	clearMailbox(t, srv, prov.AID())
	// Delivered over p2p during start-up: accepted, marked working, running.
	if err := prov.Inbound().Receive(ctx, env); err != nil {
		t.Fatal(err)
	}
	<-slow.started
	// New reaches recoverInterrupted.
	prov.recoverInterrupted()
	pix, err := prov.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if pix.State != interactions.StateFailed || !strings.Contains(pix.ResultMeta, "interrupted") {
		t.Fatalf("provider: %s %s; attack failed", pix.State, pix.ResultMeta)
	}
	// The firmware flash completes; its result is thrown away.
	close(slow.gate)
	waitUntil(t, "the call to end", func() bool { _, running := prov.running.Load(id); return !running })
	pix, _ = prov.ix.Get(id)
	var res capabilityResult
	_ = json.Unmarshal(pix.Result, &res)
	if res.Status != string(effect.Unverified) {
		t.Fatalf("stored result %+v; the real result was kept", res)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, _ := req.ix.Get(id); cur.State != interactions.StateFailed {
		t.Fatalf("requester: %s", cur.State)
	}
	if n := slow.runs.Load(); n != 1 {
		t.Fatalf("runs = %d", n)
	}
}

// Q1 / §3.6 step 9 / §3.7 quota: any registered AID's message for an
// interaction the node does not hold is class T for ten minutes, so the
// ONLINE recipient's own daemon keeps it in the hub mailbox. The mailbox
// quota (5000 envelopes) is filled by one stranger in 250 s at the hub's
// per-sender rate (20/s) and kept full at 5000/600 s ≈ 8.4/s — below that
// rate — while every legitimate sender is refused 507 and backs off (up to
// 24 h per retry). Scaled here to a 50-envelope quota.
func TestRedteamSI10_StrangerKeepsAnOnlineNodesMailboxFull(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	const quota = 50
	setFake(t, srv.URL, func(h *fakeHub) { h.mailboxCap = quota })
	mallory := newStranger(t)
	for i := 0; i < quota; i++ {
		ix := "ix_fill_" + time.Duration(i).String()
		injectEnvelope(t, srv, prov.AID(), craft(t, mallory, prov, seal.TypeMessage, ix, chatBody(t, "x", ""), nil))
	}
	// The provider is online and polling.
	for i := 0; i < 5; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != quota {
		t.Fatalf("the provider acked stranger messages (%d left); attack failed", n)
	}
	// A legitimate delegation cannot get in.
	id, err := req.Delegate(ctx, prov.AID(), "real work", nil)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	rows, _ := req.ix.Outbox(id)
	if len(rows) != 1 || !strings.Contains(rows[0].LastError, "full") {
		t.Fatalf("requester outbox = %+v; the delegation was not refused for a full mailbox", rows)
	}
	for i := 0; i < 5; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := prov.ix.Get(id); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the provider got the delegation (%v)", err)
	}
}

// SI-10 / §3.6 step 5: "from the future" (ts > now + 5 min) is classed
// permanent and acknowledged, although it is the one refusal that time
// alone cures. A sender whose clock runs 6 minutes fast (or a receiver
// whose clock is 6 minutes slow) loses every message: the hub deletes it
// on the ack, and the very same bytes would have been accepted two
// minutes later.
func TestRedteamSI10_FromFutureIsAckedAlthoughItIsTemporary(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	before := countMsgs(t, prov, id)
	fast := uint64(time.Now().Add(6 * time.Minute).UnixMilli())
	env := craft(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "hello from a fast clock", ""),
		func(in *seal.SealedInner) { in.TS, in.Exp = fast, fast+messageLifetimeMS })
	injectEnvelope(t, srv, prov.AID(), env)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("not acked (%d left); attack failed", n)
	}
	if counter(prov, seal.ReasonFromFuture) != 1 || countMsgs(t, prov, id) != before {
		t.Fatalf("stats %v", prov.ReceiveStats())
	}
	// Two minutes later the same bytes are fine — but they are gone.
	clk := &sharedClock{now: uint64(time.Now().Add(2 * time.Minute).UnixMilli())}
	prov.setClock(clk.fn())
	if r := receive(t, prov, env); r.class != rxAccepted {
		t.Fatalf("same bytes later: %+v", r)
	}
}

// C33 / X2: every redelivery of an answered delegation makes the provider
// seal and SEND its answer again (redeliveredDelegate → resendResult), with
// no rate limit. The requester — here a stranger calling a public
// capability — replays its own delegation envelope as often as its hub
// budget allows, and the provider spends one send of its own hub budget
// (20/s per sender) per replay. A few callers drive a public agent's send
// budget to zero, so its real results to everyone else get 429 and back
// off; X2 bounds refusal notices to 10% of that budget for this reason,
// but the resend path is unbounded.
func TestRedteamSI10_ReplayedDelegationMakesTheProviderResendWithoutLimit(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setPublic(prov, lampCap)
	if _, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true}); err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	clearMailbox(t, srv, req.AID())
	sendsBefore := relayCountFor(srv.URL)
	const replays = 40
	for i := 0; i < replays; i++ {
		if r := receive(t, prov, env); r.reason != dropDuplicate {
			t.Fatalf("replay %d: %+v", i, r)
		}
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("ran %d times", n)
	}
	if got := relayCountFor(srv.URL) - sendsBefore; got != replays {
		t.Fatalf("provider sent %d answers for %d replays; attack failed", got, replays)
	}
	if n := len(queuedFor(t, srv, req.AID())); n != replays {
		t.Fatalf("%d answers queued for the caller", n)
	}
}

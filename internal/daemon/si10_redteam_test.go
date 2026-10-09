package daemon

// Red-team PoCs for SI-10 (at-least-once delivery, exactly-once
// processing). Each test asserts that the ATTACK SUCCEEDS: a passing test
// means the defect is present. See the finding named in each comment.

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
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
func (m misrouteTransport) Send(ctx context.Context, _ string, env []byte) ([]byte, bool, error) {
	dack, err := m.wrong.Inbound().Receive(ctx, env)
	return dack, true, err
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
	// The sender fell through to the hub: the provider's mailbox holds it,
	// and the row waits only for the word that it arrived.
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("hub holds %d envelopes for the provider, want the delegation", n)
	}
	if rows := outboxRows(t, req, id); len(rows) != 1 || rows[0].HandedAt == 0 || rows[0].Attempting {
		t.Fatalf("requester outbox = %+v, want the delegation handed over", rows)
	}
	// The intended provider gets the task, and the requester hears so from
	// its hub: nothing is left to retry.
	settleDelivery(t, req, prov)
	if _, err := prov.ix.Get(id); err != nil {
		t.Fatalf("provider does not have the task: %v", err)
	}
	if rows := outboxRows(t, req, id); len(rows) != 0 {
		t.Fatalf("requester outbox = %+v after the provider took it", rows)
	}
	// From the hub mailbox the same refusal stays permanent: that mailbox
	// is the node's own.
	env := craft(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "for prov", ""), nil)
	if r := receive(t, wrong, env); r.class != rxDropped || r.reason != seal.ReasonWrongRecipient {
		t.Fatalf("hub path: %+v, want a permanent %s", r, seal.ReasonWrongRecipient)
	}
}

// sharedClock is one wire clock for several daemons, advanced by the test.
// The daemons' background loops (the outbox retry ticker) read it while the
// test advances it, so it is atomic: a plain field was a data race under
// -race whenever a test outlived one retry scan.
type sharedClock struct{ ms atomic.Uint64 }

func newSharedClock(ms uint64) *sharedClock {
	c := &sharedClock{}
	c.ms.Store(ms)
	return c
}

func (c *sharedClock) fn() func() uint64 { return c.ms.Load }

// now is the clock's time in unix ms.
func (c *sharedClock) now() uint64 { return c.ms.Load() }

// set moves the clock to ms.
func (c *sharedClock) set(ms uint64) { c.ms.Store(ms) }

// add advances the clock by ms.
func (c *sharedClock) add(ms uint64) { c.ms.Add(ms) }

// driveOutage runs d's retry loop on its own backoff while the outage the
// test set up lasts, until the clock passes until. It returns the row as
// the outage leaves it.
func driveOutage(t *testing.T, ctx context.Context, d *Daemon, clk *sharedClock, rowID int64, until uint64) *interactions.OutboxRow {
	t.Helper()
	for {
		it, _, err := d.ix.OutboxState(rowID)
		if err != nil {
			t.Fatalf("outbox row: %v", err)
		}
		if uint64(it.NextAt) > until {
			return it
		}
		if uint64(it.NextAt) > clk.now() {
			clk.set(uint64(it.NextAt))
		}
		d.flushOutbox(ctx)
	}
}

// [redteam:F23] regression (was TestRedteamSI10_OutboxReorderLosesCancelAndCanceledCallExecutes).
// SI-10 / §3.6 step 9 / §4.2: after an outage in which the hub could not
// be reached at all the delegation sits in a long backoff, no byte of it
// handed over. A cancel then withdraws it (engine ENG-O4): nothing is sent
// (a cancel sent ahead of it used to be answered TaskNotFound by a
// provider that did not know the task, and the delegation came after and
// ran), the delegation never leaves, and the capability never runs.
func TestRedteamSI10_ACancelBeforeDeliveryWithdrawsTheDelegation(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	clk := newSharedClock(uint64(time.Now().UnixMilli()))
	req.setClock(clk.fn())
	prov.setClock(clk.fn())
	start := clk.now()

	// The hub cannot be reached (nothing connects): the delegation is
	// recorded and queued, and nothing of it leaves this node. (A hub that
	// answers 503 took the connection: it was handed the delegation, which
	// a cancel then cannot withdraw.)
	hubURL := req.config().HubURL
	setHubURL(req, deadHubURL)
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	rows := outboxRows(t, req, id)
	if len(rows) != 1 || rows[0].HandedAt != 0 {
		t.Fatalf("outbox = %+v, want the delegation never handed over", rows)
	}
	dRow := rows[0].ID
	// A 25-minute outage, the retry loop backing off as it does.
	d := driveOutage(t, ctx, req, clk, dRow, start+uint64((25*time.Minute).Milliseconds()))
	if d.HandedAt != 0 || d.Attempting {
		t.Fatalf("setup: the outage handed the delegation over (%+v)", d)
	}

	// The hub is back. The requester cancels the call it no longer wants.
	setHubURL(req, hubURL)
	clk.set(start + uint64((25*time.Minute + 30*time.Second).Milliseconds()))
	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cur, _ := req.ix.Get(id); cur.State != interactions.StateCanceled {
		t.Fatalf("requester state = %s, want canceled", cur.State)
	}
	// Nothing went out, and nothing is left to go.
	if q := queuedFor(t, srv, prov.AID()); len(q) != 0 {
		t.Fatalf("hub holds %d envelopes for the provider; the canceled delegation (or a cancel for it) was sent", len(q))
	}
	if rows := outboxRows(t, req, id); len(rows) != 0 {
		t.Fatalf("outbox still holds %d rows for the canceled task", len(rows))
	}

	// Long after the delegation's backoff would have ended: still nothing.
	clk.set(uint64(d.NextAt) + uint64(time.Hour.Milliseconds()))
	req.flushOutbox(ctx)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(lamp.invoked); n != 0 {
		t.Fatalf("capability ran %d times after the cancel", n)
	}
	if _, err := prov.ix.Get(id); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the provider has the canceled task (%v)", err)
	}
	if cur, _ := req.ix.Get(id); cur.State != interactions.StateCanceled {
		t.Fatalf("requester state = %s", cur.State)
	}
}

// ambiguousP2P is a direct transport that fails in a way that may have
// delivered (the envelope was handed over), while on; it delivers nothing.
type ambiguousP2P struct{ on atomic.Bool }

func (p *ambiguousP2P) Name() string { return "p2p-ambiguous" }
func (p *ambiguousP2P) Reachable(context.Context, string) bool {
	return p.on.Load()
}
func (p *ambiguousP2P) Send(context.Context, string, []byte) ([]byte, bool, error) {
	return nil, true, errors.New("p2p: no answer within the send timeout")
}

// [redteam:F23] SI-10 / §4.2: a delegation that may have reached the
// provider cannot be withdrawn, so the cancel goes to the provider — after
// the delegation was handed over, never before (engine ENG-O1). The
// delegation is frozen: whatever the provider has of it, nothing more of
// it is sent (ENG-O6), only the cancel; the capability never runs.
func TestRedteamSI10_ACancelWaitsBehindADelegationThatMayHaveArrived(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	clk := newSharedClock(uint64(time.Now().UnixMilli()))
	req.setClock(clk.fn())
	prov.setClock(clk.fn())
	p2p := &ambiguousP2P{}
	p2p.on.Store(true)
	req.RegisterTransport(p2p)
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	rows := outboxRows(t, req, id)
	if len(rows) != 1 || rows[0].HandedAt == 0 {
		t.Fatalf("setup: delegation row %+v, want one that may have been delivered", rows)
	}
	d := driveOutage(t, ctx, req, clk, rows[0].ID, clk.now()+uint64((20*time.Minute).Milliseconds()))

	// The hub is back; the direct path is gone. The cancel is queued behind
	// the delegation, which was handed over: it does not wait for it.
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = false })
	p2p.on.Store(false)
	if uint64(d.NextAt) <= clk.now() {
		t.Fatalf("setup: the delegation is due already")
	}
	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	rows = outboxRows(t, req, id)
	if len(rows) != 2 || rows[0].Type != seal.TypeDelegate || !rows[0].Frozen {
		t.Fatalf("requester outbox = %+v, want the delegation frozen and the cancel behind it", rows)
	}
	got := innerTypes(t, srv, srv.URL, prov, queuedFor(t, srv, prov.AID()))
	if len(got) != 1 || got[0] != seal.TypeMessage+":"+delegation.KindCancel {
		t.Fatalf("hub mailbox = %v, want the cancel only", got)
	}
	// Long after the delegation's backoff would have ended, the frozen
	// delegation still goes nowhere.
	clk.set(uint64(d.NextAt) + uint64(time.Hour.Milliseconds()))
	req.flushOutbox(ctx)
	if got := innerTypes(t, srv, srv.URL, prov, queuedFor(t, srv, prov.AID())); len(got) != 1 {
		t.Fatalf("hub mailbox = %v after the backoff; the frozen delegation went again", got)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(lamp.invoked); n != 0 {
		t.Fatalf("capability ran %d times after the cancel", n)
	}
}

// [redteam:F24] regression (was TestRedteamSI10_StrangerPinsTheRelayCursorAndStarvesHeldMail).
// Decision Q1 / SI-10: a peer that sends one message per poll round for an
// interaction the node does not hold (class T: held for the unknown-ix
// window) keeps every page after the cursor non-empty and "held". The
// cursor used to go back to the head only after an empty page, so a
// legitimate envelope held back once (here a single store error) was never
// tried again while the stream went on, and the held messages were never
// read again to turn permanent. Now a round reads from the head at least
// every relayHeadEvery rounds: the follow-up is taken within that, and the
// stream's own messages turn into TaskNotFound once their window ends.
// (The sender is one this node allows: a stranger's messages for unknown
// tasks are not held at all, [redteam:F25].)
func TestRedteamSI10_ASteadyStreamDoesNotPinTheRelayCursor(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "text task", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov) // the follow-up goes once it is confirmed (engine ENG-O2)
	clk := newSharedClock(uint64(time.Now().UnixMilli()))
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

	// A peer sends one message per round, a minute of wire time apart, for
	// interactions this node does not hold.
	mallory := newStranger(t)
	allowPeers(t, prov, mallory.aid)
	const rounds = 2 * (relayHeadEvery + 1)
	processedAt := -1
	for i := 0; i < rounds; i++ {
		clk.add(60_000)
		now := clk.now()
		ix := "ix_mallory_" + time.Duration(i).String()
		injectEnvelope(t, srv, prov.AID(), craft(t, mallory, prov, seal.TypeMessage, ix,
			chatBody(t, "x", ""), func(in *seal.SealedInner) { in.TS, in.Exp = now, now+messageLifetimeMS }))
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if processedAt < 0 && countMsgs(t, prov, id) == before+1 {
			processedAt = i
		}
	}
	if processedAt < 0 || processedAt > relayHeadEvery {
		t.Fatalf("the held follow-up was taken at round %d of the stream, want within %d rounds", processedAt, relayHeadEvery)
	}
	// The stream's messages were read again once their window ended, and
	// answered TaskNotFound and acknowledged.
	if counter(prov, dropUnknownIX) == 0 {
		t.Fatalf("no held message turned permanent during a %d-minute stream: %v", rounds, prov.ReceiveStats())
	}
	if n := len(queuedFor(t, srv, prov.AID())); n >= rounds {
		t.Fatalf("%d envelopes still queued after %d rounds; the expired ones were not acknowledged", n, rounds)
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

// [redteam:F30] regression (was
// TestRedteamSI10_StartupRecoveryReportsAP2PCallRunningNowAsInterrupted).
// C1 / SI-10 / SI-6: startup recovery classifies what an EARLIER process
// left. A long call this process is running is not a leftover: run over
// recoverInterrupted (as New would have, had the delivery come before it —
// awaitReady now holds such a delivery, TestADeliveryDuringStartupWaitsForRecovery),
// it is left alone, and its real result and receipt are what the requester
// gets.
func TestRedteamSI10_StartupRecoveryLeavesACallRunningNowAlone(t *testing.T) {
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
	// Delivered over p2p: accepted, marked working, running.
	if err := recvErr(prov.Inbound(), ctx, env); err != nil {
		t.Fatal(err)
	}
	<-slow.started
	prov.recoverInterrupted()
	pix, err := prov.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if pix.State != interactions.StateWorking {
		t.Fatalf("provider: %s %s; the running call was taken for a leftover", pix.State, pix.ResultMeta)
	}
	// The firmware flash completes; its result is what is kept and sent.
	close(slow.gate)
	waitUntil(t, "the call to end", func() bool { _, running := prov.running.Load(id); return !running })
	pix, _ = prov.ix.Get(id)
	var res capabilityResult
	_ = json.Unmarshal(pix.Result, &res)
	if pix.State != interactions.StateCompleted || res.Status != string(effect.OK) || len(pix.Receipt) == 0 {
		t.Fatalf("provider: %s, stored result %+v, receipt %d bytes; want the real result", pix.State, res, len(pix.Receipt))
	}
	waitUntil(t, "the result at the hub", func() bool { return len(queuedFor(t, srv, req.AID())) > 0 })
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, _ := req.ix.Get(id); cur.State != interactions.StateCompleted {
		t.Fatalf("requester: %s", cur.State)
	}
	if n := slow.runs.Load(); n != 1 {
		t.Fatalf("runs = %d", n)
	}
}

// [redteam:F25] regression (was TestRedteamSI10_StrangerKeepsAnOnlineNodesMailboxFull).
// Q1 / §3.6 step 9 / §3.7 quota: a registered AID's messages for
// interactions the node does not hold used to be class T for ten minutes,
// so the ONLINE recipient's own daemon kept them in its hub mailbox, and one
// stranger could keep the mailbox at its quota (507 for every legitimate
// sender) while sending below its own rate limit. A stranger's messages are
// now answered TaskNotFound and acknowledged at once: the mailbox empties
// on the next poll and the legitimate delegation gets in. Scaled here to a
// 50-envelope quota.
func TestRedteamSI10_AStrangerCannotKeepAnOnlineNodesMailboxFull(t *testing.T) {
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
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("the provider left %d stranger messages in its mailbox", n)
	}
	if n := counter(prov, transientUnknownIX); n != 0 {
		t.Fatalf("%d stranger messages were held", n)
	}
	// A legitimate delegation gets in and is taken.
	id, err := req.Delegate(ctx, prov.AID(), "real work", nil)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if rows := outboxRows(t, req, id); len(rows) != 1 || rows[0].HandedAt == 0 || len(queuedFor(t, srv, prov.AID())) != 1 {
		t.Fatalf("requester outbox = %+v; the delegation did not get in", rows)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.Get(id); err != nil {
		t.Fatalf("the provider did not get the delegation: %v", err)
	}
}

// [redteam:F25] A peer this node deals with keeps the wait window for its
// messages that overtake their delegation, but only unknownIXHeldPerSender
// at a time: past that they are answered TaskNotFound and acknowledged, so
// even an allowed peer cannot fill the mailbox with held messages. One that
// waited and whose delegation then arrives is processed as before.
func TestRedteamSI10_AKnownPeerHoldsOnlySoManyEarlyMessages(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	peer := newStranger(t)
	allowPeers(t, prov, peer.aid)
	const extra = 8
	for i := 0; i < unknownIXHeldPerSender+extra; i++ {
		injectEnvelope(t, srv, prov.AID(), craft(t, peer, prov, seal.TypeMessage,
			"ix_early_"+time.Duration(i).String(), chatBody(t, "x", ""), nil))
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := counter(prov, transientUnknownIX); n != unknownIXHeldPerSender {
		t.Fatalf("%d held, want %d", n, unknownIXHeldPerSender)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != unknownIXHeldPerSender {
		t.Fatalf("%d left in the mailbox, want the %d held", n, unknownIXHeldPerSender)
	}
	if n := counter(prov, unknownIXOverCap); n != extra {
		t.Fatalf("%d answered over the cap, want %d", n, extra)
	}

	// The requester (allowed too) sends a follow-up before its delegation:
	// it waits, then is taken once the delegation arrives.
	clearMailbox(t, srv, prov.AID())
	const ix = "ix_follow_first"
	follow := craft(t, senderOf(req), prov, seal.TypeMessage, ix, chatBody(t, "follow-up", ""), nil)
	if r := receive(t, prov, follow); r.class != rxTransient || r.reason != transientUnknownIX {
		t.Fatalf("the early follow-up: %+v, want held", r)
	}
	if r := receive(t, prov, craft(t, senderOf(req), prov, seal.TypeDelegate, ix,
		delegateBody(t, req.self, ix, "the task", ""), nil)); r.class != rxAccepted {
		t.Fatalf("the delegation: %+v", r)
	}
	if r := receive(t, prov, follow); r.class != rxAccepted {
		t.Fatalf("the follow-up after its delegation: %+v", r)
	}

	// A peer this node has asked for something, and nothing more, deals
	// with it too; a stranger does not.
	asked, other := newStranger(t), newStranger(t)
	if err := prov.ix.Put("ix_asked", interactions.RoleOutbound, asked.aid, "a question", "", nil); err != nil {
		t.Fatal(err)
	}
	if r := receive(t, prov, craft(t, asked, prov, seal.TypeMessage, "ix_unknown_a", chatBody(t, "x", ""), nil)); r.reason != transientUnknownIX {
		t.Fatalf("a peer this node asked: %+v, want held", r)
	}
	if r := receive(t, prov, craft(t, other, prov, seal.TypeMessage, "ix_unknown_b", chatBody(t, "x", ""), nil)); r.reason != dropUnknownIX {
		t.Fatalf("a stranger: %+v, want TaskNotFound at once", r)
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
	clk := newSharedClock(uint64(time.Now().Add(2 * time.Minute).UnixMilli()))
	prov.setClock(clk.fn())
	if r := receive(t, prov, env); r.class != rxAccepted {
		t.Fatalf("same bytes later: %+v", r)
	}
}

package daemon

// Spot checks of the red-team fix wave after the round-5b merge
// (an internal note). Each test is a variant of an attack behind a critical
// or high finding that the finding's own regression tests do not run:
// another entry point, another message type, copies racing each other, a
// restart in between. Each asserts that the attack fails.
//
//   - F6/F7: a swapped TaskDoc under the accepted message id (the replay
//     table's branch), after a restart, and racing the accepted request.
//   - F5: a quota refusal replayed once the quota has recovered, an
//     operator's rejection of a held delegation, and copies of one
//     delegation racing an allow.
//   - F22: a provider's result misrouted over a direct transport.
//   - F23: a follow-up queued behind a delegation in backoff, a crash in
//     the middle of the delegation's attempt, and a cancel racing an
//     attempt that is under way.
//   - F11: a provider's conversation message (not a status) claiming the
//     payment completed.
//   - fx-e KEL panic: a sender whose KEL rotates to a 31-byte key.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/provider"
)

// countingLamp is lampProvider safe to invoke from several goroutines: it
// counts the calls per caller.
type countingLamp struct {
	mu    sync.Mutex
	calls map[string]int
}

func (l *countingLamp) ID() string { return "lamp" }
func (l *countingLamp) Capabilities(context.Context) ([]string, error) {
	return []string{lampCap}, nil
}
func (l *countingLamp) Describe(context.Context) (string, error) { return "", nil }
func (l *countingLamp) Health(context.Context) error             { return nil }
func (l *countingLamp) Invoke(_ context.Context, c provider.Call) (effect.Effect, error) {
	l.mu.Lock()
	if l.calls == nil {
		l.calls = map[string]int{}
	}
	l.calls[c.CallerAID]++
	l.mu.Unlock()
	return effect.Effect{Status: effect.OK, Record: &tsir.EffectRecord{Metrics: map[string]float64{"power_state": 1}}}, nil
}

func (l *countingLamp) by(aid string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls[aid]
}

func (l *countingLamp) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		n += c
	}
	return n
}

// ---- F6/F7 ----

// The swap sealed under the message id of the accepted delegation. The
// replay table holds (from, mid), so the copy is not judged in step 9's
// existing-ix branch but goes to the duplicate branch: answered tok(1)
// (engine ENG-V13), and since it is not the request the interaction was
// accepted for, nothing is done for it (ENG-V18) — and a redelivery would
// run only the recorded call anyway.
func TestSpotASwapUnderTheAcceptedMessageIDDoesNotRun(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, prov, PolicyOpen)
	s := newStranger(t)
	ix, _ := newInteractionID()
	var mid []byte
	chat := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "hello there", ""),
		func(in *seal.SealedInner) { mid = append([]byte(nil), in.MID...) })
	if r := receive(t, prov, chat); r.class != rxAccepted {
		t.Fatalf("chat task: %+v", r)
	}
	swap := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "turn it on", lampCap),
		func(in *seal.SealedInner) { in.MID = mid })
	before := counter(prov, redeliveryNotAuthorized)
	assertSwapNotRun(t, prov, receive(t, prov, swap), before, lamp, ix)
	if seen, err := prov.ix.ReplaySeen(s.aid, mid); err != nil || !seen {
		t.Fatalf("precondition: the accepted message id is in the replay table (%v %v)", seen, err)
	}
}

// Across a restart: what binds a redelivery to its request is the stored
// row, not anything the process held.
func TestSpotASwapAfterARestartDoesNotRun(t *testing.T) {
	srv := newFakeHub(t)
	prov := registered(t, srv.URL, "prov")
	setPolicy(t, prov, PolicyOpen)
	s := newStranger(t)
	ix, _ := newInteractionID()
	var mid []byte
	if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "hello", ""),
		func(in *seal.SealedInner) { mid = append([]byte(nil), in.MID...) })); r.class != rxAccepted {
		t.Fatalf("chat task: %+v", r)
	}
	prov = restartDaemon(t, prov)
	lamp := &lampProvider{}
	if err := prov.Providers().Register(context.Background(), lamp); err != nil {
		t.Fatal(err)
	}
	// A new message id, and the accepted one.
	assertSwapRefused(t, prov, swapDelegateFrom(t, s, prov, ix, lampCap), lamp, ix)
	before := counter(prov, redeliveryNotAuthorized)
	assertSwapNotRun(t, prov, receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix,
		delegateBody(t, s.ctrl, ix, "turn it on", lampCap), func(in *seal.SealedInner) { in.MID = mid })), before, lamp, ix)
}

// swapDelegateFrom is swapDelegate for a bare identity.
func swapDelegateFrom(t *testing.T, s sender, prov *Daemon, ix, capID string) rxResult {
	t.Helper()
	return receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "anything", capID), nil))
}

// The accepted request and several swaps of it racing each other, over the
// hub path and a direct transport at once. Whichever copy comes first, the
// swapped-in capability never runs: first, it is a new capability call
// that open refuses (§5.2 row 5); after the chat, an ix collision.
func TestSpotSwapsRacingTheAcceptedRequestNeverRun(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	lamp := &countingLamp{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, prov, PolicyOpen)
	for round := 0; round < 8; round++ {
		s := newStranger(t)
		ix, _ := newInteractionID()
		envs := [][]byte{craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "hello", ""), nil)}
		for i := 0; i < 3; i++ {
			envs = append(envs, craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "on", lampCap), nil))
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, env := range envs {
			wg.Add(1)
			go func(i int, env []byte) {
				defer wg.Done()
				<-start
				if i%2 == 1 {
					_ = recvErr(prov.Inbound(), ctx, env)
					return
				}
				prov.receiveEnvelope(ctx, env)
			}(i, env)
		}
		close(start)
		wg.Wait()
		waitUntil(t, "no call running", func() bool {
			_, running := prov.running.Load(ix)
			return !running
		})
		if n := lamp.by(s.aid); n != 0 {
			t.Fatalf("round %d: the swapped-in capability ran %d times", round, n)
		}
		if pix, err := prov.ix.Get(ix); err == nil && pix.IsCapability {
			t.Fatalf("round %d: the interaction was recorded as a capability call", round)
		}
	}
}

// ---- F5 ----

// A public capability call refused by its per-caller quota. The quota
// window passes and the in-memory list forgets the refusal (a flood of other
// refusals evicts it; no restart, no policy change): the same envelope
// handed in again stays refused, although a fresh call is admitted now.
// This is the red team's "stronger variant" of F5.
func TestSpotAQuotaRefusalStaysRefusedOnceTheQuotaRecovers(t *testing.T) {
	prov := newTestDaemon(t, "", false) // closed
	lamp := &countingLamp{}
	if err := prov.Providers().Register(context.Background(), lamp); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: lampCap, PerCallerPerMin: 1}}); err != nil {
		t.Fatal(err)
	}
	var offset atomic.Int64
	base := uint64(time.Now().UnixMilli())
	prov.setClock(func() uint64 { return base + uint64(offset.Load()) })
	s := newStranger(t)
	call := func(ix string) []byte {
		return craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "on", lampCap), nil)
	}
	if r := receive(t, prov, call("ix_spot_quota_1")); r.class != rxAccepted {
		t.Fatalf("first call: %+v", r)
	}
	second := call("ix_spot_quota_2")
	if r := receive(t, prov, second); r.class != rxDropped || !strings.HasPrefix(r.reason, dropRefusedPrefix) {
		t.Fatalf("second call inside the minute: %+v, want refused by the quota", r)
	}
	offset.Store(int64(2 * time.Minute / time.Millisecond))
	prov.refused = refusedLRU{}
	if r := receive(t, prov, second); r.class != rxDropped || r.reason != dropRefusedReplay {
		t.Fatalf("the refused call handed in again after the quota recovered: %+v, want %s", r, dropRefusedReplay)
	}
	if n := lamp.by(s.aid); n != 1 {
		t.Fatalf("lamp ran %d times, want only the first call", n)
	}
	if _, err := prov.ix.Get("ix_spot_quota_2"); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the refused call was recorded: %v", err)
	}
	// Control: the quota really did recover.
	if r := receive(t, prov, call("ix_spot_quota_3")); r.class != rxAccepted {
		t.Fatalf("a fresh call after the window: %+v", r)
	}
}

// Refused another way: the approve policy held the delegation and the
// operator rejected it (the requester is told rejected). The operator then
// allows the requester and the daemon restarts; a hub replaying the held
// envelope does not get it run. The hold wrote the replay row, which
// outlives the pending row.
func TestSpotAnOperatorRejectionStaysFinalAfterAllowAndRestart(t *testing.T) {
	prov := newTestDaemon(t, "", false)
	setPolicy(t, prov, PolicyApprove)
	s := newStranger(t)
	ix, _ := newInteractionID()
	env := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "on", lampCap), nil)
	if r := receive(t, prov, env); r.class != rxAccepted {
		t.Fatalf("held: %+v", r)
	}
	if err := prov.RejectPending(ix); err != nil {
		t.Fatal(err)
	}
	allowPeers(t, prov, s.aid)
	prov = restartDaemon(t, prov)
	lamp := &countingLamp{}
	if err := prov.Providers().Register(context.Background(), lamp); err != nil {
		t.Fatal(err)
	}
	if r := receive(t, prov, env); r.class != rxDropped {
		t.Fatalf("replay of the rejected delegation: %+v, want dropped", r)
	}
	if lamp.total() != 0 {
		t.Fatalf("the rejected delegation ran")
	}
	if _, err := prov.ix.Get(ix); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the rejected delegation was recorded: %v", err)
	}
	if _, err := prov.ix.GetPending(ix); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the rejected delegation is held again: %v", err)
	}
}

// Copies of one capability call (the hub's and a direct one, several of
// each) race the operator allowing the caller. Each round the delegation is
// decided once: it runs at most once, and never when it was refused — a
// requester told `rejected` must not see the call run. A replay after the
// race changes neither.
func TestSpotCopiesRacingAnAllowAreDecidedOnce(t *testing.T) {
	prov := newTestDaemon(t, "", false) // closed
	ctx := context.Background()
	lamp := &countingLamp{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	refusedRounds, ranRounds := 0, 0
	for round := 0; round < 12; round++ {
		s := newStranger(t)
		ix := fmt.Sprintf("ix_spot_race_%02d", round)
		var mid []byte
		env := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "on", lampCap),
			func(in *seal.SealedInner) { mid = append([]byte(nil), in.MID...) })
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				if i%2 == 1 {
					_ = recvErr(prov.Inbound(), ctx, env)
					return
				}
				prov.receiveEnvelope(ctx, env)
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if round%3 != 0 {
				time.Sleep(time.Duration(round%4) * time.Millisecond)
			}
			_, _ = prov.AllowPeer(ctx, s.aid, ListAllow)
		}()
		close(start)
		wg.Wait()
		waitUntil(t, "no call running", func() bool {
			_, running := prov.running.Load(ix)
			return !running
		})
		// Handed in again after the list forgot it.
		prov.refused = refusedLRU{}
		prov.receiveEnvelope(ctx, env)
		waitUntil(t, "no call running", func() bool {
			_, running := prov.running.Load(ix)
			return !running
		})
		exact, _, _, err := prov.ix.Refused(s.aid, mid)
		if err != nil {
			t.Fatal(err)
		}
		ran := lamp.by(s.aid)
		switch {
		case ran > 1:
			t.Fatalf("round %d: one delegation ran %d times", round, ran)
		case exact && ran > 0:
			t.Fatalf("round %d: the delegation was refused and ran", round)
		case !exact && ran == 0:
			if _, gerr := prov.ix.Get(ix); gerr == nil {
				t.Fatalf("round %d: accepted and never run", round)
			}
		}
		if exact {
			refusedRounds++
		}
		if ran == 1 {
			ranRounds++
		}
	}
	t.Logf("refused first in %d rounds, ran in %d", refusedRounds, ranRounds)
}

// ---- F22 ----

// A provider's result, not a delegation, misrouted over a direct transport
// (the requester's address in the rendezvous now leads to another node).
// The other node cannot open it and does not acknowledge it, the provider
// falls through to the hub, and the requester gets its answer.
func TestSpotAMisroutedResultReachesTheRequesterThroughTheHub(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	wrong := newTestDaemon(t, srv.URL, false)
	prov.RegisterTransport(misrouteTransport{wrong: wrong})
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("lamp ran %d times", n)
	}
	waitUntil(t, "the provider's answer to reach the hub", func() bool {
		rows, err := prov.ix.OutboxRows(id)
		if err != nil || len(rows) == 0 {
			return false
		}
		for _, r := range rows {
			if r.HandedAt == 0 || r.Attempting {
				return false
			}
		}
		return len(queuedFor(t, srv, req.AID())) > 0
	})
	if n := counter(wrong, transientDirectUnopened); n == 0 {
		t.Fatalf("the wrong node never refused the misrouted result (%v)", wrong.ReceiveStats())
	}
	if n := counter(wrong, seal.ReasonWrongRecipient); n != 0 {
		t.Fatalf("the wrong node acknowledged the misrouted result as wrong-recipient")
	}
	waitUntil(t, "the requester to complete", func() bool {
		_ = req.pollOnce(ctx)
		ix, err := req.ix.Get(id)
		return err == nil && ix.State == interactions.StateCompleted && len(ix.Receipt) > 0
	})
	// The requester's tokens reach the provider through its hub: nothing
	// is left to send.
	askHubNow(t, prov)
	if rows := outboxRows(t, prov, id); len(rows) != 0 {
		t.Fatalf("the provider's outbox = %+v after the requester took the answer", rows)
	}
}

// ---- F23 ----

// innerTypes opens every envelope the hub holds for to and returns their
// inner types, in mailbox order.
func innerTypes(t *testing.T, srv interface{ Close() }, url string, to *Daemon, envs [][]byte) []string {
	t.Helper()
	var out []string
	for _, env := range envs {
		op, err := seal.Open(env, to.AID(), to.enc, nil)
		if err != nil {
			t.Fatal(err)
		}
		typ := op.Inner.Type
		if typ == seal.TypeMessage {
			if cm, err := delegation.UnmarshalChatMsg(op.Inner.Body); err == nil {
				typ += ":" + cm.Kind
			}
		}
		out = append(out, typ)
	}
	return out
}

// A follow-up (not a cancel) queued while the hub is still down, behind a
// text delegation in backoff. Once the hub is back the follow-up does not
// go out ahead of the delegation, nor with it: a follow-up goes only once
// the provider confirmed the delegation (engine ENG-O2, rule 2). When the
// delegation's backoff ends it goes; the provider takes it, its token
// confirms it, and the follow-up goes after.
func TestSpotAFollowUpNeverOvertakesItsDelegation(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	clk := newSharedClock(uint64(time.Now().UnixMilli()))
	req.setClock(clk.fn())
	prov.setClock(clk.fn())
	start := clk.now()
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
	id, err := req.Delegate(ctx, prov.AID(), "summarise the attached notes", nil)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	rows := outboxRows(t, req, id)
	if len(rows) != 1 {
		t.Fatalf("outbox = %+v", rows)
	}
	d := driveOutage(t, ctx, req, clk, rows[0].ID, start+uint64((25*time.Minute).Milliseconds()))
	_ = req.SendMessage(ctx, id, "and keep it short", nil) // queued; the hub is still down
	rows = outboxRows(t, req, id)
	if len(rows) != 2 || rows[0].Type != seal.TypeDelegate || rows[0].NextAt != d.NextAt {
		t.Fatalf("outbox = %+v, want the delegation, its backoff as it was, and the follow-up", rows)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = false })
	clk.set(start + uint64((25*time.Minute + 30*time.Second).Milliseconds()))
	if uint64(d.NextAt) <= clk.now() {
		t.Fatalf("setup: the delegation is due already")
	}
	req.flushOutbox(ctx)
	if q := queuedFor(t, srv, prov.AID()); len(q) != 0 {
		t.Fatalf("the hub holds %v before the delegation's backoff ended; the follow-up went first",
			innerTypes(t, srv, srv.URL, prov, q))
	}
	clk.set(uint64(d.NextAt))
	req.flushOutbox(ctx)
	req.flushOutbox(ctx)
	got := innerTypes(t, srv, srv.URL, prov, queuedFor(t, srv, prov.AID()))
	if len(got) != 1 || got[0] != seal.TypeDelegate {
		t.Fatalf("hub mailbox = %v, want the delegation alone until it is confirmed", got)
	}
	// The provider takes it, and its token reaches the requester through
	// the hub: the follow-up goes now.
	settleDelivery(t, req, prov)
	req.flushOutbox(ctx)
	got = innerTypes(t, srv, srv.URL, prov, queuedFor(t, srv, prov.AID()))
	if len(got) != 1 || got[0] != seal.TypeMessage+":"+delegation.ChatText {
		t.Fatalf("hub mailbox = %v after the delegation was confirmed, want the follow-up", got)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := counter(prov, dropUnknownIX); n != 0 {
		t.Fatalf("the follow-up reached the provider before its delegation: %v", prov.ReceiveStats())
	}
	if n := countMsgs(t, prov, id); n != 2 {
		t.Fatalf("provider holds %d messages for the task, want the goal and the follow-up", n)
	}
}

// The requester crashed in the middle of the delegation's attempt: the
// version was written and the row marked attempting (A8), and the outcome
// never recorded. After the restart the version counts as handed over
// (engine CR2): a cancel does not withdraw it — the provider may have it —
// but freezes it (CR9, ENG-O4) and goes out after it; the frozen
// delegation never goes again (ENG-O6).
func TestSpotACrashMidAttemptLeavesTheCancelBehindTheDelegation(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	// Sealed, but nothing connects: written, never handed over.
	knowPeer(t, req, prov.AID())
	hubURL := req.config().HubURL
	setHubURL(req, deadHubURL)
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	setHubURL(req, hubURL)
	rows := outboxRows(t, req, id)
	if len(rows) != 1 || rows[0].CurVer == 0 || rows[0].HandedAt != 0 || rows[0].Attempting {
		t.Fatalf("setup: %+v", rows)
	}
	// The process stops inside an attempt: begun, outcome never recorded.
	if err := req.ix.Update(func(tx *interactions.Tx) error { return tx.BeginOutboxAttempt(rows[0].ID) }); err != nil {
		t.Fatal(err)
	}
	// The hub's relay refuses sends while the node starts again, so what
	// the first pass sends is answered 503 and nothing reaches a mailbox.
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
	req = restartDaemon(t, req)
	// The start's first pass may be attempting it again (and is answered
	// 503); once it is done, the row is handed over.
	waitUntil(t, "the start's first pass", func() bool {
		rows := outboxRows(t, req, id)
		return len(rows) == 1 && !rows[0].Attempting
	})
	if rows = outboxRows(t, req, id); len(rows) != 1 || rows[0].HandedAt == 0 {
		t.Fatalf("after the restart: %+v, want the delegation handed over", rows)
	}
	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	rows = outboxRows(t, req, id)
	if len(rows) != 2 || rows[0].Type != seal.TypeDelegate || !rows[0].Frozen {
		t.Fatalf("outbox = %+v, want the delegation frozen and the cancel behind it", rows)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = false })
	if err := retryNow(t, req, rows[1].ID); err != nil {
		t.Fatalf("the cancel: %v", err)
	}
	_ = retryNow(t, req, rows[0].ID)
	got := innerTypes(t, srv, srv.URL, prov, queuedFor(t, srv, prov.AID()))
	if len(got) != 1 || got[0] != seal.TypeMessage+":"+delegation.KindCancel {
		t.Fatalf("hub mailbox = %v, want the cancel only", got)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(lamp.invoked); n != 0 {
		t.Fatalf("the canceled call ran %d times", n)
	}
}

// gateP2P is a direct transport whose sends wait for the test: the first
// entry is reported on entered. Once release is closed a send fails; it
// handed nothing over, or — with to set — it handed the envelope to to's
// Inbound and lost the answer (no token came back).
type gateP2P struct {
	armed   atomic.Bool
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	to      *Daemon
}

func (g *gateP2P) Name() string                           { return "p2p-gate" }
func (g *gateP2P) Reachable(context.Context, string) bool { return g.armed.Load() }
func (g *gateP2P) Send(ctx context.Context, _ string, env []byte) ([]byte, bool, error) {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	if g.to != nil {
		_, _ = g.to.Inbound().Receive(ctx, env)
		return nil, true, errors.New("p2p gate: the answer was lost")
	}
	return nil, false, errors.New("p2p gate: nothing sent")
}

// A cancel while the delegation's first attempt is under way. The cancel
// cannot withdraw a delegation an attempt is sending: it freezes it, and
// waits for that attempt (engine ENG-O1, O4). Nothing more of the frozen
// delegation goes, on any path (ENG-O6). An attempt that handed nothing
// over ends in the withdrawal of the whole queue, the cancel with it, and
// neither path got a byte (ENG-O5, E-T13); one that handed the delegation
// over is followed by the cancel, so the provider sees the delegation,
// then the cancel.
func TestSpotACancelRacingAnAttemptGoesOutBehindIt(t *testing.T) {
	t.Run("the attempt hands nothing over", func(t *testing.T) { cancelRacingAnAttempt(t, false) })
	t.Run("the attempt hands the delegation over", func(t *testing.T) { cancelRacingAnAttempt(t, true) })
}

func cancelRacingAnAttempt(t *testing.T, hands bool) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	g := &gateP2P{entered: make(chan struct{}), release: make(chan struct{})}
	if hands {
		g.to = prov
	}
	g.armed.Store(true)
	req.RegisterTransport(g)
	delegated := make(chan error, 1)
	go func() {
		_, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
		delegated <- err
	}()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the delegation's attempt never started")
	}
	list, err := req.ix.ListAll(interactions.ListFilter{Role: interactions.RoleOutbound})
	if err != nil || len(list) != 1 {
		t.Fatalf("outbound tasks: %d (%v)", len(list), err)
	}
	id := list[0].ID
	// The task's interaction key (FS-X1): a delegation that never left
	// takes it at once (engine ENG-X4).
	k, err := seal.GenerateIXKey(seal.SuiteX25519, id, prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	if err := req.ix.Update(func(tx *interactions.Tx) error {
		return tx.PutIXKey(interactions.IXKey{IX: id, PeerAID: prov.AID(), Suite: k.Suite, Pub: k.Pub, Priv: k.Private})
	}); err != nil {
		t.Fatal(err)
	}
	// From here the direct path is gone: a cancel that did not wait for the
	// attempt under way would reach the hub at once, ahead of it.
	g.armed.Store(false)
	canceled := make(chan error, 1)
	go func() {
		_, err := req.CancelTask(ctx, id)
		canceled <- err
	}()
	time.Sleep(150 * time.Millisecond) // the cancel is queued and waits on the attempt
	close(g.release)
	for _, ch := range []chan error{delegated, canceled} {
		select {
		case err := <-ch:
			if err != nil && !errors.Is(err, ErrNotCancelable) {
				t.Logf("call returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a call never returned")
		}
	}
	if cur, err := req.ix.Get(id); err != nil || cur.State != interactions.StateCanceled {
		t.Fatalf("requester: %+v (%v), want canceled", cur, err)
	}
	if !hands {
		if rows := outboxRows(t, req, id); len(rows) != 0 {
			t.Fatalf("requester outbox = %+v, want the queue withdrawn", rows)
		}
		if q := queuedFor(t, srv, prov.AID()); len(q) != 0 {
			t.Fatalf("the hub holds %v; nothing of a withdrawn queue may leave", innerTypes(t, srv, srv.URL, prov, q))
		}
		if _, err := prov.ix.Get(id); !errors.Is(err, interactions.ErrNotFound) {
			t.Fatalf("the provider has the withdrawn task (%v)", err)
		}
		if rk, err := req.ix.OwnRK(id); err != nil || rk != nil {
			t.Fatalf("the withdrawn task's interaction key is kept (%v)", err)
		}
		return
	}
	// The cancel follows the delegation the attempt handed over.
	waitUntil(t, "the cancel at the hub", func() bool { return len(queuedFor(t, srv, prov.AID())) == 1 })
	rows := outboxRows(t, req, id)
	if len(rows) == 0 || rows[0].Type != seal.TypeDelegate || !rows[0].Frozen || rows[0].HandedAt == 0 {
		t.Fatalf("requester outbox = %+v, want the delegation handed over and frozen", rows)
	}
	got := innerTypes(t, srv, srv.URL, prov, queuedFor(t, srv, prov.AID()))
	if len(got) != 1 || got[0] != seal.TypeMessage+":"+delegation.KindCancel {
		t.Fatalf("hub mailbox = %v, want the cancel only: the frozen delegation goes nowhere", got)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := counter(prov, dropUnknownIX); n != 0 {
		t.Fatalf("the cancel reached the provider before its delegation: %v", prov.ReceiveStats())
	}
	msgs, err := prov.ix.Messages(id)
	if err != nil || len(msgs) == 0 || msgs[len(msgs)-1].Kind != interactions.MsgCancel {
		t.Fatalf("provider messages %+v (%v), want the cancel last", msgs, err)
	}
}

// ---- F11 ----

// A provider's conversation message (anet.message), not a status, on a
// text task this node never paid for, claiming the payment completed with
// receipts of its own. It is the provider's newest word while the task
// waits for input, so it is shown as status.message: with no settlement
// this node verified, the payment claim is not stated there, and the task's
// metadata states no settlement at all.
func TestSpotAProviderChatMessageDoesNotStateAPayment(t *testing.T) {
	srv, req, prov := quietPair(t)
	_ = srv
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "draft a reply for me", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := mustMarshal(t, &delegation.StatusMsg{State: delegation.StateInputRequired, Text: "which tone?",
		At: uint64(time.Now().UnixMilli())})
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeStatus, id, st)); r.class != rxAccepted {
		t.Fatalf("status: %+v", r)
	}
	meta, _ := json.Marshal(map[string]any{
		"x402.payment.status": "payment-completed",
		"x402.payment.receipts": []any{map[string]any{"success": true, "transaction": "tx-in-a-chat-message",
			"network": "hub:x", "amount": "1000", "payer": req.AID()}},
	})
	cm := mustMarshal(t, &delegation.ChatMsg{Kind: delegation.ChatText, Body: "thanks for paying; which tone?",
		MsgID: "msg_spot_chat", Metadata: meta})
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeMessage, id, cm)); r.class != rxAccepted {
		t.Fatalf("chat message: %+v", r)
	}
	if ix, _ := req.ix.Get(id); ix.State != interactions.StateInputRequired || ix.PayState != interactions.PayNone {
		t.Fatalf("setup: state %s pay %q", ix.State, ix.PayState)
	}
	v := pvView(t, req, id)
	text, _ := json.Marshal(pvPath(v, "status", "message", "parts"))
	if !strings.Contains(string(text), "which tone?") {
		t.Fatalf("setup: status.message is not the provider's chat message: %s", text)
	}
	pvNoCompletedPayment(t, v, "tx-in-a-chat-message")
	md, _ := pvPath(v, "metadata").(map[string]any)
	for _, k := range []string{a2ashape.KeyX402Status, a2ashape.KeyX402Receipts} {
		if _, ok := md[k]; ok {
			t.Errorf("task metadata states %s on a task outside the payment flow: %v", k, md[k])
		}
	}
	// And the stream event a watcher gets is the same projection.
	ix, _ := req.ix.Get(id)
	tv, err := req.taskView(ix, viewOpts{historyLen: new(int)})
	if err != nil {
		t.Fatal(err)
	}
	su := a2ashape.StatusUpdate(tv)
	b, _ := json.Marshal(su)
	if strings.Contains(string(b), "payment-completed") || strings.Contains(string(b), "tx-in-a-chat-message") {
		t.Errorf("the status-update event states the provider's payment claim: %s", b)
	}
}

// ---- fx-e: KEL with a key of the wrong length ----

// A sender whose KEL is a well-formed icp that pre-commits to a 31-byte next
// key and the rot that reveals it (signed by the icp key: nothing else can sign).
// Before the fx-e fix, replaying it panicked in ed25519.Verify, on the
// receive path's goroutine. It is dropped as a bad KEL.
func TestSpotASenderKELRotatingToAShortKeyIsDroppedNotAPanic(t *testing.T) {
	prov := newTestDaemon(t, "", false)
	pub, k0, _ := ed25519.GenerateKey(nil)
	short := make([]byte, ed25519.PublicKeySize-1)
	h := sha256.Sum256(short) // the pre-rotation digest (identity.nextDigest)
	next := h[:]
	icp := spotSignedEvent(t, identity.KeyEvent{KELType: identity.KELType, Type: identity.Inception, Keys: [][]byte{pub},
		NextDigest: next, Threshold: 1, Timestamp: 1, Toad: identity.ToadField(identity.ToadHub)}, k0)
	aid := icp.EventID
	rot := spotSignedEvent(t, identity.KeyEvent{KELType: identity.KELType, AID: aid, Seq: 1, Prev: aid, Type: identity.Rotation,
		Keys: [][]byte{short}, NextDigest: next, Threshold: 1, Timestamp: 2, Toad: identity.ToadField(identity.ToadHub)}, k0)
	sign := func(p []byte) ([]byte, uint64) { return ed25519.Sign(k0, p), 0 }
	now := uint64(time.Now().UnixMilli())
	kp, err := seal.GenerateKeyPair(seal.SuiteX25519, now, now+seal.MaxKeyValidityMS)
	if err != nil {
		t.Fatal(err)
	}
	signedSet, err := seal.SignEncKeySet(&seal.EncKeySet{Type: seal.EncKeySetType, AID: aid, Seq: now,
		Keys: []seal.EncKey{kp.Public}, IssuedAt: now, Mbox: testMbox(t)}, sign)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := signedSet.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Its notice is never read: step 6 refuses the KEL before step 6½.
	s := sender{aid: aid, kel: []identity.SignedEvent{icp, rot}, keys: keys, ksn: 0, sign: sign, notice: []byte{0xa0}}
	ix, _ := newInteractionID()
	body := []byte(`{}`)
	var r rxResult
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("the receive path panicked on a KEL with a 31-byte key: %v", p)
			}
		}()
		r = receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix, body, nil))
	}()
	if r.class != rxDropped || r.reason != seal.ReasonBadKEL {
		t.Fatalf("envelope from a KEL with a 31-byte key: %+v, want dropped as %s", r, seal.ReasonBadKEL)
	}
}

// spotSignedEvent signs e with k and fills its event id.
func spotSignedEvent(t *testing.T, e identity.KeyEvent, k ed25519.PrivateKey) identity.SignedEvent {
	t.Helper()
	pre, err := coredet.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	id, err := anetcid.Sum(pre)
	if err != nil {
		t.Fatal(err)
	}
	return identity.SignedEvent{Event: e, Sig: ed25519.Sign(k, pre), EventID: id}
}

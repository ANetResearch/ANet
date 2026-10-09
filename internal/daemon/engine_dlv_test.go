package daemon

// The delivery cases of p2p-receipts §7 (DLV T) the outbound engine
// carries, each named by its number.

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// randomDackP2P is a direct address another node took over: it answers
// every send with a token of its own making (DLV T2).
type randomDackP2P struct{ sends atomic.Int32 }

func (p *randomDackP2P) Name() string                           { return "p2p-squatter" }
func (p *randomDackP2P) Reachable(context.Context, string) bool { return true }
func (p *randomDackP2P) Send(context.Context, string, []byte) ([]byte, bool, error) {
	p.sends.Add(1)
	dack := make([]byte, seal.DAckLen)
	_, _ = rand.Read(dack)
	return dack, true, nil
}

// DLV T2 (DLV-S-30): the direct address of a grant leads to another node,
// which answers with a random token. The sender goes on to the hub, and
// does not try that path for the peer again for 30 seconds.
func TestDLVT2_ASquattedAddressIsLeftForThirtySeconds(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	clk := newSharedClock(uint64(time.Now().UnixMilli()))
	req.setClock(clk.fn())
	p2p := &randomDackP2P{}
	req.RegisterTransport(p2p)
	if _, err := req.Delegate(ctx, prov.AID(), "first", nil); err != nil {
		t.Fatal(err)
	}
	if p2p.sends.Load() != 1 || len(queuedFor(t, srv, prov.AID())) != 1 {
		t.Fatalf("direct sends %d, at the hub %d", p2p.sends.Load(), len(queuedFor(t, srv, prov.AID())))
	}
	if counter(req, cntDAckInvalid) == 0 && counter(req, cntDAckP2PNoDAck) == 0 {
		t.Fatalf("the forged token was not counted: %v", req.ReceiveStats())
	}
	clk.add(20_000)
	if _, err := req.Delegate(ctx, prov.AID(), "within 30 seconds", nil); err != nil {
		t.Fatal(err)
	}
	if p2p.sends.Load() != 1 || len(queuedFor(t, srv, prov.AID())) != 2 {
		t.Fatalf("within 30 s: direct sends %d (want still 1), at the hub %d", p2p.sends.Load(), len(queuedFor(t, srv, prov.AID())))
	}
	clk.add(15_000)
	if _, err := req.Delegate(ctx, prov.AID(), "after 30 seconds", nil); err != nil {
		t.Fatal(err)
	}
	if p2p.sends.Load() != 2 {
		t.Fatalf("after 30 s: direct sends %d, want the path tried again", p2p.sends.Load())
	}
}

// deliverThenErrorP2P hands the envelope to the recipient's Inbound and
// then reports an error with no token: the answer was lost (DLV T3).
type deliverThenErrorP2P struct{ to *Daemon }

func (p deliverThenErrorP2P) Name() string                           { return "p2p-lost-answer" }
func (p deliverThenErrorP2P) Reachable(context.Context, string) bool { return true }
func (p deliverThenErrorP2P) Send(ctx context.Context, _ string, env []byte) ([]byte, bool, error) {
	_, _ = p.to.Inbound().Receive(ctx, env)
	return nil, true, errors.New("p2p: the answer was lost")
}

// DLV T3 (ENG-O4, ENG-X2): the direct path handed the delegation to the
// provider and lost the answer, and the hub cannot be reached. A cancel does
// not withdraw it, and goes out behind it; left to its deadline it is
// delivery_unconfirmed and UNVERIFIED, never UNAVAILABLE.
func TestDLVT3_HandedOverAndUnconfirmedIsNeverUnavailable(t *testing.T) {
	setup := func(t *testing.T) (*Daemon, *Daemon, string, *lampProvider) {
		_, req, prov := registeredPair(t)
		lamp := &lampProvider{}
		if err := prov.Providers().Register(context.Background(), lamp); err != nil {
			t.Fatal(err)
		}
		knowPeer(t, req, prov.AID())
		// The requester holds the provider's capability: no first-contact
		// session, which the hub would have to open, is needed (ENG-A6).
		heldCapFrom(t, prov, req)
		req.RegisterTransport(deliverThenErrorP2P{to: prov})
		setHubURL(req, deadHubURL)
		id, err := req.DelegateCapability(context.Background(), prov.AID(), lampCap, map[string]any{"on": true})
		if err != nil {
			t.Fatal(err)
		}
		// The direct path is acknowledged at the commit; the call runs after.
		waitUntil(t, "the provider to run the call", func() bool {
			pix, err := prov.ix.Get(id)
			return err == nil && pix.IsTerminal()
		})
		return req, prov, id, lamp
	}
	t.Run("a cancel goes behind it", func(t *testing.T) {
		req, _, id, _ := setup(t)
		if _, err := req.CancelTask(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		rows := outboxRows(t, req, id)
		if len(rows) != 2 || rows[0].Type != seal.TypeDelegate || !rows[0].Frozen || rows[1].Kind != rowKindCancel {
			t.Fatalf("outbox %+v, want the delegation frozen and the cancel behind it", rows)
		}
	})
	t.Run("at its deadline", func(t *testing.T) {
		req, _, id, _ := setup(t)
		holdGateOpen(req)
		now := req.nowMS()
		req.setClock(func() uint64 { return now + messageLifetimeMS + 60_000 })
		_ = req.deliverQueued(context.Background(), outboxRows(t, req, id)[0].ID)
		st, es, reason := taskViewMap(t, req, id)
		if st != string(a2ashape.TaskStateFailed) || es != "UNVERIFIED" || reason != reasonDeliveryUnconfirmed {
			t.Fatalf("at the deadline: %s %s %s, want failed UNVERIFIED delivery_unconfirmed", st, es, reason)
		}
	})
}

// DLV T4 (ENG-D4, D8, T1): a hub that answers 200 and keeps nothing. The
// first status query finds the message unknown, it is sent again — the
// direct path first — and confirmed. A hub that always loses it, with no
// direct path, leaves the delegation unconfirmed until no_response_after:
// then it is given up as delivery_unconfirmed, and a cancel follows.
func TestDLVT4_AHubThatLosesWhatItTook(t *testing.T) {
	t.Run("sent again and confirmed", func(t *testing.T) {
		srv, req, prov := registeredPair(t)
		clk := newSharedClock(uint64(time.Now().UnixMilli()))
		req.setClock(clk.fn())
		direct := &switchP2P{to: prov}
		req.RegisterTransport(direct)
		setFake(t, srv.URL, func(h *fakeHub) { h.dropSends = true })
		id, err := req.Delegate(context.Background(), prov.AID(), "lost by the hub", nil)
		if err != nil {
			t.Fatal(err)
		}
		setFake(t, srv.URL, func(h *fakeHub) { h.dropSends = false })
		row := outboxRows(t, req, id)[0]
		if row.CheckAt == 0 || uint64(row.CheckAt) > clk.now()+2_000 {
			t.Fatalf("first status query at %d, want 2 s on", int64(row.CheckAt)-int64(clk.now()))
		}
		clk.set(uint64(row.CheckAt))
		askHubNow(t, req)
		if counter(req, cntDAckStatusUnknown) == 0 {
			t.Fatal("the lost delegation was not found unknown")
		}
		direct.on.Store(true)
		row = outboxRows(t, req, id)[0]
		clk.set(uint64(row.NextAt))
		sends := relayCountFor(srv.URL)
		if err := req.attemptOutbox(context.Background(), row.ID); err != nil {
			t.Fatal(err)
		}
		// The direct path first; its answer carries no token yet (the
		// direct receive's token is engine ENG-1c's), so the hub too.
		if direct.sends.Load() != 1 || relayCountFor(srv.URL) != sends+1 {
			t.Fatalf("direct sends %d, hub sends %d", direct.sends.Load(), relayCountFor(srv.URL)-sends)
		}
		settleDelivery(t, req, prov)
		if rows := outboxRows(t, req, id); len(rows) != 0 {
			t.Fatalf("not confirmed: %+v", rows)
		}
	})
	t.Run("lost for good", func(t *testing.T) {
		srv, req, prov := registeredPair(t)
		req.setOutboxHold(true)
		setFake(t, srv.URL, func(h *fakeHub) { h.dropSends = true })
		id, err := req.Delegate(context.Background(), prov.AID(), "lost for good", nil)
		if err != nil {
			t.Fatal(err)
		}
		askHubNow(t, req)
		failed := req.failUnanswered(time.Now().Add(DefaultNoResponseAfter + time.Minute).UnixMilli())
		if len(failed) != 1 || failed[0] != id {
			t.Fatalf("failed %v, want %s", failed, id)
		}
		if st, _, reason := taskViewMap(t, req, id); st != string(a2ashape.TaskStateFailed) || reason != reasonDeliveryUnconfirmed {
			t.Fatalf("task %s %s", st, reason)
		}
		rows := outboxRows(t, req, id)
		if len(rows) != 1 || rows[0].Kind != rowKindCancel {
			t.Fatalf("outbox %+v, want the cancel", rows)
		}
	})
}

// ixOf is d's task id.
func ixOf(t testing.TB, d *Daemon, id string) *interactions.Interaction {
	t.Helper()
	ix, err := d.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// switchP2P is a direct path that, while on, hands the envelope to the
// recipient's Inbound and passes its token back.
type switchP2P struct {
	to    *Daemon
	on    atomic.Bool
	sends atomic.Int32
	last  atomic.Pointer[switchResult]
}

func (p *switchP2P) Name() string                           { return "p2p-switch" }
func (p *switchP2P) Reachable(context.Context, string) bool { return p.on.Load() }
func (p *switchP2P) Send(ctx context.Context, _ string, env []byte) ([]byte, bool, error) {
	p.sends.Add(1)
	dack, err := p.to.Inbound().Receive(ctx, env)
	p.last.Store(&switchResult{dack: dack, err: err})
	return dack, true, err
}

type switchResult struct {
	dack []byte
	err  error
}

// DLV T9 (ENG-D6, M5): ask before sending. A recipient offline for three
// days: the envelope is uploaded once, the hub holds it in one mailbox row
// under one rid, and the sender only asks about it.
func TestDLVT9_AQueuedVersionIsAskedAboutNotSentAgain(t *testing.T) {
	srv, req, prov := registeredPair(t)
	req.setOutboxHold(true)
	holdGateOpen(req)
	req.mu.Lock()
	req.cfg.NoResponseAfter = "0"
	req.mu.Unlock()
	clk := newSharedClock(uint64(time.Now().UnixMilli()))
	req.setClock(clk.fn())
	log := &sentLog{}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), string(bytes.Repeat([]byte("a large request "), 4096)), nil)
	if err != nil {
		t.Fatal(err)
	}
	row := outboxRows(t, req, id)[0]
	rid := rowVersions(t, req, row.ID)[0].RID
	end := clk.now() + 3*24*3600*1000
	for clk.now() < end {
		r, _, err := req.ix.OutboxState(row.ID)
		if err != nil {
			t.Fatal(err)
		}
		next := uint64(r.NextAt)
		if r.CheckAt != 0 && uint64(r.CheckAt) < next {
			next = uint64(r.CheckAt)
		}
		clk.set(max(next, clk.now()+1000))
		req.flushOutbox(ctx)
		if n := len(log.digests(t)); n > 1 {
			t.Fatalf("the envelope was uploaded again %d hours on, while the hub held it", (clk.now()-end+3*24*3600*1000)/3_600_000)
		}
	}
	if n := len(log.digests(t)); n != 1 {
		t.Fatalf("the envelope was uploaded %d times in three days", n)
	}
	if n := len(mailboxIDsFor(srv.URL, prov.AID())); n != 1 {
		t.Fatalf("the provider's mailbox holds %d rows", n)
	}
	vers := rowVersions(t, req, row.ID)
	if len(vers) != 1 || !bytes.Equal(vers[0].RID, rid) {
		t.Fatalf("versions %+v, want the one rid", vers)
	}
	if statusCalls(srv.URL) < 10 {
		t.Fatalf("only %d status queries in three days", statusCalls(srv.URL))
	}
}

// DLV T10 (ENG-C3): a direct token and the hub's done race for one row:
// the row goes once, and the task changes once.
func TestDLVT10_RacingConfirmationsApplyOnce(t *testing.T) {
	_, req, prov := registeredPair(t)
	id, err := req.Delegate(context.Background(), prov.AID(), "raced", nil)
	if err != nil {
		t.Fatal(err)
	}
	row := outboxRows(t, req, id)[0]
	before := ixOf(t, req, id).StateSeq
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			counterName := cntDAckConfirmedP2P
			if i%2 == 1 {
				counterName = cntDAckConfirmedHub
			}
			_ = req.confirmOutbox(row.ID, seal.OutcomeRefused, seal.Note{Reason: "provider_policy"}, counterName)
		}(i)
	}
	wg.Wait()
	if n := counter(req, cntDAckConfirmedP2P) + counter(req, cntDAckConfirmedHub); n != 1 {
		t.Fatalf("confirmed %d times", n)
	}
	if after := ixOf(t, req, id); after.StateSeq != before+1 || after.State != interactions.StateRejected {
		t.Fatalf("task state %s, seq %d → %d; want rejected once", after.State, before, after.StateSeq)
	}
}

// DLV T21 (ENG-O2): the local A2A client sends a task and a follow-up at
// once. The follow-up is not sent before the delegation is confirmed; the
// provider gets them in order, and never answers task_not_found.
func TestDLVT21_AFollowUpWaitsForItsDelegation(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("plan a trip", "", "m-1"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: followUp(task.ID, "to Kyoto", "m-2"), ReturnImmediately: true}); err != nil {
		t.Fatal(err)
	}
	got := innerTypes(t, srv, srv.URL, prov, queuedFor(t, srv, prov.AID()))
	if len(got) != 1 || got[0] != seal.TypeDelegate {
		t.Fatalf("hub mailbox %v, want the delegation alone", got)
	}
	settleDelivery(t, req, prov)
	waitUntil(t, "the follow-up at the hub", func() bool { return len(queuedFor(t, srv, prov.AID())) == 1 })
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := counter(prov, transientUnknownIX) + counter(prov, dropUnknownIX); n != 0 {
		t.Fatalf("task_not_found: %v", prov.ReceiveStats())
	}
	msgs, _ := prov.ix.Messages(task.ID)
	if len(msgs) != 2 || msgs[1].Body != "to Kyoto" {
		t.Fatalf("provider messages %+v, want the task then the follow-up", msgs)
	}
}

// DLV T23 (ENG-O6): once canceled, the delegation is frozen: its status
// queries go on and find it unknown, and it is not sent again; the cancel
// goes.
func TestDLVT23_AFrozenDelegationIsNotSentAgain(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	clk := newSharedClock(uint64(time.Now().UnixMilli()))
	req.setClock(clk.fn())
	setFake(t, srv.URL, func(h *fakeHub) { h.dropSends = true })
	id, err := req.Delegate(ctx, prov.AID(), "lost, then canceled", nil)
	if err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.dropSends = false })
	log := &sentLog{}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, req, id)
	if len(rows) != 2 || !rows[0].Frozen {
		t.Fatalf("outbox %+v, want the delegation frozen and the cancel", rows)
	}
	askHubNow(t, req)
	if counter(req, cntDAckStatusUnknown) == 0 {
		t.Fatal("the frozen delegation's status was not asked")
	}
	for i := 0; i < 5; i++ {
		clk.add(3_600_000)
		req.flushOutbox(ctx)
	}
	got := innerTypes(t, srv, srv.URL, prov, queuedFor(t, srv, prov.AID()))
	if len(got) != 1 || got[0] != seal.TypeMessage+":cancel" {
		t.Fatalf("hub mailbox %v, want the cancel alone", got)
	}
	for _, e := range log.envs {
		if typ := innerTypes(t, srv, srv.URL, prov, [][]byte{e}); typ[0] == seal.TypeDelegate {
			t.Fatal("the frozen delegation was sent again")
		}
	}
}

// DLV T25 (ENG-A1, ENG-X2, M15): a hub that cannot be dialed for fourteen
// days hands over nothing. The delegation is given up at its deadline as
// undeliverable, UNAVAILABLE; a cancel in between withdraws it and sends
// nothing.
func TestDLVT25_NeverHandedOver(t *testing.T) {
	setup := func(t *testing.T) (*Daemon, *Daemon, string, *sharedClock) {
		srv, req, prov := registeredPair(t)
		knowPeer(t, req, prov.AID())
		// The requester holds the provider's capability, so the hub is
		// dialed for the envelope itself (no session to open first), and
		// knows the hub's identity; then the hub goes away.
		heldCapFrom(t, prov, req)
		if _, _, err := req.hubIdentity(context.Background(), srv.URL); err != nil {
			t.Fatal(err)
		}
		holdGateOpen(req)
		req.setOutboxHold(true)
		clk := newSharedClock(uint64(time.Now().UnixMilli()))
		req.setClock(clk.fn())
		srv.Close()
		id, err := req.DelegateCapability(context.Background(), prov.AID(), lampCap, map[string]any{"on": true})
		if err != nil {
			t.Fatal(err)
		}
		return req, prov, id, clk
	}
	drive := func(req *Daemon, clk *sharedClock, until uint64) {
		for clk.now() < until {
			rows := allOutboxRows(t, req)
			if len(rows) == 0 {
				return
			}
			clk.set(max(uint64(rows[0].NextAt), clk.now()+1000))
			req.flushOutbox(context.Background())
		}
	}
	t.Run("given up at the deadline", func(t *testing.T) {
		req, _, id, clk := setup(t)
		drive(req, clk, clk.now()+messageLifetimeMS+3_600_000)
		st, es, reason := taskViewMap(t, req, id)
		if st != string(a2ashape.TaskStateFailed) || es != "UNAVAILABLE" || reason != a2ashape.ReasonUndeliverable {
			t.Fatalf("after fourteen days: %s %s %s, want failed UNAVAILABLE undeliverable", st, es, reason)
		}
	})
	t.Run("a cancel withdraws it", func(t *testing.T) {
		req, _, id, clk := setup(t)
		drive(req, clk, clk.now()+2*24*3600*1000)
		if _, err := req.CancelTask(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if rows := outboxRows(t, req, id); len(rows) != 0 {
			t.Fatalf("outbox %+v after the cancel, want nothing", rows)
		}
		if st := stateOf(t, req, id); st != interactions.StateCanceled {
			t.Fatalf("task %s", st)
		}
		if ev := lastLedgerPayload(t, req, EvDeliveryExpired); ev["reason"] != "withdrawn" || ev["handed"] != false {
			t.Fatalf("evidence %v, want withdrawn, nothing handed", ev)
		}
	})
}

// DLV T30 (ENG-T3): no_response counts from the confirmation. A provider
// away for ten minutes takes the delegation then; the task fails as
// no_response fifteen minutes after that, not after it was made.
func TestDLVT30_NoResponseCountsFromTheConfirmation(t *testing.T) {
	_, req, prov := registeredPair(t)
	clk := newSharedClock(uint64(time.Now().UnixMilli()))
	req.setClock(clk.fn())
	req.setOutboxHold(true)
	t0 := clk.now()
	id, err := req.Delegate(context.Background(), prov.AID(), "away for ten minutes", nil)
	if err != nil {
		t.Fatal(err)
	}
	clk.set(t0 + 10*60_000)
	settleDelivery(t, req, prov)
	if rows := outboxRows(t, req, id); len(rows) != 0 {
		t.Fatalf("not confirmed: %+v", rows)
	}
	if got := req.failUnanswered(int64(t0 + 24*60_000)); len(got) != 0 {
		t.Fatalf("failed %v fifteen minutes after the task was made", got)
	}
	got := req.failUnanswered(int64(t0 + 25*60_000 + 1_000))
	if len(got) != 1 || got[0] != id {
		t.Fatalf("failed %v fifteen minutes after the confirmation, want %s", got, id)
	}
	if _, _, reason := taskViewMap(t, req, id); reason != a2ashape.ReasonNoResponse {
		t.Fatalf("reason %s", reason)
	}
}

// DLV T33 (ENG-D3, M30): every /relay/status request asks about one row's
// versions only.
func TestDLVT33_AStatusQueryIsAboutOneRow(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	var ids []string
	for _, goal := range []string{"one", "two", "three"} {
		id, err := req.Delegate(ctx, prov.AID(), goal, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rowOfHandle := map[string]int64{}
	for _, id := range ids {
		row := outboxRows(t, req, id)[0]
		for _, v := range rowVersions(t, req, row.ID) {
			var k seal.AckKey
			copy(k[:], v.AckKey)
			h := k.Handle()
			rowOfHandle[hubapi.B64(h[:])] = row.ID
		}
	}
	askHubNow(t, req)
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	reqs := append([][]hubapi.RelayStatusItem(nil), h.statusRequests...)
	h.mu.Unlock()
	if len(reqs) < len(ids) {
		t.Fatalf("%d status requests for %d rows", len(reqs), len(ids))
	}
	for _, items := range reqs {
		rows := map[int64]bool{}
		for _, it := range items {
			rows[rowOfHandle[it.Handle]] = true
		}
		if len(rows) != 1 {
			t.Fatalf("a status request asked about %d rows", len(rows))
		}
	}
}

// DLV T34 (ENG-D2, SI-13): a status query names nobody — no relayauth
// header — and shares no connection with a poll or an ack.
func TestDLVT34_AStatusQueryIsAnonymousAndAlone(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := req.Delegate(ctx, prov.AID(), "asked about", nil); err != nil {
			t.Fatal(err)
		}
		askHubNow(t, req)
		if err := req.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	headers := append([]map[string][]string(nil), func() []map[string][]string {
		var out []map[string][]string
		for _, hd := range h.statusHeaders {
			out = append(out, hd)
		}
		return out
	}()...)
	h.mu.Unlock()
	if len(headers) == 0 {
		t.Fatal("setup: no status query")
	}
	for _, hd := range headers {
		for _, name := range []string{hubapi.HeaderAID, hubapi.HeaderSeq, hubapi.HeaderSig, hubapi.HeaderSession, hubapi.HeaderMAC} {
			if len(hd[name]) > 0 {
				t.Fatalf("a status query carried %s", name)
			}
		}
	}
	byConn := map[string]map[string]bool{}
	for _, r := range hubRequests(t, srv.URL) {
		if byConn[r.remote] == nil {
			byConn[r.remote] = map[string]bool{}
		}
		byConn[r.remote][r.path] = true
	}
	for conn, paths := range byConn {
		if paths[hubapi.RelayStatusPath] && (paths[hubapi.RelayPollPath] || paths[hubapi.RelayAckPath]) {
			t.Fatalf("connection %s carried a status query and a poll or an ack", conn)
		}
	}
}

// DLV T36 (ENG-X3): a delegation handed over and given up as
// delivery_unconfirmed takes the follow-ups that never left with it; the
// cancel behind it stays and goes.
func TestDLVT36_TheQueueAfterAnAbandonment(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	req.setOutboxHold(true)
	id, err := req.Delegate(ctx, prov.AID(), "never confirmed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.SendMessage(ctx, id, "a follow-up that waits", nil); err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, req, id)
	if len(rows) != 2 || rows[1].HandedAt != 0 {
		t.Fatalf("setup: %+v", rows)
	}
	failed := req.failUnanswered(time.Now().Add(DefaultNoResponseAfter + time.Minute).UnixMilli())
	if len(failed) != 1 {
		t.Fatalf("failed %v", failed)
	}
	rows = outboxRows(t, req, id)
	if len(rows) != 1 || rows[0].Kind != rowKindCancel {
		t.Fatalf("outbox %+v, want the cancel alone", rows)
	}
	req.flushOutbox(ctx)
	got := innerTypes(t, srv, srv.URL, prov, queuedFor(t, srv, prov.AID()))
	if len(got) != 2 || got[1] != seal.TypeMessage+":cancel" {
		t.Fatalf("hub mailbox %v, want the delegation then the cancel", got)
	}
}

// DLV T37 (ENG-H1, M14): a hub 404 after the delegation was handed over:
// the task fails as recipient_unknown, UNVERIFIED, and its status text is
// the same sentence.
func TestDLVT37_A404AfterHandingOverIsUnverified(t *testing.T) {
	srv, req, prov := registeredPair(t)
	setFake(t, srv.URL, func(h *fakeHub) {
		h.sendHook = func([]byte) fakeSendAnswer {
			return fakeSendAnswer{kind: fakeSendRefuse, status: 404, code: "recipient_unknown"}
		}
	})
	_, err := req.DelegateCapability(context.Background(), prov.AID(), lampCap, map[string]any{"on": true})
	if !errors.Is(err, errUndeliverable) {
		t.Fatalf("delegate: %v, want it given up", err)
	}
	list, _ := req.ix.List(interactions.RoleOutbound, "", 0, 10)
	if len(list) != 1 {
		t.Fatalf("%d tasks", len(list))
	}
	st, es, reason := taskViewMap(t, req, list[0].ID)
	if st != string(a2ashape.TaskStateFailed) || es != "UNVERIFIED" || reason != reasonRecipientUnknown {
		t.Fatalf("task %s %s %s, want failed UNVERIFIED recipient_unknown", st, es, reason)
	}
	if want := a2ashape.Sentence(reasonRecipientUnknown, a2ashape.English, nil); string(ixOf(t, req, list[0].ID).Result) != want {
		t.Fatalf("status text %q, want %q", ixOf(t, req, list[0].ID).Result, want)
	}
}

// DLV T50 (DLV-W-12b): a 200 whose rid is not one: the version counts as
// handed over without a rid, is counted, and sent again; the row is
// neither confirmed nor given up.
func TestDLVT50_A200WithABadRIDIsSentAgain(t *testing.T) {
	srv, req, prov := registeredPair(t)
	log := &sentLog{next: func(n int, _ []byte) fakeSendAnswer {
		if n == 1 {
			return fakeSendAnswer{kind: fakeSendBadRID}
		}
		return fakeSendAnswer{}
	}}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	id, err := req.Delegate(context.Background(), prov.AID(), "a bad rid", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, req, id)
	if len(rows) != 1 || rows[0].HandedAt == 0 || lastErrorCode(rows[0].LastError) != errBadRID {
		t.Fatalf("after the bad rid: %+v", rows)
	}
	if v := rowVersions(t, req, rows[0].ID); len(v) != 1 || v[0].RID != nil {
		t.Fatalf("versions %+v, want no rid", v)
	}
	if counter(req, cntHubBadRID) != 1 {
		t.Fatalf("hub-bad-rid %d", counter(req, cntHubBadRID))
	}
	if err := retryNow(t, req, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if n := len(log.digests(t)); n != 2 {
		t.Fatalf("sent %d times, want twice", n)
	}
	if v := rowVersions(t, req, rows[0].ID); len(v) != 1 || v[0].RID == nil {
		t.Fatalf("versions %+v, want the rid of the second answer", v)
	}
}

// strictly checks the ENG-Q5 refusals of a new delegation end to end: a
// recipient no hub knows, one whose directory entry does not verify, and
// one whose hub cannot be asked.
func TestStrictSendRefusesWhatCannotBeVerified(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	stranger := newStranger(t)
	before := outboundCount(t, req)
	_, err := req.Delegate(ctx, stranger.aid, "to nobody", nil)
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) || ref.Code() != reasonRecipientUnknown {
		t.Fatalf("a recipient no hub knows: %v", err)
	}
	// The same refusal through the local A2A interface.
	_, err = req.TaskSeam().Send(ctx, stranger.aid, module.TaskSend{Message: textMsg("to nobody", "", "m-u"), ReturnImmediately: true})
	if !errors.As(err, &ref) || ref.Code() != reasonRecipientUnknown {
		t.Fatalf("a recipient no hub knows, through the task seam: %v", err)
	}
	h := fakeHubAt(t, srv.URL)
	bad := entryOf(t, h, prov.AID())
	bad.Keys = append([]byte(nil), bad.Keys...)
	bad.Keys[len(bad.Keys)-1] ^= 1
	h.setDir(func(h *fakeHub) { h.dirOverride[prov.AID()] = bad })
	other := newPlainTestDaemon(t, srv.URL, false)
	if err := other.RegisterWithHub(ctx, srv.URL, "Other", nil); err != nil {
		t.Fatal(err)
	}
	_, err = other.Delegate(ctx, prov.AID(), "to an entry that does not verify", nil)
	if !errors.As(err, &ref) || ref.Code() != reasonRecipientUnverifiable {
		t.Fatalf("a recipient whose entry does not verify: %v", err)
	}
	if n := outboundCount(t, req); n != before {
		t.Fatalf("%d tasks written for refused sends", n-before)
	}
	if n, _ := other.ix.OutboxLen(); n != 0 {
		t.Fatalf("%d rows queued for a refused send", n)
	}
	// Its hub cannot be asked: queued, waiting on home_unreachable.
	third := newPlainTestDaemon(t, srv.URL, false)
	if err := third.RegisterWithHub(ctx, srv.URL, "Third", nil); err != nil {
		t.Fatal(err)
	}
	setHubURL(third, deadHubURL)
	id, err := third.Delegate(ctx, req.AID(), "while its hub is away", nil)
	if err != nil {
		t.Fatalf("a recipient whose hub cannot be asked: %v", err)
	}
	rows := outboxRows(t, third, id)
	if len(rows) != 1 || rowWait(t, third, rows[0].ID) != reasonHomeUnreachable {
		t.Fatalf("outbox %+v, want it waiting on home_unreachable", rows)
	}
}

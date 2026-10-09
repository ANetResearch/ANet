package daemon

// The outbound engine's own cases (engine §8.3, E-T), each named by its
// number: what the engine does with the answers a fake hub and a fake
// direct transport give it.

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// rowWait is the anet.delivery_wait of d's row id now (engine ENG-P1).
func rowWait(t testing.TB, d *Daemon, id int64) string {
	t.Helper()
	row, vers, err := d.ix.OutboxState(id)
	if err != nil {
		t.Fatal(err)
	}
	return deliveryWait(rowView(row), d.verViews(id, vers, d.configuredHub(), d.nowMS()), d.identityGate())
}

// sentLog records what a fake hub was sent, and answers from a script.
type sentLog struct {
	mu   sync.Mutex
	envs [][]byte
	next func(n int, env []byte) fakeSendAnswer
}

func (s *sentLog) hook(env []byte) fakeSendAnswer {
	s.mu.Lock()
	s.envs = append(s.envs, bytes.Clone(env))
	n := len(s.envs)
	next := s.next
	s.mu.Unlock()
	if next == nil {
		return fakeSendAnswer{}
	}
	return next(n, env)
}

func (s *sentLog) digests(t testing.TB) [][32]byte {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][32]byte, 0, len(s.envs))
	for _, e := range s.envs {
		d, err := seal.EnvDigest(e)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// currentDigest is the env_digest of the version row id holds now.
func currentDigest(t testing.TB, d *Daemon, id int64) [32]byte {
	t.Helper()
	_, env, err := d.ix.OutboxContent(id)
	if err != nil {
		t.Fatal(err)
	}
	dg, err := seal.EnvDigest(env)
	if err != nil {
		t.Fatal(err)
	}
	return dg
}

// rowOfKind is d's only row of ix with kind (rowKind*), or nil.
func rowOfKind(t testing.TB, d *Daemon, ix, kind string) *interactions.OutboxRow {
	t.Helper()
	for _, r := range outboxRows(t, d, ix) {
		if r.Kind == kind {
			r := r
			return &r
		}
	}
	return nil
}

// E-T8 (ENG-Q1, M19): a notice after the fact goes through the queue like
// any message. The hub takes the pending_expired notice and loses it; the
// status query finds it unknown, the notice goes again, and the requester's
// token confirms it.
func TestET8_ANoticeTheHubLostGoesAgain(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	s := registered(t, srv.URL, "requester")
	setPolicy(t, prov, PolicyApprove)
	prov.notices.configure(100, 100)
	id, err := s.Delegate(ctx, prov.AID(), "held, then expired", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.GetPending(id); err != nil {
		t.Fatalf("not held: %v", err)
	}
	waitUntil(t, "the pending_approval notice at the hub", func() bool {
		r := rowOfKind(t, prov, id, rowKindPendingApproval)
		return r != nil && r.HandedAt != 0 && !r.Attempting
	})
	prov.setOutboxHold(true)
	setFake(t, srv.URL, func(h *fakeHub) { h.dropSends = true })
	prov.expirePendingAt(int64(prov.nowMS()) + 73*3600*1000)
	row := rowOfKind(t, prov, id, rowKindPendingExpired)
	if row == nil {
		t.Fatalf("no pending_expired notice queued: %+v", outboxRows(t, prov, id))
	}
	sends0 := relayCountFor(srv.URL)
	prov.flushOutbox(ctx)
	if n := relayCountFor(srv.URL) - sends0; n != 1 {
		t.Fatalf("the notice went %d times, want once", n)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.dropSends = false })
	// The hub lost it: asked about it, it says unknown, and the notice is
	// sent again once its backoff allows.
	askHubNow(t, prov)
	if counter(prov, cntDAckStatusUnknown) == 0 {
		t.Fatal("the lost notice was not found unknown")
	}
	if err := retryNow(t, prov, row.ID); err != nil {
		t.Fatal(err)
	}
	if n := relayCountFor(srv.URL) - sends0; n != 2 {
		t.Fatalf("the notice went %d times, want twice", n)
	}
	settleDelivery(t, prov, s)
	if rows := outboxRows(t, prov, id); len(rows) != 0 {
		t.Fatalf("the notices are not confirmed: %+v", rows)
	}
	msgs, _ := s.ix.Messages(id)
	found := false
	for _, m := range msgs {
		if m.Kind == interactions.MsgStatus && bytes.Contains([]byte(m.Metadata), []byte("pending_expired")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the requester never got the pending_expired notice: %+v", msgs)
	}
}

// E-T9 (ENG-A2, M2): with v1 handed over and v2 current, a send again
// carries v2's bytes and nothing of v1.
func TestET9_ARetransmissionSendsTheCurrentVersion(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	log := &sentLog{next: func(n int, _ []byte) fakeSendAnswer {
		switch n {
		case 1:
			return fakeSendAnswer{kind: fakeSendRefuse, status: 409, code: "kid_stale"}
		case 2:
			return fakeSendAnswer{kind: fakeSendRefuse, status: 503, code: "internal"}
		}
		return fakeSendAnswer{}
	}}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	id, err := req.Delegate(ctx, prov.AID(), "two versions", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, req, id)
	if len(rows) != 1 || rows[0].CurVer != 2 {
		t.Fatalf("setup: %+v, want the second version current", rows)
	}
	vers := rowVersions(t, req, rows[0].ID)
	if len(vers) != 2 || vers[0].HandedAt == 0 || vers[1].HandedAt == 0 {
		t.Fatalf("versions %+v, want v1 and v2 both handed over", vers)
	}
	if err := retryNow(t, req, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	d := log.digests(t)
	if len(d) != 3 || d[0] == d[1] || d[2] != d[1] || d[2] != currentDigest(t, req, rows[0].ID) {
		t.Fatalf("sent %d envelopes: v1 %x, v2 %x, again %x; want the third to be v2", len(d), d[0][:4], d[1][:4], d[len(d)-1][:4])
	}
}

// E-T15 (ENG-Q7, M20): a stranger's delegation held for approval that
// expires, when this node holds no capability of the requester's for it:
// no notice is queued at all, and nothing goes as a first contact either.
func TestET15_ANoticeWithoutACapabilityIsNotSent(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	s := registered(t, srv.URL, "stranger")
	setPolicy(t, prov, PolicyApprove)
	prov.notices.configure(100, 100)
	const ix = "ix_no_grant_held"
	// Sealed as a one-shot: no grant in it.
	injectEnvelope(t, srv, prov.AID(), sealFrom(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.self, ix, "held", "")))
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.GetPending(ix); err != nil {
		t.Fatalf("not held: %v", err)
	}
	prov.expirePendingAt(int64(prov.nowMS()) + 73*3600*1000)
	if rows := outboxRows(t, prov, ix); len(rows) != 0 {
		t.Fatalf("notices queued without a capability: %+v", rows)
	}
	if n := counter(prov, cntNoticeNoGrant); n != 2 {
		t.Fatalf("notice-no-grant counted %d times, want the held and the expired notice", n)
	}
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	opens := 0
	for _, sess := range h.sessions {
		if sess.aid == prov.AID() {
			opens++
		}
	}
	sent := h.sendsBy[prov.AID()]
	h.mu.Unlock()
	if opens != 0 || sent != 0 {
		t.Fatalf("the provider opened %d first-contact sessions and sent %d", opens, sent)
	}
}

// E-T19 (SSR-D11, M27): a capability the hub refuses. The credential is
// chosen again for every attempt, never kept on the row.
func TestET19_ARefusedCapability(t *testing.T) {
	// setup: the requester holds the provider's capability for the task
	// (its question carried a grant), and the delegation is confirmed.
	setup := func(t *testing.T) (string, *fakeHub, *Daemon, *Daemon, string) {
		srv, req, prov := registeredPair(t)
		ctx := context.Background()
		id, err := req.Delegate(ctx, prov.AID(), "a task", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if err := prov.SendMessage(ctx, id, "a question", nil); err != nil {
			t.Fatal(err)
		}
		if err := req.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		held, _ := req.ix.HeldCaps(prov.AID())
		valid := 0
		for _, h := range held {
			if h.InvalidAt == 0 {
				valid++
			}
		}
		if valid == 0 {
			t.Fatal("setup: the requester holds no capability of the provider's")
		}
		return srv.URL, fakeHubAt(t, srv.URL), req, prov, id
	}
	kindOf := func(env []byte) uint64 {
		o, err := seal.ParseOuter(env)
		if err != nil {
			return 99
		}
		return uint64(o.Access.Kind)
	}

	t.Run("a: unexplained refusals give it up after ten minutes", func(t *testing.T) {
		_, h, req, _, id := setup(t)
		clk := newSharedClock(uint64(time.Now().UnixMilli()))
		req.setClock(clk.fn())
		var kinds []uint64
		var mu sync.Mutex
		h.mu.Lock()
		h.sendHook = func(env []byte) fakeSendAnswer {
			k := kindOf(env)
			mu.Lock()
			kinds = append(kinds, k)
			mu.Unlock()
			if k == uint64(seal.AccessKindCap) {
				return fakeSendAnswer{kind: fakeSendRefuse, status: 403, code: "access_invalid"}
			}
			return fakeSendAnswer{}
		}
		h.mu.Unlock()
		start := clk.now()
		if err := req.SendMessage(context.Background(), id, "the answer", nil); err != nil {
			t.Fatal(err)
		}
		rows := outboxRows(t, req, id)
		if len(rows) != 1 || lastErrorCode(rows[0].LastError) != codeCredentialRefused ||
			rowWait(t, req, rows[0].ID) != codeCredentialRefused {
			t.Fatalf("after one refusal: %+v, want it waiting on credential_refused", rows)
		}
		rowID := rows[0].ID
		for i := 0; i < 30; i++ {
			row, _, err := req.ix.OutboxState(rowID)
			if err != nil {
				t.Fatal(err)
			}
			if row.HandedAt != 0 && row.LastError == "" {
				break // taken
			}
			clk.set(max(uint64(row.NextAt), clk.now()+1000))
			_ = req.attemptOutbox(context.Background(), rowID)
		}
		mu.Lock()
		got := append([]uint64(nil), kinds...)
		mu.Unlock()
		last := len(got) - 1
		if last < 3 || got[last] != uint64(seal.AccessKindSession) {
			t.Fatalf("sends by kind %v: never went as a first contact", got)
		}
		for _, k := range got[:last] {
			if k != uint64(seal.AccessKindCap) {
				t.Fatalf("sends by kind %v: went as a first contact before the threshold", got)
			}
		}
		if clk.now()-start < 10*60*1000 {
			t.Fatalf("gave the capability up after %d ms, before ten minutes", clk.now()-start)
		}
		// Given up for good: the next message goes as a first contact.
		if err := req.SendMessage(context.Background(), id, "and another", nil); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		after := kinds[len(kinds)-1]
		mu.Unlock()
		if after != uint64(seal.AccessKindSession) {
			t.Fatal("the given-up capability was used again")
		}
	})

	t.Run("b: a risen epoch explains the refusal", func(t *testing.T) {
		url, h, req, prov, id := setup(t)
		// The provider's mailbox key goes up an epoch and the hub holds the
		// new set: the old capability is refused by the hub itself.
		if err := prov.ix.Update(func(tx *interactions.Tx) error { _, err := prov.rotateMboxTx(tx); return err }); err != nil {
			t.Fatal(err)
		}
		if err := prov.republishKeys(context.Background(), url); err != nil {
			t.Fatal(err)
		}
		var kinds []uint64
		var mu sync.Mutex
		h.mu.Lock()
		h.sendHook = func(env []byte) fakeSendAnswer {
			mu.Lock()
			kinds = append(kinds, kindOf(env))
			mu.Unlock()
			return fakeSendAnswer{}
		}
		h.mu.Unlock()
		sends0 := relayCountFor(url)
		if err := req.SendMessage(context.Background(), id, "after the rotation", nil); err != nil {
			t.Fatal(err)
		}
		if rows := outboxRows(t, req, id); len(rows) != 1 || rows[0].HandedAt == 0 || rows[0].LastError != "" {
			t.Fatalf("after the refusal: %+v, want it taken in the same attempt", rows)
		}
		// The hub refused the capability itself (the hook sees what passed
		// its check): the one envelope it took is a first contact.
		mu.Lock()
		got := append([]uint64(nil), kinds...)
		mu.Unlock()
		if len(got) != 1 || got[0] != uint64(seal.AccessKindSession) || relayCountFor(url)-sends0 != 1 {
			t.Fatalf("sends taken by kind %v, want one first contact", got)
		}
		held, _ := req.ix.HeldCaps(prov.AID())
		for _, c := range held {
			if c.Scope == "peer" && c.InvalidAt == 0 {
				t.Fatal("the capability the risen epoch explained is still used")
			}
		}
	})

	t.Run("c: one refusal then acceptance does not downgrade", func(t *testing.T) {
		_, h, req, _, id := setup(t)
		clk := newSharedClock(uint64(time.Now().UnixMilli()))
		req.setClock(clk.fn())
		var refused atomic.Bool
		var kinds []uint64
		var mu sync.Mutex
		h.mu.Lock()
		h.sendHook = func(env []byte) fakeSendAnswer {
			k := kindOf(env)
			mu.Lock()
			kinds = append(kinds, k)
			mu.Unlock()
			if k == uint64(seal.AccessKindCap) && refused.CompareAndSwap(false, true) {
				return fakeSendAnswer{kind: fakeSendRefuse, status: 403, code: "access_invalid"}
			}
			return fakeSendAnswer{}
		}
		h.mu.Unlock()
		if err := req.SendMessage(context.Background(), id, "the answer", nil); err != nil {
			t.Fatal(err)
		}
		rows := outboxRows(t, req, id)
		clk.set(uint64(rows[0].NextAt))
		if err := req.attemptOutbox(context.Background(), rows[0].ID); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		got := append([]uint64(nil), kinds...)
		mu.Unlock()
		if len(got) != 2 || got[0] != uint64(seal.AccessKindCap) || got[1] != uint64(seal.AccessKindCap) {
			t.Fatalf("sends by kind %v, want the capability twice", got)
		}
		held, _ := req.ix.HeldCaps(rows[0].ToAID)
		for _, c := range held {
			if c.RefusedN != 0 {
				t.Fatalf("an accepted capability keeps %d refusals", c.RefusedN)
			}
		}
	})
}

// E-T21 (DLV-S-5, M29): within one attempt the direct path and the hub
// carry the same version: their bytes differ in key 7 alone.
func TestET21_BothPathsCarryOneVersion(t *testing.T) {
	srv, req, prov := registeredPair(t)
	p2p := &recordingP2P{}
	req.RegisterTransport(p2p)
	log := &sentLog{}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	if _, err := req.Delegate(context.Background(), prov.AID(), "both paths", nil); err != nil {
		t.Fatal(err)
	}
	direct := p2p.digests(t)
	viaHub := log.digests(t)
	if len(direct) != 1 || len(viaHub) != 1 || direct[0] != viaHub[0] {
		t.Fatalf("direct %d, hub %d envelopes, digests equal: %v", len(direct), len(viaHub),
			len(direct) == 1 && len(viaHub) == 1 && direct[0] == viaHub[0])
	}
}

// recordingP2P takes every envelope (handed over) and answers no token.
type recordingP2P struct {
	mu   sync.Mutex
	envs [][]byte
}

func (p *recordingP2P) Name() string                           { return "p2p-recording" }
func (p *recordingP2P) Reachable(context.Context, string) bool { return true }
func (p *recordingP2P) Send(_ context.Context, _ string, env []byte) ([]byte, bool, error) {
	p.mu.Lock()
	p.envs = append(p.envs, bytes.Clone(env))
	p.mu.Unlock()
	return nil, true, errors.New("p2p: no answer")
}

func (p *recordingP2P) digests(t testing.TB) [][32]byte {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out [][32]byte
	for _, e := range p.envs {
		d, err := seal.EnvDigest(e)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// E-T24 (DLV T4's forged done, M35): a hub that answers done with a token
// of its own making confirms nothing: the token does not verify, it is
// counted, and the message is sent again.
func TestET24_AForgedDoneConfirmsNothing(t *testing.T) {
	srv, req, prov := registeredPair(t)
	id, err := req.Delegate(context.Background(), prov.AID(), "a forged done", nil)
	if err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) {
		h.statusHook = func(hubapi.RelayStatusItem) (hubapi.RelayStatusResult, bool) {
			junk := make([]byte, seal.DAckLen)
			_, _ = rand.Read(junk)
			return hubapi.RelayStatusResult{State: hubapi.RelayStatusDone, DAck: hubapi.B64(junk)}, true
		}
	})
	invalid := counter(req, cntDAckInvalid)
	askHubNow(t, req)
	rows := outboxRows(t, req, id)
	if len(rows) != 1 {
		t.Fatalf("a forged token confirmed the delegation: %+v", rows)
	}
	if n := counter(req, cntDAckInvalid) - invalid; n != 1 {
		t.Fatalf("dack-invalid +%d, want 1", n)
	}
	log := &sentLog{}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	if err := retryNow(t, req, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if n := len(log.digests(t)); n != 1 {
		t.Fatalf("sent %d times again, want once", n)
	}
}

// E-T25 (ENG-P1, M36): a hub that answers 429 strangers_busy leaves the row
// waiting with that code: anet.delivery_wait is strangers_busy. (The
// sentence on each surface is RC-5's projection.)
func TestET25_AHubWaitIsTheRowsWait(t *testing.T) {
	srv, req, prov := registeredPair(t)
	setFake(t, srv.URL, func(h *fakeHub) {
		h.sendHook = func([]byte) fakeSendAnswer {
			return fakeSendAnswer{kind: fakeSendRefuse, status: 429, code: "strangers_busy", retryAfter: "30"}
		}
	})
	id, err := req.Delegate(context.Background(), prov.AID(), "busy", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, req, id)
	if len(rows) != 1 || lastErrorCode(rows[0].LastError) != "strangers_busy" {
		t.Fatalf("row %+v, want last_error strangers_busy", rows)
	}
	if w := rowWait(t, req, rows[0].ID); w != "strangers_busy" {
		t.Fatalf("anet.delivery_wait %q, want strangers_busy", w)
	}
}

// E-T31 (ENG-C4, M23): every way the engine ends a task — outcome 2, an
// abandonment, a withdrawal, a close — goes through the unified terminal
// write exactly once, with its Cause.
func TestET31_EveryEndGoesThroughTheTerminalWrite(t *testing.T) {
	var mu sync.Mutex
	calls := map[string][]interactions.Cause{}
	restore := interactions.SetTerminalWriteHook(func(ix string, cause interactions.Cause) {
		mu.Lock()
		calls[ix] = append(calls[ix], cause)
		mu.Unlock()
	})
	defer restore()
	once := func(t *testing.T, what, ix string) {
		t.Helper()
		mu.Lock()
		got := calls[ix]
		mu.Unlock()
		if len(got) != 1 || got[0] == "" {
			t.Fatalf("%s: the terminal write ran %d times (%v), want once with a cause", what, len(got), got)
		}
	}
	srv, req, prov := registeredPair(t)
	ctx := context.Background()

	// Outcome 2: the provider's token says refused.
	refused, err := req.Delegate(ctx, prov.AID(), "refused", nil)
	if err != nil {
		t.Fatal(err)
	}
	row := outboxRows(t, req, refused)[0]
	if err := req.confirmOutbox(row.ID, seal.OutcomeRefused, seal.Note{Reason: "provider_policy"}, cntDAckConfirmedHub); err != nil {
		t.Fatal(err)
	}
	once(t, "outcome 2", refused)

	// An abandonment at the deadline.
	late, err := req.Delegate(ctx, prov.AID(), "never confirmed", nil)
	if err != nil {
		t.Fatal(err)
	}
	holdGateOpen(req)
	now := req.nowMS()
	req.setClock(func() uint64 { return now + messageLifetimeMS + 60_000 })
	_ = retryNow(t, req, outboxRows(t, req, late)[0].ID)
	req.setClock(nil)
	once(t, "abandonment", late)

	// A withdrawal: the delegation never left.
	hubURL := req.config().HubURL
	setHubURL(req, deadHubURL)
	withdrawn, err := req.Delegate(ctx, prov.AID(), "withdrawn", nil)
	if err != nil {
		t.Fatal(err)
	}
	setHubURL(req, hubURL)
	if _, err := req.CancelTask(ctx, withdrawn); err != nil {
		t.Fatal(err)
	}
	once(t, "withdrawal", withdrawn)

	// A close: every open task.
	open, err := req.Delegate(ctx, prov.AID(), "open at the close", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.ix.Update(func(tx *interactions.Tx) error {
		_, err := req.closeInteractionsTx(tx, closeKeyCompromise)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	once(t, "close", open)
	_ = srv
}

// E-T32 (ENG-P1, M42): while the gate is closed, a row that has to be
// handed over waits on the gate's code, rotation_pending or notice_stale,
// whatever its last_error says; a row the hub holds waits on nothing. Open,
// the row waits on its last_error's code again.
func TestET32_TheGateIsTheWaitWhileItIsClosed(t *testing.T) {
	queued := VerView{Ver: 1, HandedAt: 5, RID: []byte("rid"), HubState: interactions.OutboxQueued}
	unknown := VerView{Ver: 1, HandedAt: 5, RID: []byte("rid"), HubState: interactions.OutboxUnknown}
	handed := func(lastErr string) RowView {
		r := RowView{}
		r.CurVer, r.HandedAt, r.LastError = 1, 5, lastErr
		return r
	}
	busy := handed(lastErrorOf("strangers_busy"))
	open := GateState{true, IdentityOK, reasonIdentityOK}
	for _, c := range []struct {
		gate GateState
		code string
	}{
		{GateState{false, IdentityPublishing, reasonRotationPending}, "rotation_pending"},
		{GateState{false, IdentityNoticeStale, reasonNoticeStale}, "notice_stale"},
	} {
		if w := deliveryWait(RowView{}, nil, c.gate); w != c.code {
			t.Errorf("%s: a row never handed over waits on %q", c.code, w)
		}
		if w := deliveryWait(handed(""), []VerView{unknown}, c.gate); w != c.code {
			t.Errorf("%s: a row the hub lost waits on %q", c.code, w)
		}
		if w := deliveryWait(handed(""), []VerView{queued}, c.gate); w != "" {
			t.Errorf("%s: a row the hub holds waits on %q", c.code, w)
		}
		if w := deliveryWait(busy, nil, c.gate); w != c.code {
			t.Errorf("%s: a busy row waits on %q while the gate is closed", c.code, w)
		}
	}
	if w := deliveryWait(busy, nil, open); w != "strangers_busy" {
		t.Errorf("open: a busy row waits on %q", w)
	}
}

// E-T35 (ENG-K16, M44): entering stopped ends every task in flight with
// identity_stopped, queues no notice, deletes every interaction key and
// keeps the epoch keys; a crash before the close's transaction is undone
// at the next start, before anything else is sent.
func TestET35_AStopClosesEverythingAndTellsNobody(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	a, err := req.Delegate(ctx, prov.AID(), "in flight", nil)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := req.Delegate(ctx, prov.AID(), "canceled before the stop", nil)
	if err != nil {
		t.Fatal(err)
	}
	hubURL := req.config().HubURL
	setHubURL(req, deadHubURL)
	b, err := req.Delegate(ctx, prov.AID(), "never left", nil)
	if err != nil {
		t.Fatal(err)
	}
	setHubURL(req, hubURL)
	// Each task holds an interaction key (FS-X1).
	for _, id := range []string{a, b, ended} {
		k, err := seal.GenerateIXKey(seal.SuiteX25519, id, prov.AID())
		if err != nil {
			t.Fatal(err)
		}
		if err := req.ix.Update(func(tx *interactions.Tx) error {
			return tx.PutIXKey(interactions.IXKey{IX: id, PeerAID: prov.AID(), Suite: k.Suite, Pub: k.Pub, Priv: k.Private})
		}); err != nil {
			t.Fatal(err)
		}
		if rk, err := req.ix.OwnRK(id); err != nil || rk == nil {
			t.Fatalf("setup: no interaction key for %s (%v)", id, err)
		}
	}
	// One ended before the stop: its key waits on close_after (FS-X7c).
	if _, err := req.CancelTask(ctx, ended); err != nil {
		t.Fatal(err)
	}
	if rk, err := req.ix.OwnRK(ended); err != nil || rk == nil {
		t.Fatalf("setup: the ended task's key went at once (%v)", err)
	}
	epochKeys := len(req.enc.SignedSet())
	// The identity is stopped (another device rotated it, KEL D-29); the
	// process stops before its close's transaction.
	if err := req.self.SetState(identity.StateStopped); err != nil {
		t.Fatal(err)
	}
	if err := saveIdentity(req.layout, req.self); err != nil {
		t.Fatal(err)
	}
	sends := relayCountFor(srv.URL)
	req = restartDaemon(t, req)
	for _, id := range []string{a, b} {
		st, _, reason := taskViewMap(t, req, id)
		if st != string(a2ashape.TaskStateFailed) || reason != string(closeStopped) {
			t.Fatalf("%s after the start: %s %s, want failed identity_stopped", id, st, reason)
		}
		if rows := outboxRows(t, req, id); len(rows) != 0 {
			t.Fatalf("%s: rows queued after the stop: %+v", id, rows)
		}
		if rk, err := req.ix.OwnRK(id); err != nil || rk != nil {
			t.Fatalf("%s: its interaction key is kept (%v)", id, err)
		}
	}
	if rk, err := req.ix.OwnRK(ended); err != nil || rk != nil {
		t.Fatalf("the key of a task that ended before the stop is kept (%v)", err)
	}
	if n := allOutboxRows(t, req); len(n) != 0 {
		t.Fatalf("notices queued: %+v", n)
	}
	if relayCountFor(srv.URL) != sends {
		t.Fatal("something was sent for a stopped identity")
	}
	if len(req.enc.SignedSet()) == 0 || epochKeys == 0 {
		t.Fatal("the epoch keys went with the stop")
	}
}

// ENG-O6 and A12: a cancel freezes the delegation while its attempt is at
// the hub, and the hub answers 409 kid_stale. The attempt does not seal
// again for a frozen row, and it still ends as every attempt does: what it
// handed over is recorded and attempting is cleared at once, so the
// cancel behind the delegation is free to go and nothing waits for a
// later attempt to find the mark.
func TestAFreezeBetweenTwoSendsOfAnAttemptRecordsWhatLeft(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	// Queued while the hub cannot be reached: nothing of it left.
	hubURL := req.config().HubURL
	setHubURL(req, deadHubURL)
	id, err := req.Delegate(ctx, prov.AID(), "a task", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, req, id)
	if len(rows) != 1 || rows[0].HandedAt != 0 {
		t.Fatalf("setup: outbox %+v, want the delegation never handed over", rows)
	}
	setHubURL(req, hubURL)
	var first atomic.Bool
	setFake(t, srv.URL, func(h *fakeHub) {
		h.sendHook = func([]byte) fakeSendAnswer {
			if !first.CompareAndSwap(false, true) {
				return fakeSendAnswer{}
			}
			// The requester cancels while the hub holds the request: the
			// attempt holds the row, so the cancel freezes it.
			if _, err := req.CancelTask(ctx, id); err != nil {
				t.Errorf("cancel: %v", err)
			}
			return fakeSendAnswer{kind: fakeSendRefuse, status: 409, code: codeKidStale}
		}
	})
	_ = retryNow(t, req, rows[0].ID)
	if !first.Load() {
		t.Fatal("the delegation never reached the hub")
	}
	rows = outboxRows(t, req, id)
	if len(rows) != 2 || rows[0].Type != seal.TypeDelegate || !rows[0].Frozen || rows[1].Kind != rowKindCancel {
		t.Fatalf("outbox %+v, want the delegation frozen and the cancel behind it", rows)
	}
	if rows[0].Attempting || rows[0].HandedAt == 0 {
		t.Fatalf("the attempt ended with the delegation attempting %v, handed at %d: what it handed over is not recorded",
			rows[0].Attempting, rows[0].HandedAt)
	}
	if vers := rowVersions(t, req, rows[0].ID); len(vers) != 1 || vers[0].HandedAt == 0 {
		t.Fatalf("versions %+v, want the one the hub was handed, recorded so", vers)
	}
}

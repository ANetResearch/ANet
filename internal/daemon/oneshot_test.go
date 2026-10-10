package daemon

// The one-shot sender (engine §2.12, §2.11.5): what each of the three
// envelopes is sealed as, that none of them is queued or earns a token,
// the one resend of a busy card request, why one did not go, and that a
// round's answer ends its previews.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// testCoalescer stands in for the preview coalescer (preview.go): it sends
// the frames a test gives it through the one-shot sender, and records the
// rounds the engine cancels. The rest of previewCoalescer is what
// previewsUnavailable does.
type testCoalescer struct {
	previewsUnavailable
	d        *Daemon
	mu       sync.Mutex
	seq      uint64
	canceled []string
}

func (c *testCoalescer) CancelRound(ix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.canceled = append(c.canceled, ix)
}

func (c *testCoalescer) cancels() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.canceled...)
}

// frame sends one preview frame of text on task ix to `to`.
func (c *testCoalescer) frame(t testing.TB, to, ix, text string) OneShotResult {
	t.Helper()
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.mu.Unlock()
	body, err := (&delegation.ChatMsg{Kind: delegation.ChatStreamPreview, Body: text, StreamSeq: seq,
		StreamAtMS: int64(c.d.nowMS()), MsgID: "preview-" + hex.EncodeToString([]byte{byte(seq)}), Answers: "msg-1"}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return c.d.oneShotSend(context.Background(), oneShotPreview, to, ix, body)
}

// cardIX is a fresh card exchange id, card-<32 hex>.
func cardIX(t testing.TB) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "card-" + hex.EncodeToString(b)
}

// previewTask is a task req delegated to prov that prov took, with both
// sides' interaction keys as FS-7's wiring makes them: req made its key
// with the task, and prov made its own when it took the delegation and
// recorded req's rk from it. It returns the task and req's key, read back
// from req's store.
func previewTask(t *testing.T, req, prov *Daemon) (string, *seal.IXKey) {
	t.Helper()
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "a task with previews", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ix, err := prov.ix.Get(id)
	if err != nil {
		t.Fatalf("setup: the provider has no task: %v", err)
	}
	if own, err := prov.ix.OwnRK(id); err != nil || own == nil || len(ix.PeerRK) == 0 {
		t.Fatalf("setup: the provider holds key %v (%v) and the requester's rk %x", own, err, ix.PeerRK)
	}
	return id, ownIXKey(t, req, id)
}

// ownIXKey is d's own interaction key for task ix, as its store holds it.
func ownIXKey(t testing.TB, d *Daemon, ix string) *seal.IXKey {
	t.Helper()
	rk, err := d.ix.OwnRK(ix)
	if err != nil || rk == nil {
		t.Fatalf("%s holds no key for %s (%v)", d.AID(), ix, err)
	}
	k, err := d.ix.IXKeyByKID(seal.KID(rk.Suite, rk.Pub))
	if err != nil || k == nil {
		t.Fatalf("%s: read the key of %s back: %v", d.AID(), ix, err)
	}
	return &seal.IXKey{IX: k.IX, PeerAID: k.PeerAID, Suite: k.Suite, Pub: k.Pub, Private: k.Priv}
}

// outboxCounts is how many rows and versions d's outbox holds.
func outboxCounts(t testing.TB, d *Daemon) (rows, vers int) {
	t.Helper()
	for _, r := range allOutboxRows(t, d) {
		rows++
		vers += len(rowVersions(t, d, r.ID))
	}
	return rows, vers
}

// outerExp is outer key 8 of env, 0 when absent.
func outerExp(t testing.TB, env []byte) uint64 {
	t.Helper()
	o, err := seal.ParseOuter(env)
	if err != nil {
		t.Fatal(err)
	}
	if o.Exp == nil {
		return 0
	}
	return *o.Exp
}

// E-T30 (ENG-O8 to O10, ENG-K18, K19; ENG-M7): a preview frame (from a test
// coalescer), an anet.card.get/1 and an anet.card/1 each go once through
// the one-shot sender, sealed as engine ENG-O9 says, and none of them adds
// an outbox row or a version, so no ack_key of theirs is kept.
func TestET30_OneShotsAreNotQueued(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	id, reqKey := previewTask(t, req, prov)
	co := &testCoalescer{d: prov}
	prov.setPreviewer(co)
	log := &sentLog{}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	reqRows, reqVers := outboxCounts(t, req)
	provRows, provVers := outboxCounts(t, prov)

	// The preview frame: two layers, this node's rk, no KEL, notice, key
	// set or grant; outer exp a minute on.
	if res := co.frame(t, req.AID(), id, "the first words"); !res.Handed || res.HubReason != "" {
		t.Fatalf("the preview frame: %+v", res)
	}
	env := log.last(t)
	op, err := seal.Open(env, req.AID(), req.enc, oneIXKey{reqKey})
	if err != nil {
		t.Fatal(err)
	}
	in := op.Inner
	own, _ := prov.ix.OwnRK(id)
	if op.Layers != 2 || op.RK == nil || !bytes.Equal(op.RK.Pub, own.Pub) || in.KEL != nil || len(in.Notices) != 0 ||
		in.Keys != nil || in.Grant != nil || in.Type != seal.TypeMessage || in.IX != id {
		t.Fatalf("the preview frame is sealed as %d layers, rk %v, kel %d bytes, %d notices, keys %d bytes, grant %d bytes, type %s",
			op.Layers, op.RK, len(in.KEL), len(in.Notices), len(in.Keys), len(in.Grant), in.Type)
	}
	if outerExp(t, env) != in.Exp || in.Exp != in.TS+previewLifeMS {
		t.Fatalf("outer exp %d, inner ts %d exp %d; want both exps a minute on", outerExp(t, env), in.TS, in.Exp)
	}

	// The card request: one layer, KEL, notice and key set, the reply
	// grant for the card exchange; outer exp ten minutes on.
	cix := cardIX(t)
	body, _ := delegation.CardGet{}.Marshal()
	if res := req.oneShotSend(ctx, oneShotCardGet, prov.AID(), cix, body); !res.Handed || res.HubReason != "" {
		t.Fatalf("the card request: %+v", res)
	}
	env = log.last(t)
	op = openAt(t, prov, env)
	in = op.Inner
	if op.Layers != 1 || op.RK != nil || in.KEL == nil || len(in.Notices) != 1 || in.Keys == nil || in.Grant == nil ||
		in.Type != delegation.TypeCardGet || outerExp(t, env) != in.Exp || in.Exp != in.TS+cardLifeMS {
		t.Fatalf("the card request is sealed as %d layers, kel %d bytes, %d notices, grant %d bytes, type %s, exp %d (ts %d, outer %d)",
			op.Layers, len(in.KEL), len(in.Notices), len(in.Grant), in.Type, in.Exp, in.TS, outerExp(t, env))
	}
	g, capBody := receivedGrant(t, req, in.Grant, cix, prov.nowMS())
	if g.IX == nil || *g.IX != cix || capBody.Class != seal.CapClassContact {
		t.Fatalf("the reply grant: %+v %+v", g, capBody)
	}

	// The card answer goes under that grant alone, sealed to the key set
	// the request carried; no grant of its own.
	held, err := interactions.HeldFromGrant(g, capBody, prov.nowMS())
	if err != nil {
		t.Fatal(err)
	}
	// The one-shot capability a card request brings is kept in memory, not
	// in caps_held (SSR-W8); the answer takes it.
	prov.cardGrants.put(req.AID(), cix, held, prov.nowMS())
	answer, err := (&delegation.CardResp{Error: delegation.CardNotConfigured}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if res := prov.oneShotSend(ctx, oneShotCard, req.AID(), cix, answer, oneShotSealTo(keySetOf(t, req))); !res.Handed {
		t.Fatalf("the card answer: %+v", res)
	}
	env = log.last(t)
	op = openAt(t, req, env)
	in = op.Inner
	o, _ := seal.ParseOuter(env)
	if op.Layers != 1 || in.Grant != nil || in.KEL == nil || in.Type != delegation.TypeCard || o.Access.Kind != seal.AccessKindCap ||
		!bytes.Equal(o.Access.Cap.Body, mustCapBody(t, held)) || in.Exp != in.TS+cardLifeMS || outerExp(t, env) != in.Exp {
		t.Fatalf("the card answer is sealed as %d layers, grant %d bytes, type %s, access %+v", op.Layers, len(in.Grant), in.Type, o.Access)
	}

	if log.count() != 3 {
		t.Fatalf("%d envelopes reached the hub, want one of each", log.count())
	}
	if r, v := outboxCounts(t, req); r != reqRows || v != reqVers {
		t.Fatalf("the requester's outbox went from %d rows, %d versions to %d, %d", reqRows, reqVers, r, v)
	}
	if r, v := outboxCounts(t, prov); r != provRows || v != provVers {
		t.Fatalf("the provider's outbox went from %d rows, %d versions to %d, %d", provRows, provVers, r, v)
	}
}

// mustCapBody is the signed capability body held carries.
func mustCapBody(t testing.TB, held interactions.HeldCap) []byte {
	t.Helper()
	c, err := seal.UnmarshalCap(held.Cap)
	if err != nil {
		t.Fatal(err)
	}
	return c.Body
}

// E-T33 (ENG-O8a, ENG-O8b; ENG-M43): a card request the hub turns away as
// busy goes once more, the same bytes, when the hub's wait fits in the
// card's; a wait that does not fit is not waited for, and the caller learns
// the hub's code; a preview frame is never sent again.
func TestET33_ACardRequestTheHubFindsBusy(t *testing.T) {
	busyOnce := func(retryAfter string) *sentLog {
		return &sentLog{next: func(n int, _ []byte) fakeSendAnswer {
			if n == 1 {
				return fakeSendAnswer{kind: fakeSendRefuse, status: 429, code: "strangers_busy", retryAfter: retryAfter}
			}
			return fakeSendAnswer{}
		}}
	}
	cardGet := func(t *testing.T, req, prov *Daemon) OneShotResult {
		body, _ := delegation.CardGet{}.Marshal()
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cardWaitMS)*time.Millisecond)
		defer cancel()
		return req.oneShotSend(ctx, oneShotCardGet, prov.AID(), cardIX(t), body)
	}
	t.Run("a wait that fits: once more, the same bytes", func(t *testing.T) {
		srv, req, prov := quietPair(t)
		log := busyOnce("2")
		setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
		start := time.Now()
		res := cardGet(t, req, prov)
		took := time.Since(start)
		log.mu.Lock()
		envs := append([][]byte(nil), log.envs...)
		log.mu.Unlock()
		if !res.Handed || res.HubReason != "" || len(envs) != 2 || !bytes.Equal(envs[0], envs[1]) {
			t.Fatalf("%+v after %d sends (same bytes: %v)", res, len(envs), len(envs) == 2 && bytes.Equal(envs[0], envs[1]))
		}
		if took < 2*time.Second || took > time.Duration(cardWaitMS)*time.Millisecond {
			t.Fatalf("took %s, want the hub's two seconds and within the card's ten", took)
		}
		if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
			t.Fatalf("the hub holds %d requests, want 1", n)
		}
	})
	t.Run("a wait that does not fit: not waited for", func(t *testing.T) {
		srv, req, prov := quietPair(t)
		log := busyOnce("60")
		setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
		start := time.Now()
		res := cardGet(t, req, prov)
		if !res.Handed || res.HubReason != "strangers_busy" || log.count() != 1 || time.Since(start) > 5*time.Second {
			t.Fatalf("%+v after %d sends in %s", res, log.count(), time.Since(start))
		}
	})
	t.Run("busy twice: once more, and no more", func(t *testing.T) {
		srv, req, prov := quietPair(t)
		log := &sentLog{next: func(int, []byte) fakeSendAnswer {
			return fakeSendAnswer{kind: fakeSendRefuse, status: 429, code: "rate_limited", retryAfter: "1"}
		}}
		setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
		res := cardGet(t, req, prov)
		if !res.Handed || res.HubReason != "rate_limited" || log.count() != 2 {
			t.Fatalf("%+v after %d sends, want two", res, log.count())
		}
	})
	t.Run("a preview frame: never sent again", func(t *testing.T) {
		srv, req, prov := quietPair(t)
		id, _ := previewTask(t, req, prov)
		log := busyOnce("1")
		setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
		res := (&testCoalescer{d: prov}).frame(t, req.AID(), id, "busy")
		if !res.Handed || res.HubReason != "strangers_busy" || log.count() != 1 {
			t.Fatalf("%+v after %d sends", res, log.count())
		}
	})
}

// DLV T26 (DLV-R-1a, ENG-V6, CUX-P19; DLV M22, ENG-M6): previews and card
// requests, by the hub and directly, leave no delivery token anywhere —
// the hub stores none, a direct ack carries none — and what the outbox
// sends never has an outer exp.
func TestDLVT26_OneShotsEarnNoToken(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	id, _ := previewTask(t, req, prov)
	h := fakeHubAt(t, srv.URL)
	tokens := func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.relayDacks)
	}
	confirmDelegation(t, req, prov)
	before := tokens()
	co := &testCoalescer{d: prov}
	body, _ := delegation.CardGet{}.Marshal()
	for i := 0; i < 20; i++ {
		if res := co.frame(t, req.AID(), id, strings.Repeat("words ", i+1)); !res.Handed {
			t.Fatalf("frame %d: %+v", i, res)
		}
		if res := req.oneShotSend(ctx, oneShotCardGet, prov.AID(), cardIX(t), body); !res.Handed {
			t.Fatalf("card request %d: %+v", i, res)
		}
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := tokens() - before; n != 0 {
		t.Fatalf("the hub stores %d delivery tokens for one-shot envelopes", n)
	}
	// Directly: the recipient's ack carries no token.
	direct := &dackRecorder{to: req}
	prov.RegisterTransport(direct)
	heldCapFrom(t, req, prov)
	if res := co.frame(t, req.AID(), id, "direct"); !res.Handed {
		t.Fatalf("a direct frame: %+v", res)
	}
	// The frame goes directly; so may the answers to the card requests
	// above, which the recipient sends in the background (a one-shot too):
	// none of their acks carries a token.
	if direct.sends() < 1 || direct.withToken() != 0 {
		t.Fatalf("%d direct sends, %d acks with a token", direct.sends(), direct.withToken())
	}
	// What the outbox sends never carries outer key 8.
	log := &sentLog{}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	if err := req.SendMessage(ctx, id, "a follow-up", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, id, "an answer", nil); err != nil {
		t.Fatal(err)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.envs) == 0 {
		t.Fatal("nothing queued was sent")
	}
	for _, env := range log.envs {
		if outerExp(t, env) != 0 {
			t.Fatal("a queued message went with an outer exp")
		}
	}
}

// dackRecorder is a direct path to one daemon that records whether its ack
// carried a delivery token.
type dackRecorder struct {
	to     *Daemon
	mu     sync.Mutex
	n, tok int
}

func (p *dackRecorder) Name() string                           { return "p2p-dack-recorder" }
func (p *dackRecorder) Reachable(context.Context, string) bool { return true }
func (p *dackRecorder) Send(ctx context.Context, _ string, env []byte) ([]byte, bool, error) {
	dack, err := p.to.Inbound().Receive(ctx, env)
	p.mu.Lock()
	p.n++
	if len(dack) > 0 {
		p.tok++
	}
	p.mu.Unlock()
	return dack, true, err
}
func (p *dackRecorder) sends() int     { p.mu.Lock(); defer p.mu.Unlock(); return p.n }
func (p *dackRecorder) withToken() int { p.mu.Lock(); defer p.mu.Unlock(); return p.tok }

// ENG-O8, ENG-O8b, ENG-K2, ENG-K13, ENG-K19: a one-shot envelope that
// cannot go is not sent, and says why — the gate's wait, a first contact
// the hub does not take, no credential, no rk, not one of the three — and
// a hub's refusal is not met with a new sealing, kid_stale included.
func TestAOneShotThatCannotGoSaysWhy(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	id, _ := previewTask(t, req, prov)
	log := &sentLog{}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	cardGet, _ := delegation.CardGet{}.Marshal()
	notConfigured, _ := (&delegation.CardResp{Error: delegation.CardNotConfigured}).Marshal()
	for _, c := range []struct {
		name  string
		gate  *GateState
		send  func() OneShotResult
		want  OneShotResult
		count string
	}{
		{"the gate waits for the notice", &GateState{false, IdentityNoticeStale, reasonNoticeStale},
			func() OneShotResult { return req.oneShotSend(ctx, oneShotCardGet, prov.AID(), cardIX(t), cardGet) },
			OneShotResult{HubReason: reasonNoticeStale}, cntOneShotDroppedPrefix + oneShotWhyGate},
		{"the gate waits for a rotation", &GateState{false, IdentityPublishing, reasonRotationPending},
			func() OneShotResult {
				return (&testCoalescer{d: prov}).frame(t, req.AID(), id, "held")
			},
			OneShotResult{HubReason: reasonRotationPending}, cntOneShotDroppedPrefix + oneShotWhyGate},
		{"a card answer without the request's capability", nil,
			func() OneShotResult {
				return prov.oneShotSend(ctx, oneShotCard, req.AID(), cardIX(t), notConfigured, oneShotSealTo(keySetOf(t, req)))
			},
			OneShotResult{}, cntOneShotDroppedPrefix + oneShotWhyAccess},
		{"a preview frame without the peer's rk", nil,
			func() OneShotResult { return (&testCoalescer{d: req}).frame(t, prov.AID(), id, "no rk") },
			OneShotResult{}, cntPreviewNoRK},
		// The peer's rk is on record but this node holds no key of its own on
		// the task: a frame needs this node's rk inside its two layers too
		// (FS-X6, ENG-K19), so none is sent.
		{"a preview frame without this node's own rk", nil,
			func() OneShotResult {
				// The key req made with the task goes, as at its end.
				if err := req.ix.Update(func(tx *interactions.Tx) error { _, err := tx.DeleteAllIXKeys(); return err }); err != nil {
					t.Fatal(err)
				}
				k, err := seal.GenerateIXKey(seal.SuiteX25519, id, req.AID())
				if err != nil {
					t.Fatal(err)
				}
				rk, err := coredet.Marshal(k.RK())
				if err != nil {
					t.Fatal(err)
				}
				if err := req.ix.Update(func(tx *interactions.Tx) error { return tx.RecordPeerRK(id, rk) }); err != nil {
					t.Fatal(err)
				}
				return (&testCoalescer{d: req}).frame(t, prov.AID(), id, "no own rk")
			},
			OneShotResult{}, cntPreviewNoRK},
		{"not one of the three", nil,
			func() OneShotResult {
				return req.oneShotSend(ctx, oneShotCardGet, prov.AID(), cardIX(t), []byte("text"))
			},
			OneShotResult{}, cntOneShotDroppedPrefix + oneShotWhyBody},
	} {
		if c.gate != nil {
			g := *c.gate
			req.setIdentityGate(func() GateState { return g })
			prov.setIdentityGate(func() GateState { return g })
		}
		before, sent := counter(req, c.count)+counter(prov, c.count), log.count()
		if got := c.send(); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
		if n := counter(req, c.count) + counter(prov, c.count) - before; n != 1 {
			t.Errorf("%s: %s counted %d times", c.name, c.count, n)
		}
		if log.count() != sent {
			t.Errorf("%s: %d envelopes reached the hub", c.name, log.count()-sent)
		}
		req.setIdentityGate(nil)
		prov.setIdentityGate(nil)
	}
	// A first contact the hub takes no session for.
	setFake(t, srv.URL, func(h *fakeHub) { h.sessionRefuse = "admission_required" })
	stranger := registered(t, srv.URL, "stranger")
	setFake(t, srv.URL, func(h *fakeHub) { h.sessionRefuse = "admission_required" })
	if got := stranger.oneShotSend(ctx, oneShotCardGet, prov.AID(), cardIX(t), cardGet); got.Handed || got.HubReason != codeSessionUnavailable {
		t.Errorf("a first contact without a session: %+v", got)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.sessionRefuse = "" })
	// kid_stale: counted, not sealed again.
	setFake(t, srv.URL, func(h *fakeHub) {
		h.sendHook = func([]byte) fakeSendAnswer {
			return fakeSendAnswer{kind: fakeSendRefuse, status: 409, code: codeKidStale}
		}
	})
	before := counter(req, cntOneShotDroppedPrefix+codeKidStale)
	if got := req.oneShotSend(ctx, oneShotCardGet, prov.AID(), cardIX(t), cardGet); !got.Handed || got.HubReason != codeKidStale {
		t.Errorf("kid_stale: %+v", got)
	}
	if n := counter(req, cntOneShotDroppedPrefix+codeKidStale) - before; n != 1 {
		t.Errorf("kid_stale counted %d times", n)
	}
}

// ENG-K20 (CUX-P8): queuing a round's real answer — a conversation message,
// a result, or a status that ends the task — cancels the task's previews in
// the same call; a status that leaves it running does not.
func TestTheRoundsAnswerCancelsItsPreviews(t *testing.T) {
	_, req, prov := quietPair(t)
	ctx := context.Background()
	co := &testCoalescer{d: prov}
	prov.setPreviewer(co)
	id, err := req.Delegate(ctx, prov.AID(), "a task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	working, _ := noticeBody(string(interactions.StateWorking), "working", "", prov.nowMS())
	if endsPreviewRound(wireSend{typ: seal.TypeStatus, body: working}) {
		t.Fatal("a working status ends the preview round")
	}
	failed, _ := noticeBody(string(interactions.StateFailed), "failed", "", prov.nowMS())
	if !endsPreviewRound(wireSend{typ: seal.TypeStatus, body: failed}) {
		t.Fatal("a failed status does not end the preview round")
	}
	if n := len(co.cancels()); n != 0 {
		t.Fatalf("the round was canceled %d times before any answer", n)
	}
	if err := prov.SendMessage(ctx, id, "an answer", nil); err != nil {
		t.Fatal(err)
	}
	if c := co.cancels(); len(c) != 1 || c[0] != id {
		t.Fatalf("after the answer: canceled %v", c)
	}
	if err := prov.CompleteTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if c := co.cancels(); len(c) != 2 || c[1] != id {
		t.Fatalf("after the result: canceled %v", c)
	}
}

// ENG-K13 within a one-shot send (ENG-M12): the gate is read again right
// before the envelope is handed to the hub. A gate that closes while the
// send opens its first-contact session holds it back: nothing reaches the
// hub, and the caller learns the gate's wait.
func TestAGateThatClosesMidSendHoldsTheOneShot(t *testing.T) {
	srv, req, prov := quietPair(t)
	closed := GateState{false, IdentityPublishing, reasonRotationPending}
	var hooked atomic.Bool
	setFake(t, srv.URL, func(h *fakeHub) {
		h.sessionHook = func() {
			if hooked.CompareAndSwap(false, true) {
				req.setIdentityGate(func() GateState { return closed })
			}
		}
	})
	sends := relayCountFor(srv.URL)
	body, _ := delegation.CardGet{}.Marshal()
	res := req.oneShotSend(context.Background(), oneShotCardGet, prov.AID(), cardIX(t), body)
	if !hooked.Load() {
		t.Fatal("setup: the send opened no first-contact session")
	}
	if res.Handed || res.HubReason != reasonRotationPending || relayCountFor(srv.URL) != sends {
		t.Fatalf("%+v, %d sends reached the hub after the gate closed", res, relayCountFor(srv.URL)-sends)
	}
}

// TestOnlyOneShotSealsWithExp (engine ENG-I1, ENG-O8; ENG-M6): outer key 8
// is set in internal/daemon/oneshot.go alone. No other file of the module
// writes a seal.SealOpts with Exp, or sets the Exp of one it holds.
func TestOnlyOneShotSealsWithExp(t *testing.T) {
	const root = "../.."
	fset := token.NewFileSet()
	isSealOpts := func(e ast.Expr) bool {
		s, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		x, ok := s.X.(*ast.Ident)
		return ok && x.Name == "seal" && s.Sel.Name == "SealOpts"
	}
	seen, inOneShot := 0, 0
	err := filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if name := e.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			return err
		}
		seen++
		oneShot := rel == "internal/daemon/oneshot.go"
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if !isSealOpts(n.Type) {
					return true
				}
				for _, el := range n.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Exp" {
							if oneShot {
								inOneShot++
							} else {
								t.Errorf("%s: a seal.SealOpts with Exp outside the one-shot sender", fset.Position(n.Pos()))
							}
						}
					}
				}
			case *ast.FuncDecl:
				// The SealOpts a function holds — its parameters and
				// variables of that type — get no Exp set outside it.
				held := map[string]bool{}
				for _, fl := range []*ast.FieldList{n.Type.Params, n.Recv} {
					if fl == nil {
						continue
					}
					for _, p := range fl.List {
						if isSealOpts(p.Type) {
							for _, name := range p.Names {
								held[name.Name] = true
							}
						}
					}
				}
				if n.Body == nil {
					return true
				}
				ast.Inspect(n.Body, func(m ast.Node) bool {
					switch m := m.(type) {
					case *ast.ValueSpec:
						if m.Type != nil && isSealOpts(m.Type) {
							for _, name := range m.Names {
								held[name.Name] = true
							}
						}
					case *ast.AssignStmt:
						for _, lhs := range m.Lhs {
							s, ok := lhs.(*ast.SelectorExpr)
							if !ok || s.Sel.Name != "Exp" {
								continue
							}
							if x, ok := s.X.(*ast.Ident); ok && held[x.Name] && !oneShot {
								t.Errorf("%s: the Exp of a seal.SealOpts set outside the one-shot sender", fset.Position(m.Pos()))
							}
						}
					}
					return true
				})
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 200 || inOneShot != 1 {
		t.Fatalf("read %d files, %d one-shot sealings with Exp: the scan is not looking where it should", seen, inOneShot)
	}
}

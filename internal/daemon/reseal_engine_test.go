package daemon

// The reseal engine against a fake hub and a direct path (engine §3, §8.3):
// each trigger makes a new version before the next hand-over, and only then;
// every version is the same message; a version is sealed under the task's
// interaction keys of its moment; nothing is sealed while the gate is
// closed.

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// storeIXLookup opens second layers with the interaction keys a node's store
// holds (FS-R1), as the receive path does once FS-7 wires it.
type storeIXLookup struct{ s *interactions.Store }

func (l storeIXLookup) IXKey(ikid []byte) (*seal.IXKey, error) {
	k, err := l.s.IXKeyByKID(ikid)
	if err != nil || k == nil {
		return nil, err
	}
	return &seal.IXKey{IX: k.IX, PeerAID: k.PeerAID, Suite: k.Suite, Pub: k.Pub, Private: k.Priv}, nil
}

// openAt opens env as `to` would at steps 1 to 4, second layer included.
func openAt(t testing.TB, to *Daemon, env []byte) *seal.Opened {
	t.Helper()
	op, err := seal.Open(env, to.AID(), to.enc, storeIXLookup{to.ix})
	if err != nil {
		t.Fatalf("%s cannot open the envelope: %v", to.AID(), err)
	}
	return op
}

// envKID is the kid outer key 4 of env names.
func envKID(t testing.TB, env []byte) []byte {
	t.Helper()
	o, err := seal.ParseOuter(env)
	if err != nil {
		t.Fatal(err)
	}
	return o.KID
}

// lostByTheHub makes the hub take what it is sent and keep nothing, and
// returns the log of what it was sent.
func lostByTheHub(t *testing.T, url string) *sentLog {
	t.Helper()
	log := &sentLog{next: func(int, []byte) fakeSendAnswer { return fakeSendAnswer{kind: fakeSendAccept} }}
	setFake(t, url, func(h *fakeHub) { h.sendHook = log.hook })
	return log
}

// lastSent is the last envelope log holds.
func (s *sentLog) last(t testing.TB) []byte {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.envs) == 0 {
		t.Fatal("nothing was sent")
	}
	return s.envs[len(s.envs)-1]
}

func (s *sentLog) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.envs)
}

// onlyRow is d's one row of task id.
func onlyRow(t testing.TB, d *Daemon, id string) interactions.OutboxRow {
	t.Helper()
	rows := outboxRows(t, d, id)
	if len(rows) != 1 {
		t.Fatalf("task %s has %d queued rows, want 1", id, len(rows))
	}
	return rows[0]
}

// T1 (FS-O1, SI-29; FS mutation 8, ENG-M8): a version the hub lost is not
// sent again once its epoch key is past its delivery deadline; a new
// version sealed to a key valid now goes in its place, and the recipient
// opens it.
func TestT1_AVersionPastItsKeysDeadlineIsSealedAgainFirst(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	req.setOutboxHold(true)
	log := lostByTheHub(t, srv.URL)
	id, err := req.Delegate(ctx, prov.AID(), "a task the hub loses", nil)
	if err != nil {
		t.Fatal(err)
	}
	v1 := onlyRow(t, req, id)
	if v1.CurVer != 1 || v1.HandedAt == 0 || log.count() != 1 {
		t.Fatalf("setup: %+v, %d sent", v1, log.count())
	}
	// The key's deadline passes; the node's key state and notice stay as
	// they were (the gate held open past the notice's age).
	holdGateOpen(req)
	clk := newSharedClock(v1.SealedKidNA + seal.DeliveryDeadlineMS + 1)
	req.setClock(clk.fn())
	askHubNow(t, req) // the hub lost it: unknown
	if err := retryNow(t, req, v1.ID); err != nil {
		t.Fatal(err)
	}
	v2 := onlyRow(t, req, id)
	sent := log.last(t)
	if log.count() != 2 || v2.CurVer != 2 || v2.SealedKidNA <= v1.SealedKidNA || bytes.Equal(envKID(t, sent), v1.SealedKID) {
		t.Fatalf("after the key's deadline: %d sent, row %+v; want a second version under another key", log.count(), v2)
	}
	if !bytes.Equal(envKID(t, sent), v2.SealedKID) || clk.now() > v2.SealedKidNA+seal.DeliveryDeadlineMS {
		t.Fatalf("what went is not sealed to a key within its deadline: %+v", v2)
	}
	if v2.SealedKSS != v1.SealedKSS || v2.SealedNoticeAt != v1.SealedNoticeAt {
		t.Fatalf("the key state or the notice changed without a trigger: %+v, before %+v", v2, v1)
	}
	if op := openAt(t, prov, sent); !bytes.Equal(op.Inner.MID, v1.MID) {
		t.Fatal("the new version is another message")
	}
}

// T3 (KEL D-51(a), KEL M23, ENG-M9): after a planned rotation nothing goes
// out under the old key: a version the hub lost is sealed again, signed
// under the new key state and carrying the KEL that ends at it.
func TestT3_ARotationSealsTheQueuedMessageAgain(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	req.setOutboxHold(true)
	log := lostByTheHub(t, srv.URL)
	id, err := req.Delegate(ctx, prov.AID(), "a task across a rotation", nil)
	if err != nil {
		t.Fatal(err)
	}
	v1 := onlyRow(t, req, id)
	if err := identitytest.Rotate(req.self, req.nowMS()); err != nil {
		t.Fatal(err)
	}
	// The hub learns the rotation and confirms the new tip.
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil); err != nil {
		t.Fatal(err)
	}
	seq := req.self.CurrentSeq()
	if seq == v1.SealedKSS || req.tipNotice(req.self.Snapshot()) == nil {
		t.Fatalf("setup: key state %d (version at %d), notice %v", seq, v1.SealedKSS, req.tipNotice(req.self.Snapshot()))
	}
	askHubNow(t, req)
	if err := retryNow(t, req, v1.ID); err != nil {
		t.Fatal(err)
	}
	v2 := onlyRow(t, req, id)
	if v2.CurVer != 2 || v2.SealedKSS != seq {
		t.Fatalf("after the rotation: %+v, want a second version under key state %d", v2, seq)
	}
	op := openAt(t, prov, log.last(t))
	if op.Inner.KeyStateSeq != seq || len(op.KEL) != len(req.self.KEL()) {
		t.Fatalf("sent under key state %d with %d KEL events, want %d and %d", op.Inner.KeyStateSeq, len(op.KEL), seq, len(req.self.KEL()))
	}
}

// E-T10 (KEL D-51(b), ENG-K17; ENG-M10, ENG-M46): the notice a version
// carries is replaced before it is handed over once it is over three hours
// old and a newer one is held; while the witness cannot be reached and
// there is none newer, the version goes as it is, until the stop line
// closes the gate.
func TestET10_TheNoticeAgeTrigger(t *testing.T) {
	// setup: a delegation sealed while the hub could not be reached, never
	// handed over, and three hours ten minutes on.
	setup := func(t *testing.T) (*fakeHub, *Daemon, *Daemon, *sharedClock, interactions.OutboxRow, string) {
		srv, req, prov := quietPair(t)
		req.setOutboxHold(true)
		heldCapFrom(t, prov, req) // a direct path needs no session (ENG-A6)
		knowPeer(t, req, prov.AID())
		clk := newSharedClock(uint64(time.Now().UnixMilli()))
		req.setClock(clk.fn())
		setHubURL(req, deadHubURL)
		id, err := req.Delegate(context.Background(), prov.AID(), "sealed, not handed over", nil)
		if err != nil {
			t.Fatal(err)
		}
		v1 := onlyRow(t, req, id)
		if v1.CurVer != 1 || v1.HandedAt != 0 || v1.SealedNoticeAt == 0 {
			t.Fatalf("setup: %+v", v1)
		}
		clk.set(v1.SealedNoticeAt + 3*hourMS + 10*minuteMS)
		h := fakeHubAt(t, srv.URL)
		return h, req, prov, clk, v1, srv.URL
	}
	t.Run("a: a newer notice is put in before the hand-over", func(t *testing.T) {
		h, req, prov, clk, v1, url := setup(t)
		setHubURL(req, url)
		h.mu.Lock()
		h.noticeAt = clk.fn()
		h.mu.Unlock()
		if err := req.pollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		held, _, _ := req.idGate.read()
		if held == nil || held.Body.ObservedAt != clk.now() {
			t.Fatal("setup: the poll brought no newer notice")
		}
		log := &sentLog{}
		setFake(t, url, func(h *fakeHub) { h.sendHook = log.hook })
		if err := retryNow(t, req, v1.ID); err != nil {
			t.Fatal(err)
		}
		v2 := onlyRow(t, req, v1.IX)
		op := openAt(t, prov, log.last(t))
		if v2.CurVer != 2 || v2.SealedNoticeAt != held.Body.ObservedAt || len(op.Notices) != 1 || !bytes.Equal(op.Notices[0], held.Wire) {
			t.Fatalf("the version handed over: %+v, carrying the notice of %d; want the newer one", v2, v2.SealedNoticeAt)
		}
		if vers := rowVersions(t, req, v1.ID); len(vers) != 1 || vers[0].Ver != 2 {
			t.Fatalf("versions %+v: the one never handed over is not replaced", vers)
		}
	})
	t.Run("b: no newer notice, the version goes as it is until the stop line", func(t *testing.T) {
		_, req, prov, clk, v1, _ := setup(t)
		direct := &directTo{to: prov}
		req.RegisterTransport(direct)
		if err := retryNow(t, req, v1.ID); err != nil && !errors.Is(err, errUndeliverable) {
			t.Log(err) // the hub is out of reach: the attempt fails after the direct hand-over
		}
		// The direct hand-over of version 1 as it is was taken and its
		// delivery token confirmed it: no row is left (the v3 receive
		// answers a direct delivery with the token).
		if rows := outboxRows(t, req, v1.IX); direct.sent.Load() != 1 || len(rows) != 0 {
			t.Fatalf("%d direct sends, rows %+v; want version 1 handed over as it is and confirmed", direct.sent.Load(), rows)
		}
		if _, err := prov.ix.Get(v1.IX); err != nil {
			t.Fatalf("the provider did not take the delegation: %v", err)
		}
		// At the stop line the gate closes: nothing more is sealed or sent,
		// and a new message waits on notice_stale.
		clk.set(v1.SealedNoticeAt + NoticeStopLine + 1)
		if g := req.identityGate(); g.Open || g.State != IdentityNoticeStale {
			t.Fatalf("gate %+v at the stop line", g)
		}
		id2, err := req.Delegate(context.Background(), prov.AID(), "after the stop line", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.flushOutboxAt(context.Background(), int64(clk.now())+int64(dayMS))
		second := onlyRow(t, req, id2)
		if direct.sent.Load() != 1 || second.CurVer != 0 || len(outboxRows(t, req, v1.IX)) != 0 {
			t.Fatalf("past the stop line: %d direct sends, new row %+v", direct.sent.Load(), second)
		}
		if w := rowWait(t, req, second.ID); w != reasonNoticeStale {
			t.Fatalf("anet.delivery_wait %q, want notice_stale", w)
		}
	})
}

// E-T11 (ENG-R11, FS-O2; ENG-M11): a recipient's key set the sender keeps
// is out of date; the hub answers kid_stale, the sender fetches the set
// again from the directory, bypassing what it keeps, and the second send
// goes through.
func TestET11_AStaleKidFetchesTheKeysAgain(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	knowPeer(t, req, prov.AID())
	old := keySetOf(t, prov)
	// The provider's keys are made anew and published; the requester still
	// keeps the old set.
	if _, err := prov.regenerateKeys(prov.self.CurrentSeq() + 100); err != nil {
		t.Fatal(err)
	}
	if err := prov.publishKeys(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	isOld := func(kid []byte) bool {
		for _, k := range old.Keys {
			if bytes.Equal(k.KID, kid) {
				return true
			}
		}
		return false
	}
	log := &sentLog{next: func(_ int, env []byte) fakeSendAnswer {
		if o, err := seal.ParseOuter(env); err == nil && isOld(o.KID) {
			return fakeSendAnswer{kind: fakeSendRefuse, status: 409, code: codeKidStale}
		}
		return fakeSendAnswer{}
	}}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	id, err := req.Delegate(ctx, prov.AID(), "to a node whose keys changed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := log.count(); n != 2 || isOld(envKID(t, log.last(t))) {
		t.Fatalf("%d sends, the last under an old key: %v; want the second under a new one", n, n > 0 && isOld(envKID(t, log.last(t))))
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.Get(id); err != nil {
		t.Fatalf("the provider did not take the delegation: %v", err)
	}
}

// E-T20 (SS L12; ENG-M28): a version sealed again within an attempt goes
// under a proof of possession signed for its own bytes, and the hub takes
// it.
func TestET20_AResealedVersionCarriesItsOwnProof(t *testing.T) {
	srv, req, prov := quietPair(t)
	heldCapFrom(t, prov, req)
	log := &sentLog{next: func(n int, _ []byte) fakeSendAnswer {
		if n == 1 {
			return fakeSendAnswer{kind: fakeSendRefuse, status: 409, code: codeKidStale}
		}
		return fakeSendAnswer{}
	}}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	id, err := req.Delegate(context.Background(), prov.AID(), "under a capability", nil)
	if err != nil {
		t.Fatal(err)
	}
	d := log.digests(t)
	if len(d) != 2 || d[0] == d[1] {
		t.Fatalf("%d envelopes reached the hub's checks, want two versions", len(d))
	}
	o, err := seal.ParseOuter(log.last(t))
	if err != nil || o.Access == nil || o.Access.Kind != seal.AccessKindCap {
		t.Fatalf("the second version went under %+v (%v), want the capability", o, err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("the hub holds %d envelopes for the provider, want the second version", n)
	}
	if row := onlyRow(t, req, id); row.CurVer != 2 || row.HandedAt == 0 {
		t.Fatalf("row %+v", row)
	}
}

// E-T26 (ENG-S7; ENG-M38): the answer to a public capability call is
// queued when the call ends; its first version is sealed to the key set
// kept on the ended task, and after a kid_stale the next one to the
// directory's — and it arrives.
func TestET26_AnEndedPublicCallStillSealsItsAnswerAgain(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	if err := prov.Providers().Register(ctx, &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: lampCap}}); err != nil {
		t.Fatal(err)
	}
	id, err := stranger.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	var stale atomic.Bool
	stale.Store(true)
	log := &sentLog{next: func(_ int, env []byte) fakeSendAnswer {
		if o, err := seal.ParseOuter(env); err == nil && o.To == stranger.AID() && stale.CompareAndSwap(true, false) {
			return fakeSendAnswer{kind: fakeSendRefuse, status: 409, code: codeKidStale}
		}
		return fakeSendAnswer{}
	}}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the answer at the hub", func() bool { return len(queuedFor(t, srv, stranger.AID())) == 1 })
	pix, err := prov.ix.Get(id)
	if err != nil || pix.Trust != interactions.TrustPublicCap || !pix.IsTerminal() {
		t.Fatalf("the call: %+v %v", pix, err)
	}
	if stale.Load() || len(outboxRows(t, prov, id)) != 1 || onlyRow(t, prov, id).CurVer != 2 {
		t.Fatalf("the answer was not sealed again after kid_stale: %+v", outboxRows(t, prov, id))
	}
	if err := stranger.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if six, err := stranger.ix.Get(id); err != nil || !six.IsTerminal() {
		t.Fatalf("the caller has no answer: %+v %v", six, err)
	}
}

// DLV T12 (ENG-D6, ENG-R3 S1; ENG-M5): a version the hub holds is not
// sealed again while it holds it, whatever the triggers; once the hub no
// longer knows it a second version goes; the recipient takes the first
// from its mailbox and answers its token, which confirms the row under the
// first version's ack_key.
func TestDLVT12_AVersionTheHubHoldsIsNotSealedAgain(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	req.setOutboxHold(true)
	req.mu.Lock()
	req.cfg.NoResponseAfter = "0"
	req.mu.Unlock()
	log := &sentLog{}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	id, err := req.Delegate(ctx, prov.AID(), "queued at the hub", nil)
	if err != nil {
		t.Fatal(err)
	}
	v1 := onlyRow(t, req, id)
	askHubNow(t, req)
	if vers := rowVersions(t, req, v1.ID); len(vers) != 1 || vers[0].HubState != interactions.OutboxQueued {
		t.Fatalf("setup: versions %+v, want the first queued", vers)
	}
	// Past the key's deadline, the notice over three hours old and a newer
	// one held: T1 and T4 both fire.
	clk := newSharedClock(max(v1.SealedKidNA+seal.DeliveryDeadlineMS, v1.SealedNoticeAt+NoticeResealAge) + minuteMS)
	req.setClock(clk.fn())
	prov.setClock(clk.fn())
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	h.noticeAt = clk.fn()
	h.mu.Unlock()
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	row := onlyRow(t, req, id)
	vv := req.verViews(row.ID, rowVersions(t, req, row.ID), req.configuredHub(), clk.now())
	if !resealNeeded(rowView(&row), vv, clk.now(), req.keyCursor()) {
		t.Fatal("setup: no trigger fires")
	}
	for range 3 {
		askHubNow(t, req)
		if err := retryNow(t, req, row.ID); err != nil {
			t.Fatal(err)
		}
	}
	if r := onlyRow(t, req, id); r.CurVer != 1 || log.count() != 1 {
		t.Fatalf("while the hub holds version 1: row %+v, %d sends; want nothing sealed or sent", r, log.count())
	}
	// The hub forgets it (as after a restore from backup); the status query
	// says unknown, and a second version goes.
	rid := hubapi.B64(rowVersions(t, req, row.ID)[0].RID)
	setFake(t, srv.URL, func(h *fakeHub) {
		h.statusHook = func(it hubapi.RelayStatusItem) (hubapi.RelayStatusResult, bool) {
			if it.RID == rid {
				return hubapi.RelayStatusResult{State: hubapi.RelayStatusUnknown}, true
			}
			return hubapi.RelayStatusResult{}, false
		}
	})
	askHubNow(t, req)
	if err := retryNow(t, req, row.ID); err != nil {
		t.Fatal(err)
	}
	if r := onlyRow(t, req, id); r.CurVer != 2 || log.count() != 2 {
		t.Fatalf("after unknown: row %+v, %d sends; want version 2 sent", r, log.count())
	}
	// The recipient takes version 1 (first in its mailbox), then version 2
	// as a duplicate; the hub's done for version 1 confirms the row.
	setFake(t, srv.URL, func(h *fakeHub) { h.statusHook = nil })
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	askHubNow(t, req)
	if rows := outboxRows(t, req, id); len(rows) != 0 {
		t.Fatalf("the row is not confirmed: %+v", rows)
	}
	if n := counter(req, cntDAckConfirmedHub); n != 1 {
		t.Fatalf("confirmed by the hub %d times", n)
	}
}

// ENG-R7, SI-82 (ENG-M3, ENG-M4; P-ENG-2's "the message is the same"):
// versions made by kid_stale, a rotation and a lost hand-over all carry
// the row's mid, ts, exp, ix, type and body byte for byte; the recipient
// handles the message once.
func TestEveryVersionIsTheSameMessage(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	req.setOutboxHold(true)
	log := &sentLog{next: func(n int, _ []byte) fakeSendAnswer {
		if n == 1 {
			return fakeSendAnswer{kind: fakeSendRefuse, status: 409, code: codeKidStale}
		}
		return fakeSendAnswer{kind: fakeSendAccept}
	}}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	id, err := req.Delegate(ctx, prov.AID(), "one message, many versions", nil)
	if err != nil {
		t.Fatal(err)
	}
	row := onlyRow(t, req, id)
	// A minute on, so that a version sealed with the time of its sealing
	// would show it.
	clk := newSharedClock(req.nowMS() + minuteMS)
	req.setClock(clk.fn())
	if err := identitytest.Rotate(req.self, req.nowMS()); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil); err != nil {
		t.Fatal(err)
	}
	askHubNow(t, req)
	if err := retryNow(t, req, row.ID); err != nil {
		t.Fatal(err)
	}
	log.mu.Lock()
	envs := append([][]byte(nil), log.envs...)
	log.mu.Unlock()
	if len(envs) != 3 || onlyRow(t, req, id).CurVer != 3 {
		t.Fatalf("%d versions sent, row %+v; want three", len(envs), onlyRow(t, req, id))
	}
	first := openAt(t, prov, envs[0]).Inner
	for i, env := range envs {
		in := openAt(t, prov, env).Inner
		if !bytes.Equal(in.MID, row.MID) || in.TS != row.TS || in.Exp != row.Exp || in.IX != first.IX || in.Type != first.Type ||
			!bytes.Equal(in.Body, first.Body) || in.From != first.From {
			t.Fatalf("version %d is another message: mid %x ts %d exp %d ix %s type %s", i+1, in.MID, in.TS, in.Exp, in.IX, in.Type)
		}
	}
	// Every version reaches the provider: it handles the delegation once.
	for _, env := range envs {
		injectEnvelope(t, srv, prov.AID(), env)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countMsgs(t, prov, id); n != 1 {
		t.Fatalf("the provider stored %d messages for three versions of one", n)
	}
	if counter(prov, dropDuplicate) != 2 {
		t.Fatalf("duplicates: %v", prov.ReceiveStats())
	}
}

// ENG-K10, ENG-K11 (FS-X4, FS-X5): a version carries this node's rk while
// it holds its key for the task; once the peer announced its rk, the next
// version is sealed in two layers to it — a single-layer task becomes a
// double-layer one at a reseal.
func TestAVersionCarriesTheTasksInteractionKeys(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	req.setOutboxHold(true)
	log := lostByTheHub(t, srv.URL)
	id, err := req.Delegate(ctx, prov.AID(), "keys of the task", nil)
	if err != nil {
		t.Fatal(err)
	}
	row := onlyRow(t, req, id)
	if op := openAt(t, prov, log.last(t)); op.Layers != 1 || op.RK != nil {
		t.Fatalf("without interaction keys: %d layers, rk %v", op.Layers, op.RK)
	}
	// This node's key for the task, and the provider's, as FS-7's wiring
	// makes them; the provider's rk recorded as its first message carried it.
	own, err := seal.GenerateIXKey(seal.SuiteX25519, id, prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	peer, err := seal.GenerateIXKey(seal.SuiteX25519, id, req.AID())
	if err != nil {
		t.Fatal(err)
	}
	putIXKey(t, req, own)
	if err := req.ix.Update(func(tx *interactions.Tx) error {
		rk, err := coredet.Marshal(peer.RK())
		if err != nil {
			return err
		}
		return tx.RecordPeerRK(id, rk)
	}); err != nil {
		t.Fatal(err)
	}
	if err := identitytest.Rotate(req.self, req.nowMS()); err != nil { // T3: a new version is due
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil); err != nil {
		t.Fatal(err)
	}
	askHubNow(t, req)
	if err := retryNow(t, req, row.ID); err != nil {
		t.Fatal(err)
	}
	op, err := seal.Open(log.last(t), prov.AID(), prov.enc, oneIXKey{peer})
	if err != nil {
		t.Fatal(err)
	}
	if op.Layers != 2 || op.RK == nil || !bytes.Equal(op.RK.Pub, own.Pub) || !bytes.Equal(op.IKID, peer.RK().KID()) {
		t.Fatalf("after the peer's rk: %d layers, rk %+v, ikid %x", op.Layers, op.RK, op.IKID)
	}
	// Once this node's key is gone, a version carries no rk (FS-X4).
	if err := req.ix.Update(func(tx *interactions.Tx) error { _, err := tx.DeleteAllIXKeys(); return err }); err != nil {
		t.Fatal(err)
	}
	if k, err := req.ixKeysForSeal(id); err != nil || k.own != nil || k.peer == nil {
		t.Fatalf("after the key went: %+v %v", k, err)
	}
}

// oneIXKey opens a second layer sealed to k alone.
type oneIXKey struct{ k *seal.IXKey }

func (l oneIXKey) IXKey(ikid []byte) (*seal.IXKey, error) {
	if bytes.Equal(ikid, l.k.RK().KID()) {
		return l.k, nil
	}
	return nil, nil
}

// putIXKey stores k as d's interaction key.
func putIXKey(t testing.TB, d *Daemon, k *seal.IXKey) {
	t.Helper()
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		return tx.PutIXKey(interactions.IXKey{IX: k.IX, PeerAID: k.PeerAID, Suite: k.Suite, Pub: k.Pub, Priv: k.Private})
	}); err != nil {
		t.Fatal(err)
	}
}

// E-T12 (KEL D-1, engine ENG-K13, ENG-K21, SI-84; ENG-M12): in each state
// that closes the outbound gate, nothing is handed over — no queued
// message, no preview frame, no card request or answer, on the hub or a
// direct path — nothing is sealed, and no signed request goes to the hub.
// (The poll that notice_stale lets through is TestGateReopensFromPoll.)
func TestET12_TheGateHoldsEveryEnvelope(t *testing.T) {
	for _, state := range []string{IdentityNoHub, IdentityPublishing, IdentityNoticeStale, IdentityStopped, IdentityDeactivated} {
		t.Run(state, func(t *testing.T) {
			srv, req, prov := quietPair(t)
			ctx := context.Background()
			id, _ := previewTask(t, req, prov)
			heldCapFrom(t, prov, req)
			heldCapFrom(t, req, prov)
			closed := GateState{Open: false, State: state}
			req.setIdentityGate(func() GateState { return closed })
			prov.setIdentityGate(func() GateState { return closed })
			direct := &recordingP2P{}
			req.RegisterTransport(direct)
			prov.RegisterTransport(direct)
			from, sends := len(hubRequests(t, srv.URL)), relayCountFor(srv.URL)
			versions := func() int {
				_, v := outboxCounts(t, req)
				_, w := outboxCounts(t, prov)
				return v + w
			}
			before := versions()

			_, _ = req.Delegate(ctx, prov.AID(), "held by the gate", nil) // refused at once, or queued
			_ = req.SendMessage(ctx, id, "a follow-up", nil)
			req.flushOutboxAt(ctx, int64(req.nowMS())+int64(dayMS))
			prov.flushOutboxAt(ctx, int64(prov.nowMS())+int64(dayMS))
			cardGet, _ := delegation.CardGet{}.Marshal()
			answer, _ := (&delegation.CardResp{Error: delegation.CardBusy}).Marshal()
			for _, res := range []OneShotResult{
				(&testCoalescer{d: prov}).frame(t, req.AID(), id, "a preview"),
				req.oneShotSend(ctx, oneShotCardGet, prov.AID(), cardIX(t), cardGet),
				prov.oneShotSend(ctx, oneShotCard, req.AID(), cardIX(t), answer, oneShotSealTo(keySetOf(t, req))),
			} {
				if res.Handed {
					t.Errorf("a one-shot envelope was handed over: %+v", res)
				}
			}
			if n := relayCountFor(srv.URL) - sends; n != 0 {
				t.Errorf("%d envelopes reached the hub", n)
			}
			if n := len(direct.digests(t)); n != 0 {
				t.Errorf("%d envelopes went directly", n)
			}
			if n := versions() - before; n != 0 {
				t.Errorf("%d versions sealed", n)
			}
			if signed := signedPaths(hubRequests(t, srv.URL)[from:]); len(signed) != 0 {
				t.Errorf("signed requests went to the hub: %v", signed)
			}
		})
	}
}

// E-T12's emergency case (ENG-K13, ENG-R3 S3; ENG-M13): the gate is read
// again where a version is sealed, so a gate that closed after the attempt
// decided to transmit — an emergency rotation starting — signs nothing;
// nor does a key state no notice confirms (a rotation built after the gate
// was read). The same sealing serves the one-shot sender.
func TestNothingIsSealedWhileTheGateIsClosed(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	req.setOutboxHold(true)
	log := lostByTheHub(t, srv.URL)
	id, err := req.Delegate(ctx, prov.AID(), "sealed once", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := onlyRow(t, req, id)
	row, _, err := req.ix.OutboxState(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := req.ix.OutboxContent(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{IdentityPublishing, IdentityNoticeStale, IdentityStopped} {
		closed := GateState{Open: false, State: state}
		req.setIdentityGate(func() GateState { return closed })
		if sv, err := req.sealVersion(ctx, row, body, false); !errors.Is(err, errGateClosed) {
			t.Fatalf("%s: sealed %v (%v), want nothing while the gate is closed", state, sv != nil, err)
		}
	}
	// The key state moved on, and no notice confirms it yet.
	holdGateOpen(req)
	if err := identitytest.Rotate(req.self, req.nowMS()); err != nil {
		t.Fatal(err)
	}
	if sv, err := req.sealVersion(ctx, row, body, false); !errors.Is(err, errNoTipNotice) {
		t.Fatalf("a key state no notice confirms: sealed %v (%v)", sv != nil, err)
	}
	// An attempt that meets either waits: no version, no send, no failure.
	askHubNow(t, req) // the hub lost it: due again, and T3 fires
	r = onlyRow(t, req, id)
	vv := req.verViews(r.ID, rowVersions(t, req, r.ID), req.configuredHub(), req.nowMS())
	if !needsTransmit(rowView(&r), vv) || !resealNeeded(rowView(&r), vv, req.nowMS(), req.keyCursor()) {
		t.Fatal("setup: the attempt would not seal")
	}
	sent := log.count()
	if err := retryNow(t, req, r.ID); err != nil {
		t.Fatal(err)
	}
	if after := onlyRow(t, req, id); after.CurVer != r.CurVer || after.Attempts != r.Attempts || log.count() != sent {
		t.Fatalf("an attempt with no notice for the key state: row %+v (was %+v), %d sends", after, r, log.count()-sent)
	}
}

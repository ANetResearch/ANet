package daemon

// Tests of the peer key state (KEL §8.1, KEL-11): the federation hub KEL set
// (KEL D-24), a held task judged at its arrival (D-25), directory material
// sealed to or recorded only with a fresh notice (D-26) and the key_state a
// listing shows (D-28, M-3, A-2). Acceptance: TestHubKELSetExtendOnly,
// TestWitnessUnknownRefreshesOncePerMinute,
// TestApproveRejectsAfterPeerEmergencyRotation, TestKeyStateFreshStale,
// TestFirstContactNeedsFreshNotice, TestRefreshWithoutNoticeKeepsStoredRecord.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/dirbox"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// moduleQuery is a listing of text.stats agents, with the uncarded ones
// when uncarded.
func moduleQuery(uncarded bool) module.AgentQuery {
	return module.AgentQuery{Skill: "text.stats", IncludeUncarded: uncarded}
}

// agentWitnessedBy is a fresh agent identity whose witness is the hub w,
// designated at the start of time.
func agentWitnessedBy(t testing.TB, w *identity.Controller) *identity.Controller {
	t.Helper()
	c, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	if err := identitytest.SwitchWitness(c, w.AID(), identitytest.InceptTS+1); err != nil {
		t.Fatal(err)
	}
	return c
}

// noticeBy is the notice w signs, now under its current key, for the tip of
// c's KEL as observed at observedAt.
func noticeBy(t testing.TB, w identity.Signer, c *identity.Controller, observedAt uint64) []byte {
	t.Helper()
	kel := c.KEL()
	tip := kel[len(kel)-1]
	n, err := identity.SignNotice(w, identity.NoticeBody{AID: c.AID(), Seq: tip.Event.Seq, Head: tip.EventID, ObservedAt: observedAt})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// heldKELLen is how many events of aid's key history d holds as a hub's,
// 0 when it holds none.
func heldKELLen(d *Daemon, aid string) int {
	kel, ok := d.hubKELs(aid)
	if !ok {
		return 0
	}
	return len(kel)
}

// KEL D-24, M19 (the daemon's side): the hub KELs a node holds follow its
// hub's /hub/kels by extension only. A peer hub's rotation is taken, and a
// notice its retired key signs after it is refused; a hub listing the peer's
// history before the rotation does not move the node back, so that notice
// stays refused. A fork keeps the pinned history and is counted; an agent's
// history (toad 1) is never taken for a hub's, nor one adding an event from
// the future (R-9), nor one listed under an AID it does not replay to.
func TestHubKELSetExtendOnly(t *testing.T) {
	srv := newFakeHub(t)
	h := fakeHubAt(t, srv.URL)
	d := newTestDaemon(t, srv.URL, false)
	ctx := context.Background()
	sync := func(why string) bool {
		t.Helper()
		changed, err := d.syncHubKELs(ctx)
		if err != nil {
			t.Fatalf("%s: %v", why, err)
		}
		return changed
	}
	now := d.nowMS()
	p, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	a := agentWitnessedBy(t, p)
	old := p.Snapshot()
	h.setHubKEL(p.AID(), p.KEL())
	if !sync("first listing") || heldKELLen(d, p.AID()) != 1 {
		t.Fatalf("the peer hub's key history is not held: %d events", heldKELLen(d, p.AID()))
	}
	if err := identity.VerifyNotice(noticeBy(t, old, a, now), a.KEL(), d.hubKELs, now); err != nil {
		t.Fatalf("a notice under the held key: %v", err)
	}
	past := append([]identity.SignedEvent(nil), p.KEL()...)
	if err := identitytest.Rotate(p, now-3_600_000); err != nil {
		t.Fatal(err)
	}
	h.setHubKEL(p.AID(), p.KEL())
	if !sync("the rotation") || heldKELLen(d, p.AID()) != 2 {
		t.Fatalf("the peer hub's rotation was not taken: %d events", heldKELLen(d, p.AID()))
	}
	if err := identity.VerifyNotice(noticeBy(t, p, a, now), a.KEL(), d.hubKELs, now); err != nil {
		t.Fatalf("a notice under the new key: %v", err)
	}
	stale := noticeBy(t, old, a, now)
	if err := identity.VerifyNotice(stale, a.KEL(), d.hubKELs, now); !errors.Is(err, identity.ErrNoticeInvalid) {
		t.Fatalf("a notice the retired key signed after the rotation: %v, want witness-invalid", err)
	}
	// The history before the rotation, listed again: kept as it is.
	h.setHubKEL(p.AID(), past)
	if sync("the past listed") || heldKELLen(d, p.AID()) != 2 {
		t.Fatalf("a strict prefix replaced the held history: %d events", heldKELLen(d, p.AID()))
	}
	if err := identity.VerifyNotice(stale, a.KEL(), d.hubKELs, now); !errors.Is(err, identity.ErrNoticeInvalid) {
		t.Fatalf("after the past was listed, the retired key's notice: %v, want witness-invalid", err)
	}
	// A fork: two histories from one inception.
	fa, fb := identity.SuiteController(), identity.SuiteController()
	if err := identitytest.Rotate(fa, identity.SuiteInceptionTS+1000); err != nil {
		t.Fatal(err)
	}
	if err := identitytest.Rotate(fb, identity.SuiteInceptionTS+2000); err != nil {
		t.Fatal(err)
	}
	h.setHubKEL(fa.AID(), fa.KEL())
	sync("a")
	forks := counter(d, cntHubKELFork)
	h.setHubKEL(fa.AID(), fb.KEL())
	sync("a fork")
	if held, _ := d.hubKELs(fa.AID()); len(held) != 2 || held[1].EventID != fa.KEL()[1].EventID || counter(d, cntHubKELFork) != forks+1 {
		t.Fatalf("a fork: held %d events, hub-kel-fork %d", len(held), counter(d, cntHubKELFork)-forks)
	}
	// An agent's history listed as a hub's.
	h.setHubKEL(a.AID(), a.KEL())
	sync("an agent's history")
	if heldKELLen(d, a.AID()) != 0 || counter(d, cntHubKELNotHub) == 0 {
		t.Fatal("an agent's key history was held as a hub's")
	}
	// An event from the future (R-9).
	q, _ := identitytest.Hub()
	h.setHubKEL(q.AID(), q.KEL())
	sync("q")
	if err := identitytest.Rotate(q, now+identity.EventClockWindow+60_000); err != nil {
		t.Fatal(err)
	}
	h.setHubKEL(q.AID(), q.KEL())
	sync("q's rotation from the future")
	if heldKELLen(d, q.AID()) != 1 || counter(d, cntHubKELFuture) == 0 {
		t.Fatalf("an event from the future was taken: %d events", heldKELLen(d, q.AID()))
	}
	// A history listed under another hub's AID.
	r, _ := identitytest.Hub()
	h.setHubKEL(r.AID(), q.KEL())
	bad := counter(d, cntHubKELsBadEntry)
	sync("a history under another AID")
	if heldKELLen(d, r.AID()) != 0 || counter(d, cntHubKELsBadEntry) != bad+1 {
		t.Fatal("a history listed under an AID it does not replay to was held")
	}
	// The hub's own pin (GET /hub/identity) follows the same rule.
	if _, err := d.pinPeerKEL(p.AID(), past, interactions.PinHub); err != nil || heldKELLen(d, p.AID()) != 2 {
		t.Fatalf("pinning the past through the hub's identity: %v, %d events", err, heldKELLen(d, p.AID()))
	}
}

// KEL D-24: a notice under a witness this node does not hold the KEL of
// has the hub KEL set read again — once a minute at most, whatever asks —
// and is checked again; until the hub lists the witness the envelope stays
// temporary.
func TestWitnessUnknownRefreshesOncePerMinute(t *testing.T) {
	srv := newFakeHub(t)
	h := fakeHubAt(t, srv.URL)
	prov := registered(t, srv.URL, "prov")
	s, w := unknownWitnessSender(t)
	env := craft(t, s, prov, seal.TypeMessage, "ix_any", chatBody(t, "x", ""), nil)
	reads := h.hubKELReadCount()
	for i, why := range []string{"first", "again within the minute"} {
		if r := receive(t, prov, env); r.class != rxTransient || r.reason != seal.ReasonWitnessUnknown {
			t.Fatalf("%s: %+v, want temporary witness-unknown", why, r)
		}
		if n := h.hubKELReadCount() - reads; n != 1 {
			t.Fatalf("%s (%d): /hub/kels read %d times, want once", why, i, n)
		}
	}
	h.setHubKEL(w.AID(), w.KEL())
	if r := receive(t, prov, env); r.class != rxTransient || h.hubKELReadCount()-reads != 1 {
		t.Fatalf("listed, within the minute: %+v after %d reads", r, h.hubKELReadCount()-reads)
	}
	base := prov.nowMS()
	prov.setClock(func() uint64 { return base + 61_000 })
	if r := receive(t, prov, env); r.reason != dropUnknownIX {
		t.Fatalf("a minute on: %+v, want past step 6½ (%s)", r, dropUnknownIX)
	}
	if n := h.hubKELReadCount() - reads; n != 2 {
		t.Fatalf("a minute on: /hub/kels read %d times, want twice", n)
	}
	if heldKELLen(prov, w.AID()) != 1 {
		t.Fatal("the witness's key history is not held")
	}
}

// KEL D-25, M25, M40: a delegation held for approval is judged, when the
// operator approves it, by its sender's key state at the arrival kept with
// it, under the latest KEL this node knows. An emergency rotation of the
// sender since — even dated after the arrival — refuses the approval with
// key_state (the approve variant's sentence), removes the held task and
// tells the sender nothing. A scheduled rotation since does not: the task
// arrived under a key then current, and is approved hours later.
func TestApproveRejectsAfterPeerEmergencyRotation(t *testing.T) {
	_, _, prov := registeredPair(t)
	setPolicy(t, prov, PolicyApprove)
	hold := func(s sender, ix string) {
		t.Helper()
		env := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "please", ""), nil)
		if r := receive(t, prov, env); r.class != rxAccepted {
			t.Fatalf("held: %+v", r)
		}
		if _, err := prov.ix.GetPending(ix); err != nil {
			t.Fatal(err)
		}
	}
	queued := func() int {
		n, err := prov.ix.OutboxLen()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	s := newStranger(t)
	const ix = "ix_held_then_emergency"
	hold(s, ix)
	before := queued()
	// The sender rotates in an emergency after the task arrived, and this
	// node learns of it (a later message of the sender's carried the KEL).
	if err := identitytest.RotateEmergency(s.ctrl, prov.nowMS()+1000); err != nil {
		t.Fatal(err)
	}
	if err := prov.notePeer(s.aid, s.ctrl.KEL(), nil, "", false); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	prov.hInboundApprove(rec, httptest.NewRequest(http.MethodPost, "/inbound/approve", strings.NewReader(`{"interaction_id":"`+ix+`"}`)))
	var body a2ashape.ErrorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusConflict || body.Reason != "key_state" ||
		body.Error != a2ashape.Sentence("key_state.approve", a2ashape.Lang(), nil) {
		t.Fatalf("approval after the sender's emergency rotation: %d %s", rec.Code, rec.Body)
	}
	if _, err := prov.ix.GetPending(ix); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the refused task is still held: %v", err)
	}
	if _, err := prov.ix.Get(ix); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("the refused task became an interaction")
	}
	if n := queued(); n != before {
		t.Fatalf("%d messages queued to the sender of a refused task", n-before)
	}
	if _, err := prov.ApprovePending(ix); !errors.Is(err, ErrNotPending) {
		t.Fatalf("approving it again: %v", err)
	}

	// A scheduled rotation since, approved two hours on: approved.
	s2 := newStranger(t)
	const ix2 = "ix_held_then_scheduled"
	hold(s2, ix2)
	if err := identitytest.Rotate(s2.ctrl, prov.nowMS()+1000); err != nil {
		t.Fatal(err)
	}
	if err := prov.notePeer(s2.aid, s2.ctrl.KEL(), nil, "", false); err != nil {
		t.Fatal(err)
	}
	later := prov.nowMS() + 2*3_600_000
	prov.setClock(func() uint64 { return later })
	if _, err := prov.ApprovePending(ix2); err != nil {
		t.Fatalf("approval after the sender's scheduled rotation: %v", err)
	}
}

// decodeHubKELs reads every /hub/kels answer without panicking, and what it
// returns replays to the AID it is listed under.
func FuzzDecodeHubKELs(f *testing.F) {
	p, _ := identitytest.Hub()
	kel, _ := identity.MarshalKEL(p.KEL())
	good, _ := json.Marshal(map[string]any{"hubs": []map[string]string{{"aid": p.AID(), "kel": hubapi.B64(kel)}}})
	f.Add(good)
	f.Add([]byte(`{"hubs":[]}`))
	f.Add([]byte(`{"hubs":null}`))
	f.Add([]byte(`{"hubs":[{"aid":"x","kel":"AA"}]}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, b []byte) {
		out, skipped, err := decodeHubKELs(b)
		if err != nil {
			if len(out) != 0 || skipped != 0 {
				t.Fatal("an error with entries")
			}
			return
		}
		if len(out) > hubKELsMax {
			t.Fatalf("%d entries past the bound", len(out))
		}
		for _, e := range out {
			if got, err := replayedAID(e.KEL); err != nil || got != e.AID {
				t.Fatalf("an entry that does not replay to %s", e.AID)
			}
		}
	})
}

// TestDecodeHubKELs is decodeHubKELs on the answers a hub could give.
func TestDecodeHubKELs(t *testing.T) {
	p, _ := identitytest.Hub()
	kel, _ := identity.MarshalKEL(p.KEL())
	for _, c := range []struct {
		name       string
		body       string
		n, skipped int
		wantErr    bool
	}{
		{"one", `{"hubs":[{"aid":"` + p.AID() + `","kel":"` + hubapi.B64(kel) + `"}]}`, 1, 0, false},
		{"not base64url", `{"hubs":[{"aid":"` + p.AID() + `","kel":"` + hubapi.B64(kel) + `+"}]}`, 0, 1, false},
		{"not an AID", `{"hubs":[{"aid":"x","kel":"` + hubapi.B64(kel) + `"}]}`, 0, 1, false},
		{"no hubs", `{}`, 0, 0, true},
		{"not JSON", `hubs`, 0, 0, true},
	} {
		out, skipped, err := decodeHubKELs([]byte(c.body))
		if (err != nil) != c.wantErr || len(out) != c.n || skipped != c.skipped {
			t.Errorf("%s: %d entries, %d skipped, %v", c.name, len(out), skipped, err)
		}
	}
}

// kidsOf are the kids of a SignedEncKeySet encoding of aid's, verified
// under kel.
func kidsOf(t testing.TB, signed []byte, aid string, kel []identity.SignedEvent) [][]byte {
	t.Helper()
	s, err := seal.UnmarshalSignedEncKeySet(signed)
	if err != nil {
		t.Fatal(err)
	}
	set, err := seal.VerifyEncKeySet(s, aid, kel, uint64(time.Now().UnixMilli()))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, k := range set.Keys {
		out = append(out, k.KID)
	}
	return out
}

// sentKIDs are the kids the envelopes of log were sealed to.
func sentKIDs(t testing.TB, log *sentLog) [][]byte {
	t.Helper()
	log.mu.Lock()
	defer log.mu.Unlock()
	var out [][]byte
	for _, env := range log.envs {
		o, err := seal.ParseOuter(env)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, o.KID)
	}
	return out
}

// KEL D-26, M41: this node's own hub hands it a first contact's material —
// a key history cut before the recipient's rotation, and a key set its
// thief signed with the retired key — without a notice, or with one over
// six hours old. Nothing is sealed to the thief's key: the delegation is
// queued waiting on home_unreachable, the directory is asked again past
// every cache at most once every ten minutes, and nothing of the material
// is recorded. Once the directory holds the recipient's real entry with
// its witness's fresh notice, the next attempt seals to the real key.
func TestFirstContactNeedsFreshNotice(t *testing.T) {
	srv := newFakeHub(t)
	h := fakeHubAt(t, srv.URL)
	req := registered(t, srv.URL, "req")
	ctx := context.Background()
	log := &sentLog{next: func(int, []byte) fakeSendAnswer { return fakeSendAnswer{kind: fakeSendAccept} }}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })

	// The recipient V, witnessed by the peer hub P, which this node's hub
	// federates with (it lists P's key history).
	p, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	h.setHubKEL(p.AID(), p.KEL())
	v := agentWitnessedBy(t, p)
	now := req.nowMS()
	truncated := append([]identity.SignedEvent(nil), v.KEL()...)
	stolen := signedKeysFor(t, v, 0) // signed with the key the thief holds
	staleNotice := noticeBy(t, p, v, now-identity.NoticeMaxAge-3_600_000)
	if err := identitytest.RotateEmergency(v, now-60_000); err != nil {
		t.Fatal(err)
	}
	real := signedKeysFor(t, v, 0)
	truncatedBytes, _ := identity.MarshalKEL(truncated)
	thief := func(notice []byte) {
		h.setDir(func(h *fakeHub) {
			h.dirOverride[v.AID()] = dirbox.DirEntry{AID: v.AID(), Home: h.self.AID(), KEL: truncatedBytes, Keys: stolen, Notice: notice}
		})
	}
	stolenKIDs := kidsOf(t, stolen, v.AID(), truncated)

	thief(nil)
	q0 := queryCount(h)
	id, err := req.Delegate(ctx, v.AID(), "a task for V", nil)
	if err != nil {
		t.Fatalf("the delegation is refused instead of queued: %v", err)
	}
	rows := outboxRows(t, req, id)
	if len(rows) != 1 || rowWait(t, req, rows[0].ID) != reasonHomeUnreachable {
		t.Fatalf("outbox %+v, want the delegation waiting on home_unreachable", rows)
	}
	if n := queryCount(h) - q0; n != 2 {
		t.Fatalf("%d bucket queries, want the lookup and one past every cache", n)
	}
	if _, err := req.ix.PeerIdentity(v.AID()); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("material without a notice was recorded: %v", err)
	}
	// With a notice over six hours old, ten minutes on: asked once more
	// past every cache, and still nothing is sealed.
	thief(staleNotice)
	base := req.nowMS()
	req.setClock(func() uint64 { return base + noticeRefetchMS + 1000 })
	q1 := queryCount(h)
	_ = retryNow(t, req, rows[0].ID)
	if rowWait(t, req, rows[0].ID) != reasonHomeUnreachable || queryCount(h)-q1 != 1 {
		t.Fatalf("with a stale notice: waits on %q after %d queries", rowWait(t, req, rows[0].ID), queryCount(h)-q1)
	}
	// Within the ten minutes, the directory is not asked again.
	q2 := queryCount(h)
	_ = retryNow(t, req, rows[0].ID)
	if queryCount(h) != q2 {
		t.Fatal("the directory was asked again past every cache within ten minutes")
	}
	if kids := sentKIDs(t, log); len(kids) != 0 {
		t.Fatalf("%d envelopes sent to a recipient with no fresh notice", len(kids))
	}

	// The real entry, with the witness's fresh notice, ten minutes on.
	later := base + 2*noticeRefetchMS + 2000
	req.setClock(func() uint64 { return later })
	full, _ := identity.MarshalKEL(v.KEL())
	h.setDir(func(h *fakeHub) {
		h.dirOverride[v.AID()] = dirbox.DirEntry{AID: v.AID(), Home: h.self.AID(), KEL: full, Keys: real, Notice: noticeBy(t, p, v, later)}
	})
	if err := retryNow(t, req, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	kids := sentKIDs(t, log)
	if len(kids) != 1 || !bytes.Equal(kids[0], kidsOf(t, real, v.AID(), v.KEL())[0]) {
		t.Fatalf("%d envelopes sent, want one sealed to the real key", len(kids))
	}
	for _, k := range kids {
		for _, s := range stolenKIDs {
			if bytes.Equal(k, s) {
				t.Fatal("an envelope was sealed to the thief's key")
			}
		}
	}
	if row, err := req.ix.PeerIdentity(v.AID()); err != nil || row.KELLen != len(v.KEL()) {
		t.Fatalf("the real material is not recorded: %+v %v", row, err)
	}
}

// KEL D-26 item 4, SSR-D18: the daily recheck of an active contact writes
// what it finds only when the entry carries a fresh notice of its witness;
// without one the stored record stays as it is and keeps being used — the
// send goes on under it — and with one the newer key set is recorded.
func TestRefreshWithoutNoticeKeepsStoredRecord(t *testing.T) {
	srv, req, prov := plainRegisteredPair(t)
	ctx := context.Background()
	h := fakeHubAt(t, srv.URL)
	setFake(t, srv.URL, func(h *fakeHub) { h.noticeAt = req.nowMS })
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	row, err := req.ix.PeerIdentity(prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	seq0, checked0 := row.KeySetSeq, row.KeysCheckedAt
	start := uint64(time.Now().UnixMilli())
	prov.setClock(func() uint64 { return start - start%seal.EpochMS + seal.EpochMS + 60_000 })
	prov.maintainKeyRing()
	newSeq := prov.enc.Seq()
	if newSeq <= seq0 {
		t.Fatal("the provider did not rotate")
	}
	// A day on, the directory's entries carry no notice.
	setFake(t, srv.URL, func(h *fakeHub) { h.noSuiteNotices = true })
	day := uint64(checked0) + activeRecheckMS + 60_000
	req.setClock(func() uint64 { return day })
	q := queryCount(h)
	if err := req.SendMessage(ctx, id, "a day on", nil); err != nil {
		t.Fatalf("a send to an active contact whose recheck finds no notice: %v", err)
	}
	waitUntil(t, "the recheck", func() bool { return queryCount(h) > q })
	time.Sleep(100 * time.Millisecond)
	if again, _ := req.ix.PeerIdentity(prov.AID()); again.KeySetSeq != seq0 || again.KeysCheckedAt != checked0 {
		t.Fatalf("a recheck without a notice changed the record: seq %d checked %d", again.KeySetSeq, again.KeysCheckedAt)
	}
	for _, r := range outboxRows(t, req, id) {
		if w := rowWait(t, req, r.ID); w == reasonHomeUnreachable {
			t.Fatalf("row %d waits on %s: the stored record should have been used", r.ID, w)
		}
	}
	// Another day on, with the witness's notice: the newer set is recorded.
	setFake(t, srv.URL, func(h *fakeHub) { h.noSuiteNotices = false })
	day2 := day + activeRecheckMS + 60_000
	req.setClock(func() uint64 { return day2 })
	if err := req.SendMessage(ctx, id, "two days on", nil); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the recheck to record the newer key set", func() bool {
		r, err := req.ix.PeerIdentity(prov.AID())
		return err == nil && r.KeySetSeq == newSeq
	})
}

// KEL D-28, M-3, A-2: what a listing and a card lookup show of an agent
// says key_state: fresh when the directory's entry carried a notice of its
// witness that checks here now, stale otherwise — a stale notice, none, or
// an agent the registry lists without a card. A card shown from the
// discovery cache is checked again when shown: its notice may have aged.
func TestKeyStateFreshStale(t *testing.T) {
	ctx := context.Background()
	h := newRegistryHub(t)
	fresh := newCardAgent(t, "Fresh", "fresh", "text.stats")
	stale := newCardAgent(t, "Stale", "stale", "text.stats")
	none := newCardAgent(t, "None", "none", "text.stats")
	h.list(fresh, nil)
	kelOf := func(a cardAgent) []identity.SignedEvent {
		raw, _ := hubapi.DecodeB64(a.kel, -1)
		kel, err := identity.UnmarshalKEL(raw)
		if err != nil {
			t.Fatal(err)
		}
		return kel
	}
	old := suiteNotice(t, kelOf(stale), uint64(time.Now().UnixMilli())-identity.NoticeMaxAge-60_000)
	h.list(stale, func(e *hubapi.A2AAgentEntry) { e.KeyStateNotice = hubapi.B64(old) })
	e := h.entries[stale.aid]
	e.Notice = old
	h.entries[stale.aid] = e
	h.list(none, func(e *hubapi.A2AAgentEntry) { e.KeyStateNotice = "" })
	e = h.entries[none.aid]
	e.Notice = nil
	h.entries[none.aid] = e
	// A notice five and a half hours old: fresh when listed, past its age
	// within the hour the listing is kept.
	aging := newCardAgent(t, "Aging", "aging", "text.stats")
	older := suiteNotice(t, kelOf(aging), uint64(time.Now().UnixMilli())-identity.NoticeMaxAge+30*60_000)
	h.list(aging, func(e *hubapi.A2AAgentEntry) { e.KeyStateNotice = hubapi.B64(older) })
	e = h.entries[aging.aid]
	e.Notice = older
	h.entries[aging.aid] = e
	h.dir = []hubDirEntry{{AID: newStranger(t).aid, Name: "uncarded", Caps: []string{"text.stats"}}}

	d := newTestDaemon(t, h.srv.URL, false)
	want := map[string]string{fresh.aid: "fresh", aging.aid: "fresh", stale.aid: "stale", none.aid: "stale", h.dir[0].AID: "stale"}
	agents, _, err := d.listAgents(ctx, moduleQuery(true))
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != len(want) {
		t.Fatalf("%d agents listed", len(agents))
	}
	for _, a := range agents {
		if a.KeyState != want[a.AID] {
			t.Errorf("%s: key_state %q, want %s", a.AID, a.KeyState, want[a.AID])
		}
	}
	p := newPlaneFor(t, d, "tok")
	resp, raw := p.req(t, "POST", "/agents/list", `{"skill":"text.stats"}`, p.bearer)
	if resp.StatusCode != http.StatusOK || strings.Count(string(raw), `"key_state":"fresh"`) != 2 ||
		strings.Count(string(raw), `"key_state":"stale"`) != 2 {
		t.Fatalf("/agents/list: %d %s", resp.StatusCode, raw)
	}
	for aid, w := range map[string]string{fresh.aid: "fresh", stale.aid: "stale", none.aid: "stale"} {
		ra, err := d.agentCard(ctx, aid)
		if err != nil || ra.KeyState != w || ra.Verification != cardVerified {
			t.Errorf("the card of %s: %v key_state %q (%s), want %s", aid, err, ra.KeyState, ra.Verification, w)
		}
	}
	if ra, err := d.agentCard(ctx, newStranger(t).aid); err != nil || ra.KeyState != "stale" {
		t.Errorf("an agent no hub knows: %v key_state %q", err, ra.KeyState)
	}
	// Forty-five minutes on, the card listed with a notice then five and a
	// half hours old comes from the discovery cache, and is checked now.
	base := d.nowMS()
	d.setClock(func() uint64 { return base + 45*60_000 })
	if ra, err := d.agentCard(ctx, aging.aid); err != nil || ra.KeyState != "stale" {
		t.Errorf("a cached card whose notice aged: %v key_state %q, want stale", err, ra.KeyState)
	}
	// Seven hours on, the notice the card came with is past its age.
	d.setClock(func() uint64 { return base + identity.NoticeMaxAge + 3_600_000 })
	if ra, err := d.agentCard(ctx, fresh.aid); err != nil || ra.KeyState != "stale" {
		t.Errorf("seven hours on: %v key_state %q, want stale", err, ra.KeyState)
	}
}

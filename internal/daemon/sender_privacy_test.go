package daemon

// Tests of sender_privacy.go and of the area's sentences on the task
// surfaces (sealed-sender §4.8, §5.2–§5.4; SS-12).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// TestPeersListSenderHidden is SSR-U3 and SSR-U1: the sender identity of a
// peer is hidden exactly when this node holds a peer capability it may use
// now that does not say the peer prices something; one that does is the
// .paid sentence; anything else — none, given up, about to expire, under
// an older mailbox key, only one-shot ones, someone else's — is visible.
// POST /peers/list gives every listed peer once, with its lists and the
// column's sentence in this process's language.
func TestPeersListSenderHidden(t *testing.T) {
	peer := func(o ...func(*interactions.HeldCap)) interactions.HeldCap {
		return heldCap(interactions.CapScopePeer, "", 1, o...)
	}
	pay := func(h *interactions.HeldCap) { h.Pay = true }
	givenUp := func(h *interactions.HeldCap) { h.InvalidAt = tNow - 1 }
	oneShot := heldCap(interactions.CapScopeIX, "ix-1", 2)
	for _, c := range []struct {
		name   string
		held   []interactions.HeldCap
		epoch  uint64
		hidden bool
		key    string
	}{
		{"no capability", nil, 0, false, senderVisibleKey},
		{"a peer capability", []interactions.HeldCap{peer()}, 0, true, senderHiddenKey},
		{"under the peer's current mailbox key", []interactions.HeldCap{peer()}, 3, true, senderHiddenKey},
		{"under an older mailbox key", []interactions.HeldCap{peer()}, 4, false, senderVisibleKey},
		{"from a peer that prices something", []interactions.HeldCap{peer(pay)}, 0, false, senderVisiblePaidKey},
		{"given up", []interactions.HeldCap{peer(givenUp)}, 0, false, senderVisibleKey},
		{"given up, from a peer that prices something", []interactions.HeldCap{peer(pay, givenUp)}, 0, false, senderVisibleKey},
		{"a minute from its end", []interactions.HeldCap{peer(func(h *interactions.HeldCap) { h.CapExp = tNow + capValidMarginMS - 1 })},
			0, false, senderVisibleKey},
		{"just far enough from its end", []interactions.HeldCap{peer(func(h *interactions.HeldCap) { h.CapExp = tNow + capValidMarginMS })},
			0, true, senderHiddenKey},
		{"a one-shot capability only", []interactions.HeldCap{oneShot}, 0, false, senderVisibleKey},
		{"a one-shot and a peer capability", []interactions.HeldCap{oneShot, peer()}, 0, true, senderHiddenKey},
		{"another peer's capability", []interactions.HeldCap{peer(func(h *interactions.HeldCap) { h.Issuer = "peer-aid-0002" })},
			0, false, senderVisibleKey},
	} {
		hidden, key := senderIdentity(tPeer, c.held, c.epoch, tNow)
		if hidden != c.hidden || key != c.key {
			t.Errorf("%s: hidden %v (%s), want %v (%s)", c.name, hidden, key, c.hidden, c.key)
		}
	}

	// The control plane.
	d := newTestDaemon(t, "", false)
	const (
		a = "did:anet:peer-a" // allow and trust; gave this node a peer capability
		b = "did:anet:peer-b" // allow; its capability says it prices something
		c = "did:anet:peer-c" // deny; gave nothing
		x = "did:anet:peer-x" // on no list, gave a capability: not listed
	)
	allowPeers(t, d, a, b)
	trustPeers(t, d, a)
	denyPeers(t, d, c)
	now := d.nowMS()
	put := func(issuer string, id byte, paid bool) {
		h := heldCap(interactions.CapScopePeer, "", id, func(h *interactions.HeldCap) {
			h.Issuer, h.CapExp, h.ReceivedAt, h.Pay = issuer, now+capLifeContactMS, now, paid
		})
		if err := d.ix.Update(func(tx *interactions.Tx) error { _, err := tx.PutHeldGrant(h); return err }); err != nil {
			t.Fatal(err)
		}
	}
	put(a, 1, false)
	put(b, 2, true)
	put(x, 3, false)
	list := func(t *testing.T) (PeersList, map[string]any) {
		t.Helper()
		code, out := controlCall(t, d.hPeersList, "/peers/list", `{}`)
		if code != http.StatusOK {
			t.Fatalf("/peers/list: %d %v", code, out)
		}
		rec := httptest.NewRecorder()
		d.hPeersList(rec, httptest.NewRequest(http.MethodPost, "/peers/list", strings.NewReader(`{}`)))
		var pl PeersList
		if err := json.Unmarshal(rec.Body.Bytes(), &pl); err != nil {
			t.Fatal(err)
		}
		return pl, out
	}
	for _, lang := range []string{"C", "zh_CN.UTF-8"} {
		t.Run(lang, func(t *testing.T) {
			t.Setenv("ANET_LANG", lang)
			l := a2ashape.Lang()
			pl, raw := list(t)
			want := []PeerView{
				{AID: a, Lists: []string{ListAllow, ListTrust}, SenderHidden: true, SenderHiddenText: a2ashape.Sentence(senderHiddenKey, l, nil)},
				{AID: b, Lists: []string{ListAllow}, SenderHiddenText: a2ashape.Sentence(senderVisiblePaidKey, l, nil)},
				{AID: c, Lists: []string{ListDeny}, SenderHiddenText: a2ashape.Sentence(senderVisibleKey, l, nil)},
			}
			if !reflect.DeepEqual(pl.Peers, want) {
				t.Fatalf("peers\n got:  %+v\n want: %+v", pl.Peers, want)
			}
			// What the answer said before stays: the policy and the lists.
			for _, k := range []string{"policy", "allow", "deny", "trust", "public_capabilities", "pending"} {
				if _, ok := raw[k]; !ok {
					t.Errorf("/peers/list lost %q", k)
				}
			}
			items, _ := raw["peers"].([]any)
			for _, it := range items {
				m, _ := it.(map[string]any)
				keys := make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				if !reflect.DeepEqual(keys, []string{"aid", "lists", "sender_hidden", "sender_hidden_text"}) {
					t.Errorf("a /peers/list item has %v", keys)
				}
			}
		})
	}

	// The peer withdrew the credential (SS L4): its column turns visible.
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		_, err := tx.MarkHeldInvalid(a, interactions.CapScopePeer, "", bytes.Repeat([]byte{1}, seal.CapIDLen), now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pl, _ := list(t)
	if len(pl.Peers) != 3 || pl.Peers[0].AID != a || pl.Peers[0].SenderHidden ||
		pl.Peers[0].SenderHiddenText != a2ashape.Sentence(senderVisibleKey, a2ashape.Lang(), nil) {
		t.Fatalf("after the withdrawal: %+v", pl.Peers)
	}
}

// TestPeersListAfterAReply: end to end, a peer that answered this node's
// task gave it a peer capability with the answer, so its column turns from
// visible to hidden (SS §5.2 peers.sender_visible: "this changes on its own
// once they reply").
func TestPeersListAfterAReply(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	allowPeers(t, req, prov.AID())
	column := func() PeerView {
		t.Helper()
		for _, p := range req.PeersList().Peers {
			if p.AID == prov.AID() {
				return p
			}
		}
		t.Fatalf("%s is not in the list", prov.AID())
		return PeerView{}
	}
	if p := column(); p.SenderHidden || p.SenderHiddenText != a2ashape.Sentence(senderVisibleKey, a2ashape.Lang(), nil) {
		t.Fatalf("before any reply: %+v", p)
	}
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
	if p := column(); !p.SenderHidden || p.SenderHiddenText != a2ashape.Sentence(senderHiddenKey, a2ashape.Lang(), nil) {
		t.Fatalf("after the reply: %+v", p)
	}

	// A credential under another mailbox key than the one this node holds
	// of the peer is not one it may use: the column reads the peer's
	// current key from the store, so it is visible again.
	held, err := req.ix.HeldCaps(prov.AID())
	if err != nil || len(held) == 0 {
		t.Fatalf("held: %v %v", held, err)
	}
	other := held[0]
	other.CapID = bytes.Repeat([]byte{0x7e}, seal.CapIDLen)
	other.Epoch++
	if err := req.ix.Update(func(tx *interactions.Tx) error { _, err := tx.PutHeldGrant(other); return err }); err != nil {
		t.Fatal(err)
	}
	if p := column(); p.SenderHidden || p.SenderHiddenText != a2ashape.Sentence(senderVisibleKey, a2ashape.Lang(), nil) {
		t.Fatalf("under another mailbox key: %+v", p)
	}
}

// statusSenderPrivacy is GET /status's sender_privacy object.
func statusSenderPrivacy(t *testing.T, d *Daemon) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	d.hStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var st map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	sp, ok := st["sender_privacy"].(map[string]any)
	if !ok {
		t.Fatalf("/status has no sender_privacy: %v", st)
	}
	return sp
}

// TestStatusSenderPrivacy is SSR-U2: /status.sender_privacy has exactly its
// members, each read from its source — the mailbox key's registration and
// the impossible epoch (SS-7), the first-contact session (SS-9), the
// credentials held and issued, the oldest withdrawal the hub does not have
// and how many waited an hour or more, the downgrades after unexplained
// refusals (SS-8) and the small directory groups (SS-10) of the last 24
// hours.
func TestStatusSenderPrivacy(t *testing.T) {
	d := newTestDaemon(t, "", false)
	now := uint64(time.Now().UnixMilli())
	d.setClock(func() uint64 { return now })
	sp := statusSenderPrivacy(t, d)
	keys := make([]string, 0, len(sp))
	for k := range sp {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"held", "hub_epoch_invalid", "issued", "mailbox_registered", "refusal_downgrades_24h",
		"session_ok", "small_buckets_24h", "unpushed_revocation_age_s", "unpushed_revocations_1h"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("/status.sender_privacy has %v, want %v", keys, want)
	}
	if sp["session_ok"] != true || sp["held"] != 0.0 || sp["issued"] != 0.0 || sp["refusal_downgrades_24h"] != 0.0 ||
		sp["small_buckets_24h"] != 0.0 || sp["hub_epoch_invalid"] != false || sp["unpushed_revocation_age_s"] != 0.0 {
		t.Fatalf("a node that did nothing yet: %v", sp)
	}

	// The mailbox key: registered at the hub, and an impossible epoch seen.
	d.mboxSt.mu.Lock()
	d.mboxSt.registered = true
	d.mboxSt.mu.Unlock()
	d.mboxSt.epochInvalid.Store(true)
	// Downgrades and small groups: one a day and a minute ago is gone.
	day := uint64(24 * time.Hour / time.Millisecond)
	d.caps.noteDowngrade(now - day - 60_000)
	d.caps.noteDowngrade(now - 3_600_000)
	d.caps.noteDowngrade(now - 1)
	d.peers.noteSmallBucket("https://hub.example", now-day-60_000)
	d.peers.noteSmallBucket("https://hub.example", now-10)
	// Held: a valid capability of each scope, a given-up one and an
	// expired one. Issued: two in force, one expired, and withdrawals two
	// hours and ten minutes ago (unpushed), three hours ago (pushed).
	for i, h := range []interactions.HeldCap{
		heldCap(interactions.CapScopePeer, "", 1, func(h *interactions.HeldCap) { h.Issuer, h.CapExp = "did:anet:p1", now+day }),
		heldCap(interactions.CapScopeIX, "ix-1", 2, func(h *interactions.HeldCap) { h.Issuer, h.CapExp = "did:anet:p1", now+day }),
		heldCap(interactions.CapScopePeer, "", 3, func(h *interactions.HeldCap) {
			h.Issuer, h.CapExp, h.InvalidAt = "did:anet:p2", now+day, now-1
		}),
		heldCap(interactions.CapScopePeer, "", 4, func(h *interactions.HeldCap) { h.Issuer, h.CapExp = "did:anet:p3", now-1 }),
	} {
		inv := h.InvalidAt
		h.InvalidAt = 0
		if err := d.ix.Update(func(tx *interactions.Tx) error {
			if _, err := tx.PutHeldGrant(h); err != nil {
				return err
			}
			if inv != 0 {
				_, err := tx.MarkHeldInvalid(h.Issuer, h.Scope, h.IX, h.CapID, inv)
				return err
			}
			return nil
		}); err != nil {
			t.Fatalf("held %d: %v", i, err)
		}
	}
	issued := func(id byte, exp uint64) []byte {
		capID := bytes.Repeat([]byte{id}, seal.CapIDLen)
		c := interactions.IssuedCap{CapID: capID, Grantee: "did:anet:g", Class: seal.CapClassContact,
			Scope: interactions.CapScopePeer, HolderSeed: bytes.Repeat([]byte{id}, 32), HolderPub: bytes.Repeat([]byte{id}, 32),
			Epoch: 1, CapExp: exp, IssuedAt: now - day}
		if err := d.ix.Update(func(tx *interactions.Tx) error { return tx.PutIssuedCap(c) }); err != nil {
			t.Fatal(err)
		}
		return capID
	}
	revoke := func(capID []byte, at uint64) {
		if err := d.ix.Update(func(tx *interactions.Tx) error {
			_, err := tx.RevokeIssuedCap(capID, at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	issued(11, now+day)
	issued(12, now+day)
	issued(13, now-1)
	revoke(issued(14, now+day), now-2*3_600_000)
	revoke(issued(15, now+day), now-10*60_000)
	pushed := issued(16, now+day)
	revoke(pushed, now-3*3_600_000)
	if _, err := d.ix.MarkRevocationsPushed([][]byte{pushed}); err != nil {
		t.Fatal(err)
	}

	sp = statusSenderPrivacy(t, d)
	want := map[string]any{
		"mailbox_registered": true, "hub_epoch_invalid": true, "session_ok": true,
		"held": 2.0, "issued": 2.0,
		"unpushed_revocation_age_s": 7200.0, "unpushed_revocations_1h": 1.0,
		"refusal_downgrades_24h": 2.0, "small_buckets_24h": 1.0,
	}
	if !reflect.DeepEqual(sp, want) {
		t.Fatalf("/status.sender_privacy\n got:  %v\n want: %v", sp, want)
	}

	// session_ok: the last opening at this node's hub failed.
	srv, req, prov := registeredPair(t)
	if v := statusSenderPrivacy(t, req)["session_ok"]; v != true {
		t.Fatalf("before any first contact: session_ok %v", v)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.sessionRefuse = "admission_required" })
	req.dropSessions()
	_, _, err := req.relaySendV3(context.Background(), sealFrom(t, req, prov, seal.TypeMessage, "ix", chatBody(t, "m", "")),
		AccessChoice{Send: true, Kind: seal.AccessKindSession})
	if !errors.Is(err, errSessionUnavailable) {
		t.Fatalf("a refused opening: %v", err)
	}
	if v := statusSenderPrivacy(t, req)["session_ok"]; v != false {
		t.Fatalf("after a refused opening: session_ok %v", v)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.sessionRefuse = "" })
	if _, _, err := req.relaySendV3(context.Background(), sealFrom(t, req, prov, seal.TypeMessage, "ix", chatBody(t, "m", "")),
		AccessChoice{Send: true, Kind: seal.AccessKindSession}); err != nil {
		t.Fatal(err)
	}
	if v := statusSenderPrivacy(t, req)["session_ok"]; v != true {
		t.Fatalf("after a session opened again: session_ok %v", v)
	}
}

// taskSurfaces is what the control plane (the CLI and MCP read it) and the
// local A2A interface show of task id: the status message's text, the
// anet.reason and the anet.delivery_wait.
type taskSurface struct {
	name, text, reason, wait string
	hasWait                  bool
}

func taskSurfaces(t *testing.T, d *Daemon, peer, id string) []taskSurface {
	t.Helper()
	code, out := controlCall(t, d.hTasksGet, "/tasks/get", `{"task_id":"`+id+`"}`)
	if code != http.StatusOK {
		t.Fatalf("/tasks/get: %d %v", code, out)
	}
	b, _ := json.Marshal(out)
	var ct a2ashape.Task
	if err := json.Unmarshal(b, &ct); err != nil {
		t.Fatal(err)
	}
	at, err := (&DaemonTaskSeam{d: d}).Get(context.Background(), peer, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	var s []taskSurface
	for _, x := range []struct {
		name string
		t    a2ashape.Task
	}{{"control", ct}, {"a2a", at}} {
		ts := taskSurface{name: x.name}
		if m := x.t.Status.Message; m != nil {
			for _, p := range m.Parts {
				ts.text += p.Text
			}
		}
		ts.reason, _ = x.t.Metadata[a2ashape.KeyReason].(string)
		var w any
		w, ts.hasWait = x.t.Metadata["anet.delivery_wait"]
		ts.wait, _ = w.(string)
		s = append(s, ts)
	}
	return s
}

// TestSenderPrivacyCodesSayOneSentence is UX-T1 for the area's codes on the
// task surfaces (SS-12; ENG E-T25 for strangers_busy): a message the hub
// refused for good fails its task with the code's sentence, word for word
// and under the same code, on the control plane and the local A2A
// interface; a message the hub holds back waits on its code, and the task
// says so on both.
func TestSenderPrivacyCodesSayOneSentence(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		code   string
		status int
	}{{"recipient_unknown", http.StatusNotFound}, {"too_large_for_stranger", http.StatusRequestEntityTooLarge}} {
		t.Run(c.code, func(t *testing.T) {
			srv, req, prov := registeredPair(t)
			setFake(t, srv.URL, func(h *fakeHub) {
				h.sendHook = func([]byte) fakeSendAnswer {
					return fakeSendAnswer{kind: fakeSendRefuse, status: c.status, code: c.code}
				}
			})
			_, _ = req.Delegate(ctx, prov.AID(), "refused for good", nil)
			list, err := req.ix.List(interactions.RoleOutbound, "", 0, 10)
			if err != nil || len(list) != 1 {
				t.Fatalf("%d tasks: %v", len(list), err)
			}
			want := a2ashape.Sentence(c.code, a2ashape.Lang(), nil)
			for _, s := range taskSurfaces(t, req, prov.AID(), list[0].ID) {
				if s.text != want || s.reason != c.code {
					t.Errorf("%s: %q under %q, want %q under %q", s.name, s.text, s.reason, want, c.code)
				}
			}
		})
	}

	waits := []struct {
		code  string
		setup func(t *testing.T) (*Daemon, *Daemon, string)
	}{
		{"strangers_busy", func(t *testing.T) (*Daemon, *Daemon, string) {
			srv, req, prov := registeredPair(t)
			setFake(t, srv.URL, func(h *fakeHub) {
				h.sendHook = func([]byte) fakeSendAnswer {
					return fakeSendAnswer{kind: fakeSendRefuse, status: http.StatusTooManyRequests, code: "strangers_busy", retryAfter: "30"}
				}
			})
			id, err := req.Delegate(ctx, prov.AID(), "busy", nil)
			if err != nil {
				t.Fatal(err)
			}
			return req, prov, id
		}},
		{"session_unavailable", func(t *testing.T) (*Daemon, *Daemon, string) {
			srv, req, prov := registeredPair(t)
			setFake(t, srv.URL, func(h *fakeHub) { h.sessionRefuse = "admission_required" })
			req.dropSessions()
			id, err := req.Delegate(ctx, prov.AID(), "no session", nil)
			if err != nil {
				t.Fatal(err)
			}
			return req, prov, id
		}},
		{"credential_refused", func(t *testing.T) (*Daemon, *Daemon, string) {
			srv, req, prov := registeredPair(t)
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
			setFake(t, srv.URL, func(h *fakeHub) {
				h.sendHook = func(env []byte) fakeSendAnswer {
					if o, err := seal.ParseOuter(env); err == nil && o.Access != nil && o.Access.Kind == seal.AccessKindCap {
						return fakeSendAnswer{kind: fakeSendRefuse, status: http.StatusForbidden, code: "access_invalid"}
					}
					return fakeSendAnswer{}
				}
			})
			if err := req.SendMessage(ctx, id, "the answer", nil); err != nil {
				t.Fatal(err)
			}
			return req, prov, id
		}},
	}
	for _, w := range waits {
		t.Run(w.code, func(t *testing.T) {
			req, prov, id := w.setup(t)
			rows := outboxRows(t, req, id)
			if len(rows) == 0 {
				t.Fatal("nothing waits")
			}
			last := rows[len(rows)-1]
			if got := rowWait(t, req, last.ID); got != w.code {
				t.Fatalf("the row waits on %q, want %q (ENG-P1)", got, w.code)
			}
			for _, lang := range []a2ashape.Language{a2ashape.Chinese, a2ashape.English} {
				line := a2ashape.DeliveryText(a2ashape.DeliveryPending, a2ashape.DeliverySent, w.code, false, time.Time{}, lang)
				if line != a2ashape.Sentence(w.code, lang, nil) {
					t.Errorf("%v: the delivery line %q is not the code's sentence", lang, line)
				}
			}
			surfaces := taskSurfaces(t, req, prov.AID(), id)
			for _, s := range surfaces {
				if s.wait != w.code {
					t.Errorf("%s: anet.delivery_wait %q, want %q", s.name, s.wait, w.code)
				}
			}
		})
	}
}

package daemon

// revoke_push_test.go: withdrawals of the delivery credentials this node
// issued — by the peer lists, at start after a crash, and their push to the
// hub (sealed-sender §4.3.8, SSR-D20, SSR-D21, item 4).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// hMailboxRevoke mirrors the hub's POST /mailbox/revoke (ANetHub
// internal/aghub/revoke.go): the owner's relayauth revoke signature, 1 to
// 1000 ids of 16 bytes in strict base64url, {revoked, total}; 429
// revocation_list_full when revokeFull, 503 for the next revokeFail
// requests.
func (h *fakeHub) hMailboxRevoke(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 64<<10)
	if !ok {
		return
	}
	aid := h.verifyAuth(w, r, body, relayauth.ActionRevoke, nil)
	if aid == "" {
		return
	}
	var req hubapi.MailboxRevokeRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || len(req.CapIDs) == 0 || len(req.CapIDs) > hubapi.MailboxRevokeMaxIDs {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	for _, c := range req.CapIDs {
		if _, err := hubapi.DecodeB64(c, seal.CapIDLen); err != nil {
			fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
			return
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.revokeFail > 0 {
		h.revokeFail--
		fakeHubRefuse(w, http.StatusServiceUnavailable, "unavailable", nil)
		return
	}
	if h.revokeFull {
		fakeHubRefuse(w, http.StatusTooManyRequests, reasonRevocationListFull, nil)
		return
	}
	if h.revocations == nil {
		h.revocations = map[string]map[string]bool{}
	}
	set := h.revocations[aid]
	if set == nil {
		set = map[string]bool{}
		h.revocations[aid] = set
	}
	fresh := 0
	for _, c := range req.CapIDs {
		if !set[c] {
			set[c], fresh = true, fresh+1
		}
	}
	h.revokeBatches = append(h.revokeBatches, len(req.CapIDs))
	fakeHubJSON(w, http.StatusOK, hubapi.MailboxRevokeResponse{Revoked: uint64(fresh), Total: uint64(len(set))})
}

// hubRevocations is what the fake hub holds of aid's withdrawals, and the
// sizes of the batches it took.
func hubRevocations(t *testing.T, url, aid string) (int, []int) {
	t.Helper()
	h := fakeHubAt(t, url)
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.revocations[aid]), append([]int(nil), h.revokeBatches...)
}

// keepRevokeLoopOut leaves the push to the test's own revokeTick calls and
// kicks: the loop's ticker and its list pass wait an hour.
func keepRevokeLoopOut(t *testing.T) {
	a, b := revokeTickEvery.set(time.Hour), listsReconcileEvery.set(time.Hour)
	t.Cleanup(func() { revokeTickEvery.set(a); listsReconcileEvery.set(b) })
}

// issueTo writes a caps_issued row for grantee straight into d's store (the
// engine's commit is commitGrantTx; these tests are about what happens to
// rows afterwards). id is the first byte of the cap_id.
func issueTo(t *testing.T, d *Daemon, grantee, scope, ix string, class uint64, id byte) interactions.IssuedCap {
	t.Helper()
	c := interactions.IssuedCap{CapID: append([]byte{id}, bytes.Repeat([]byte{0x5a}, seal.CapIDLen-1)...), Grantee: grantee,
		Class: class, Scope: scope, IX: ix, HolderSeed: bytes.Repeat([]byte{id}, 32), HolderPub: bytes.Repeat([]byte{id}, 32),
		Epoch: 1, CapExp: d.nowMS() + capLifeStrangerMS, IssuedAt: d.nowMS()}
	if err := d.ix.Update(func(tx *interactions.Tx) error { return tx.PutIssuedCap(c) }); err != nil {
		t.Fatal(err)
	}
	return c
}

// issuedState is the (revoked, pushed) state of each listed capability.
func issuedState(t *testing.T, d *Daemon, caps ...interactions.IssuedCap) string {
	t.Helper()
	var out []string
	for _, c := range caps {
		r, err := d.ix.IssuedCap(c.CapID)
		if err != nil {
			out = append(out, "gone")
			continue
		}
		s := "given"
		if r.RevokedAt != 0 {
			s = "withdrawn"
		}
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

// listRoute calls a peer list control route the way the CLI does.
func listRoute(t *testing.T, h http.HandlerFunc, aid string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"aid": aid})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/peers", bytes.NewReader(b)))
	return w
}

// SS §4.3.8 item 1 through the control routes: allowing a peer withdraws
// nothing; removing it from the lists withdraws its contact peer
// credential and nothing else; denying it withdraws everything given to
// it. Each route answers after the withdrawal, which then reaches the hub.
func TestListChangesRevoke(t *testing.T) {
	srv, _, prov := registeredPair(t)
	const g = "did:anet:list-peer-0001"
	contactPeer := issueTo(t, prov, g, "peer", "", seal.CapClassContact, 1)
	strangerPeer := issueTo(t, prov, g, "peer", "", seal.CapClassStranger, 2)
	contactOne := issueTo(t, prov, g, "ix", "ix-1", seal.CapClassContact, 3)
	strangerOne := issueTo(t, prov, g, "ix", "ix-2", seal.CapClassStranger, 4)
	other := issueTo(t, prov, "did:anet:list-peer-0002", "peer", "", seal.CapClassContact, 5)
	all := []interactions.IssuedCap{contactPeer, strangerPeer, contactOne, strangerOne, other}

	if w := listRoute(t, prov.hPeersAllow, g); w.Code != http.StatusOK {
		t.Fatalf("allow: %d %s", w.Code, w.Body)
	}
	if got := issuedState(t, prov, all...); got != "given,given,given,given,given" {
		t.Fatalf("after allow: %s", got)
	}
	if w := listRoute(t, prov.hPeersRemove, g); w.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if got := issuedState(t, prov, all...); got != "withdrawn,given,given,given,given" {
		t.Fatalf("after remove: %s", got)
	}
	if w := listRoute(t, prov.hPeersDeny, g); w.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	if got := issuedState(t, prov, all...); got != "withdrawn,withdrawn,withdrawn,withdrawn,given" {
		t.Fatalf("after deny: %s", got)
	}
	// The route kicked the push: the hub has the four.
	waitUntil(t, "the hub to hold the withdrawals", func() bool {
		n, _ := hubRevocations(t, srv.URL, prov.AID())
		return n == 4
	})
	// Allowing a denied peer gives nothing back, and withdraws whatever it
	// still holds (here written straight into the table): the deny list
	// still names it.
	late := issueTo(t, prov, g, "ix", "ix-3", seal.CapClassStranger, 6)
	if w := listRoute(t, prov.hPeersAllow, g); w.Code != http.StatusOK {
		t.Fatalf("allow again: %d %s", w.Code, w.Body)
	}
	if got := issuedState(t, prov, append(all, late)...); got != "withdrawn,withdrawn,withdrawn,withdrawn,given,withdrawn" {
		t.Fatalf("after allowing a denied peer: %s", got)
	}
}

// The lists edited by hand: the five-minute pass and the start apply them
// to every credential given; a list that cannot be read withdraws nothing.
func TestListsEditedByHandAreApplied(t *testing.T) {
	keepRevokeLoopOut(t)
	d := newTestDaemon(t, "", false)
	const a, b, c = "did:anet:hand-peer-0001", "did:anet:hand-peer-0002", "did:anet:hand-peer-0003"
	allowPeers(t, d, a, b, c)
	ca := issueTo(t, d, a, "peer", "", seal.CapClassContact, 1)
	cb := issueTo(t, d, b, "peer", "", seal.CapClassContact, 2)
	cbOne := issueTo(t, d, b, "ix", "ix-1", seal.CapClassContact, 3)
	cc := issueTo(t, d, c, "ix", "ix-2", seal.CapClassStranger, 4)
	if n, err := d.reconcileCapsWithLists(); err != nil || n != 0 {
		t.Fatalf("lists unchanged: %d %v", n, err)
	}
	// a is taken off by hand, c is denied by hand.
	allow := d.peerFile(d.config().inbound().AllowFile)
	if err := os.WriteFile(allow, []byte(b+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	denyPeers(t, d, c)
	if n, err := d.reconcileCapsWithLists(); err != nil || n != 2 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	if got := issuedState(t, d, ca, cb, cbOne, cc); got != "withdrawn,given,given,withdrawn" {
		t.Fatalf("after the pass: %s", got)
	}
	// A deny list that cannot be read withdraws nothing (readPeers would
	// take it for "deny everyone").
	deny := d.peerFile(d.config().inbound().DenyFile)
	if err := os.Remove(deny); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(deny, 0o700); err != nil {
		t.Fatal(err)
	}
	if n, err := d.reconcileCapsWithLists(); err == nil || n != 0 {
		t.Fatalf("an unreadable list: %d %v", n, err)
	}
	if got := issuedState(t, d, cb, cbOne); got != "given,given" {
		t.Fatalf("an unreadable list withdrew: %s", got)
	}
}

// withdrawn makes n withdrawals of credentials given to grantee in d's
// store, not pushed yet; ids start at first.
func withdrawn(t *testing.T, d *Daemon, grantee string, first, n int) {
	t.Helper()
	now := d.nowMS()
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		for i := first; i < first+n; i++ {
			id := make([]byte, seal.CapIDLen)
			id[0], id[1], id[2] = byte(i>>16), byte(i>>8), byte(i)
			c := interactions.IssuedCap{CapID: id, Grantee: grantee, Class: 2, Scope: "ix", IX: fmt.Sprintf("ix-%d", i),
				HolderSeed: bytes.Repeat([]byte{1}, 32), HolderPub: bytes.Repeat([]byte{1}, 32), Epoch: 1,
				CapExp: now + capLifeStrangerMS, IssuedAt: now}
			if err := tx.PutIssuedCap(c); err != nil {
				return err
			}
		}
		_, err := tx.RevokeIssued(interactions.IssuedFilter{Grantee: grantee}, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func pushState(d *Daemon) (failures int, retryAt time.Time, requests int) {
	d.caps.push.mu.Lock()
	defer d.caps.push.mu.Unlock()
	return d.caps.push.failures, d.caps.push.retryAt, d.caps.push.requests
}

// SSR-D20: withdrawals go to the hub in batches of at most 1000, oldest
// first, and a failed push is tried again 5 seconds later, doubling to 10
// minutes, until the hub has them all.
func TestRevokePushBatchesAndRetries(t *testing.T) {
	keepRevokeLoopOut(t)
	srv, _, prov := registeredPair(t)
	if r := prov.joinSnapshot(); r == nil || r.State != JoinRegistered {
		t.Fatalf("the provider is not registered: %+v", r)
	}
	ctx := context.Background()
	withdrawn(t, prov, "did:anet:batch-peer-0001", 0, 2500)
	start := time.Now()
	prov.revokeTick(ctx, start)
	if n, batches := hubRevocations(t, srv.URL, prov.AID()); n != 2500 || fmt.Sprint(batches) != "[1000 1000 500]" {
		t.Fatalf("the hub holds %d in batches %v", n, batches)
	}
	if left, err := prov.ix.UnpushedRevocations(10); err != nil || len(left) != 0 {
		t.Fatalf("%d not marked pushed (%v)", len(left), err)
	}

	// The hub fails twice.
	withdrawn(t, prov, "did:anet:batch-peer-0002", 3000, 3)
	setFake(t, srv.URL, func(h *fakeHub) { h.revokeFail = 2 })
	_, _, before := pushState(prov)
	prov.revokeTick(ctx, start)
	f, at, req := pushState(prov)
	if f != 1 || !at.Equal(start.Add(5*time.Second)) || req != before+1 {
		t.Fatalf("after one failure: %d failures, retry at +%v, %d requests", f, at.Sub(start), req-before)
	}
	prov.revokeTick(ctx, start.Add(4*time.Second)) // not due
	if _, _, r := pushState(prov); r != req {
		t.Fatal("a push before its retry was due")
	}
	prov.revokeTick(ctx, start.Add(5*time.Second))
	if f, at, _ := pushState(prov); f != 2 || !at.Equal(start.Add(15*time.Second)) {
		t.Fatalf("after two failures: %d, retry at +%v", f, at.Sub(start))
	}
	prov.revokeTick(ctx, start.Add(15*time.Second))
	if f, at, _ := pushState(prov); f != 0 || !at.IsZero() {
		t.Fatalf("after the push went through: %d, %v", f, at)
	}
	if n, _ := hubRevocations(t, srv.URL, prov.AID()); n != 2503 {
		t.Fatalf("the hub holds %d", n)
	}
	for f, want := range map[int]time.Duration{1: 5 * time.Second, 2: 10 * time.Second, 3: 20 * time.Second,
		7: 320 * time.Second, 8: 10 * time.Minute, 50: 10 * time.Minute} {
		if got := revokeRetryAfter(f); got != want {
			t.Errorf("revokeRetryAfter(%d) = %v, want %v", f, got, want)
		}
	}
}

// SSR-D21 (M22, daemon side): a registration answer with fewer withdrawals
// than this node pushed in the last 31 days makes it push them all again;
// one with as many sends nothing.
func TestRegisterRepushesRevocations(t *testing.T) {
	keepRevokeLoopOut(t)
	srv, _, prov := registeredPair(t)
	ctx := context.Background()
	withdrawn(t, prov, "did:anet:repush-peer-0001", 0, 3)
	prov.revokeTick(ctx, time.Now())
	if n, _ := hubRevocations(t, srv.URL, prov.AID()); n != 3 {
		t.Fatalf("the hub holds %d", n)
	}
	_, _, before := pushState(prov)
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil); err != nil {
		t.Fatal(err)
	}
	prov.revokeTick(ctx, time.Now())
	if _, _, after := pushState(prov); after != before {
		t.Fatalf("%d pushes after a registration that holds them all", after-before)
	}
	// The hub is restored from a backup without them.
	setFake(t, srv.URL, func(h *fakeHub) { delete(h.revocations, prov.AID()) })
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil); err != nil {
		t.Fatal(err)
	}
	if left, _ := prov.ix.UnpushedRevocations(10); len(left) != 3 {
		t.Fatalf("%d queued again, want 3", len(left))
	}
	prov.revokeTick(ctx, time.Now())
	if n, _ := hubRevocations(t, srv.URL, prov.AID()); n != 3 {
		t.Fatalf("after the registration the hub holds %d", n)
	}
	// A hub that says nothing is not compared.
	prov.noteRegisterRevocations(nil)
	if left, _ := prov.ix.UnpushedRevocations(10); len(left) != 0 {
		t.Fatalf("an answer without revocations queued %d", len(left))
	}
}

// SS §4.3.8 item 4: a hub that keeps no more withdrawals of this node
// answers revocation_list_full; the node makes a new mailbox signing key,
// empties caps_issued with it and publishes the key set that carries it,
// and the push is not counted as failed.
func TestRevocationListFullRotatesMbox(t *testing.T) {
	keepRevokeLoopOut(t)
	keepMboxLoopOut(t)
	srv, _, prov := registeredPair(t)
	ctx := context.Background()
	before := currentMbox(t, prov)
	issueTo(t, prov, "did:anet:full-peer-0001", "peer", "", seal.CapClassContact, 1)
	withdrawn(t, prov, "did:anet:full-peer-0002", 10, 5)
	setFake(t, srv.URL, func(h *fakeHub) { h.revokeFull = true })
	prov.revokeTick(ctx, time.Now())
	after := currentMbox(t, prov)
	if after.Epoch != before.Epoch+1 || bytes.Equal(after.Pub, before.Pub) {
		t.Fatalf("the mailbox key %d → %d", before.Epoch, after.Epoch)
	}
	if grantees, err := prov.ix.IssuedGrantees(); err != nil || len(grantees) != 0 {
		t.Fatalf("caps_issued still gives to %v (%v)", grantees, err)
	}
	if left, _ := prov.ix.UnpushedRevocations(10); len(left) != 0 {
		t.Fatalf("%d withdrawals left to push", len(left))
	}
	if f, at, _ := pushState(prov); f != 0 || !at.IsZero() {
		t.Fatalf("counted as a failure: %d %v", f, at)
	}
	fake := fakeHubAt(t, srv.URL)
	fake.mu.Lock()
	raw := append([]byte(nil), fake.agents[prov.AID()].keyset...)
	fake.mu.Unlock()
	s, err := seal.UnmarshalSignedEncKeySet(raw)
	if err != nil {
		t.Fatal(err)
	}
	var set seal.EncKeySet
	if err := coredet.Unmarshal(s.Set, &set); err != nil {
		t.Fatal(err)
	}
	if set.Mbox.Epoch != after.Epoch || !bytes.Equal(set.Mbox.Pub, after.Pub) {
		t.Fatalf("the hub holds mailbox key %d, the node %d", set.Mbox.Epoch, after.Epoch)
	}
	if reg, _ := prov.mailboxStatus(); !reg {
		t.Fatal("the new mailbox key is not reported registered")
	}
}

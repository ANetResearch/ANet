package daemon

// Tests of peer_resolve.go: where this node learns another agent's material
// without naming it to a hub (sealed-sender §4.3.7, SSR-D14 to SSR-D18), and
// SS-10's acceptance list: TestNoPeerAIDInHubRequests, TestDirQueryBitsCapped,
// TestSmallBucketWidensQuery, TestRefusalTriggersBypassingRefresh,
// TestResolvePeerSourcesAndVerdicts, TestPartialBucketIsNotNotFound,
// TestEntrySelectionLongestKEL, TestActiveContactRevalidatedOncePer24h,
// TestUnknownRecipientRefreshesOnce, TestKidStaleBypassesCaches.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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

// bucketKeys fetches aid's key set from the directory, bypassing every
// cache, and records it, as the send path does when the stored set has no
// usable key.
func bucketKeys(d *Daemon, aid, pin string) (*peerKeySet, error) {
	m, v := d.resolvePeer(context.Background(), aid, resolveOpts{need: needKeys, bypass: true, record: true, pin: pin})
	if v != verdictFound {
		return nil, &peerResolveError{AID: aid, Verdict: v, Cause: m.Err}
	}
	return m.Keys, nil
}

// overrideDirEntry makes the fake hub file, for aid, an entry of the given
// KEL and key set (both their encodings) instead of the one its
// registration makes: a hub substituting an entry.
func overrideDirEntry(t testing.TB, h *fakeHub, aid string, kel, keys []byte) {
	t.Helper()
	h.setDir(func(h *fakeHub) {
		h.dirOverride[aid] = dirbox.DirEntry{AID: aid, Home: h.self.AID(), KEL: kel, Keys: keys}
	})
}

// entryOf is the directory entry the fake files for an agent registered
// with it, as it stands.
func entryOf(t testing.TB, h *fakeHub, aid string) dirbox.DirEntry {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.agents[aid]
	if a == nil || len(a.keyset) == 0 {
		t.Fatalf("%s is not registered at the fake with a key set", aid)
	}
	return dirbox.DirEntry{AID: aid, Home: h.self.AID(), KEL: append([]byte(nil), a.kel...), Keys: append([]byte(nil), a.keyset...)}
}

// queryCount is how many bucket queries the fake got.
func queryCount(h *fakeHub) int { return len(h.queries()) }

// noNewQuery waits a little for background work and fails when the fake got
// a bucket query beyond n.
func noNewQuery(t *testing.T, h *fakeHub, n int, why string) {
	t.Helper()
	for i := 0; i < 20; i++ {
		if got := queryCount(h); got != n {
			t.Fatalf("%s: %d bucket queries, want %d", why, got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// hashForms are the spellings of aid's full directory hash h a request could
// carry.
func hashForms(t testing.TB, aid string) []string {
	t.Helper()
	h, err := dirbox.Hash(aid)
	if err != nil {
		t.Fatal(err)
	}
	return []string{hex.EncodeToString(h), base64.RawURLEncoding.EncodeToString(h),
		base64.StdEncoding.EncodeToString(h), base64.RawStdEncoding.EncodeToString(h), base64.URLEncoding.EncodeToString(h)}
}

// SSR-D14 (SI-12, M11 on the daemon's side, M19): nothing a node sends a hub
// names another agent — neither its AID nor its full directory hash — in
// the request line, a header or the body, whatever the node does that
// concerns the other agent: a first contact, the messages after it, a card,
// a listing, a pin, a refusal review, a kid_stale refetch, `anet verify`
// through the daemon and without one, and a send ten minutes on, which
// fetches nothing again (the ten-minute recheck is gone). The only
// exceptions are the envelope of /relay/send and its X-ANet-Access (core
// SSR-D14's first item) and the hub's own AID; a signed request may name its
// signer.
func TestNoPeerAIDInHubRequests(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	h := fakeHubAt(t, srv.URL)
	third := newTestDaemon(t, srv.URL, false)
	if err := third.RegisterWithHub(ctx, srv.URL, "Third", nil); err != nil {
		t.Fatal(err)
	}
	fourth := newTestDaemon(t, srv.URL, false)
	if err := fourth.RegisterWithHub(ctx, srv.URL, "Fourth", nil); err != nil {
		t.Fatal(err)
	}
	start := len(hubRequests(t, srv.URL))
	q0 := queryCount(h)

	id, err := req.Delegate(ctx, prov.AID(), "a task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if queryCount(h) == q0 {
		t.Fatal("set-up: the first contact read no bucket")
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := req.SendMessage(ctx, id, "and another thing", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := req.listAgents(ctx, module.AgentQuery{Skill: "x", Query: third.AID(), IncludeUncarded: true}); err != nil {
		t.Fatal(err)
	}
	if ra, err := req.agentCard(ctx, third.AID()); err != nil || ra.Verification == cardUnverified {
		t.Fatalf("third's card: %v %+v", err, ra)
	}
	if err := req.PinPeer(ctx, third.AID(), interactions.PinAllow); err != nil {
		t.Fatal(err)
	}
	if r := req.reviewPeer(ctx, prov.AID()); r.Verdict != reviewFound {
		t.Fatalf("review: %+v", r)
	}
	if _, err := req.refreshForKidStale(ctx, prov.AID()); err != nil {
		t.Fatal(err)
	}
	p := newPlaneFor(t, req, "tok")
	if resp, raw := p.req(t, "POST", "/agents/kel", `{"aid":"`+fourth.AID()+`"}`, p.bearer); resp.StatusCode != 200 {
		t.Fatalf("/agents/kel: %d %s", resp.StatusCode, raw)
	}
	if _, err := DirectoryKEL(ctx, srv.URL, fourth.AID()); err != nil {
		t.Fatal(err)
	}
	// Eleven minutes later the stored key set is used as it is: nothing is
	// fetched again, and the record is not touched (M19).
	row, err := req.ix.PeerIdentity(prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	later := req.nowMS() + 11*60*1000
	req.setClock(func() uint64 { return later })
	q1 := queryCount(h)
	if err := req.SendMessage(ctx, id, "eleven minutes on", nil); err != nil {
		t.Fatal(err)
	}
	noNewQuery(t, h, q1, "a send eleven minutes on")
	if again, _ := req.ix.PeerIdentity(prov.AID()); again.KeysCheckedAt != row.KeysCheckedAt {
		t.Fatal("a send eleven minutes on rechecked the recipient's key set")
	}

	agents := []string{req.AID(), prov.AID(), third.AID(), fourth.AID()}
	forms := map[string][]string{}
	for _, aid := range agents {
		forms[aid] = append([]string{aid, url.PathEscape(aid)}, hashForms(t, aid)...)
	}
	hubAID := hubAIDOf(srv.URL)
	checked := 0
	for _, r := range hubRequests(t, srv.URL)[start:] {
		signer := r.header.Get(hubapi.HeaderAID)
		relaySend := r.path == hubapi.RelaySendPath
		parts := []string{r.method + " " + r.path + "?" + r.query}
		for name, vs := range r.header {
			if relaySend && name == hubapi.HeaderAccess {
				continue // Cap.owner is the envelope's to (SSR-D14 item 1)
			}
			parts = append(parts, name+": "+strings.Join(vs, ","))
		}
		if !relaySend {
			if r.bodyLen > fakeHubKeepBody {
				t.Fatalf("%s %s: a body of %d bytes the test did not keep", r.method, r.path, r.bodyLen)
			}
			parts = append(parts, string(r.body))
		}
		for _, aid := range agents {
			if aid == signer || aid == hubAID {
				continue
			}
			for _, part := range parts {
				for _, f := range forms[aid] {
					if strings.Contains(part, f) {
						t.Errorf("%s %s (signer %q) names %s: %q", r.method, r.path, signer, aid, part)
					}
				}
			}
		}
		if r.path == hubapi.DirectoryBucketPath {
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("set-up: no bucket query was checked")
	}
	for _, q := range h.queries() {
		if q.Bits > hubapi.DirQueryMaxBits {
			t.Errorf("a query of %d bits", q.Bits)
		}
	}
	// Every one of those requests went out on the side the table gives it:
	// none was refused before it was sent as a route without a side, or as
	// an anonymous request that carries a signature or a session (SI-13,
	// hubTransmit). A directory or registry read built with a naming header
	// would fail here rather than go unnoticed in a fallback.
	for _, n := range []*Daemon{req, prov, third, fourth} {
		if got := n.ReceiveStats()[counterHubRouteUnmapped]; got != 0 {
			t.Errorf("%s refused %d of its own hub requests before sending them (%s)", n.AID(), got, counterHubRouteUnmapped)
		}
	}
}

// SSR-D15a, M38: whatever dir_max_bits a hub reports (24 here, in its
// identity and in every answer), a query names at most DirQueryMaxBits
// bits of the hash it is after — the daemon's and the CLI's (anet verify
// with no daemon), which take the width from hubapi.DirQueryBits alone.
func TestDirQueryBitsCapped(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	h := fakeHubAt(t, srv.URL)
	// Enough entries in every bucket that no small-bucket cut narrows the
	// query below the cap.
	setFake(t, srv.URL, func(h *fakeHub) { h.dirMaxBits, h.dirPad = 24, 40 })
	if _, err := req.Delegate(ctx, prov.AID(), "a task", nil); err != nil {
		t.Fatal(err)
	}
	kel, err := DirectoryKEL(ctx, srv.URL, prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	if len(kel) != len(prov.self.KEL()) {
		t.Fatalf("the CLI's lookup found a KEL of %d events", len(kel))
	}
	qs := h.queries()
	if len(qs) < 2 {
		t.Fatalf("%d bucket queries, want the daemon's and the CLI's", len(qs))
	}
	for i, q := range qs {
		if q.Bits != hubapi.DirQueryMaxBits {
			t.Errorf("query %d: %d bits, want %d", i, q.Bits, hubapi.DirQueryMaxBits)
		}
		if p, err := hubapi.DecodeB64(q.Prefix, -1); err != nil || len(p) != (hubapi.DirQueryMaxBits+7)/8 {
			t.Errorf("query %d: prefix %q", i, q.Prefix)
		}
	}
	for _, c := range []struct{ maxBits, cut, want int }{{24, 0, 10}, {256, 0, 10}, {10, 0, 10}, {9, 0, 9},
		{0, 0, 0}, {-3, 0, 0}, {24, 3, 7}, {5, 9, 0}, {24, -1, 10}} {
		if got := hubapi.DirQueryBits(c.maxBits, c.cut); got != c.want {
			t.Errorf("DirQueryBits(%d, %d) = %d, want %d", c.maxBits, c.cut, got, c.want)
		}
	}
}

// SSR-D15a, M39: a bucket narrower than the whole directory that holds fewer
// than 32 entries in a whole (not partial) answer is counted, and every
// later query to that hub is one bit wider, down to the whole directory; a
// partial one is not; small_buckets_24h counts them; a restart forgets the
// cut.
func TestSmallBucketWidensQuery(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	h := fakeHubAt(t, srv.URL)
	setFake(t, srv.URL, func(h *fakeHub) { h.dirMaxBits, h.dirPad = 10, 40 })
	bitsOfNext := func(d *Daemon) int {
		t.Helper()
		n := queryCount(h)
		d.resolvePeer(ctx, prov.AID(), resolveOpts{need: needKeys, bypass: true})
		qs := h.queries()
		if len(qs) != n+1 {
			t.Fatalf("%d queries for one lookup", len(qs)-n)
		}
		return qs[len(qs)-1].Bits
	}
	// Full buckets: the width stays.
	if b := bitsOfNext(req); b != 10 {
		t.Fatalf("a full bucket: %d bits", b)
	}
	if b := bitsOfNext(req); b != 10 || counter(req, cntDirBucketSmall) != 0 {
		t.Fatalf("after a full bucket: %d bits, %d small", b, counter(req, cntDirBucketSmall))
	}
	// Small buckets: each one widens the next query by a bit.
	setFake(t, srv.URL, func(h *fakeHub) { h.dirPad = 0 })
	if b := bitsOfNext(req); b != 10 {
		t.Fatalf("the first small bucket was asked at %d bits", b)
	}
	if b := bitsOfNext(req); b != 9 {
		t.Fatalf("after one small bucket: %d bits, want 9", b)
	}
	if b := bitsOfNext(req); b != 8 || counter(req, cntDirBucketSmall) != 3 || req.smallBuckets24h(req.nowMS()) != 3 {
		t.Fatalf("after two small buckets: %d bits, %d counted, %d in 24h", b, counter(req, cntDirBucketSmall),
			req.smallBuckets24h(req.nowMS()))
	}
	// A partial bucket is not taken for a small one.
	setFake(t, srv.URL, func(h *fakeHub) { h.dirPartial = true })
	bitsOfNext(req)
	if b := bitsOfNext(req); b != 7 || counter(req, cntDirBucketSmall) != 3 {
		t.Fatalf("after a partial small bucket: %d bits, %d counted", b, counter(req, cntDirBucketSmall))
	}
	// A day on, the count is gone; the cut is not.
	if n := req.smallBuckets24h(req.nowMS() + smallBucketsWindowMS); n != 0 {
		t.Fatalf("%d small buckets a day later", n)
	}
	// A restart forgets the cut.
	setFake(t, srv.URL, func(h *fakeHub) { h.dirPartial = false })
	again := restartDaemon(t, req)
	if b := bitsOfNext(again); b != 10 {
		t.Fatalf("after a restart: %d bits, want 10", b)
	}
	// The whole directory is never cut: bits 0 is not a small bucket.
	setFake(t, srv.URL, func(h *fakeHub) { h.dirMaxBits = 0 })
	again2 := restartDaemon(t, again)
	for i := 0; i < 2; i++ {
		if b := bitsOfNext(again2); b != 0 {
			t.Fatalf("the whole directory asked at %d bits", b)
		}
	}
	if counter(again2, cntDirBucketSmall) != 0 {
		t.Fatal("the whole directory was counted a small bucket")
	}
}

// SSR-D11 item 1: after a 403 access_invalid the peer's key set is fetched
// again through a bucket, past this node's record and every cache, at most
// once a minute per peer; refusals within the minute, concurrent ones
// included, take the same review. A higher mailbox epoch it finds explains
// the refusal (onAccessRefused invalidates the capability) and is recorded,
// which voids the capability in the same write (SSR-D19).
func TestRefusalTriggersBypassingRefresh(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	h := fakeHubAt(t, srv.URL)
	if _, err := req.Delegate(ctx, prov.AID(), "a task", nil); err != nil {
		t.Fatal(err)
	}
	held := heldCapFrom(t, prov, req)
	q0 := queryCount(h)
	r := req.reviewPeer(ctx, prov.AID())
	if r.Verdict != reviewFound || r.Epoch != held.Epoch || queryCount(h) != q0+1 {
		t.Fatalf("review %+v after %d queries; want found at epoch %d after one", r, queryCount(h)-q0, held.Epoch)
	}
	if dec := onAccessRefused(held, r, req.nowMS()); dec.Action != refusalWait {
		t.Fatalf("a review at the same epoch explains the refusal: %+v", dec)
	}
	// Within the minute: the same review, nothing asked, however many ask.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if again := req.reviewPeer(ctx, prov.AID()); again != r {
				t.Errorf("a review within the minute: %+v", again)
			}
		}()
	}
	wg.Wait()
	if n := queryCount(h); n != q0+1 {
		t.Fatalf("%d queries within the minute, want 1", n-q0)
	}
	// The provider moves to a new mailbox key; a minute on, the review sees
	// it and the capability of the old epoch is void.
	if err := prov.ix.Update(func(tx *interactions.Tx) error { _, err := prov.rotateMboxTx(tx); return err }); err != nil {
		t.Fatal(err)
	}
	if err := prov.republishKeys(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	later := req.nowMS() + reviewEveryMS + 1
	req.setClock(func() uint64 { return later })
	var concurrent [4]peerReview
	for i := range concurrent {
		wg.Add(1)
		go func() { defer wg.Done(); concurrent[i] = req.reviewPeer(ctx, prov.AID()) }()
	}
	wg.Wait()
	r2 := concurrent[0]
	for _, c := range concurrent {
		if c != r2 {
			t.Fatalf("concurrent reviews differ: %+v", concurrent)
		}
	}
	if n := queryCount(h); n != q0+2 {
		t.Fatalf("%d queries for a minute later, want one more", n-q0-1)
	}
	if r2.Verdict != reviewFound || r2.Epoch <= held.Epoch {
		t.Fatalf("the review a minute on: %+v, want an epoch above %d", r2, held.Epoch)
	}
	if dec := onAccessRefused(held, r2, later); dec.Action != refusalInvalidate {
		t.Fatalf("a higher epoch does not explain the refusal: %+v", dec)
	}
	now, err := req.ix.HeldCaps(prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range now {
		if bytes.Equal(c.CapID, held.CapID) && c.InvalidAt == 0 {
			t.Fatal("the capability of the older epoch is still valid after the record took the new one")
		}
	}
	// A peer the directory no longer shows: not found, which explains
	// nothing (a hub can hide an entry).
	setFake(t, srv.URL, func(h *fakeHub) { h.dirHide = map[string]bool{prov.AID(): true} })
	later2 := later + reviewEveryMS + 1
	req.setClock(func() uint64 { return later2 })
	if r3 := req.reviewPeer(ctx, prov.AID()); r3.Verdict != reviewNotFound {
		t.Fatalf("a hidden entry: %+v", r3)
	}
}

// SSR-D15: the three sources in order — this node's record, the discovery
// cache (what a listing of the registry returned, verified here), a bucket —
// and the four verdicts.
func TestResolvePeerSourcesAndVerdicts(t *testing.T) {
	ctx := context.Background()
	t.Run("sources", func(t *testing.T) {
		h := newRegistryHub(t)
		a := newCardAgent(t, "Alpha", "first", "text.stats")
		b := newCardAgent(t, "Beta", "second", "text.stats")
		h.list(a, nil)
		h.list(b, nil)
		d := newTestDaemon(t, h.srv.URL, false)
		buckets := func() int {
			n := 0
			for _, u := range h.requests() {
				if strings.HasPrefix(u, hubapi.DirectoryBucketPath) {
					n++
				}
			}
			return n
		}
		// Never heard of: a bucket.
		m, v := d.resolvePeer(ctx, a.aid, resolveOpts{need: needCard})
		if v != verdictFound || m.Source != sourceBucket || string(m.Card) != string(a.card) || m.Home != h.self.AID() || buckets() != 1 {
			t.Fatalf("from a bucket: %v %+v after %d queries", v, m, buckets())
		}
		// Listed: the discovery cache, and no bucket.
		if _, _, err := d.listAgents(ctx, module.AgentQuery{}); err != nil {
			t.Fatal(err)
		}
		m, v = d.resolvePeer(ctx, b.aid, resolveOpts{need: needKeys})
		if v != verdictFound || m.Source != sourceDiscovery || m.Keys == nil || buckets() != 1 {
			t.Fatalf("from the discovery cache: %v source %v keys %v after %d queries", v, m.Source, m.Keys != nil, buckets())
		}
		// Recorded: this node's own record comes first.
		if _, v := d.resolvePeer(ctx, b.aid, resolveOpts{need: needKeys, record: true}); v != verdictFound {
			t.Fatal(v)
		}
		if m, v := d.resolvePeer(ctx, b.aid, resolveOpts{need: needKeys}); v != verdictFound || m.Source != sourceLocal {
			t.Fatalf("from the record: %v source %v", v, m.Source)
		}
		// A card is never in the record: the discovery cache has it.
		if m, v := d.resolvePeer(ctx, b.aid, resolveOpts{need: needCard}); v != verdictFound || m.Source != sourceDiscovery ||
			string(m.Card) != string(b.card) {
			t.Fatalf("a recorded peer's card: %v source %v", v, m.Source)
		}
		// Past every cache: a bucket again.
		if m, v := d.resolvePeer(ctx, b.aid, resolveOpts{need: needKeys, bypass: true}); v != verdictFound || m.Source != sourceBucket || buckets() != 2 {
			t.Fatalf("bypassing: %v source %v after %d queries", v, m.Source, buckets())
		}
	})
	t.Run("verdicts", func(t *testing.T) {
		srv, req, prov := registeredPair(t)
		h := fakeHubAt(t, srv.URL)
		if _, v := req.resolvePeer(ctx, prov.AID(), resolveOpts{need: needKeys}); v != verdictFound {
			t.Fatalf("a registered agent: %v", v)
		}
		nobody := newStranger(t)
		if m, v := req.resolvePeer(ctx, nobody.aid, resolveOpts{need: needKeys}); v != verdictNotFound || m.Err == nil {
			t.Fatalf("an agent no hub knows: %v %v", v, m.Err)
		}
		if _, v := req.resolvePeer(ctx, "not an aid", resolveOpts{need: needKeys}); v != verdictNotFound {
			t.Fatalf("not an AID: %v", v)
		}
		mallory := newStranger(t)
		kel, _ := identity.MarshalKEL(mallory.kel)
		forged := newStranger(t)
		overrideDirEntry(t, h, forged.aid, kel, mallory.keys)
		if _, v := req.resolvePeer(ctx, forged.aid, resolveOpts{need: needKeys}); v != verdictUnverifiable {
			t.Fatalf("an entry that does not verify: %v", v)
		}
		setFake(t, srv.URL, func(h *fakeHub) { h.dirDown = true })
		if _, v := req.resolvePeer(ctx, nobody.aid, resolveOpts{need: needKeys, bypass: true}); v != verdictUnavailable {
			t.Fatalf("a hub that does not answer: %v", v)
		}
		setFake(t, srv.URL, func(h *fakeHub) { h.dirDown, h.dirPartial = false, true })
		if _, v := req.resolvePeer(ctx, nobody.aid, resolveOpts{need: needKeys, bypass: true}); v != verdictUnavailable {
			t.Fatalf("a partial answer without the agent: %v", v)
		}
		if _, v := req.resolvePeer(ctx, prov.AID(), resolveOpts{need: needKeys, bypass: true}); v != verdictFound {
			t.Fatalf("a partial answer with the agent: %v", v)
		}
		// A new delegation's lookup says which (engine ENG-Q5): the two
		// refusals are refused at once with their codes, a hub that
		// cannot be asked queues the delegation waiting on
		// home_unreachable.
		for v, want := range map[peerVerdict][2]string{verdictNotFound: {reasonRecipientUnknown, ""},
			verdictUnverifiable: {reasonRecipientUnverifiable, ""}, verdictUnavailable: {"", reasonHomeUnreachable}} {
			wait, err := strictOutcome(&peerResolveError{AID: nobody.aid, Verdict: v})
			var ref *a2ashape.Refusal
			refused := ""
			if errors.As(err, &ref) {
				refused = ref.Code()
			}
			if refused != want[0] || lastErrorCode(wait) != want[1] {
				t.Errorf("%v: refused %q, waits on %q", v, refused, wait)
			}
		}
		// A cached bucket that lacks an agent is asked again before the
		// agent is said not to exist: it may have registered since.
		setFake(t, srv.URL, func(h *fakeHub) { h.dirPartial = false })
		if _, v := req.resolvePeer(ctx, nobody.aid, resolveOpts{need: needKeys}); v != verdictNotFound {
			t.Fatal(v)
		}
		late := newTestDaemon(t, srv.URL, false)
		if err := late.RegisterWithHub(ctx, srv.URL, "Late", nil); err != nil {
			t.Fatal(err)
		}
		if _, v := req.resolvePeer(ctx, late.AID(), resolveOpts{need: needKeys}); v != verdictFound {
			t.Fatalf("an agent that registered after the bucket was cached: %v", v)
		}
	})
}

// SSR-W21, SSR-D15, M36 (the daemon's side): an agent missing from a
// partial bucket — a peer hub did not answer — is temporarily unavailable,
// never unknown: a delegation to it is queued, not failed. Once the peer
// hub answers, the agent is found, its entry naming that hub as its home,
// which becomes the first contact's home hint (SSR-D13).
func TestPartialBucketIsNotNotFound(t *testing.T) {
	ctx := context.Background()
	srvA, srvB := newFakeHub(t), newFakeHub(t)
	hA, hB := fakeHubAt(t, srvA.URL), fakeHubAt(t, srvB.URL)
	setFake(t, srvA.URL, func(h *fakeHub) { h.peers = []*fakeHub{hB} })
	req := newTestDaemon(t, srvA.URL, false)
	prov := newTestDaemon(t, srvB.URL, true)
	if err := req.RegisterWithHub(ctx, srvA.URL, "Req", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srvB.URL, "Prov", nil); err != nil {
		t.Fatal(err)
	}
	setFake(t, srvB.URL, func(h *fakeHub) { h.dirDown = true })
	m, v := req.resolvePeer(ctx, prov.AID(), resolveOpts{need: needKeys})
	if v != verdictUnavailable || m.Err == nil || !strings.Contains(m.Err.Error(), "partial") {
		t.Fatalf("an agent behind a peer hub that does not answer: %v %v", v, m.Err)
	}
	id, err := req.Delegate(ctx, prov.AID(), "a task", nil)
	if err != nil {
		t.Fatalf("the delegation failed rather than wait: %v", err)
	}
	ix, err := req.ix.Get(id)
	if err != nil || ix.State == interactions.StateFailed {
		t.Fatalf("the task: %+v %v", ix, err)
	}
	if items, err := req.ix.OutboxRows(id); err != nil || len(items) != 1 {
		t.Fatalf("the queued delegation: %d rows %v", len(items), err)
	}
	setFake(t, srvB.URL, func(h *fakeHub) { h.dirDown = false })
	m, v = req.resolvePeer(ctx, prov.AID(), resolveOpts{need: needKeys, bypass: true})
	if v != verdictFound || m.Home != hB.self.AID() {
		t.Fatalf("once the peer hub answers: %v home %q, want %s", v, m.Home, hB.self.AID())
	}
	if hint := req.kindZeroHome(prov.AID(), ""); hint != hB.self.AID() {
		t.Fatalf("the first contact's home hint is %q, want the entry's home %s", hint, hB.self.AID())
	}
	_ = hA
}

// SSR-W22: every entry under the agent's hash is opened and verified on its
// own, and the one with the longest KEL is taken, wherever it is in the
// answer; an entry that does not open is skipped and counted; two entries
// whose KELs fork make the agent unverifiable. A card that does not verify
// is withheld without costing the entry its key set.
func TestEntrySelectionLongestKEL(t *testing.T) {
	ctx := context.Background()
	srv := newFakeHub(t)
	req := newTestDaemon(t, srv.URL, false)
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil); err != nil {
		t.Fatal(err)
	}
	h := fakeHubAt(t, srv.URL)
	// An agent the suite witness witnesses: each entry carries its notice
	// (the fake passes it on), without which none would be sealed to (KEL
	// D-26).
	s := witnessedIdentity(t)
	exported, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	fork, err := identity.Restore(exported)
	if err != nil {
		t.Fatal(err)
	}
	entry := func(c *identity.Controller, card []byte) dirbox.DirEntry {
		kel, err := identity.MarshalKEL(c.KEL())
		if err != nil {
			t.Fatal(err)
		}
		return dirbox.DirEntry{AID: c.AID(), Home: h.self.AID(), KEL: kel, Keys: signedKeysFor(t, c, 0), Card: card}
	}
	short := entry(s, nil)
	now := uint64(time.Now().UnixMilli())
	if err := identitytest.Rotate(s, now-1000); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // a later key set seq
	long := entry(s, nil)
	sh, _ := dirbox.Hash(s.AID())
	garbage := hubapi.DirBucketEntry{H: hubapi.B64(sh), Box: hubapi.B64(bytes.Repeat([]byte{7}, 80))}
	for i, order := range [][]dirbox.DirEntry{{short, long}, {long, short}} {
		h.setDir(func(h *fakeHub) { h.dirExtra[s.AID()], h.dirRaw = order, []hubapi.DirBucketEntry{garbage} })
		skipped := counter(req, cntDirEntrySkipped)
		m, v := req.resolvePeer(ctx, s.AID(), resolveOpts{need: needKeys, bypass: true})
		if v != verdictFound || len(m.KEL) != len(s.KEL()) || !bytes.Equal(m.Keys.signed, long.Keys) {
			t.Fatalf("order %d: %v, a KEL of %d events, want the longer (%d)", i, v, len(m.KEL), len(s.KEL()))
		}
		if counter(req, cntDirEntrySkipped) != skipped+1 {
			t.Fatalf("order %d: the entry that does not open was not counted", i)
		}
	}
	// A fork of the history from before the rotation, verifying on its own.
	if err := identitytest.Rotate(fork, now-2000); err != nil {
		t.Fatal(err)
	}
	forked := entry(fork, nil)
	forks := counter(req, dropKELFork)
	h.setDir(func(h *fakeHub) { h.dirExtra[s.AID()] = []dirbox.DirEntry{long, forked} })
	if m, v := req.resolvePeer(ctx, s.AID(), resolveOpts{need: needKeys, bypass: true}); v != verdictUnverifiable ||
		!errors.Is(m.Err, identity.ErrKELFork) || counter(req, dropKELFork) != forks+1 {
		t.Fatalf("two forked entries: %v %v", v, m.Err)
	}
	// A card that does not verify: the entry still serves its key set, the
	// card is withheld with the reason.
	bad := entry(s, []byte(`{"name":"not signed"}`))
	h.setDir(func(h *fakeHub) { h.dirExtra[s.AID()] = []dirbox.DirEntry{bad} })
	m, v := req.resolvePeer(ctx, s.AID(), resolveOpts{need: needCard, bypass: true})
	if v != verdictFound || m.Card != nil || m.CardErr == "" || m.Keys == nil {
		t.Fatalf("an entry with a bad card: %v card %q (%s), keys %v", v, m.Card, m.CardErr, m.Keys != nil)
	}
	// Two entries of one KEL whose key sets differ at the same seq: the
	// agent's own signature on both, and no way to tell which one it
	// meant. Neither is sealed to.
	seq := uint64(time.Now().UnixMilli())
	one, two := entry(s, nil), entry(s, nil)
	one.Keys, two.Keys = signedKeysFor(t, s, seq), signedKeysFor(t, s, seq)
	keyForks := counter(req, keysFork)
	h.setDir(func(h *fakeHub) { h.dirExtra[s.AID()] = []dirbox.DirEntry{one, two} })
	if m, v := req.resolvePeer(ctx, s.AID(), resolveOpts{need: needKeys, bypass: true}); v != verdictUnverifiable ||
		counter(req, keysFork) != keyForks+1 {
		t.Fatalf("two key sets at one seq: %v (%v), %d key-set forks counted", v, m.Err, counter(req, keysFork)-keyForks)
	}
}

// SSR-D18: an active contact's stored key set is used as it is, and looked
// up in the directory again — in the background, at a send — only once a
// day: not at eleven minutes, not at twenty-three hours; past a day once,
// which picks up the set the peer published since, and not again for a
// day.
func TestActiveContactRevalidatedOncePer24h(t *testing.T) {
	srv, req, prov := plainRegisteredPair(t)
	ctx := context.Background()
	h := fakeHubAt(t, srv.URL)
	// The witness's notices the directory passes on are observed at the
	// requester's time, which the test moves days ahead: the recheck's
	// material comes with a fresh one, without which it is not recorded
	// (KEL D-26 item 4; TestRefreshWithoutNoticeKeepsStoredRecord).
	setFake(t, srv.URL, func(h *fakeHub) { h.noticeAt = req.nowMS })
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov) // the sends below are follow-ups (engine ENG-O2)
	row, err := req.ix.PeerIdentity(prov.AID())
	if err != nil || row.PinnedReason != interactions.PinOutbound {
		t.Fatalf("the recipient's record: %+v %v", row, err)
	}
	seq0, checked0 := row.KeySetSeq, uint64(row.KeysCheckedAt)
	// The provider rotates its encryption key and publishes the new set.
	start := uint64(time.Now().UnixMilli())
	prov.setClock(func() uint64 { return start - start%seal.EpochMS + seal.EpochMS + 60_000 })
	prov.maintainKeyRing()
	newSeq := prov.enc.Seq()
	if newSeq <= seq0 {
		t.Fatal("the provider did not rotate")
	}
	send := func(at uint64, text string) {
		t.Helper()
		req.setClock(func() uint64 { return at })
		if err := req.SendMessage(ctx, id, text, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, after := range []uint64{11 * 60 * 1000, 23 * 3600 * 1000} {
		q := queryCount(h)
		send(checked0+after, "not yet")
		noNewQuery(t, h, q, "a send within the day")
		if row, _ := req.ix.PeerIdentity(prov.AID()); row.KeySetSeq != seq0 || uint64(row.KeysCheckedAt) != checked0 {
			t.Fatalf("%d ms on, the stored set was fetched again", after)
		}
	}
	q := queryCount(h)
	day := checked0 + activeRecheckMS + 60*1000
	send(day, "a day on")
	waitUntil(t, "the recheck to store the provider's new key set", func() bool {
		row, err := req.ix.PeerIdentity(prov.AID())
		return err == nil && row.KeySetSeq == newSeq
	})
	if n := queryCount(h); n != q+1 {
		t.Fatalf("the recheck made %d queries, want 1", n-q)
	}
	q = queryCount(h)
	send(day+2*3600*1000, "two hours later")
	noNewQuery(t, h, q, "a send two hours after the recheck")
	// A recheck that finds nothing — the directory no longer lists the
	// peer — keeps the stored set and its time, and is not made again for
	// a day all the same.
	setFake(t, srv.URL, func(h *fakeHub) { h.dirHide = map[string]bool{prov.AID(): true} })
	row, _ = req.ix.PeerIdentity(prov.AID())
	twoDays := uint64(row.KeysCheckedAt) + activeRecheckMS + 60*1000
	q = queryCount(h)
	send(twoDays, "a day after the recheck")
	waitUntil(t, "the second recheck", func() bool { return queryCount(h) == q+1 })
	time.Sleep(50 * time.Millisecond)
	send(twoDays+3600*1000, "an hour after a recheck that found nothing")
	noNewQuery(t, h, q+1, "a send an hour after a recheck that found nothing")
	if again, _ := req.ix.PeerIdentity(prov.AID()); again.KeysCheckedAt != row.KeysCheckedAt || again.KeySetSeq != newSeq {
		t.Fatal("a recheck that found nothing changed the stored record")
	}
}

// SSR-D12, M35: a first contact the hub answers 404 recipient_unknown looks
// the recipient up in the directory again before it gives up — once, when
// that was not done in the last hour — and, the home hint having changed,
// sends the same envelope again at once; a second 404 within the hour does
// not look again, and the message is given up.
func TestUnknownRecipientRefreshesOnce(t *testing.T) {
	// The requester is witnessed (a node no witness confirms seals
	// nothing, engine ENG-K13); the provider names none, as nothing here
	// is received: the hub's answers to a first contact are the subject.
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newSuiteWitnessedTestDaemon(t, srv.URL, false)
	prov := newPlainTestDaemon(t, srv.URL, true)
	for _, d := range []*Daemon{req, prov} {
		if err := d.RegisterWithHub(ctx, srv.URL, "x", nil); err != nil {
			t.Fatal(err)
		}
	}
	h := fakeHubAt(t, srv.URL)
	moved, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	// The directory names another home for the provider; the requester
	// learns it. The requester holds a record of the provider (one it had
	// before: a provider that names no witness has no notice, and directory
	// material without one is never recorded, KEL D-26), so the home hint
	// is the directory's.
	old := entryOf(t, h, prov.AID())
	old.Home = moved.AID()
	h.setDir(func(h *fakeHub) { h.dirOverride[prov.AID()] = old })
	m, v := req.resolvePeer(ctx, prov.AID(), resolveOpts{need: needKeySet})
	if v != verdictFound || req.peerDirHome(prov.AID()) != moved.AID() {
		t.Fatalf("set-up: %v home %q", v, req.peerDirHome(prov.AID()))
	}
	if err := req.notePeer(prov.AID(), m.KEL, m.Keys, "", true); err != nil {
		t.Fatal(err)
	}
	// The provider's entry now names this hub, which finds it only with
	// that hint. An hour on, the requester's material is due a refresh.
	h.setDir(func(h *fakeHub) {
		delete(h.dirOverride, prov.AID())
		h.homeMustBe = map[string]string{prov.AID(): h.self.AID()}
	})
	later := req.nowMS() + unknownRefreshMS + 60*1000
	req.setClock(func() uint64 { return later })
	q0 := queryCount(h)
	id, err := req.Delegate(ctx, prov.AID(), "a task", nil)
	if err != nil {
		t.Fatalf("the delegation was given up although the refresh found its home: %v", err)
	}
	if n := queryCount(h); n != q0+1 {
		t.Fatalf("%d refreshes, want 1", n-q0)
	}
	if req.peerDirHome(prov.AID()) != h.self.AID() || len(queuedFor(t, srv, prov.AID())) != 1 {
		t.Fatalf("home %q, %d queued", req.peerDirHome(prov.AID()), len(queuedFor(t, srv, prov.AID())))
	}
	if ix, err := req.ix.Get(id); err != nil || ix.State == interactions.StateFailed {
		t.Fatalf("the task: %+v %v", ix, err)
	}
	// A recipient the hub still does not find, within the hour: no second
	// refresh, and the message is given up as recipient_unknown.
	setFake(t, srv.URL, func(h *fakeHub) { h.homeMustBe[prov.AID()] = moved.AID() })
	_, err = req.Delegate(ctx, prov.AID(), "another task", nil)
	if !errors.Is(err, errUndeliverable) || !strings.Contains(err.Error(), reasonRecipientUnknown) {
		t.Fatalf("a recipient still unknown: %v", err)
	}
	if n := queryCount(h); n != q0+1 {
		t.Fatalf("%d refreshes within the hour, want still 1", n-q0)
	}
}

// Engine ENG-R11 (SSR-D18's first item): after kid_stale the recipient's key
// set is fetched again past this node's record and every cache; the newer
// set the recipient published is recorded and returned.
func TestKidStaleBypassesCaches(t *testing.T) {
	srv, req, prov := plainRegisteredPair(t)
	ctx := context.Background()
	h := fakeHubAt(t, srv.URL)
	if _, err := req.Delegate(ctx, prov.AID(), "task", nil); err != nil {
		t.Fatal(err)
	}
	row, _ := req.ix.PeerIdentity(prov.AID())
	seq0 := row.KeySetSeq
	start := uint64(time.Now().UnixMilli())
	prov.setClock(func() uint64 { return start - start%seal.EpochMS + seal.EpochMS + 60_000 })
	prov.maintainKeyRing()
	newSeq := prov.enc.Seq()
	if newSeq <= seq0 {
		t.Fatal("the provider did not rotate")
	}
	q0 := queryCount(h)
	// Without the bypass the record answers, and nothing is asked.
	if m, v := req.resolvePeer(ctx, prov.AID(), resolveOpts{need: needKeys}); v != verdictFound || m.Source != sourceLocal ||
		m.Keys.set.Seq != seq0 || queryCount(h) != q0 {
		t.Fatalf("set-up: %v source %v seq %d", v, m.Source, m.Keys.set.Seq)
	}
	ks, err := req.refreshForKidStale(ctx, prov.AID())
	if err != nil || ks.set.Seq != newSeq || queryCount(h) != q0+1 {
		t.Fatalf("after kid_stale: %v seq %d after %d queries, want %d after 1", err, ks.set.Seq, queryCount(h)-q0, newSeq)
	}
	if row, _ := req.ix.PeerIdentity(prov.AID()); row.KeySetSeq != newSeq {
		t.Fatalf("the newer set was not recorded (seq %d)", row.KeySetSeq)
	}
	// And from a cached bucket that is older still: asked again.
	if _, err := req.refreshForKidStale(ctx, prov.AID()); err != nil || queryCount(h) != q0+2 {
		t.Fatalf("a second kid_stale: %v after %d queries", err, queryCount(h)-q0)
	}
	_ = seal.EpochMS
}

// A hub that refuses a query as narrower than its dir_max_bits (400
// bucket_too_narrow, SSR-D15; the dir_max_bits this node held was an hour
// old) is asked once more, at the max_bits it gave, which only ever widens
// the bucket; later queries take that width at once.
func TestBucketTooNarrowIsAskedOnceWider(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	h := fakeHubAt(t, srv.URL)
	setFake(t, srv.URL, func(h *fakeHub) { h.dirMaxBits, h.dirPad = 10, 40 })
	restarted := restartDaemon(t, req) // forgets any dir_max_bits read before
	if _, v := restarted.resolvePeer(ctx, prov.AID(), resolveOpts{need: needKeys, bypass: true}); v != verdictFound {
		t.Fatalf("set-up: %v", v)
	}
	if qs := h.queries(); qs[len(qs)-1].Bits != 10 {
		t.Fatalf("set-up: the query was %d bits wide", qs[len(qs)-1].Bits)
	}
	// The hub's directory shrank since: it now answers at most 6 bits.
	setFake(t, srv.URL, func(h *fakeHub) { h.dirMaxBits = 6 })
	n, narrow := queryCount(h), counter(restarted, cntDirBucketNarrow)
	if m, v := restarted.resolvePeer(ctx, prov.AID(), resolveOpts{need: needKeys, bypass: true}); v != verdictFound {
		t.Fatalf("after bucket_too_narrow: %v (%v)", v, m.Err)
	}
	qs := h.queries()
	if len(qs) != n+2 || qs[n].Bits != 10 || qs[n+1].Bits != 6 || counter(restarted, cntDirBucketNarrow) != narrow+1 {
		t.Fatalf("%d queries (want 2: 10 bits, then 6), %d refusals counted", len(qs)-n, counter(restarted, cntDirBucketNarrow)-narrow)
	}
	n = queryCount(h)
	restarted.resolvePeer(ctx, prov.AID(), resolveOpts{need: needKeys, bypass: true})
	if qs := h.queries(); len(qs) != n+1 || qs[n].Bits != 6 {
		t.Fatalf("the next lookup: %d queries, the last %d bits wide", len(qs)-n, qs[len(qs)-1].Bits)
	}
}

// DirectoryKEL (anet verify with no daemon) returns the key history of the
// AID it was asked about and of no one else: an entry whose KEL replays to
// another AID is passed over, the longest of the agent's own is taken, two
// that disagree are an error, and an agent missing from a partial answer is
// "try again", not "not in the directory".
func TestDirectoryKELTakesOnlyTheAgentsHistory(t *testing.T) {
	srv, _, prov := registeredPair(t)
	ctx := context.Background()
	h := fakeHubAt(t, srv.URL)
	marshal := func(kel []identity.SignedEvent) []byte {
		t.Helper()
		b, err := identity.MarshalKEL(kel)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// An entry filed under the agent's hash carrying another identity's
	// history, longer than the agent's own.
	mallory, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	if err := identitytest.Rotate(mallory, uint64(time.Now().UnixMilli())-1000); err != nil {
		t.Fatal(err)
	}
	impostor := dirbox.DirEntry{AID: prov.AID(), Home: h.self.AID(), KEL: marshal(mallory.KEL()),
		Keys: signedKeysFor(t, mallory, 0)}
	h.setDir(func(h *fakeHub) { h.dirExtra[prov.AID()] = []dirbox.DirEntry{impostor} })
	kel, err := DirectoryKEL(ctx, srv.URL, prov.AID())
	if err != nil || !bytes.Equal(marshal(kel), marshal(prov.self.KEL())) {
		t.Fatalf("an agent with an impostor's entry beside its own: %v", err)
	}
	stranger := newStranger(t)
	theirs := dirbox.DirEntry{AID: stranger.aid, Home: h.self.AID(), KEL: marshal(mallory.KEL()), Keys: signedKeysFor(t, mallory, 0)}
	h.setDir(func(h *fakeHub) { h.dirOverride[stranger.aid] = theirs })
	if kel, err := DirectoryKEL(ctx, srv.URL, stranger.aid); err == nil {
		t.Fatalf("an entry holding only another identity's history was returned (%d events)", len(kel))
	}
	// Two histories of one identity that fork.
	s, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	exported, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	fork, err := identity.Restore(exported)
	if err != nil {
		t.Fatal(err)
	}
	short := dirbox.DirEntry{AID: s.AID(), Home: h.self.AID(), KEL: marshal(s.KEL()), Keys: signedKeysFor(t, s, 0)}
	now := uint64(time.Now().UnixMilli())
	if err := identitytest.Rotate(s, now-1000); err != nil {
		t.Fatal(err)
	}
	long := dirbox.DirEntry{AID: s.AID(), Home: h.self.AID(), KEL: marshal(s.KEL()), Keys: signedKeysFor(t, s, 0)}
	h.setDir(func(h *fakeHub) { h.dirExtra[s.AID()] = []dirbox.DirEntry{long, short} })
	if kel, err := DirectoryKEL(ctx, srv.URL, s.AID()); err != nil || len(kel) != len(s.KEL()) {
		t.Fatalf("a short and a long history: %v, %d events, want %d", err, len(kel), len(s.KEL()))
	}
	if err := identitytest.Rotate(fork, now-2000); err != nil {
		t.Fatal(err)
	}
	forked := dirbox.DirEntry{AID: s.AID(), Home: h.self.AID(), KEL: marshal(fork.KEL()), Keys: signedKeysFor(t, fork, 0)}
	h.setDir(func(h *fakeHub) { h.dirExtra[s.AID()] = []dirbox.DirEntry{long, forked} })
	if _, err := DirectoryKEL(ctx, srv.URL, s.AID()); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("two histories that fork: %v", err)
	}
	// Not in a partial answer: try again later.
	nobody := newStranger(t)
	setFake(t, srv.URL, func(h *fakeHub) { h.dirPartial = true })
	if _, err := DirectoryKEL(ctx, srv.URL, nobody.aid); err == nil || !strings.Contains(err.Error(), "try again later") {
		t.Fatalf("an agent missing from a partial answer: %v", err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.dirPartial = false })
	if _, err := DirectoryKEL(ctx, srv.URL, nobody.aid); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Fatalf("an agent missing from a whole answer: %v", err)
	}
}

// POST /agents/kel (anet verify through the daemon) answers this node's own
// key history, a recorded peer's, and one the directory holds, and says
// which; an agent the directory does not hold is 404 with the daemon's
// reason, a directory that cannot be read is 503, and what is not an AID
// is 400. The KEL is the agent's, in the control plane's standard base64.
func TestAgentsKELSources(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	if _, err := req.Delegate(ctx, prov.AID(), "a task", nil); err != nil {
		t.Fatal(err)
	}
	third := newTestDaemon(t, srv.URL, false)
	if err := third.RegisterWithHub(ctx, srv.URL, "Third", nil); err != nil {
		t.Fatal(err)
	}
	p := newPlaneFor(t, req, "tok")
	ask := func(aid string) (int, map[string]string) {
		t.Helper()
		resp, raw := p.req(t, "POST", "/agents/kel", `{"aid":"`+aid+`"}`, p.bearer)
		var out map[string]string
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	for _, c := range []struct {
		aid, source string
		kel         []identity.SignedEvent
	}{
		{req.AID(), "self", req.self.KEL()},
		{prov.AID(), "record", prov.self.KEL()},
		{third.AID(), "directory", third.self.KEL()},
	} {
		code, out := ask(c.aid)
		want, _ := identity.MarshalKEL(c.kel)
		if code != 200 || out["source"] != c.source || out["aid"] != c.aid || out["kel"] != base64.StdEncoding.EncodeToString(want) {
			t.Errorf("%s: %d, source %q, want 200 from %s with its KEL", c.source, code, out["source"], c.source)
		}
	}
	nobody := newStranger(t)
	if code, out := ask(nobody.aid); code != 404 || !strings.Contains(out["error"], "no hub knows") {
		t.Errorf("an agent the directory does not hold: %d %v", code, out)
	}
	if code, _ := ask("not an aid"); code != 400 {
		t.Errorf("not an AID: %d", code)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.dirDown = true })
	if code, _ := ask(newStranger(t).aid); code != 503 {
		t.Errorf("a directory that cannot be read: %d, want 503", code)
	}
}

// A hub whose bucket answer names, as the next page, the page just read is
// not read forever, and nothing is concluded from the part read: the CLI
// says to try again, as the daemon does (fetchBucket).
func TestDirectoryKELStopsAtACursorThatDoesNotMove(t *testing.T) {
	asked := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/hub/identity":
			_ = json.NewEncoder(w).Encode(hubapi.HubIdentity{AID: "bhubwithaloopingcursor"})
		case hubapi.DirectoryBucketPath:
			asked++
			_ = json.NewEncoder(w).Encode(hubapi.DirBucketResponse{Entries: []hubapi.DirBucketEntry{}, Next: "again"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	_, err := DirectoryKEL(context.Background(), hub.URL, newStranger(t).aid)
	if err == nil || !strings.Contains(err.Error(), "try again later") || asked != 2 {
		t.Fatalf("a cursor that does not move: %v after %d pages", err, asked)
	}
}

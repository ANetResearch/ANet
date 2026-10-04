package daemon

// The daemon's hub client of wire 3 (sealed-sender §4.3.11, SSR-D24 to
// SSR-D28; core CORE-V1, CORE-E5): every request states wire 3, goes on the
// side of the route it is, carries one fixed User-Agent and no cookie; a
// first contact goes under a session MAC and a capability send names no
// one; a hub's binary values are read as base64url only.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// hubRequests is a copy of every request the fake behind url got.
func hubRequests(t testing.TB, url string) []fakeHubRequest {
	t.Helper()
	h := fakeHubAt(t, url)
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fakeHubRequest(nil), h.requests...)
}

// relaySends is the requests to /relay/send among rs.
func relaySends(rs []fakeHubRequest) []fakeHubRequest {
	var out []fakeHubRequest
	for _, r := range rs {
		if r.path == hubapi.RelaySendPath {
			out = append(out, r)
		}
	}
	return out
}

// recordedSide is the side SI-13 puts a recorded request on, decided from
// the request alone and not from hubapi.Routes: it names this node when it
// carries a relayauth signature or a first-contact session, or one of the
// daemons' own AIDs in its path; every other request names no one.
func recordedSide(r fakeHubRequest, own []string) hubapi.Side {
	if r.header.Get(hubapi.HeaderAID) != "" || r.header.Get(hubapi.HeaderSession) != "" {
		return hubapi.SideSigned
	}
	for _, aid := range own {
		if strings.Contains(r.path, aid) {
			return hubapi.SideSigned
		}
	}
	return hubapi.SideAnonymous
}

// heldCapFrom gives `to` a contact peer capability from issuer, as issuer's
// grant would after the recipient checked it (step 8, step 10): the held
// row a kind-1 send goes under.
func heldCapFrom(t *testing.T, issuer, to *Daemon) interactions.HeldCap {
	t.Helper()
	now := issuer.nowMS()
	grant, _ := issueNow(t, issuer, to.AID(), decContactPeer(now), now)
	if grant == nil {
		t.Fatal("the issuer granted nothing")
	}
	g, body := receivedGrant(t, issuer, grant, "ix-1", now)
	h, err := interactions.HeldFromGrant(g, body, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := to.ix.Update(func(tx *interactions.Tx) error { _, err := tx.PutHeldGrant(h); return err }); err != nil {
		t.Fatal(err)
	}
	return h
}

// underCapability is env (sealed by `from` to `to`) with outer key 7 the
// capability `from` holds from `to`, its proof signed for this envelope,
// and the choice that goes with it.
func underCapability(t *testing.T, from, to *Daemon, env []byte) ([]byte, AccessChoice) {
	t.Helper()
	held, err := from.ix.HeldCaps(to.AID())
	if err != nil {
		t.Fatal(err)
	}
	choice := accessFor(accessRow{To: to.AID(), PeerEpoch: from.peerMboxEpoch(to.AID(), "")}, held, accessOrdinary, from.nowMS())
	if choice.Kind != seal.AccessKindCap {
		t.Fatalf("no capability to send under: %+v", choice)
	}
	digest, err := seal.EnvDigest(env)
	if err != nil {
		t.Fatal(err)
	}
	a, err := accessFromChoice(choice, digest)
	if err != nil {
		t.Fatal(err)
	}
	out, err := seal.SetAccess(env, a)
	if err != nil {
		t.Fatal(err)
	}
	return out, choice
}

// headerNames is the set of header names of h, canonical, sorted.
func headerNames(h http.Header) []string {
	var out []string
	for k := range h {
		out = append(out, http.CanonicalHeaderKey(k))
	}
	sort.Strings(out)
	return out
}

// TestHubRequestsSpeakWire3 (core CORE-V1): every request the daemon makes
// to its hub — the registration, the session, the sends, the poll and the
// ack, the directory, the key set, the module's — states X-ANet-Wire: 3.
// A hub that answers 426 stops the conversation: the call fails with
// errHubWire, the join records wire_too_old naming the release the hub
// asked for, and the node is no longer registered, so the relay loop stops
// polling.
func TestHubRequestsSpeakWire3(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	delegateThrough(t, req, prov.AID(), "hello")
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := req.Find(ctx, "anything"); err != nil {
		t.Fatal(err)
	}
	if err := prov.publishKeys(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.hubSeamDo(ctx, srv.URL, "/x402/supported"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range hubRequests(t, srv.URL) {
		seen[r.method+" "+r.path] = true
		if v := r.header.Values(hubapi.WireVersionHeader); len(v) != 1 || v[0] != "3" {
			t.Errorf("%s %s states wire %q", r.method, r.path, v)
		}
	}
	for _, want := range []string{"POST /register", "GET /hub/identity", "POST /relay/session", "POST /relay/send",
		"POST /relay/poll", "POST /relay/ack", "GET /agents", "GET /x402/supported"} {
		if !seen[want] {
			t.Errorf("no %s among the requests: %v", want, seen)
		}
	}

	// The hub moves to wire 4.
	setFake(t, srv.URL, func(h *fakeHub) { h.wire = 4 })
	if err := prov.pollOnce(ctx); !errors.Is(err, errHubWire) {
		t.Fatalf("a poll answered 426: %v, want errHubWire", err)
	}
	waitUntil(t, "the join records wire_too_old", func() bool {
		r := prov.joinSnapshot()
		return r != nil && r.State == JoinRefused && r.Reason == reasonWireTooOld
	})
	if prov.joinRegistered() {
		t.Fatal("still registered at a hub that refuses this node's wire")
	}
	r := prov.joinSnapshot()
	if r.Version != "0.4.0" {
		t.Fatalf("wire_too_old names %q, want the release the 426 named", r.Version)
	}
	if msg := prov.viewOfJoin(r, time.Now(), a2ashape.English).Message; !strings.Contains(msg, "0.4.0") || strings.Contains(msg, "—") {
		t.Fatalf("the status sentence: %q", msg)
	}
}

// hubSeamDo sends a GET of route to the hub through the seam a module gets
// (module.PaymentSeam.HubDo), as the x402 module does.
func (d *Daemon) hubSeamDo(ctx context.Context, hubURL, route string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hubURL, "/")+route, nil)
	if err != nil {
		return 0, err
	}
	resp, err := paymentSeam{d: d}.HubDo(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// TestFixedUserAgentNoCookies (SSR-D25): both sides send one User-Agent,
// anet/<version>, and no cookie, whatever cookies the hub sets — a cookie
// carried from a signed request to an anonymous one would tie them.
func TestFixedUserAgentNoCookies(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	setFake(t, srv.URL, func(h *fakeHub) { h.setCookie = true })
	heldCapFrom(t, prov, req)
	delegateThrough(t, req, prov.AID(), "hello")
	env, choice := underCapability(t, req, prov, sealFrom(t, req, prov, seal.TypeMessage, "ix-c", chatBody(t, "x", "")))
	if _, _, err := req.relaySendV3(ctx, env, choice); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := req.Find(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	sides := map[hubapi.Side]int{}
	for _, r := range hubRequests(t, srv.URL) {
		sides[recordedSide(r, []string{req.AID(), prov.AID()})]++
		if ua := r.header.Get("User-Agent"); ua != "anet/"+Version {
			t.Errorf("%s %s: User-Agent %q", r.method, r.path, ua)
		}
		if c := r.header.Values("Cookie"); len(c) != 0 {
			t.Errorf("%s %s carried a cookie: %v", r.method, r.path, c)
		}
	}
	if sides[hubapi.SideSigned] == 0 || sides[hubapi.SideAnonymous] == 0 {
		t.Fatalf("both sides were not exercised: %v", sides)
	}
}

// TestRelaySendV3Headers (SSR-D28, M12 the daemon's part): a first contact
// carries the access, the session id, the time and the MAC; a send under a
// capability the access alone; both the home hint, the envelope's content
// type, the wire and the User-Agent — and neither X-ANet-AID, X-ANet-Seq
// nor X-ANet-Sig.
func TestRelaySendV3Headers(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	home := hubAIDOf(srv.URL)
	base := []string{"Accept-Encoding", "Content-Length", "Content-Type", "User-Agent", "X-Anet-Access", "X-Anet-Home", "X-Anet-Wire"}
	check := func(what string, env []byte, choice AccessChoice, extra ...string) {
		t.Helper()
		before := len(relaySends(hubRequests(t, srv.URL)))
		res, handed, err := req.relaySendV3(ctx, env, choice)
		if err != nil || !handed || !res.HasRID {
			t.Fatalf("%s: %+v handed %v: %v", what, res, handed, err)
		}
		sends := relaySends(hubRequests(t, srv.URL))
		if len(sends) != before+1 {
			t.Fatalf("%s: %d sends, want one more than %d", what, len(sends), before)
		}
		r := sends[len(sends)-1]
		want := append(append([]string(nil), base...), extra...)
		sort.Strings(want)
		if got := headerNames(r.header); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: headers %v, want %v", what, got, want)
		}
		if r.header.Get("Content-Type") != hubapi.EnvelopeContentType || r.header.Get(hubapi.HeaderHome) != home {
			t.Errorf("%s: content type %q, home %q", what, r.header.Get("Content-Type"), r.header.Get(hubapi.HeaderHome))
		}
		for _, h := range []string{hubapi.HeaderAID, hubapi.HeaderSeq, hubapi.HeaderSig} {
			if r.header.Get(h) != "" {
				t.Errorf("%s carries %s", what, h)
			}
		}
		access, _, err := outerAccess(env)
		if err != nil || r.header.Get(hubapi.HeaderAccess) != hubapi.B64(access) {
			t.Errorf("%s: X-ANet-Access is not the envelope's key 7", what)
		}
	}
	env0 := sealFrom(t, req, prov, seal.TypeMessage, "ix-0", chatBody(t, "first", ""))
	check("a first contact", env0, AccessChoice{Send: true, Kind: seal.AccessKindSession, HintHome: home},
		"X-Anet-Mac", "X-Anet-Session", "X-Anet-Ts")
	heldCapFrom(t, prov, req)
	env1, choice := underCapability(t, req, prov, sealFrom(t, req, prov, seal.TypeMessage, "ix-1", chatBody(t, "later", "")))
	check("a capability send", env1, choice)

	// A credential of another kind than the envelope's is not sent at all.
	if _, handed, err := req.relaySendV3(ctx, env1, AccessChoice{Send: true, Kind: seal.AccessKindSession}); err == nil || handed {
		t.Fatalf("a kind-1 envelope sent as a first contact: handed %v, %v", handed, err)
	}
}

// TestSessionClientReuseAndReopen (SSR-D27): one session carries every
// first contact until five minutes before it ends; a session the hub no
// longer knows is opened again once and the send goes through; a hub that
// refuses to open one is session_unavailable, with nothing sent; and many
// first contacts at once open one session between them.
func TestSessionClientReuseAndReopen(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	fake := fakeHubAt(t, srv.URL)
	opens := func() int {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.sessionOpens
	}
	kind0 := AccessChoice{Send: true, Kind: seal.AccessKindSession}
	send := func(n int) error {
		_, _, err := req.relaySendV3(ctx, sealFrom(t, req, prov, seal.TypeMessage, "ix", chatBody(t, "m", "")), kind0)
		return err
	}
	for i := 0; i < 3; i++ {
		if err := send(i); err != nil {
			t.Fatal(err)
		}
	}
	if n := opens(); n != 1 {
		t.Fatalf("three first contacts opened %d sessions, want 1", n)
	}

	// The hub restarted: its sessions are gone.
	fake.forgetSessions()
	before := len(relaySends(hubRequests(t, srv.URL)))
	if err := send(3); err != nil {
		t.Fatalf("a send after the hub forgot the session: %v", err)
	}
	if n := opens(); n != 2 {
		t.Fatalf("%d sessions opened, want a second one after session_invalid", n)
	}
	if n := len(relaySends(hubRequests(t, srv.URL))) - before; n != 2 {
		t.Fatalf("%d send requests, want the refused one and its one retry", n)
	}

	// Sessions that end within five minutes are never reused.
	setFake(t, srv.URL, func(h *fakeHub) { h.sessionLife = 4 * time.Minute })
	req.dropSessions()
	for i := 0; i < 2; i++ {
		if err := send(4 + i); err != nil {
			t.Fatal(err)
		}
	}
	if n := opens(); n != 4 {
		t.Fatalf("%d sessions opened, want one per send while each ends within five minutes", n)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.sessionLife = 0 })

	// The hub will not open one: nothing is sent.
	req.dropSessions()
	setFake(t, srv.URL, func(h *fakeHub) { h.sessionRefuse = "admission_required" })
	before = len(relaySends(hubRequests(t, srv.URL)))
	_, handed, err := req.relaySendV3(ctx, sealFrom(t, req, prov, seal.TypeMessage, "ix", chatBody(t, "m", "")), kind0)
	if !errors.Is(err, errSessionUnavailable) || handed {
		t.Fatalf("no session: handed %v, %v; want session_unavailable", handed, err)
	}
	if !strings.HasPrefix(err.Error(), "session_unavailable") || req.sessionOK(srv.URL) {
		t.Fatalf("the failure reads %q, session_ok %v", err, req.sessionOK(srv.URL))
	}
	if n := len(relaySends(hubRequests(t, srv.URL))); n != before {
		t.Fatal("a first contact without a session was sent")
	}
	if permanent, _ := permanentRefusal(err); permanent != "" {
		t.Fatalf("session_unavailable ends the message (%s); it is a wait", permanent)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.sessionRefuse = "" })

	// Ten at once share one opening.
	req.dropSessions()
	n0 := opens()
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- send(10 + i)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := opens() - n0; n != 1 {
		t.Fatalf("ten first contacts at once opened %d sessions", n)
	}
	if !req.sessionOK(srv.URL) {
		t.Fatal("session_ok is false after a session opened")
	}
}

// TestHomeHintOrder (SSR-D13): a first contact's X-ANet-Home is the witness
// at the tip of the recipient's KEL, else the home of a capability it gave,
// else its directory entry's home, else none; a value that is not an AID
// is never a hint. A capability send's hint is its capability's home.
func TestHomeHintOrder(t *testing.T) {
	aids := make([]string, 4)
	for i := range aids {
		c, err := identitytest.Hub()
		if err != nil {
			t.Fatal(err)
		}
		aids[i] = c.AID()
	}
	w, c, d := aids[0], aids[1], aids[2]
	for _, tc := range []struct{ witness, cap, dir, want string }{
		{w, c, d, w}, {"", c, d, c}, {"", "", d, d}, {"", "", "", ""},
		{"not an aid", c, d, c}, {"", "x\x00y", d, d}, {"", "", "https://hub.example", ""},
	} {
		if got := homeHint(tc.witness, tc.cap, tc.dir); got != tc.want {
			t.Errorf("homeHint(%q, %q, %q) = %q, want %q", tc.witness, tc.cap, tc.dir, got, tc.want)
		}
	}

	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	// A recipient whose KEL designates its witness: that hub is the hint,
	// before the home of a capability it gave.
	peer, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	if err := identitytest.SwitchWitness(peer, aids[3], uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if _, err := req.pinPeerKEL(peer.AID(), peer.KEL(), interactions.PinOutbound); err != nil {
		t.Fatal(err)
	}
	if got := req.kindZeroHome(peer.AID(), c); got != aids[3] {
		t.Fatalf("the hint for a recipient with a witness: %q, want %q", got, aids[3])
	}
	if got := req.kindZeroHome(prov.AID(), c); got != c {
		t.Fatalf("the hint for a recipient without one: %q, want the capability's home %q", got, c)
	}
	// On the wire, and under a capability its home.
	if _, _, err := req.relaySendV3(ctx, sealFrom(t, req, prov, seal.TypeMessage, "ix", chatBody(t, "m", "")),
		AccessChoice{Send: true, Kind: seal.AccessKindSession, HintHome: c}); err != nil {
		t.Fatal(err)
	}
	held := heldCapFrom(t, prov, req)
	env, choice := underCapability(t, req, prov, sealFrom(t, req, prov, seal.TypeMessage, "ix-1", chatBody(t, "m", "")))
	if _, _, err := req.relaySendV3(ctx, env, choice); err != nil {
		t.Fatal(err)
	}
	sends := relaySends(hubRequests(t, srv.URL))
	if got := sends[len(sends)-2].header.Get(hubapi.HeaderHome); got != c {
		t.Errorf("first contact's X-ANet-Home %q, want %q", got, c)
	}
	if got := sends[len(sends)-1].header.Get(hubapi.HeaderHome); got != held.Home || got == "" {
		t.Errorf("capability send's X-ANet-Home %q, want the capability's home %q", got, held.Home)
	}
}

// TestRelaySendV3ParsesViaHub (core §2.12.2, DLV-W-12, W-12a, W-12b): the
// send's answer gives the engine its status, its rid and the hub it was
// queued at; a rid or via_hub out of shape is dropped, not trusted, and a
// 200 is a handed-over envelope whatever its body.
func TestRelaySendV3ParsesViaHub(t *testing.T) {
	srv, req, prov := registeredPair(t)
	other, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendViaHub = other.AID() })
	res, handed, err := req.relaySendV3(context.Background(), sealFrom(t, req, prov, seal.TypeMessage, "ix", chatBody(t, "m", "")),
		AccessChoice{Send: true, Kind: seal.AccessKindSession})
	if err != nil || !handed || res.Status != hubapi.RelayQueued || !res.HasRID || res.ViaHub != other.AID() {
		t.Fatalf("send: %+v handed %v: %v", res, handed, err)
	}
	rid := make([]byte, hubapi.RIDLen)
	_, _ = rand.Read(rid)
	r := hubapi.B64(rid)
	for _, tc := range []struct {
		body string
		want relaySendResult
	}{
		{`{"status":"duplicate","rid":"` + r + `","via_hub":"` + other.AID() + `","recipient_quiet":true}`,
			relaySendResult{Status: "duplicate", RID: [16]byte(rid), HasRID: true, ViaHub: other.AID(), Quiet: true}},
		{`{"status":"queued","rid":"` + r + `=","recipient_quiet":false,"extra":1}`, relaySendResult{Status: "queued"}},
		{`{"status":"forwarded","rid":"` + r + `","via_hub":"https://hub.example"}`,
			relaySendResult{Status: "queued", RID: [16]byte(rid), HasRID: true}},
		{`not json`, relaySendResult{Status: "queued"}},
	} {
		if got := parseRelaySendAnswer([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.body, got, tc.want)
		}
	}
}

// TestEmergencyDropsLocalSession (SSR-D23): the emergency rotation's first
// step ends this node's sessions and zeroes their keys; the next first
// contact opens a new one.
func TestEmergencyDropsLocalSession(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	s, err := req.firstContactSession(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	key := s.key
	if err := req.ix.Update(req.capsEmergencyTx); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, make([]byte, len(key))) {
		t.Fatal("the session key survived the emergency rotation")
	}
	req.hubc.sessions.mu.Lock()
	left := len(req.hubc.sessions.byHub)
	req.hubc.sessions.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d sessions left after the emergency rotation", left)
	}
	fake := fakeHubAt(t, srv.URL)
	fake.mu.Lock()
	n0 := fake.sessionOpens
	fake.mu.Unlock()
	if _, _, err := req.relaySendV3(ctx, sealFrom(t, req, prov, seal.TypeMessage, "ix", chatBody(t, "m", "")),
		AccessChoice{Send: true, Kind: seal.AccessKindSession}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	n1 := fake.sessionOpens
	fake.mu.Unlock()
	if n1 != n0+1 {
		t.Fatalf("the first contact after the rotation opened %d sessions, want 1", n1-n0)
	}
}

// TestDryRunWireTooOldNamesTheVersion (LEC §2.3.2): a hub newer than this
// node is wire_too_old with {version} filled — the release a 426 names,
// else the wire the hub states — never "—".
func TestDryRunWireTooOldNamesTheVersion(t *testing.T) {
	srv := newFakeHub(t)
	setFake(t, srv.URL, func(h *fakeHub) { h.wire = 4 })
	d, _ := joinDaemon(t)
	code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"dry_run":true`))
	msg, _ := out["error"].(string)
	if code != http.StatusConflict || out["reason"] != "wire_too_old" || !strings.Contains(msg, "wire 4") || strings.Contains(msg, "—") {
		t.Fatalf("a hub of wire 4: %d %v", code, out)
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(hubapi.WireVersionHeader, "5")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUpgradeRequired)
		_, _ = w.Write([]byte(`{"reason":"wire_too_old","error":"x","required_wire":5,"required_anet":"0.5.0"}`))
	}))
	t.Cleanup(refusing.Close)
	code, out = controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(refusing.URL, `,"dry_run":true`))
	msg, _ = out["error"].(string)
	if code != http.StatusConflict || out["reason"] != "wire_too_old" || !strings.Contains(msg, "0.5.0") {
		t.Fatalf("a hub that answers 426: %d %v", code, out)
	}
}

// TestDaemonDecodesHubBinaryAsBase64URL (CORE-E5, daemon side): a binary
// value in a hub's answer — its identity's KEL, a polled envelope, an
// agent's KEL, this node's stored key set — is read as base64url without
// padding in its one spelling, and the standard alphabet is refused. In
// the code of this repository, standard base64 is decoded only where the
// value is not a hub's wire-3 answer: an x402 payload (CORE-E5 exception
// one), what the control plane or a command line carries, local files, and
// the lookups by AID that SS-10 deletes.
func TestDaemonDecodesHubBinaryAsBase64URL(t *testing.T) {
	srv, req, prov := registeredPair(t)
	hub := fakeHubAt(t, srv.URL)
	kel, err := identity.MarshalKEL(hub.self.KEL())
	if err != nil {
		t.Fatal(err)
	}
	// A KEL whose standard spelling has +, / or =.
	std := base64.StdEncoding.EncodeToString(kel)
	if !strings.ContainsAny(std, "+/=") {
		t.Skip("the hub KEL's standard base64 has no character outside base64url")
	}
	fresh := newTestDaemon(t, "", false)
	if _, _, err := fresh.pinHubIdentity(srv.URL, hubapi.HubIdentity{AID: hub.self.AID(), KEL: std}); err == nil {
		t.Error("a hub identity with a standard-base64 KEL was pinned")
	}
	if _, _, err := fresh.pinHubIdentity(srv.URL, hubapi.HubIdentity{AID: hub.self.AID(), KEL: hubapi.B64(kel)}); err != nil {
		t.Errorf("a hub identity with a base64url KEL: %v", err)
	}
	if hubKELReplays(hubapi.HubIdentity{AID: hub.self.AID(), KEL: std}) {
		t.Error("dry_run took a standard-base64 KEL")
	}
	if _, err := decodeHubB64(std); err == nil {
		t.Error("this node's key set in standard base64 was read")
	}
	// A polled envelope in the standard alphabet is not an envelope.
	env := sealFrom(t, req, prov, seal.TypeMessage, "ix", chatBody(t, "m", ""))
	stdEnv := base64.StdEncoding.EncodeToString(env)
	if !strings.ContainsAny(stdEnv, "+/=") {
		env = append(env, 0) // any envelope will do; this one needs one of them
		stdEnv = base64.StdEncoding.EncodeToString(env)
	}
	before := counter(prov, dropBadTransport)
	poll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(hubapi.WireVersionHeader, "3")
		if r.URL.Path == "/hub/identity" {
			fakeHubJSON(w, http.StatusOK, hubapi.HubIdentity{AID: hub.self.AID(), KEL: hubapi.B64(kel)})
			return
		}
		if r.URL.Path == "/relay/poll" {
			fakeHubJSON(w, http.StatusOK, hubapi.RelayPollResponse{Messages: []hubapi.RelayMessage{{ID: 1, Envelope: stdEnv}}})
			return
		}
		fakeHubJSON(w, http.StatusOK, map[string]any{})
	}))
	t.Cleanup(poll.Close)
	prov.mu.Lock()
	prov.cfg.HubURL = poll.URL
	prov.mu.Unlock()
	if err := prov.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if counter(prov, dropBadTransport) != before+1 {
		t.Error("a polled envelope in standard base64 was not dropped as unreadable")
	}

	// The code: every standard-base64 decoding names why it is not a hub's
	// answer at wire 3.
	notHub := map[string]string{
		"internal/daemon/x402task.go payloadAuth":                "x402 payload (CORE-E5 exception one)",
		"module/x402/check.go decodePayload":                     "x402 payload (CORE-E5 exception one)",
		"module/x402/pay.go VerifyReceipt":                       "x402 settlement receipt, inside the x402 payload (CORE-E5 exception one)",
		"internal/daemon/hubjoin_daemon.go decodeHubRegisterReq": "the control plane's request (the control plane keeps standard base64)",
		"internal/mcpserv/tools.go buildMessage":                 "an MCP tool's argument",
		"internal/daemon/ledger.go decodeRecord":                 "this node's evidence chain file",
		"internal/release/sshsig.go ParseAuthorizedKey":          "a release signing key file",
		"internal/release/sshsig.go dearmor":                     "a release signature file",
		"module/blackboard/module.go add":                        "a capability call's argument",
		"module/cas/module.go put":                               "a capability call's argument",
		"module/org/module.go Start":                             "the module's configuration",
		"module/org/module.go verify":                            "a capability call's argument",
		"cmd/anet/audit.go verifyChain":                          "a file or argument the user gives",
		"cmd/anet/verify.go fetchKEL":                            "a receipt the user gives",
		"cmd/anet/verify.go verifyAttestation":                   "an attestation the user gives",
		"tools/anetfixture/attack.go readEnvelope":               "the fixture's command line",
		"tools/anetfixture/main.go cmdOrgCredential":             "the fixture's command line",
		"tools/anetfixture/main.go cmdOrgID":                     "the fixture's command line",
		// Not this package's to change: each goes with its item.
		"internal/daemon/seal_send.go fetchRecipientKeys": "v030-todo(SS-10): the lookup by AID, deleted with it",
		"cmd/anet/verify.go report":                       "v030-todo(SS-10): the KEL anet verify fetches by AID, which moves to the control plane",
		"module/x402/audit.go auditIssuance":              "v030-todo(PAY-13): per-event issuance records, replaced by supply epochs",
		"module/x402/voucher.go RedeemVoucher":            "v030-todo(PAY-9): vouchers are removed",
	}
	found := stdBase64Decoders(t, anetRoot(t))
	for site := range found {
		if _, ok := notHub[site]; !ok {
			t.Errorf("%s decodes standard base64: a hub's binary value is base64url (hubapi.DecodeB64)", site)
		}
	}
	for site := range notHub {
		if !found[site] {
			t.Errorf("%s is listed but decodes no standard base64 any more: delete it from the list", site)
		}
	}
}

// stdBase64Decoders is every function of the repository at root (tests and
// docs aside) that calls a standard-base64 decoder, as "<path> <func>".
func stdBase64Decoders(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "docs", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var buf bytes.Buffer
				_ = printer.Fprint(&buf, fset, c.Fun)
				s := buf.String()
				decoder := strings.HasSuffix(s, ".DecodeString") || strings.HasSuffix(s, ".Decode") || strings.HasSuffix(s, ".NewDecoder")
				std := strings.Contains(s, "StdEncoding")
				if strings.HasSuffix(s, ".NewDecoder") && len(c.Args) > 0 {
					var a bytes.Buffer
					_ = printer.Fprint(&a, fset, c.Args[0])
					std = strings.Contains(a.String(), "StdEncoding")
				}
				if decoder && std {
					out[filepath.ToSlash(rel)+" "+fd.Name.Name] = true
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestHubSidesNeverShareAConnection is SI-13 (M10): with the hub's
// connections kept alive, no connection carries a request that names this
// node and one that does not; and over TLS, every request on a connection of
// its own, no anonymous connection resumes a session — not even one the
// signed side earned.
func TestHubSidesNeverShareAConnection(t *testing.T) {
	for _, tlsOn := range []bool{false, true} {
		srv := startFakeHub(t, tlsOn)
		ctx := context.Background()
		var roots *x509.CertPool
		if tlsOn {
			roots = x509.NewCertPool()
			roots.AddCert(srv.Certificate())
			setFake(t, srv.URL, func(h *fakeHub) { h.closeConns = true })
		}
		join := func(d *Daemon, name string) {
			if tlsOn {
				d.setHubRoots(roots)
			}
			if err := d.RegisterWithHub(ctx, srv.URL, name, nil); err != nil {
				t.Fatal(err)
			}
		}
		req := newTestDaemon(t, srv.URL, false)
		prov := newTestDaemon(t, srv.URL, true)
		join(req, "Req")
		join(prov, "Prov")
		heldCapFrom(t, prov, req)
		// Alternate the sides, so that one pool would reuse one connection
		// across them.
		for i := 0; i < 3; i++ {
			if _, err := req.Find(ctx, "x"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := req.relaySendV3(ctx, sealFrom(t, req, prov, seal.TypeMessage, "ix", chatBody(t, "m", "")),
				AccessChoice{Send: true, Kind: seal.AccessKindSession}); err != nil {
				t.Fatal(err)
			}
			env, choice := underCapability(t, req, prov, sealFrom(t, req, prov, seal.TypeMessage, "ix-1", chatBody(t, "c", "")))
			if _, _, err := req.relaySendV3(ctx, env, choice); err != nil {
				t.Fatal(err)
			}
			var ks hubapi.KeysResponse
			if err := req.hubGet(ctx, srv.URL, "/agents/"+url.PathEscape(req.AID())+"/keys", nil, &ks); err != nil {
				t.Fatal(err)
			}
			if _, err := req.hubSeamDo(ctx, srv.URL, "/x402/supported"); err != nil {
				t.Fatal(err)
			}
			if err := prov.pollOnce(ctx); err != nil {
				t.Fatal(err)
			}
		}
		own := []string{req.AID(), prov.AID()}
		conns := map[string]map[hubapi.Side][]string{}
		resumed := map[hubapi.Side]int{}
		for _, r := range hubRequests(t, srv.URL) {
			side := recordedSide(r, own)
			if conns[r.remote] == nil {
				conns[r.remote] = map[hubapi.Side][]string{}
			}
			conns[r.remote][side] = append(conns[r.remote][side], r.method+" "+r.path)
			if r.resumed {
				resumed[side]++
			}
		}
		sides := map[hubapi.Side]bool{}
		for remote, by := range conns {
			if len(by) > 1 {
				t.Errorf("tls %v: connection %s carried both sides: %v", tlsOn, remote, by)
			}
			for s := range by {
				sides[s] = true
			}
		}
		if !sides[hubapi.SideSigned] || !sides[hubapi.SideAnonymous] {
			t.Fatalf("tls %v: both sides were not exercised: %v", tlsOn, conns)
		}
		if tlsOn {
			if resumed[hubapi.SideAnonymous] != 0 {
				t.Errorf("%d anonymous connections resumed a TLS session", resumed[hubapi.SideAnonymous])
			}
			t.Logf("signed connections that resumed a session: %d", resumed[hubapi.SideSigned])
		}
	}
}

// TestAnUnmappedHubRouteIsNeverSent (SSR-D26, M30): a request whose route
// the side table does not hold is refused before anything is sent, and
// counted hub-route-unmapped; a module's request to a host other than its
// node's hub is refused the same way.
func TestAnUnmappedHubRouteIsNeverSent(t *testing.T) {
	srv, req, _ := registeredPair(t)
	ctx := context.Background()
	before := counter(req, counterHubRouteUnmapped)
	err := req.hubGet(ctx, srv.URL, "/tasks/board", nil, nil)
	var unmapped *hubRouteError
	if !errors.As(err, &unmapped) {
		t.Fatalf("an unmapped route: %v", err)
	}
	if counter(req, counterHubRouteUnmapped) != before+1 {
		t.Fatal("hub-route-unmapped was not counted")
	}
	for _, r := range hubRequests(t, srv.URL) {
		if r.path == "/tasks/board" {
			t.Fatal("the unmapped request reached the hub")
		}
	}
	elsewhere, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:1/x402/supported", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (paymentSeam{d: req}).HubDo(elsewhere); err == nil {
		t.Fatal("a module's request to another host was sent")
	}
}

package daemon

// [redteam:F3] Regression for the si2 red-team PoC
// TestRedteamSI2_SendingAnnouncesTheRecipientInAnUnauthenticatedRequestLine:
// the first message a daemon sealed to a peer (and every send after a
// 10-minute gap, keysRevalidateMS) was preceded by an unauthenticated
// GET {hub}/agents/{recipient}/keys from the sender's address. That request
// line is what a reverse proxy in front of the hub logs, so the hub host's
// logs held sender-address -> recipient for every conversation after the
// relay row was deleted. The daemon now asks POST /agents/keys:lookup with
// the AID in the body, and falls back to the GET only for a hub that does
// not serve the route. See ANetHub internal/aghub/proxylog_test.go for the
// hub side.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

type loggedRequest struct{ method, uri, signer string }

// recordingHub puts a recorder in front of a fake hub, as a proxy that logs
// request lines would be, and returns its URL and the log.
func recordingHub(t *testing.T) (string, *fakeHub, func() []loggedRequest) {
	t.Helper()
	base := newFakeHub(t)
	h := fakeHubAt(t, base.URL)
	var mu sync.Mutex
	var log []loggedRequest
	rec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		log = append(log, loggedRequest{r.Method, r.URL.RequestURI(), r.Header.Get(hubapi.HeaderAID)})
		mu.Unlock()
		h.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(rec.Close)
	hubsByURL.Store(rec.URL, h)
	return rec.URL, h, func() []loggedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]loggedRequest(nil), log...)
	}
}

// Delegating to a peer the daemon has never written to looks the peer's
// keys up without naming it in any request line: no request names an AID
// other than its own signer's.
func TestSendingDoesNotNameTheRecipientInARequestLine(t *testing.T) {
	hub, h, lines := recordingHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, hub, false)
	prov := newTestDaemon(t, hub, true)
	if err := req.RegisterWithHub(ctx, hub, "Alice", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, hub, "Bob", nil, ""); err != nil {
		t.Fatal(err)
	}
	start := len(lines())
	if _, err := req.Delegate(ctx, prov.AID(), "a task", nil); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	var b strings.Builder
	for _, l := range lines()[start:] {
		b.WriteString(l.method + " " + l.uri + " signer=" + l.signer + "\n")
		for _, aid := range []string{req.AID(), prov.AID()} {
			if strings.Contains(l.uri, aid) && l.signer != aid {
				t.Errorf("a request line names %s, and it is not that agent's own signed request: %s %s (signer %q)",
					aid, l.method, l.uri, l.signer)
			}
		}
	}
	h.mu.Lock()
	lookups, gets := h.keysLookups, h.keysGets
	h.mu.Unlock()
	if lookups == 0 || gets != 0 {
		t.Errorf("key lookups: %d POST %s, %d GET /agents/{aid}/keys; want the POST only\n%s",
			lookups, hubapi.KeysLookupPath, gets, b.String())
	}
}

// A hub that predates POST /agents/keys:lookup answers it 405 from its mux;
// the daemon then asks the old GET and the message is delivered.
func TestAHubWithoutTheKeysLookupIsAskedByGET(t *testing.T) {
	hub, h, _ := recordingHub(t)
	h.mu.Lock()
	h.noKeysLookup = true
	h.mu.Unlock()
	ctx := context.Background()
	req := newTestDaemon(t, hub, false)
	prov := newTestDaemon(t, hub, true)
	if err := req.RegisterWithHub(ctx, hub, "Alice", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, hub, "Bob", nil, ""); err != nil {
		t.Fatal(err)
	}
	before := relayCountFor(hub)
	if _, err := req.Delegate(ctx, prov.AID(), "a task", nil); err != nil {
		t.Fatalf("delegate through a hub without the lookup route: %v", err)
	}
	h.mu.Lock()
	gets := h.keysGets
	h.mu.Unlock()
	if gets == 0 || relayCountFor(hub) == before {
		t.Fatalf("GET fallback: %d GETs, %d relayed", gets, relayCountFor(hub)-before)
	}
}

// The lookup route's own 404 (a JSON error: nobody holds a key set for the
// AID) is an answer about the recipient, not a missing route: it is not
// retried by GET and it ends the message as undelivered-unknown.
func TestTheLookupRoutesOwn404IsAnUnknownRecipient(t *testing.T) {
	hub, h, _ := recordingHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, hub, false)
	if err := req.RegisterWithHub(ctx, hub, "Alice", nil, ""); err != nil {
		t.Fatal(err)
	}
	stranger := newTestDaemon(t, hub, true) // never registered: no key set anywhere
	var out hubapi.KeysResponse
	err := req.lookupKeys(ctx, hub, stranger.AID(), &out)
	if got, ok := permanentRefusal(err); !ok || got != undeliveredUnknown {
		t.Fatalf("lookup of an AID nobody holds: %v, classified %q %v; want %q", err, got, ok, undeliveredUnknown)
	}
	h.mu.Lock()
	gets := h.keysGets
	h.mu.Unlock()
	if gets != 0 {
		t.Fatalf("a JSON 404 from the lookup route was retried with %d GETs", gets)
	}
}

// Before an A2A client writes to a peer through its daemon it reads the
// peer's proxy card, and the daemon reads the peer's network card and,
// to verify it, the peer's KEL (MCP get_agent_card does the same). Those
// two lookups still named the peer in their request lines after the key
// lookup was moved to the body, so the A2A path left sender-address -> peer
// in a logging proxy's lines [redteam:F3]. They take the AID in the body
// now, and fall back to the GETs on a hub without the routes.
func TestReadingAPeersCardDoesNotNameItInARequestLine(t *testing.T) {
	for _, old := range []bool{false, true} {
		h := newRegistryHub(t)
		h.noLookup = old
		peer := newCardAgent(t, "Peer", "does things", "text.stats")
		h.list(peer, nil)
		d := newTestDaemon(t, h.srv.URL, false)

		ra, err := (&DaemonTaskSeam{d: d}).Card(context.Background(), peer.aid)
		if err != nil || ra.Verification != cardVerified || string(ra.Card) != string(peer.card) {
			t.Fatalf("hub without the lookups %v: the peer's card: %v %+v", old, err, ra)
		}
		named := 0
		for _, u := range h.requests() {
			if strings.Contains(u, peer.aid) {
				named++
			}
		}
		switch {
		case !old && named != 0:
			t.Errorf("reading a peer's card named it in %d request lines: %v", named, h.requests())
		case old && named != 2:
			t.Errorf("a hub without the lookups was asked by GET %d times, want the card and the KEL: %v",
				named, h.requests())
		}
	}
}

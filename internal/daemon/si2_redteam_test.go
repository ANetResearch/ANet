package daemon

// Red-team PoC for SI-2 / X1 (lens si2). A passing test means the defect
// is present.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestRedteamSI2_SendingAnnouncesTheRecipientInAnUnauthenticatedRequestLine:
// the first message a daemon seals to a peer (and every send after a
// 10-minute gap, keysRevalidateMS) is preceded by
// GET {hub}/agents/{recipient}/keys, an unauthenticated request whose
// request line names the recipient and which comes from the sender's
// address. The /relay/send body keeps the sender out of relay_message, but
// this request line is what the hub's reverse proxy logs (deploy/nginx-hub.conf
// keeps nginx's default combined access log), so the hub host's logs hold
// sender-address -> recipient-AID for every conversation. See the hub-side
// PoC ANetHub/internal/aghub/si2_redteam_test.go.
func TestRedteamSI2_SendingAnnouncesTheRecipientInAnUnauthenticatedRequestLine(t *testing.T) {
	base := newFakeHub(t)
	h := fakeHubAt(t, base.URL)
	var mu sync.Mutex
	type line struct{ method, uri, signer string }
	var log []line
	rec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		log = append(log, line{r.Method, r.URL.RequestURI(), r.Header.Get("X-ANet-AID")})
		mu.Unlock()
		h.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(rec.Close)
	hubsByURL.Store(rec.URL, h)

	ctx := context.Background()
	req := newTestDaemon(t, rec.URL, false)
	prov := newTestDaemon(t, rec.URL, true)
	if err := req.RegisterWithHub(ctx, rec.URL, "Alice", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, rec.URL, "Bob", nil, ""); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	start := len(log)
	mu.Unlock()
	if _, err := req.Delegate(ctx, prov.AID(), "a task", nil); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	lookup, sendAt := -1, -1
	for i, l := range log[start:] {
		if l.method == http.MethodGet && l.uri == "/agents/"+prov.AID()+"/keys" && l.signer == "" {
			lookup = i
		}
		if l.method == http.MethodPost && l.uri == "/relay/send" && l.signer == req.AID() && sendAt < 0 {
			sendAt = i
		}
	}
	if lookup < 0 || sendAt < 0 || lookup > sendAt {
		var b strings.Builder
		for _, l := range log[start:] {
			b.WriteString(l.method + " " + l.uri + " signer=" + l.signer + "\n")
		}
		t.Fatalf("no unauthenticated recipient lookup right before the send:\n%s", b.String())
	}
	t.Logf("ATTACK OK: delegate to %s issued unauthenticated GET /agents/%s/keys from the sender, "+
		"then the signed /relay/send", prov.AID(), prov.AID())
}

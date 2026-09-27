package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/ANetResearch/ANet/internal/anethome"
)

// SI-7, the control plane's half of the symmetric pair (the local A2A
// interface's half is module/a2a si7_test.go, which runs this daemon with
// the real module): each surface takes only its own credential, and both
// answer 421 to a Host that is not a loopback name with their port.
//
// The local A2A token is made here the way module/a2a makes it (32 random
// bytes, hex) and put where it keeps it, so the test states the situation it
// guards: a client holding a2a_token.txt — a coding tool, Hermes — must not
// reach the control plane with it, whose token authorizes everything
// (policy, peers, payments of every tier, shutdown).
func TestTheLocalA2ATokenIsRefusedByTheControlPlane(t *testing.T) {
	p := newPlane(t)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	a2aToken := hex.EncodeToString(raw)
	dir := anethome.A2ADir(p.d.layout.Root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, anethome.A2ATokenFile), []byte(a2aToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withToken := func(tok string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
	}
	routes := []struct{ method, path, body string }{
		{"GET", "/status", ""},
		{"POST", "/tasks/list", "{}"},
		{"POST", "/tasks/get", `{"task_id":"t"}`},
		{"POST", "/tasks/pay", `{"task_id":"t","decision":"reject"}`},
		{"POST", "/console/ticket", "{}"},
		{"POST", "/inbound/policy", `{"policy":"open"}`},
	}
	for _, rt := range routes {
		resp, b := p.req(t, rt.method, rt.path, rt.body, withToken(a2aToken))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with the local A2A token = %d %s, want 401", rt.method, rt.path, resp.StatusCode, b)
		}
		// The same token in the scheme the A2A interface also accepts.
		resp, b = p.req(t, rt.method, rt.path, rt.body, func(r *http.Request) {
			r.Header.Set("Authorization", "bearer "+a2aToken)
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with the local A2A token (lower-case scheme) = %d %s, want 401", rt.method, rt.path,
				resp.StatusCode, b)
		}
		// 421 comes before any credential is looked at, the right one included.
		for _, host := range []string{"evil.example:" + p.port, "localtest.me:" + p.port, "127.0.0.2:" + p.port} {
			resp, b = p.req(t, rt.method, rt.path, rt.body, func(r *http.Request) { r.Host = host; p.bearer(r) })
			if resp.StatusCode != http.StatusMisdirectedRequest {
				t.Errorf("%s %s, Host %s = %d %s, want 421", rt.method, rt.path, host, resp.StatusCode, b)
			}
		}
	}
	// The control token itself still passes the gate (the handler may then
	// refuse the body; what matters is that it is not a 401 or 421).
	resp, b := p.req(t, "POST", "/tasks/list", "{}", p.bearer)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusMisdirectedRequest {
		t.Fatalf("the control token was refused: %d %s", resp.StatusCode, b)
	}
}

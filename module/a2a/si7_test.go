//go:build !no_a2a

package a2a

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/anethome"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// si7Node is a real daemon with this module started by it, as in a
// default build: the control plane on a loopback listener, the local A2A
// interface where the module put it, and both tokens as the files hold
// them.
type si7Node struct {
	ctlURL, ctlPort string
	ctlToken        string
	a2aURL, a2aPort string
	a2aToken        string
}

func newSI7Node(t *testing.T) *si7Node {
	t.Helper()
	isolateHome(t)
	root := t.TempDir()
	cfg, _ := json.Marshal(map[string]any{"control_addr": "127.0.0.1:0"})
	if err := os.WriteFile(filepath.Join(root, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	layout := daemon.NewLayout(root)
	d, err := daemon.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ctlToken := strings.Repeat("c1", 32)
	if err := os.WriteFile(layout.ControlTokenPath(), []byte(ctlToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctl := httptest.NewServer(d.ControlHandler(ctlToken))
	t.Cleanup(ctl.Close)
	cu, _ := url.Parse(ctl.URL)

	// The module's files, found the way anet doctor and anet agents wire
	// find them.
	dir := anethome.A2ADir(root)
	addr, err := readSmallFile(filepath.Join(dir, anethome.A2AAddrFile))
	if err != nil {
		t.Fatalf("the daemon did not start the local A2A interface: %v", err)
	}
	tok, err := readSmallFile(filepath.Join(dir, anethome.A2ATokenFile))
	if err != nil {
		t.Fatal(err)
	}
	addr = strings.TrimSpace(addr)
	_, a2aPort, _ := net.SplitHostPort(addr)
	n := &si7Node{ctlURL: ctl.URL, ctlPort: cu.Port(), ctlToken: ctlToken,
		a2aURL: "http://" + addr, a2aPort: a2aPort, a2aToken: strings.TrimSpace(tok)}
	if n.a2aToken == "" || n.a2aToken == n.ctlToken {
		t.Fatalf("the local A2A token must be its own secret: %q", n.a2aToken)
	}
	return n
}

func si7Do(t *testing.T, method, u, body string, mod func(*http.Request)) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, u, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if mod != nil {
		mod(r)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp, string(b)
}

func bearerOf(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

// SI-7, both halves against one real node: each surface takes only its own
// credential — the control token and a console session are refused by the
// local A2A interface, the local A2A token by the control plane — and both
// answer 421 to a Host that is not a loopback name with their own port.
// (internal/daemon ctlsec_si7_test.go holds the control plane's half without
// this module; the host rule both apply is internal/loopguard.)
func TestEachLocalSurfaceTakesOnlyItsOwnCredential(t *testing.T) {
	n := newSI7Node(t)
	const remote = "bafyremoteagent"
	rpc := n.a2aURL + agentsPath + "/" + remote + "/jsonrpc"
	a2aRoutes := []struct{ method, url, body string }{
		{"POST", rpc, rpcGet},
		{"GET", n.a2aURL + agentsPath, ""},
		{"GET", n.a2aURL + agentsPath + "/" + remote + "/.well-known/agent-card.json", ""},
		{"GET", n.a2aURL + agentsPath + "/" + remote + "/rest/tasks/task1", ""},
	}
	ctlRoutes := []struct{ method, url, body string }{
		{"GET", n.ctlURL + "/status", ""},
		{"POST", n.ctlURL + "/tasks/list", "{}"},
		{"POST", n.ctlURL + "/tasks/pay", `{"task_id":"t","decision":"reject"}`},
		{"POST", n.ctlURL + "/console/ticket", "{}"},
	}

	// The control token on the local A2A interface: 401.
	for _, rt := range a2aRoutes {
		if resp, b := si7Do(t, rt.method, rt.url, rt.body, bearerOf(n.ctlToken)); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("A2A %s %s with the control token = %d %s, want 401", rt.method, rt.url, resp.StatusCode, b)
		}
	}
	// The local A2A token on the control plane: 401.
	for _, rt := range ctlRoutes {
		if resp, b := si7Do(t, rt.method, rt.url, rt.body, bearerOf(n.a2aToken)); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("control %s %s with the local A2A token = %d %s, want 401", rt.method, rt.url, resp.StatusCode, b)
		}
	}
	// A non-loopback Host, with each surface's own valid token: 421 on both.
	for _, host := range []string{"evil.example", "localtest.me", "127.0.0.2"} {
		for _, rt := range a2aRoutes {
			resp, b := si7Do(t, rt.method, rt.url, rt.body, func(r *http.Request) {
				bearerOf(n.a2aToken)(r)
				r.Host = host + ":" + n.a2aPort
			})
			if resp.StatusCode != http.StatusMisdirectedRequest {
				t.Errorf("A2A %s %s, Host %s = %d %s, want 421", rt.method, rt.url, host, resp.StatusCode, b)
			}
		}
		for _, rt := range ctlRoutes {
			resp, b := si7Do(t, rt.method, rt.url, rt.body, func(r *http.Request) {
				bearerOf(n.ctlToken)(r)
				r.Host = host + ":" + n.ctlPort
			})
			if resp.StatusCode != http.StatusMisdirectedRequest {
				t.Errorf("control %s %s, Host %s = %d %s, want 421", rt.method, rt.url, host, resp.StatusCode, b)
			}
		}
	}
	// Each surface's Host names its own port: the other surface's port is
	// a wrong port.
	if resp, _ := si7Do(t, "POST", rpc, rpcGet, func(r *http.Request) {
		bearerOf(n.a2aToken)(r)
		r.Host = "127.0.0.1:" + n.ctlPort
	}); resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("A2A with the control plane's port in Host = %d, want 421", resp.StatusCode)
	}

	// A console session. Cookies are not scoped by port, so a browser holding
	// the console's cookie sends it to the A2A interface too.
	resp, b := si7Do(t, "POST", n.ctlURL+"/console/ticket", "{}", bearerOf(n.ctlToken))
	var tk struct {
		Ticket string `json:"ticket"`
	}
	if resp.StatusCode != 200 || json.Unmarshal([]byte(b), &tk) != nil || tk.Ticket == "" {
		t.Fatalf("ticket: %d %s", resp.StatusCode, b)
	}
	sb, _ := json.Marshal(map[string]string{"ticket": tk.Ticket})
	resp, b = si7Do(t, "POST", n.ctlURL+"/console/session", string(sb), func(r *http.Request) {
		r.Header.Set("Origin", "http://127.0.0.1:"+n.ctlPort)
	})
	var sess struct {
		CSRF string `json:"csrf"`
	}
	_ = json.Unmarshal([]byte(b), &sess)
	cookies := resp.Cookies()
	if resp.StatusCode != 200 || len(cookies) == 0 || sess.CSRF == "" {
		t.Fatalf("session: %d %s", resp.StatusCode, b)
	}
	withSession := func(origin bool) func(*http.Request) {
		return func(r *http.Request) {
			for _, c := range cookies {
				r.AddCookie(c)
			}
			r.Header.Set("X-Anet-CSRF", sess.CSRF)
			if origin {
				r.Header.Set("Origin", "http://127.0.0.1:"+n.ctlPort)
			}
		}
	}
	// The session works where it belongs...
	if resp, b := si7Do(t, "GET", n.ctlURL+"/status", "", withSession(true)); resp.StatusCode != 200 {
		t.Fatalf("the console session does not reach /status: %d %s", resp.StatusCode, b)
	}
	// ...and nowhere on the A2A interface: 401 as a replayed cookie, 403 as a
	// page (every Origin is refused there).
	for _, rt := range a2aRoutes {
		if resp, b := si7Do(t, rt.method, rt.url, rt.body, withSession(false)); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("A2A %s %s with the console session = %d %s, want 401", rt.method, rt.url, resp.StatusCode, b)
		}
		if resp, b := si7Do(t, rt.method, rt.url, rt.body, withSession(true)); resp.StatusCode != http.StatusForbidden {
			t.Errorf("A2A %s %s with the console session from the page = %d %s, want 403", rt.method, rt.url,
				resp.StatusCode, b)
		}
	}

	// Each token passes its own surface's gate.
	if resp, b := si7Do(t, "POST", rpc, rpcGet, bearerOf(n.a2aToken)); resp.StatusCode != 200 || !strings.Contains(b, `"error"`) {
		t.Errorf("A2A GetTask with the local A2A token = %d %s, want 200 with a TaskNotFound error", resp.StatusCode, b)
	}
	if resp, b := si7Do(t, "POST", n.ctlURL+"/tasks/list", "{}", bearerOf(n.ctlToken)); resp.StatusCode != 200 {
		t.Errorf("control /tasks/list with the control token = %d %s", resp.StatusCode, b)
	}
}

// The module's state directory name is the one anethome gives the CLI.
func TestStateDirIsTheOneTheCLIReads(t *testing.T) {
	if name != anethome.A2AModule || AddrFile != anethome.A2AAddrFile || TokenFile != anethome.A2ATokenFile {
		t.Fatalf("module a2a keeps %s/{%s,%s}; anethome says %s/{%s,%s}", name, AddrFile, TokenFile,
			anethome.A2AModule, anethome.A2AAddrFile, anethome.A2ATokenFile)
	}
}

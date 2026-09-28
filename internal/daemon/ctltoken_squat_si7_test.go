package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/localpeer"
)

// F18, second pass (red team si7): the control plane's side of what module/a2a does for its port. anet's
// own clients verify the listener before sending the control token, but not every client of the
// control plane is anet's — a curl in a script reading control_token.txt, an anet older than the check.
// While the daemon is down another local user can hold its port and collect the token from them; the
// daemon then started on another port with the SAME token, so the collected one kept working. When the
// holder may be another user, the daemon now replaces the token as it moves; a holder that is provably
// this user (another identity's daemon) is not a boundary and the token stays.

type ctlSquat struct {
	addr string
	mu   sync.Mutex
	got  []string
}

func startCtlSquat(t *testing.T) *ctlSquat {
	t.Helper()
	ln, _, err := AllocControlListener() // inside the auto-assigned range, like a real control port
	if err != nil {
		t.Fatal(err)
	}
	s := &ctlSquat{addr: ln.Addr().String()}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.got = append(s.got, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		s.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return s
}

// servedAfterSquat initializes a node whose control port is s.addr, lets a client that does not verify
// the listener send the token there, starts the daemon, and returns the layout, the token that client
// sent, and the address the daemon moved to.
func servedAfterSquat(t *testing.T, s *ctlSquat) (Layout, string, string) {
	t.Helper()
	t.Setenv("ANET_HOME", t.TempDir())
	rt := t.TempDir()
	_ = os.Chmod(rt, 0o700)
	t.Setenv("XDG_RUNTIME_DIR", rt)
	root := t.TempDir()
	cfg, _ := json.Marshal(map[string]any{"control_addr": s.addr})
	if err := os.WriteFile(filepath.Join(root, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	layout := NewLayout(root)
	tok, err := loadOrGenControlToken(layout)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.addr+"/status", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	s.mu.Lock()
	captured := append([]string(nil), s.got...)
	s.mu.Unlock()
	if len(captured) != 1 || captured[0] != tok {
		t.Fatalf("setup: the squatter holds %q", captured)
	}

	d, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- d.ServeControl(ctx) }()
	t.Cleanup(func() { cancel(); <-errc; d.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := LoadConfig(layout); err == nil && c.ControlAddr != s.addr {
			if resp, err := http.Get("http://" + c.ControlAddr + "/ping"); err == nil {
				resp.Body.Close()
				return layout, tok, c.ControlAddr
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon did not come up on another port")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func statusWith(t *testing.T, addr, tok string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/status", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestTheControlTokenAnotherUserCollectedIsWorthlessOnceTheDaemonMoves(t *testing.T) {
	s := startCtlSquat(t)
	t.Cleanup(localpeer.TreatAsForeignForTest(s.addr))
	layout, collected, moved := servedAfterSquat(t, s)
	if code := statusWith(t, moved, collected); code != http.StatusUnauthorized {
		t.Fatalf("the token another user collected on the old port opens the moved daemon: HTTP %d", code)
	}
	b, err := os.ReadFile(layout.ControlTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	cur := strings.TrimSpace(string(b))
	if cur == collected || statusWith(t, moved, cur) != http.StatusOK {
		t.Fatalf("control_token.txt does not hold a working replacement")
	}
	if fi, err := os.Stat(layout.ControlTokenPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("control_token.txt mode: %v %v", fi.Mode(), err)
	}
}

func TestTheControlTokenStaysWhenThisUserHeldThePort(t *testing.T) {
	s := startCtlSquat(t) // not marked foreign: the socket table shows this uid
	_, tok, moved := servedAfterSquat(t, s)
	if code := statusWith(t, moved, tok); code != http.StatusOK {
		t.Fatalf("a same-user holder cost the token: HTTP %d", code)
	}
}

// A port the operator chose is not moved off (the daemon reports the collision and stops), but the token
// clients may have sent to its holder is replaced all the same: the next start, after the holder lets go,
// must not accept it.
func TestTheControlTokenIsReplacedWhenAChosenPortIsTaken(t *testing.T) {
	t.Setenv("ANET_HOME", t.TempDir())
	rt := t.TempDir()
	_ = os.Chmod(rt, 0o700)
	t.Setenv("XDG_RUNTIME_DIR", rt)
	ln, err := net.Listen("tcp", "127.0.0.1:0") // outside the allocator's range: a deliberate address
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	if autoAssignedControlAddr(addr) {
		t.Skipf("%s falls in the auto-assigned range", addr)
	}
	t.Cleanup(localpeer.TreatAsForeignForTest(addr))
	root := t.TempDir()
	cfg, _ := json.Marshal(map[string]any{"control_addr": addr})
	if err := os.WriteFile(filepath.Join(root, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	layout := NewLayout(root)
	old, err := loadOrGenControlToken(layout)
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.ServeControl(context.Background()); err == nil {
		t.Fatal("the daemon served on a chosen port another process holds")
	}
	b, _ := os.ReadFile(layout.ControlTokenPath())
	if strings.TrimSpace(string(b)) == old {
		t.Fatal("the token clients may have sent to the port's holder was kept")
	}
}

// The same daemon started twice (`anet daemon` while one runs, on the port the operator chose) finds its
// port taken by itself: the second start fails, and the token the running one serves stays — replacing
// it would lock every client out of the running daemon.
func TestASecondStartDoesNotReplaceTheRunningDaemonsToken(t *testing.T) {
	t.Setenv("ANET_HOME", t.TempDir())
	rt := t.TempDir()
	_ = os.Chmod(rt, 0o700)
	t.Setenv("XDG_RUNTIME_DIR", rt)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	if autoAssignedControlAddr(addr) {
		t.Skipf("%s falls in the auto-assigned range", addr)
	}
	root := t.TempDir()
	cfg, _ := json.Marshal(map[string]any{"control_addr": addr})
	if err := os.WriteFile(filepath.Join(root, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	layout := NewLayout(root)
	tok, err := loadOrGenControlToken(layout)
	if err != nil {
		t.Fatal(err)
	}
	first, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- first.ServeControl(ctx) }()
	t.Cleanup(func() { cancel(); <-errc; first.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for statusWithErr(addr, tok) != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("the first daemon never answered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cfg2, err := LoadConfig(layout)
	if err != nil {
		t.Fatal(err)
	}
	second := &Daemon{layout: layout, cfg: cfg2} // its control plane is all that is under test
	if err := second.ServeControl(context.Background()); err == nil {
		t.Fatal("a second daemon served on the running one's port")
	}
	b, _ := os.ReadFile(layout.ControlTokenPath())
	if strings.TrimSpace(string(b)) != tok || statusWithErr(addr, tok) != http.StatusOK {
		t.Fatal("the second start replaced the running daemon's token")
	}
}

func statusWithErr(addr, tok string) int {
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/status", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

package main

// Regression tests for F18 (red team, si7): the CLI sent the control token to whatever held the
// configured loopback port. `anet up` probes the port before it starts the daemon — exactly when the
// daemon is not holding it — and the daemon then moved to another port keeping the same token, so a
// process of another local user that took the port collected a token the real daemon accepted
// (skeptic PoC TestSkepticSI7_UpLeaksControlTokenToSquatterAndItStaysValid). Every client of the
// control plane now verifies the listener before the token is written (internal/localpeer,
// A2A-DESIGN §7 item 10 [redteam:F18]).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/anethome"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/localpeer"
)

// squatter holds addr-in-the-control-range for "another local user" and records every Authorization
// header it is sent. It answers 503, so `anet up` goes on to start a daemon.
type squatter struct {
	addr string
	mu   sync.Mutex
	got  []string
}

func (s *squatter) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

func newSquatter(t *testing.T) *squatter {
	t.Helper()
	ln, _, err := daemon.AllocControlListener()
	if err != nil {
		t.Fatal(err)
	}
	s := &squatter{addr: ln.Addr().String()}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			s.mu.Lock()
			s.got = append(s.got, a)
			s.mu.Unlock()
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Cleanup(localpeer.TreatAsForeignForTest(s.addr))
	return s
}

// victimLayout is a node initialized earlier on the squatted (auto-assigned) port, with a token on disk
// and its daemon not running.
func victimLayout(t *testing.T, controlAddr string) (daemon.Layout, string) {
	t.Helper()
	root := t.TempDir()
	cfg, _ := json.Marshal(map[string]any{"control_addr": controlAddr})
	if err := os.WriteFile(filepath.Join(root, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	layout := daemon.NewLayout(root)
	var raw [24]byte
	_, _ = rand.Read(raw[:])
	token := hex.EncodeToString(raw[:])
	if err := os.WriteFile(layout.ControlTokenPath(), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return layout, token
}

func TestUpDoesNotSendTheControlTokenToAPortSquatter(t *testing.T) {
	t.Setenv("ANET_HOME", t.TempDir())
	rt := t.TempDir()
	_ = os.Chmod(rt, 0o700)
	t.Setenv("XDG_RUNTIME_DIR", rt)
	sq := newSquatter(t)
	layout, token := victimLayout(t, sq.addr)

	// Step 1 of `anet up`: the probe. It must not hand the token over, and says why it is not up.
	up, err := probeLocalDaemon(layout)
	if up || !errors.Is(err, localpeer.ErrNotOurs) {
		t.Fatalf("probe of a squatted port: up=%v err=%v", up, err)
	}
	if got := sq.sent(); len(got) != 0 {
		t.Fatalf("the squatter received %q", got)
	}

	// The real daemon starts (as `anet up` would), finds its port taken and moves; the CLI follows
	// config.json to it and gets through with the token.
	d, err := daemon.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- d.ServeControl(ctx) }()
	t.Cleanup(func() { cancel(); <-errc; d.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for !localDaemonUp(layout) {
		if time.Now().After(deadline) {
			t.Fatal("the moved daemon never answered the CLI")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cfg, _ := daemon.LoadConfig(layout); cfg.ControlAddr == sq.addr {
		t.Fatal("the daemon did not move off the squatted port")
	}
	if got := sq.sent(); len(got) != 0 {
		t.Fatalf("the squatter received %q (token %s)", got, token)
	}
}

// Every other way the CLI reaches the control plane goes through the same check: a command, the guide
// printed by a bare `anet`, `anet mcp`, and doctor's liveness probe.
func TestNoCLIPathSendsTheControlTokenToAPortSquatter(t *testing.T) {
	t.Setenv("ANET_HOME", t.TempDir())
	rt := t.TempDir()
	_ = os.Chmod(rt, 0o700)
	t.Setenv("XDG_RUNTIME_DIR", rt)
	sq := newSquatter(t)
	layout, _ := victimLayout(t, sq.addr)

	err := runClient(layout, "status", nil, true)
	if err == nil || !strings.Contains(err.Error(), "the token was not sent") {
		t.Errorf("anet status against a squatter: %v", err)
	}
	_ = captureStdout(t, func() { guide(layout) })
	if daemonAnswersAt(sq.addr, layout.ControlTokenPath()) {
		t.Error("doctor took the squatter for the daemon")
	}
	mcpDone := make(chan error, 1)
	go func() { mcpDone <- runMCP(layout, true) }()
	select {
	case err := <-mcpDone:
		if err == nil {
			t.Error("anet mcp started against a squatter")
		}
	case <-time.After(10 * time.Second):
		t.Error("anet mcp started serving against a squatter")
	}
	if got := sq.sent(); len(got) != 0 {
		t.Fatalf("the squatter received %q", got)
	}
}

// The A2A side of F18: the local A2A interface stays down rather than move off a port another process
// holds, and says so where the operator looks — doctor fails with the record module/a2a left, and names
// another user's listener on the recorded port even while the daemon is down, when a configured client
// (Hermes' a2a_agents) would be handing it the token.
func TestDoctorReportsATakenA2APort(t *testing.T) {
	layout := freshInit(t)
	dir := anethome.A2ADir(layout.Root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const addr = "127.0.0.1:43811"
	if err := os.WriteFile(filepath.Join(dir, anethome.A2AAddrFile), []byte(addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := testDoctorEnv(t)
	env.listenerUIDs = func(a string) ([]int, error) {
		if a != addr {
			t.Errorf("checked %s", a)
		}
		return []int{os.Getuid() + 1}, nil
	}
	if err := os.WriteFile(filepath.Join(dir, anethome.A2AConflictFile),
		[]byte("2026-09-28T00:00:00Z "+addr+" is held by a process of uid 1002 (another local user)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := collectDoctor(layout, env)
	if err != nil {
		t.Fatal(err)
	}
	var sawDown, sawHolder bool
	for _, c := range rep.Checks {
		switch c.ID {
		case "a2a":
			sawDown = c.Status == stFail && strings.Contains(c.Detail, addr) && strings.Contains(c.Hint, "anet agents wire --refresh")
		case "a2a.port":
			sawHolder = c.Status == stFail && strings.Contains(c.Detail, "another local user")
		}
	}
	if !sawDown || !sawHolder || rep.OK {
		t.Fatalf("doctor: down=%v holder=%v ok=%v\n%+v", sawDown, sawHolder, rep.OK, rep.Checks)
	}
}

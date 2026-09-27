package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// servedDaemon runs one daemon's control plane on a loopback listener and returns its layout.
func servedDaemon(t *testing.T) (daemon.Layout, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg, _ := json.Marshal(map[string]any{"control_addr": ln.Addr().String()})
	if err := os.WriteFile(filepath.Join(root, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	layout := daemon.NewLayout(root)
	d, err := daemon.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	token := "cli-test-token"
	if err := os.WriteFile(layout.ControlTokenPath(), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: d.ControlHandler(token), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); d.Close() })
	return layout, ln.Addr().String()
}

// `anet console --url` prints a single-use ticket URL for this identity's daemon, not a bare /console
// address (the page carries no credential and needs the ticket).
func TestConsoleCommandPrintsATicketURL(t *testing.T) {
	layout, addr := servedDaemon(t)
	var err error
	out := captureStdout(t, func() { err = runClient(layout, "console", []string{"--url"}, true) })
	if err != nil {
		t.Fatal(err)
	}
	u := strings.TrimSpace(out)
	if !strings.HasPrefix(u, "http://"+addr+"/console#t=") || len(u) < len("http://"+addr+"/console#t=")+20 {
		t.Fatalf("console URL %q", u)
	}
	// The ticket opens a session once.
	tk := strings.TrimPrefix(u, "http://"+addr+"/console#t=")
	body, _ := json.Marshal(map[string]string{"ticket": tk})
	req, _ := http.NewRequest("POST", "http://"+addr+"/console/session", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://"+addr)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the printed ticket does not open a session: %d", resp.StatusCode)
	}
}

// `anet mcp` with an explicitly selected identity resolves strictly: an identity without its own
// daemon is an error even when the uid-wide pointer names a running daemon.
func TestMCPResolvesStrictlyForAnExplicitIdentity(t *testing.T) {
	rt := t.TempDir()
	if err := os.Chmod(rt, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", rt)
	running, addr := servedDaemon(t)
	if err := os.MkdirAll(filepath.Join(rt, "anet"), 0o700); err != nil {
		t.Fatal(err)
	}
	ptr, _ := json.Marshal(map[string]string{"control_addr": addr, "data_dir": running.Root})
	if err := os.WriteFile(filepath.Join(rt, "anet", "daemon.json"), ptr, 0o600); err != nil {
		t.Fatal(err)
	}
	other := daemon.NewLayout(t.TempDir()) // selected explicitly, has no daemon
	done := make(chan error, 1)
	go func() { done <- runMCP(other, true) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("anet mcp for an identity without a daemon connected to another daemon")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("anet mcp for an identity without a daemon started serving another daemon")
	}
}

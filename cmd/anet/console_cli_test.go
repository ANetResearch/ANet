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
//
// ANET_HOME is a temporary directory for the test: daemon.New starts the
// compiled-in modules, and module/a2a (linked into this package) binds a
// port and reads the other identities under ANET_HOME to skip their A2A
// ports. The operator's real ~/.anet is neither read nor raced for a port.
func servedDaemon(t *testing.T) (daemon.Layout, string) {
	t.Helper()
	t.Setenv("ANET_HOME", t.TempDir())
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

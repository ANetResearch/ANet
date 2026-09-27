package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// The CLI's liveness probes (anet up/daemon --detach, anet doctor) carry the
// control token, so they are held to the rule the daemon starts under: a
// loopback control address or nothing (A2A-DESIGN §7.1). A config naming a
// wildcard or LAN address — what control_allow_remote used to allow — is
// never sent the token. The listener here is bound on 0.0.0.0, which a
// connection to 0.0.0.0 reaches on this machine, so the probe would arrive
// if it were made.
func TestTheCLISendsTheControlTokenOnlyToLoopback(t *testing.T) {
	var seen atomic.Int32
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skipf("cannot listen on the wildcard address: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			seen.Add(1)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	root := t.TempDir()
	wild := "0.0.0.0:" + port
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"control_addr":"`+wild+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := daemon.NewLayout(root)
	if err := os.WriteFile(layout.ControlTokenPath(), []byte("secret-control-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if localDaemonUp(layout) {
		t.Error("localDaemonUp reported a daemon at a wildcard control address")
	}
	if daemonAnswersAt(wild, layout.ControlTokenPath()) {
		t.Error("doctor's probe reported a daemon at a wildcard control address")
	}
	if n := seen.Load(); n != 0 {
		t.Fatalf("the control token was sent to %s %d time(s)", wild, n)
	}
	// The same listener over a loopback name is probed, so what kept the
	// token back above is the rule, not an unreachable address.
	if !daemonAnswersAt("127.0.0.1:"+port, layout.ControlTokenPath()) || seen.Load() == 0 {
		t.Fatal("the probe does not reach a loopback control address")
	}
}

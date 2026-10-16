package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// An emergency rotation changes the daemon's control token while the command waits for it; the
// next /identity/status call is made with the token the data directory holds then, not the
// one read at the start (T-1 F2: "the daemon answered 401: unauthorized", exit 1).
func TestIdentityCallsFollowAControlTokenThatChanged(t *testing.T) {
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
	current := "token-1"
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+current {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"state":"ok","key_seq":4}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	write := func(tok string) {
		if err := os.WriteFile(layout.ControlTokenPath(), []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(current)
	x := newIdentityCLI(layout, true, nil)
	st, err := x.statusNow()
	if err != nil || st.KeySeq != 4 || x.c == nil {
		t.Fatalf("first call: %+v %v", st, err)
	}
	current = "token-2" // the rotation swapped the token
	write(current)
	st, done, err := x.settle(4)
	if err != nil || !done || st.KeySeq != 4 {
		t.Fatalf("after the token changed: %+v done=%v err=%v", st, done, err)
	}
	var out daemonStatusProbe
	current = "token-3"
	write(current)
	if err := x.post("/identity/status", map[string]any{}, &out); err != nil {
		t.Fatalf("post after a second change: %v", err)
	}
}

type daemonStatusProbe struct {
	State string `json:"state"`
}

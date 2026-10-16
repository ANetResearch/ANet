package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// An emergency rotation replaces the daemon's control token while the CLI is
// still polling for the result. The old token is answered 401; the command
// reads the token file again and goes on, instead of ending with "the daemon
// answered 401" after the rotation completed.
func TestIdentityCallsFollowAReplacedControlToken(t *testing.T) {
	layout := daemon.NewLayout(t.TempDir())
	current := "new-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+current {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"state":"ok"}`))
	}))
	defer srv.Close()
	if err := os.WriteFile(layout.ConfigPath(), []byte(`{"control_addr":"`+strings.TrimPrefix(srv.URL, "http://")+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.ControlTokenPath(), []byte(current+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	x := &identityCLI{layout: layout, explicit: true,
		c: &client{base: srv.URL, token: "old-token", timeout: 5 * time.Second, dataDir: layout.Root}}
	b, code, err := x.postRaw("/identity/status", map[string]any{})
	if err != nil || code != 200 {
		t.Fatalf("postRaw: %d %v %s", code, err, b)
	}
	if x.c.token != current {
		t.Errorf("token %q, want the replaced one", x.c.token)
	}
	// A token file that holds a token the daemon refuses is a real 401.
	current = "rotated-again"
	if _, code, _ := x.postRaw("/identity/status", nil); code != 401 {
		t.Errorf("code %d, want 401", code)
	}
}

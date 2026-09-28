package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// F41 (red team, supply): the hub invite was taken as `--token <invite>`, so it sat in the argv of anet
// (and of install.sh, which passed it on), where every local user can read it from /proc/<pid>/cmdline
// and register with it first — a single-use invite is then spent for its owner. The invite now comes
// from ANET_INVITE or --token-file, and a value on the command line is refused before anything is sent.
func TestHubRegisterTakesTheInviteOffTheCommandLine(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"registered":true}`))
	}))
	defer ctl.Close()
	c := &client{base: ctl.URL, token: "ctl"}
	last := func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		if len(bodies) == 0 {
			return nil
		}
		return bodies[len(bodies)-1]
	}
	t.Setenv(inviteEnv, "")

	// On the command line: refused, and nothing reaches the daemon.
	for _, args := range [][]string{
		{"https://hub.example", "--name", "n", "--token", "anetinv_ARGV"},
		{"https://hub.example", "--token=anetinv_ARGV"},
	} {
		err := runHubRegister(c, args)
		if !errors.Is(err, errInviteOnCommandLine) || !strings.Contains(err.Error(), inviteEnv) {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if b := last(); b != nil {
		t.Fatalf("the daemon was called: %v", b)
	}

	// From the environment.
	t.Setenv(inviteEnv, " anetinv_ENV\n")
	_ = captureStdout(t, func() {
		if err := runHubRegister(c, []string{"https://hub.example", "--name", "n"}); err != nil {
			t.Error(err)
		}
	})
	if b := last(); b == nil || b["token"] != "anetinv_ENV" {
		t.Fatalf("invite from %s: %v", inviteEnv, b)
	}

	// From a file, which wins over the environment.
	f := filepath.Join(t.TempDir(), "invite")
	if err := os.WriteFile(f, []byte("anetinv_FILE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = captureStdout(t, func() {
		if err := runHubRegister(c, []string{"https://hub.example", "--token-file", f}); err != nil {
			t.Error(err)
		}
	})
	if b := last(); b == nil || b["token"] != "anetinv_FILE" {
		t.Fatalf("invite from --token-file: %v", b)
	}

	// None: an open hub needs none, and none is sent.
	t.Setenv(inviteEnv, "")
	_ = captureStdout(t, func() { _ = runHubRegister(c, []string{"https://hub.example"}) })
	if b := last(); b == nil || b["token"] != nil {
		t.Fatalf("no invite: %v", b)
	}
}

// The detached daemon does not inherit an invite from the shell that started it.
func TestTheDaemonDoesNotInheritTheInvite(t *testing.T) {
	env := envWithout([]string{"A=1", inviteEnv + "=anetinv_x", "ANET_INVITEX=keep", "B=2"}, inviteEnv)
	if strings.Join(env, " ") != "A=1 ANET_INVITEX=keep B=2" {
		t.Fatalf("%v", env)
	}
}

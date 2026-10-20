package main

// `anet hub-register --passphrase-later` (design D9, 2.5): the join takes the
// path of KEL D-11 even with a terminal, so it asks nothing.

import (
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// noPrompt makes opening the terminal a test failure.
func noPrompt(t *testing.T) {
	t.Helper()
	prev := openTTY
	openTTY = func() (io.ReadWriteCloser, error) {
		t.Errorf("the terminal was opened")
		return nil, errors.New("no terminal in this test")
	}
	t.Cleanup(func() { openTTY = prev })
}

// With a terminal present and --passphrase-later, no prompt is opened; the
// join carries next_digest and unprotected, and the unprotected recovery
// files of D-11 are written, private to the user.
func TestHubRegisterPassphraseLaterWithATerminal(t *testing.T) {
	pending := joinAnswer(daemon.JoinPendingAdmission, "hub_pending_admission", a2ashape.Args{"hub": testHub, "aid": testAID}, true)
	hc := &hubControl{dryStatus: 200, dry: dryJoin(), joinStatus: 200, join: pending}
	c := hc.serve(t, nil)
	c.dataDir, _ = freshIdentityDir(t)
	h, out, errb := testHubCLI(nil)
	asked := 0
	h.terminal = func() bool { asked++; return true }
	noPrompt(t)
	if err := h.hubRegister(c, []string{testHub, "--name", "coder", "--passphrase-later"}); err != nil {
		t.Fatalf("exit: %v (stderr %s)", err, errb)
	}
	if asked != 0 {
		t.Fatalf("the terminal was looked for %d times", asked)
	}
	if !strings.Contains(out.String(), "anet identity protect") || errb.Len() != 0 {
		t.Fatalf("stdout:\n%s\nstderr: %s", out, errb)
	}
	reqs := hc.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests %v", reqs)
	}
	j := reqs[1]
	digest, err := base64.StdEncoding.DecodeString(j["next_digest"].(string))
	if err != nil || len(digest) != 32 || j["unprotected"] != true || j["name"] != "coder" {
		t.Fatalf("join request %v", j)
	}
	for _, p := range []string{localKeyPath(c.dataDir), localRecPath(c.dataDir)} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !privateFile(p, fi) {
			t.Fatalf("%s is not private", p)
		}
	}
}

// A hub switch is a key rotation that needs the passphrase: with
// --passphrase-later it is not attempted here, and the daemon answers it.
func TestHubRegisterPassphraseLaterDoesNotSwitch(t *testing.T) {
	refused := map[string]any{"error": "switching hubs needs a key rotation", "reason": "hub_switch_requires_rotation"}
	hc := &hubControl{dryStatus: 200, dry: map[string]any{"hub": testHub, "action": "switch"}, joinStatus: 409, join: refused}
	c := hc.serve(t, nil)
	c.dataDir, _ = freshIdentityDir(t)
	h, _, errb := testHubCLI(nil)
	h.terminal = func() bool { return true }
	noPrompt(t)
	if err := h.hubRegister(c, []string{testHub, "--passphrase-later"}); !errors.Is(err, errExit1) {
		t.Fatalf("exit: %v", err)
	}
	if reqs := hc.requests(); len(reqs) != 2 || reqs[1]["next_digest"] != nil {
		t.Fatalf("requests %v", reqs)
	}
	if !strings.Contains(errb.String(), "hub_switch_requires_rotation") {
		t.Fatalf("stderr: %s", errb)
	}
}

// --passphrase-later is a usage error together with a flag that needs a
// passphrase now, or with a value; nothing is asked of the daemon.
func TestHubRegisterPassphraseLaterUsage(t *testing.T) {
	for _, args := range [][]string{
		{testHub, "--passphrase-later", "--recovery-file", "/tmp/r.anetrec"},
		{testHub, "--new-recovery", "--passphrase-later"},
		{testHub, "--passphrase-later=no"},
	} {
		hc := &hubControl{dryStatus: 200, dry: dryJoin()}
		c := hc.serve(t, nil)
		c.dataDir, _ = freshIdentityDir(t)
		h, _, _ := testHubCLI(nil)
		noPrompt(t)
		var u usageError
		if err := h.hubRegister(c, args); !errors.As(err, &u) || !strings.Contains(u.usage, "--passphrase-later") {
			t.Fatalf("%v: %v", args, err)
		}
		if reqs := hc.requests(); len(reqs) != 0 {
			t.Fatalf("%v: requests %v", args, reqs)
		}
	}
}

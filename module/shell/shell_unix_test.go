//go:build shell && !windows

package shell

// shell_unix_test.go holds the tests whose command lines are POSIX shell (/bin/sh): redirections,
// background jobs, pipes and paths that only a POSIX system has. The Windows counterparts are in
// shell_windows_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
)

// The command line belongs to the operator; the arguments come off the
// network. Quoting is what keeps the two from becoming the same thing.
func TestCallerArgumentsCannotBecomeCommands(t *testing.T) {
	cfg := Config{AllowLocal: true, Commands: map[string]Command{
		"say": {Run: "echo", Args: true},
	}}
	_, h := started(t, cfg)
	eff, err := invoke(t, h, runPrefix+"say", "", map[string]any{
		"argv": []any{"hi; touch /tmp/anet-shell-injection", "&& whoami", "$(id)", "`id`"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := eff.Evidence.ObservedState
	// Every metacharacter came back as text, which is only possible if
	// the shell treated the whole thing as one word.
	for _, want := range []string{"hi; touch", "&& whoami", "$(id)", "`id`"} {
		if !strings.Contains(out, want) {
			t.Fatalf("argument %q was interpreted rather than passed: %q", want, out)
		}
	}
	if _, err := os.Stat("/tmp/anet-shell-injection"); err == nil {
		os.Remove("/tmp/anet-shell-injection")
		t.Fatal("an injected argument executed")
	}
	if strings.Contains(out, "uid=") {
		t.Fatalf("command substitution ran: %q", out)
	}
}

func TestExecRefusesAnUnknownArgumentKey(t *testing.T) {
	cfg := echoCfg()
	cfg.AllowArbitrary = true
	_, h := started(t, cfg)
	if _, err := invoke(t, h, CapExec, "", map[string]any{"cmd": "id"}); err == nil {
		t.Fatal("shell.exec accepted a key it ignores")
	}
	if _, err := invoke(t, h, CapExec, "", map[string]any{"command": "echo ok", "dir": "/"}); err != nil {
		t.Fatalf("the documented keys were refused: %v", err)
	}
}

// A command that failed is not a call that failed: the node did what it
// was asked and the answer is the failure. Reporting OK with the output
// attached would have the caller act on a result that never happened.
func TestNonZeroExitIsFailedAndCarriesTheCodeAndOutput(t *testing.T) {
	_, h := started(t, echoCfg())
	eff, err := invoke(t, h, runPrefix+"fail", "", nil)
	if err != nil {
		t.Fatalf("a failing command is an effect, not a transport error: %v", err)
	}
	if eff.Status != effect.Failed {
		t.Fatalf("exit 3 must be FAILED, got %s", eff.Status)
	}
	if eff.Record.Metrics["exit_code"] != 3 {
		t.Fatalf("the exit code must survive: %+v", eff.Record.Metrics)
	}
	// stderr is where a failing command says why. Losing it leaves the
	// caller with a number.
	if !strings.Contains(eff.Evidence.ObservedState, "to-stderr") {
		t.Fatalf("stderr must be captured: %q", eff.Evidence.ObservedState)
	}
}

// A command that was killed must not look like one that finished with no
// output, which is what a caller would otherwise act on.
func TestTimeoutIsFailedAndKillsTheWholeProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survivor")
	cfg := Config{AllowLocal: true, Commands: map[string]Command{
		// A grandchild that outlives its shell unless the group is killed.
		"slow": {Run: "(sleep 2 && touch " + marker + ") & sleep 10", TimeoutS: 1},
	}}
	_, h := started(t, cfg)

	start := time.Now()
	eff, err := invoke(t, h, runPrefix+"slow", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Failed {
		t.Fatalf("a killed command must be FAILED, got %s", eff.Status)
	}
	if !strings.Contains(eff.Message, "killed") {
		t.Fatalf("the message must say it was killed, got %q", eff.Message)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the timeout did not take effect: %s", d)
	}
	// The grandchild would have created the marker at t+2s.
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a process survived the timeout: only the shell was killed, not its group")
	}
}

// Unbounded output is how one command takes down the daemon holding it.
func TestOutputIsBounded(t *testing.T) {
	cfg := Config{
		AllowLocal:     true,
		MaxOutputBytes: 1024,
		Commands:       map[string]Command{"flood": {Run: "yes anet | head -c 100000"}},
	}
	_, h := started(t, cfg)
	eff, err := invoke(t, h, runPrefix+"flood", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	out := eff.Evidence.ObservedState
	if len(out) > 1024+64 {
		t.Fatalf("output was not bounded: %d bytes", len(out))
	}
	// Silent truncation would have the caller read a cut-off result as a
	// complete one.
	if !strings.Contains(out, "truncated") {
		t.Fatalf("truncation must be stated in the output: %q", out[max(0, len(out)-100):])
	}
}

// Every execution is on this node's own chain before the caller hears
// back, so the record exists whether or not the answer ever arrives.
func TestEveryExecutionIsRecordedAsEvidence(t *testing.T) {
	cfg := echoCfg()
	cfg.Allow = []string{"did:anet:friend"}
	_, h := started(t, cfg)
	if _, err := invoke(t, h, runPrefix+"fail", "did:anet:friend", nil); err != nil {
		t.Fatal(err)
	}
	runs := h.events(EvCommandRun)
	if len(runs) != 1 {
		t.Fatalf("expected one record, got %d", len(runs))
	}
	r := runs[0]
	if r["caller"] != "did:anet:friend" || r["command"] != "fail" {
		t.Fatalf("the record must name who asked and what ran: %+v", r)
	}
	if r["exit_code"] != 3 {
		t.Fatalf("a failed run must be recorded as failed: %+v", r)
	}
}

// On unix the interpreter is what the operator configured, or /bin/sh, run as `<shell> -c <line>`, and
// newModule accepts every value, as before the Windows port.
func TestUnixInterpreterIsAsConfigured(t *testing.T) {
	for configured, want := range map[string]string{"": "/bin/sh", "/bin/bash": "/bin/bash", "zsh": "zsh"} {
		ip, err := resolveInterpreter(configured)
		if err != nil || ip.path != want || ip.family != familyPosix {
			t.Fatalf("%q: %+v %v", configured, ip, err)
		}
	}
	m, err := newModule([]byte(`{"allow_arbitrary":true,"shell":"notepad.exe"}`))
	if err != nil || m.(*Module).ip.path != "notepad.exe" {
		t.Fatalf("newModule: %+v %v", m, err)
	}
	ip, _ := resolveInterpreter("")
	cmd, err := ip.command(t.Context(), "echo x")
	if err != nil || strings.Join(cmd.Args, "\x00") != "/bin/sh\x00-c\x00echo x" {
		t.Fatalf("command: %q %v", cmd.Args, err)
	}
	if dir, err := commandDir(""); dir != "" || err != nil {
		t.Fatalf("commandDir: %q %v", dir, err)
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// shellCtlAID is a whole AID (identity.ValidAID).
const shellCtlAID = "bafyreie6k7xot2qpitfcoseuevnuq7zcztdpdgx4coe3p2jlqjjl65hqu4"

// shellCtlTTY is a terminal that answers answer once; opened counts how often a terminal was opened.
type shellCtlTTY struct {
	in     io.Reader
	prompt bytes.Buffer
}

func (f *shellCtlTTY) Read(p []byte) (int, error)  { return f.in.Read(p) }
func (f *shellCtlTTY) Write(p []byte) (int, error) { return f.prompt.Write(p) }
func (f *shellCtlTTY) Close() error                { return nil }

// withShellCtl sets up a binary that contains the shell module (or not), and a terminal answering
// answer ("" for no terminal). It returns a pointer to the number of terminals opened.
func withShellCtl(t *testing.T, hasShell bool, answer string) *int {
	t.Helper()
	prevTTY, prevErr, prevCompiled := openTTY, ttyStderr, compiledModules
	t.Cleanup(func() { openTTY, ttyStderr, compiledModules = prevTTY, prevErr, prevCompiled })
	ttyStderr = io.Discard
	compiledModules = func() []string {
		if hasShell {
			return []string{"a2a", "shell"}
		}
		return []string{"a2a"}
	}
	opened := new(int)
	openTTY = func() (io.ReadWriteCloser, error) {
		*opened++
		if answer == "" {
			return nil, errors.New("no terminal")
		}
		return &shellCtlTTY{in: strings.NewReader(answer + "\n")}, nil
	}
	return opened
}

func shellCtlLayout(t *testing.T) daemon.Layout {
	t.Helper()
	return daemon.NewLayout(filepath.Join(t.TempDir(), "data"))
}

// The configuration's modules.shell and the peers.allow entries of layout.
func shellCtlOnDisk(t *testing.T, l daemon.Layout) (map[string]any, string) {
	t.Helper()
	raw, err := os.ReadFile(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Modules map[string]map[string]any `json:"modules"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	allow, _ := os.ReadFile(filepath.Join(l.Root, "peers.allow"))
	return cfg.Modules["shell"], string(allow)
}

// D9, 2.5: yes on the terminal writes the block (with "shell" only on Windows) and the allow entry,
// asked once; a second run asks nothing and exits 0.
func TestInitShellControllerCommandYes(t *testing.T) {
	l := shellCtlLayout(t)
	opened := withShellCtl(t, true, "yes")
	var out bytes.Buffer
	if err := initShellControllerTo(&out, l, false, shellCtlAID); err != nil {
		t.Fatalf("error %v (exit %d)", err, exitStatus(err, io.Discard))
	}
	if *opened != 1 {
		t.Fatalf("terminal opened %d times, want 1", *opened)
	}
	block, allow := shellCtlOnDisk(t, l)
	want := map[string]any{"allow": []any{shellCtlAID}, "allow_arbitrary": true}
	if interp := shellControllerInterp(); interp != "" {
		want["shell"] = interp
	}
	if !reflect.DeepEqual(block, want) {
		t.Fatalf("modules.shell = %v", block)
	}
	if !strings.Contains(allow, shellCtlAID+"\n") {
		t.Fatalf("peers.allow = %q", allow)
	}
	for _, s := range []string{"+ modules.shell {", "+ peers.allow " + shellCtlAID, "Only " + shellCtlAID + " can run commands", "anet up"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out.String())
		}
	}

	out.Reset()
	if err := initShellControllerTo(&out, l, false, shellCtlAID); err != nil {
		t.Fatal(err)
	}
	if *opened != 1 {
		t.Fatalf("an unchanged grant opened the terminal (%d)", *opened)
	}
	if !strings.Contains(out.String(), "already name "+shellCtlAID+"; nothing changed") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// Any answer but yes, and no terminal at all, end with exit status 1 and nothing written.
func TestInitShellControllerCommandNoAndNoTerminal(t *testing.T) {
	for _, answer := range []string{"no", ""} {
		l := shellCtlLayout(t)
		withShellCtl(t, true, answer)
		err := initShellControllerTo(io.Discard, l, false, shellCtlAID)
		if err == nil || exitStatus(err, io.Discard) != 1 {
			t.Fatalf("answer %q: error %v", answer, err)
		}
		if _, err := os.Stat(l.Root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("answer %q: the data directory was created", answer)
		}
	}
}

// --json prints the plan.
func TestInitShellControllerCommandJSON(t *testing.T) {
	l := shellCtlLayout(t)
	withShellCtl(t, true, "yes")
	var out bytes.Buffer
	if err := initShellControllerTo(&out, l, true, shellCtlAID); err != nil {
		t.Fatal(err)
	}
	var plan daemon.ShellControllerPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("%v:\n%s", err, out.String())
	}
	if plan.AID != shellCtlAID || !plan.Changed || !plan.AddAllow || plan.ModuleBlock == nil {
		t.Fatalf("plan %+v", plan)
	}
}

// Input that has to change is exit status 2, before a terminal is opened or anything is written.
func TestInitShellControllerCommandRefusals(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		hasShell bool
	}{
		{"missing value", []string{"--shell-controller"}, true},
		{"with --without", []string{"--shell-controller", shellCtlAID, "--without", "cas"}, true},
		{"with --with", []string{"--shell-controller", shellCtlAID, "--with", "cas"}, true},
		{"short aid", []string{"--shell-controller", "bafyreie6k7"}, true},
		{"no shell module", []string{"--shell-controller", shellCtlAID}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := shellCtlLayout(t)
			opened := withShellCtl(t, tc.hasShell, "yes")
			err := runInit(l, tc.args)
			if err == nil || exitStatus(err, io.Discard) != 2 {
				t.Fatalf("error %v", err)
			}
			if *opened != 0 {
				t.Fatal("a terminal was opened")
			}
			if _, err := os.Stat(l.Root); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the data directory was created")
			}
		})
	}
	t.Run("conflicting block", func(t *testing.T) {
		l := shellCtlLayout(t)
		if err := os.MkdirAll(l.Root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(l.ConfigPath(), []byte(`{"modules":{"shell":{"allow":["`+shellCtlAID+`"]}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		opened := withShellCtl(t, true, "yes")
		err := initShellControllerTo(io.Discard, l, false, shellCtlAID)
		if err == nil || exitStatus(err, io.Discard) != 2 || *opened != 0 {
			t.Fatalf("error %v, terminal opened %d times", err, *opened)
		}
	})
}

// A running daemon (its data lock held) is exit status 1, with nothing asked.
func TestInitShellControllerCommandDaemonRunning(t *testing.T) {
	l := shellCtlLayout(t)
	held, err := daemon.AcquireDataLock(l)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	opened := withShellCtl(t, true, "yes")
	prev := os.Stderr
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stderr = devnull
	err = initShellControllerTo(io.Discard, l, false, shellCtlAID)
	os.Stderr = prev
	devnull.Close()
	if err == nil || exitStatus(err, io.Discard) != 1 || *opened != 0 {
		t.Fatalf("error %v, terminal opened %d times", err, *opened)
	}
}

package daemon

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallHermesIdempotentAndPreserving(t *testing.T) {
	home := t.TempDir()
	soul := filepath.Join(home, "SOUL.md")
	if err := os.WriteFile(soul, []byte("# SOUL\nYou are a helpful agent.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMES_HOME", home)

	for i := 0; i < 2; i++ { // run twice: must not duplicate the managed block
		changes, err := InstallAgent("hermes")
		if err != nil {
			t.Fatalf("install run %d: %v", i, err)
		}
		if len(changes) == 0 {
			t.Fatalf("install run %d reported no changes", i)
		}
	}
	s := readFile(t, soul)
	if n := strings.Count(s, anetBlockBegin); n != 1 {
		t.Fatalf("want exactly 1 anet block, got %d", n)
	}
	if !strings.Contains(s, "You are a helpful agent.") {
		t.Fatal("original persona content was lost")
	}
	if !strings.Contains(s, "anet find") || !strings.Contains(s, "AgentNetwork") {
		t.Fatal("anet guidance missing from SOUL.md")
	}
}

func TestInstallHermesCreatesSoulWhenMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	if _, err := InstallAgent("hermes"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, filepath.Join(home, "SOUL.md")), anetBlockBegin) {
		t.Fatal("SOUL.md not created with anet block")
	}
}

func TestInstallUnknownAgentErrors(t *testing.T) {
	if _, err := InstallAgent("nope"); err == nil {
		t.Fatal("expected error for unknown agent")
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every agent the join page offers must actually install.
//
// The hub's join page tells a newcomer to run `anet install --agent <id>`
// for five agents, and only hermes had a test. The others write to
// different places under $HOME — .cursor/rules, .claude, .codex,
// .openclaw — and a path that broke would fail at the first step a new
// user takes, with nothing to tell them whether the page or their typing
// was wrong.
//
// Rooted at a temporary HOME so a test never edits the developer's own
// agent configuration.
func TestEveryOfferedAgentInstalls(t *testing.T) {
	for _, id := range SupportedInstallAgents() {
		t.Run(id, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("HERMES_HOME", filepath.Join(home, ".hermes"))
			if id == "hermes" {
				if err := os.MkdirAll(filepath.Join(home, ".hermes"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			changes, err := InstallAgent(id)
			if err != nil {
				t.Fatalf("install %s: %v", id, err)
			}
			if len(changes) == 0 {
				t.Fatalf("install %s reported no changes — an install that "+
					"tells the operator nothing is indistinguishable from one "+
					"that did nothing", id)
			}
			// Something was written, under this HOME and nowhere else.
			var found []string
			_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					found = append(found, p)
				}
				return nil
			})
			if len(found) == 0 {
				t.Fatalf("install %s changed nothing on disk but reported %v", id, changes)
			}
			// And the guidance is in it, or the agent's model will not
			// learn anything from the install.
			var carries bool
			for _, p := range found {
				if raw, err := os.ReadFile(p); err == nil &&
					strings.Contains(string(raw), "AgentNetwork") {
					carries = true
				}
			}
			if !carries {
				t.Errorf("install %s wrote %v, none of which mentions the network",
					id, found)
			}
		})
	}
}

// Installing twice must not write the guidance twice.
//
// A persona file rides in every prompt. A block that accumulates on each
// install grows the prompt without adding anything, and an operator
// re-running a command they are unsure about is the ordinary case, not a
// mistake.
func TestInstallingTwiceIsOneBlock(t *testing.T) {
	for _, id := range SupportedInstallAgents() {
		t.Run(id, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("HERMES_HOME", filepath.Join(home, ".hermes"))
			if id == "hermes" {
				if err := os.MkdirAll(filepath.Join(home, ".hermes"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if _, err := InstallAgent(id); err != nil {
					t.Fatalf("install %d: %v", i, err)
				}
			}
			_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				raw, rerr := os.ReadFile(p)
				if rerr != nil {
					return nil
				}
				if n := strings.Count(string(raw), "AgentNetwork"); n > 1 &&
					strings.Count(string(raw), "anet:begin") > 1 {
					t.Errorf("%s carries the guidance %d times after two installs", p, n)
				}
				return nil
			})
		})
	}
}

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `anet install --agent X` is how an operator's coding tool learns that
// this network exists. Every agent in the registry must actually write
// something an agent will read, into a path that agent reads — an install
// that reports success and writes nothing leaves the tool unaware of
// anet while telling the operator it is wired up.
//
// hermes is skipped: it installs into an existing hermes-agent home and
// refuses when there is none, which is correct behaviour and not
// something a temp HOME can stand in for.
func TestEveryAgentInstallWritesGuidanceSomewhereTheAgentReads(t *testing.T) {
	for _, a := range agentRegistry() {
		if a.id == agentHermes {
			continue
		}
		t.Run(a.id, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)

			notes, err := a.install()
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			if len(notes) == 0 {
				t.Fatal("install reported nothing it did")
			}

			var written []string
			err = filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return err
				}
				b, rerr := os.ReadFile(p)
				if rerr == nil && strings.Contains(string(b), "anet") {
					written = append(written, p)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(written) == 0 {
				t.Fatalf("install said %q but wrote no file mentioning anet under %s", notes, home)
			}
			// The note has to name the file, or an operator cannot check
			// or undo what was done to their machine.
			joined := strings.Join(notes, " ")
			named := false
			for _, w := range written {
				if strings.Contains(joined, filepath.Base(w)) {
					named = true
				}
			}
			if !named {
				t.Errorf("install wrote %v but its report %q names none of them", written, notes)
			}
		})
	}
}

// Installing twice must not duplicate the block. An operator re-running
// install after an upgrade is the normal case, and a file that grows a
// copy of the guidance each time eventually crowds out the agent's own
// instructions.
func TestInstallingTwiceLeavesOneCopy(t *testing.T) {
	for _, a := range agentRegistry() {
		if a.id == agentHermes {
			continue
		}
		t.Run(a.id, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			if _, err := a.install(); err != nil {
				t.Fatal(err)
			}
			if _, err := a.install(); err != nil {
				t.Fatalf("second install: %v", err)
			}
			_ = filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return err
				}
				b, rerr := os.ReadFile(p)
				if rerr != nil {
					return nil
				}
				if n := strings.Count(string(b), anetBlockBegin); n > 1 {
					t.Errorf("%s carries the managed block %d times", p, n)
				}
				return nil
			})
		})
	}
}

// Two agents that share a file-name convention must not share the file.
// codex and opencode both read AGENTS.md; installing one and not the
// other would otherwise silently wire up both, and uninstalling one would
// take the other's guidance with it.
func TestCodexAndOpencodeDoNotShareAFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if _, err := installCodex(); err != nil {
		t.Fatal(err)
	}
	codexFile := filepath.Join(home, ".codex", "AGENTS.md")
	opencodeFile := filepath.Join(home, ".config", "opencode", "AGENTS.md")
	if _, err := os.Stat(codexFile); err != nil {
		t.Fatalf("codex install did not write %s: %v", codexFile, err)
	}
	if _, err := os.Stat(opencodeFile); err == nil {
		t.Error("installing codex also wired up opencode")
	}

	if _, err := installOpenCode(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(opencodeFile); err != nil {
		t.Fatalf("opencode install did not write %s: %v", opencodeFile, err)
	}
}

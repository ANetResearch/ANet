package scripts

// scripts/mutations/mutate.sh builds a mutated binary set, runs joint.sh section C against it and says
// whether the run caught the mutation. Its verdict is what a mutation is recorded with, so it must not
// read another run's reports as this one's.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mutate.sh check reads the section C reports of the latest joint.sh run in J, and only those: a run
// that stopped before replacing J/run leaves an earlier run's reports behind — perhaps of another
// mutation, which was caught — and they must not make this one look caught.
func TestMutateCheckCountsOnlyTheLatestRun(t *testing.T) {
	needLinuxShell(t)
	mut, err := filepath.Abs(filepath.Join("mutations", "mutate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	check := func(j string) (string, int) {
		cmd := exec.Command("bash", mut, "check", j)
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}
	j := t.TempDir()
	can := filepath.Join(j, "run", "canary")
	if err := os.MkdirAll(can, 0o700); err != nil {
		t.Fatal(err)
	}
	hit := []byte(`{"hits":[{"canary":"goal","variant":"base64/1","where":"traffic.log","offset":1}]}`)
	clean := []byte(`{"hits":[]}`)
	if out, code := check(j); code != 2 {
		t.Errorf("no .joint-dir: exit %d, want 2: %s", code, out)
	}
	marker := filepath.Join(j, ".joint-dir")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := check(j); code != 2 {
		t.Errorf("no reports: exit %d, want 2: %s", code, out)
	}
	// An earlier run's report with a hit, then a run that stopped at its start.
	old := filepath.Join(can, "scan-c-tap.json")
	if err := os.WriteFile(old, hit, 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if out, code := check(j); code != 2 || strings.Contains(out, "caught") {
		t.Errorf("only an earlier run's report: exit %d, want 2: %s", code, out)
	}
	// This run's reports: clean, then with a hit.
	if err := os.WriteFile(filepath.Join(can, "scan-c-hubdir.json"), clean, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := check(j); code != 1 || !strings.Contains(out, "SURVIVED") {
		t.Errorf("this run found nothing: exit %d, want 1: %s", code, out)
	}
	if err := os.WriteFile(filepath.Join(can, "scan-c-admindir.json"), hit, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := check(j); code != 0 || !strings.Contains(out, "caught") || !strings.Contains(out, "c-admindir") {
		t.Errorf("this run found a canary: exit %d, want 0 naming the surface: %s", code, out)
	}
}

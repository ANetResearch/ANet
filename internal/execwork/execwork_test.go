package execwork

import (
	"os"
	"path/filepath"
	"testing"
)

// SweepAll removes the work directories under both roots and the session bindings of the interactions that
// are not open, and leaves the open ones and the self-test directory.
func TestSweepAllRemovesWhatEndedAndKeepsWhatIsOpen(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	aid := "bafyreiexampleaidforthetest0000000000"
	state := t.TempDir()
	mk := func(root, ix string) string {
		p := filepath.Join(root, AidShort(aid), IxDirName(ix))
		if err := os.MkdirAll(filepath.Join(p, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ended, open, selftest := mk(WorkRoot(), "ix-ended"), mk(WorkRoot(), "ix-open"), mk(SandboxWorkRoot(), "")
	endedSB := mk(SandboxWorkRoot(), "ix-ended")
	s := Sessions(state)
	s.Set("ix-ended", "s1")
	s.Set("ix-open", "s2")
	werr, serr := SweepAll(state, aid, func() (map[string]bool, error) { return map[string]bool{"ix-open": true}, nil })
	if werr != nil || serr != nil {
		t.Fatal(werr, serr)
	}
	for _, p := range []string{ended, endedSB} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s stays (%v)", p, err)
		}
	}
	for _, p := range []string{open, selftest} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	if s.Get("ix-ended") != "" || s.Get("ix-open") != "s2" {
		t.Errorf("sessions: ended %q, open %q", s.Get("ix-ended"), s.Get("ix-open"))
	}
	if _, err := os.Stat(filepath.Join(state, "sessions.json")); err != nil {
		t.Errorf("the store is not persisted in its state directory: %v", err)
	}
}

func TestIxDirNameCarriesNothingOfTheID(t *testing.T) {
	got := IxDirName("ix_secret/../../x")
	if len(got) != len("ix-")+32 || filepath.Base(got) != got {
		t.Errorf("%q", got)
	}
	if IxDirName("") != "selftest" {
		t.Error("no interaction is the self-test")
	}
}

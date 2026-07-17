package daemon

import (
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

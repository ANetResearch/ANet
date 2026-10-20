//go:build !windows

package autoreply

import (
	"os"
	"path/filepath"
	"testing"
)

// On unix an agent binary's name is compared exactly.
func TestAgentBinaryNamesAreExactOnUnix(t *testing.T) {
	for bin, want := range map[string]bool{
		"/usr/bin/cursor": true, "cursor": true, "/opt/Cursor": false, "/usr/bin/cursor.exe": false, "/usr/bin/cursor-agent": false,
	} {
		if got := agentBinIs(bin, "cursor"); got != want {
			t.Errorf("agentBinIs(%q, cursor) = %v, want %v", bin, got, want)
		}
	}
}

// An agent found only through a relative PATH entry (exec.ErrDot) is not found: an agent never runs from
// the daemon's working directory.
func TestAgentInTheWorkingDirectoryIsNotFound(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("PATH", ".")
	if p, err := detectClaudeBin(); err == nil {
		t.Fatalf("an agent in the working directory was found: %s", p)
	}
	t.Setenv("PATH", dir)
	if p, err := detectClaudeBin(); err != nil || p != filepath.Join(dir, "claude") {
		t.Fatalf("an agent on PATH: %q %v", p, err)
	}
}

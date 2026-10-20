//go:build !windows

package anethome

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNameTaken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANET_HOME", home)
	if NameTaken("coder") {
		t.Error("taken without an ids directory")
	}
	if err := os.MkdirAll(filepath.Join(home, IDsDir, "coder"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, IDsDir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !NameTaken("coder") || !NameTaken("file") || NameTaken("other") {
		t.Errorf("NameTaken: coder %v, file %v, other %v", NameTaken("coder"), NameTaken("file"), NameTaken("other"))
	}
	// Exact match: on a case-sensitive file system another case is another name.
	if runtime.GOOS == "linux" && NameTaken("Coder") {
		t.Error(`NameTaken("Coder") on linux`)
	}
}

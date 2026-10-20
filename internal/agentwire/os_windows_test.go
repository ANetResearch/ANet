//go:build !no_mcp && windows

package agentwire

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

// Windows names are case-insensitive and .exe is optional, in any case.
func TestWindowsIsAnetCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		`C:\x\ANET.EXE`: true,
		`C:\Users\A B\AppData\Local\Programs\anet\anet.exe`: true,
		`C:\x\Anet`:      true,
		`anet`:           true,
		`C:/x/anet.Exe`:  true,
		`C:\x\anet.exe2`: false,
		`C:\x\anetd.exe`: false,
		`C:\x\anet.cmd`:  false,
		`C:\x\exe`:       false,
	} {
		if got := isAnetCommand(cmd); got != want {
			t.Errorf("isAnetCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// A tool CLI that is a batch file is not run (cmd.exe would parse the arguments); an .exe is.
func TestWindowsToolCLIRefusesBatchShim(t *testing.T) {
	for p, ok := range map[string]bool{
		`C:\npm\claude.cmd`:           false,
		`C:\npm\claude.BAT`:           false,
		`C:\Program Files\claude.exe`: true,
	} {
		o := &Options{LookPath: func(string) (string, error) { return p, nil }}
		got, err := toolCLI(o, "claude")
		if ok != (err == nil) || (ok && got != p) {
			t.Errorf("%s: %q %v", p, got, err)
		}
		if !ok && !errors.Is(err, errBatchShim) {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// A file written with a private mode has a private ACL; its mode reads as 0600, and an absent file
// as 0. Narrowing an existing file's mode to 0600 makes its ACL private.
func TestWindowsPrivateWrites(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := writeAtomic(p, []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckPrivate(p); err != nil {
		t.Fatal(err)
	}
	if m := accessMode(p); m != 0o600 {
		t.Fatalf("mode %o", m)
	}
	if m := accessMode(filepath.Join(dir, "absent")); m != 0 {
		t.Fatalf("absent: mode %o", m)
	}
	q := filepath.Join(dir, "other.yaml")
	if err := os.WriteFile(q, []byte("b: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := chmodPrivate(q, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckPrivate(q); err != nil {
		t.Fatal(err)
	}
	o := &Options{Now: func() time.Time { return time.Unix(0, 0) }}
	bak, err := backup(o, q, []byte("secret"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckPrivate(bak); err != nil {
		t.Fatalf("backup: %v", err)
	}
}

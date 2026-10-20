package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The restart hint is two commands on two lines: Windows PowerShell 5.1
// has no && operator.
func TestWindowsUpdateRestartHint(t *testing.T) {
	var b bytes.Buffer
	printRestartHint(&b)
	if got := b.String(); got != "    anet stop --all\n    anet up --all\n" || strings.Contains(got, "&&") {
		t.Fatalf("restart hint %q", got)
	}
}

// A path that cannot be resolved is replaced where it is.
func TestWindowsUpdateResolveExe(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent", "anet.exe")
	if got, err := resolveExe(missing); err != nil || got != missing {
		t.Fatalf("resolveExe(%q) = %q, %v", missing, got, err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveExe(exe)
	if err != nil {
		t.Fatal(err)
	}
	a, err1 := os.Stat(got)
	b, err2 := os.Stat(exe)
	if err1 != nil || err2 != nil || !os.SameFile(a, b) {
		t.Fatalf("resolveExe(%q) = %q, not the same file (%v, %v)", exe, got, err1, err2)
	}
}

// --verify-dir on Windows: the running test binary, named in a manifest as
// the windows build of its variant, is verified by its own sha256.
func TestWindowsUpdateVerifyDir(t *testing.T) {
	dir, _ := verifyDirRelease(t, nil)
	var err error
	out, errOut := captureCLI(t, func() { err = runUpdate([]string{"--verify-dir", dir, "--installed", "0.0.1"}) })
	if err != nil || errOut != "" || !strings.Contains(out, `"asset":"`) || !strings.Contains(out, "-windows-") {
		t.Fatalf("err %v, stdout %q, stderr %q", err, out, errOut)
	}
}

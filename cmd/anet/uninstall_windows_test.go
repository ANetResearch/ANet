//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The binary is renamed aside (<exe>.old-<milliseconds>) and its deletion
// scheduled; anet.exe is gone at once. The registry is not touched here.
func TestUninstallWindowsRenamesTheRunningBinary(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "anet.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	note, err := removeInstalledExe(exe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(exe); err == nil {
		t.Fatal("anet.exe is still there")
	}
	asides, _ := filepath.Glob(filepath.Join(dir, "anet.exe.old-*"))
	if len(asides) != 1 || !strings.Contains(note, asides[0]) {
		t.Fatalf("asides %v, note %q", asides, note)
	}
	if installerDir(dir) {
		t.Error("a temporary directory taken for an install directory of install.ps1")
	}
	if !sameExe(strings.ToUpper(exe), exe) {
		t.Error("paths that differ in case only")
	}
}

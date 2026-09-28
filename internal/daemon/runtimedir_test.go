package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// §7.8: a runtime dir is used only when it is a real directory of this uid with mode 0700.
func TestCheckPrivateDirRefusesLinksAndOpenModes(t *testing.T) {
	ok := filepath.Join(t.TempDir(), "ok")
	if err := os.Mkdir(ok, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateDir(ok); err != nil {
		t.Fatalf("private dir refused: %v", err)
	}
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if checkPrivateDir(open) == nil {
		t.Error("a 0755 dir accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(ok, link); err != nil {
		t.Fatal(err)
	}
	if checkPrivateDir(link) == nil {
		t.Error("a symbolic link to a private dir accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if checkPrivateDir(file) == nil {
		t.Error("a regular file accepted")
	}
}

// ensurePrivateDir narrows an own dir to 0700 and refuses a symbolic link.
func TestEnsurePrivateDirNarrowsAndRefusesLinks(t *testing.T) {
	d := filepath.Join(t.TempDir(), "rt")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(d); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(d, link); err != nil {
		t.Fatal(err)
	}
	if ensurePrivateDir(link) == nil {
		t.Fatal("a symbolic link accepted as runtime dir")
	}
}

// RuntimeDir prefers $XDG_RUNTIME_DIR/anet when XDG_RUNTIME_DIR is private, and ignores it otherwise.
func TestRuntimeDirPrefersAPrivateXDGRuntimeDir(t *testing.T) {
	x := t.TempDir()
	if err := os.Chmod(x, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", x)
	if got := RuntimeDir(); got != filepath.Join(x, "anet") {
		t.Fatalf("RuntimeDir = %q", got)
	}
	if err := os.Chmod(x, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := RuntimeDir(); got != legacyRuntimeDir() {
		t.Fatalf("a non-private XDG_RUNTIME_DIR was used: %q", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "relative/dir")
	if got := RuntimeDir(); got != legacyRuntimeDir() {
		t.Fatalf("a relative XDG_RUNTIME_DIR was used: %q", got)
	}
}

// Readers skip a registry or pointer in a runtime dir that another user could have prepared.
func TestRegistryAndPointerInANonPrivateDirAreIgnored(t *testing.T) {
	x := t.TempDir()
	if err := os.Chmod(x, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", x)
	rd := filepath.Join(x, "anet")
	if err := os.MkdirAll(filepath.Join(rd, "daemons"), 0o700); err != nil {
		t.Fatal(err)
	}
	entry, _ := json.Marshal(IdentityEntry{AID: "E-planted", Name: "planted", ControlAddr: "127.0.0.1:1", DataDir: "/nonexistent"})
	if err := os.WriteFile(filepath.Join(rd, "daemons", "p.json"), entry, 0o600); err != nil {
		t.Fatal(err)
	}
	ptr, _ := json.Marshal(daemonPointer{ControlAddr: "127.0.0.1:1", DataDir: "/nonexistent-planted"})
	if err := os.WriteFile(filepath.Join(rd, "daemon.json"), ptr, 0o600); err != nil {
		t.Fatal(err)
	}
	found := func() (bool, bool) {
		reg, _ := listRegistry()
		inReg := false
		for _, e := range reg {
			if e.AID == "E-planted" {
				inReg = true
			}
		}
		b, _ := readDaemonPointerFile()
		return inReg, b != nil && json.Valid(b) && string(b) == string(ptr)
	}
	if inReg, inPtr := found(); !inReg || !inPtr {
		t.Fatalf("private dir: registry %v pointer %v, want both read", inReg, inPtr)
	}
	// An open registry dir inside a private runtime dir is skipped on its own.
	if err := os.Chmod(filepath.Join(rd, "daemons"), 0o755); err != nil {
		t.Fatal(err)
	}
	if inReg, inPtr := found(); inReg || !inPtr {
		t.Fatalf("open registry dir: registry %v pointer %v, want only the pointer read", inReg, inPtr)
	}
	// An open runtime dir is skipped entirely.
	if err := os.Chmod(rd, 0o755); err != nil {
		t.Fatal(err)
	}
	if inReg, inPtr := found(); inReg || inPtr {
		t.Fatalf("a non-private runtime dir was read: registry %v pointer %v", inReg, inPtr)
	}
}

// A daemon that stops removes the uid-scoped pointer only while it still names that daemon: the last
// daemon to start owns the pointer, and one stopping afterwards must not strand it (0034 §G7.3).
func TestStoppingADaemonLeavesAnotherDaemonsPointer(t *testing.T) {
	x := t.TempDir()
	if err := os.Chmod(x, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", x)
	p := DaemonPointerPath()

	writeDaemonPointer("127.0.0.1:29610", "/var/lib/a/.anet")  // A starts
	writeDaemonPointer("127.0.0.1:39811", "/root/.anet")       // B starts later and owns the pointer
	removeDaemonPointer("127.0.0.1:29610", "/var/lib/a/.anet") // A stops
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("A's shutdown removed B's pointer: %v", err)
	}
	var dp daemonPointer
	if json.Unmarshal(b, &dp) != nil || dp.ControlAddr != "127.0.0.1:39811" || dp.DataDir != "/root/.anet" {
		t.Fatalf("pointer after A stopped = %s, want B's", b)
	}
	// The same address with another data dir is another daemon too.
	removeDaemonPointer("127.0.0.1:39811", "/somewhere/else")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("a daemon with another data dir removed the pointer: %v", err)
	}

	removeDaemonPointer("127.0.0.1:39811", "/root/.anet") // B stops
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("B's own pointer survived B's shutdown: %v", err)
	}
	removeDaemonPointer("127.0.0.1:39811", "/root/.anet") // nothing left: no error, nothing written
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("pointer reappeared: %v", err)
	}
}

//go:build windows

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/ANetResearch/ANet/internal/fsperm"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// D12: the runtime dir is <LocalAppData>\anet\run, read from the known folder, and the only candidate.
// Nothing is created there by this test.
func TestWindowsRuntimeDirIsUnderLocalAppData(t *testing.T) {
	lad, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		t.Skipf("this account has no LocalAppData known folder: %v", err)
	}
	want := filepath.Join(lad, "anet", "run")
	if got := RuntimeDir(); got != want {
		t.Fatalf("RuntimeDir() = %q, want %q", got, want)
	}
	if got := legacyRuntimeDir(); got != want {
		t.Fatalf("legacyRuntimeDir() = %q, want %q", got, want)
	}
	if DaemonPointerPath() != filepath.Join(want, "daemon.json") || DaemonsDir() != filepath.Join(want, "daemons") {
		t.Fatalf("pointer %q, registry %q", DaemonPointerPath(), DaemonsDir())
	}
	for _, c := range runtimeDirCandidates() {
		if c != want {
			t.Fatalf("candidate %q", c)
		}
	}
}

// D2, D12: ensurePrivateDir creates a private directory; checkPrivateDir refuses a directory whose access
// control list grants another account access, and a junction, with errNotPrivateDir.
func TestWindowsCheckPrivateDir(t *testing.T) {
	base := t.TempDir()
	priv := filepath.Join(base, "a", "b", "rt")
	if err := ensurePrivateDir(priv); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateDir(priv); err != nil {
		t.Fatalf("a directory ensurePrivateDir made is refused: %v", err)
	}
	if err := fsperm.CheckPrivate(priv); err != nil {
		t.Fatal(err)
	}

	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	setDACL(t, open, everyoneReadSDDL, true)
	err := checkPrivateDir(open)
	if !errors.Is(err, errNotPrivateDir) || !errors.Is(err, fsperm.ErrNotPrivate) {
		t.Fatalf("a directory readable by Everyone: %v", err)
	}
	// ensurePrivateDir repairs a directory of this user.
	if err := ensurePrivateDir(open); err != nil {
		t.Fatalf("ensurePrivateDir did not repair a directory of this user: %v", err)
	}
	if err := checkPrivateDir(open); err != nil {
		t.Fatal(err)
	}

	junction := filepath.Join(base, "junction")
	makeJunction(t, junction, priv)
	if err := checkPrivateDir(junction); !errors.Is(err, errNotPrivateDir) {
		t.Fatalf("a junction to a private directory: %v", err)
	}
	if err := ensurePrivateDir(junction); err == nil {
		t.Fatal("ensurePrivateDir accepted a junction")
	}

	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if checkPrivateDir(file) == nil {
		t.Fatal("a regular file accepted")
	}
}

// Layout.EnsureRoot makes the data directory private even under a parent that grants Everyone access.
func TestWindowsEnsureRootIsPrivate(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	setDACL(t, parent, everyoneWriteSDDL, true)
	l := NewLayout(filepath.Join(parent, "data"))
	if err := l.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckPrivate(l.Root); err != nil {
		t.Fatal(err)
	}
}

// D13: the data lock is exclusive per handle, also within one process, and released with the handle.
func TestWindowsDataLock(t *testing.T) {
	l := NewLayout(filepath.Join(t.TempDir(), "data"))
	if DataLockHeld(l) {
		t.Fatal("a directory without a lock file is held")
	}
	k, err := AcquireDataLock(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireDataLock(l); !errors.Is(err, ErrDataLockHeld) {
		t.Fatalf("second AcquireDataLock: %v, want ErrDataLockHeld", err)
	}
	if !DataLockHeld(l) {
		t.Fatal("DataLockHeld false while the lock is held")
	}
	k.Release()
	if DataLockHeld(l) {
		t.Fatal("DataLockHeld true after Release")
	}
	k2, err := AcquireDataLock(l)
	if err != nil {
		t.Fatalf("the lock cannot be taken again after Release: %v", err)
	}
	k2.Release()
}

// F13: the boot clock reads QueryInterruptTime and moves forward.
func TestWindowsBootClock(t *testing.T) {
	a := bootClock()
	if a == 0 {
		t.Fatal("bootClock() = 0: QueryInterruptTime could not be read")
	}
	b := bootClock()
	if b < a {
		t.Fatalf("the boot clock went back: %d then %d", a, b)
	}
}

// D2: a file written by writeFileAtomic (0600) or writeSecretFile is private, whatever its directory
// lets it inherit, and replacing an existing file works.
func TestWindowsSecretWritesArePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setDACL(t, dir, everyoneReadSDDL, true)
	for i, write := range []func(string, []byte) error{
		func(p string, b []byte) error { return writeFileAtomic(p, b, 0o600) },
		writeSecretFile,
		func(p string, b []byte) error { return writeFileSync(p, b, 0o600) },
		func(p string, b []byte) error { return writeMboxFile(p, b, nil) },
	} {
		p := filepath.Join(dir, "secret"+string(rune('a'+i)))
		for _, content := range []string{"one", "two"} {
			if err := write(p, []byte(content)); err != nil {
				t.Fatalf("writer %d: %v", i, err)
			}
			if b, err := os.ReadFile(p); err != nil || string(b) != content {
				t.Fatalf("writer %d: read %q, %v", i, b, err)
			}
			if err := fsperm.CheckPrivate(p); err != nil {
				t.Fatalf("writer %d: %v", i, err)
			}
		}
	}
	// A file meant to be readable by others is not narrowed.
	p := filepath.Join(dir, "public")
	if err := writeFileAtomic(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := fsperm.Inspect(p); err != nil || !info.OthersRead {
		t.Fatalf("a 0644 file was narrowed: %+v, %v", info, err)
	}
}

// Setup: a config.json readable by others is made private and reported once; a private one is not
// reported.
func TestWindowsInitLayoutNarrowsConfig(t *testing.T) {
	l := NewLayout(filepath.Join(t.TempDir(), "data"))
	if _, err := InitLayout(l); err != nil {
		t.Fatal(err)
	}
	setDACL(t, l.ConfigPath(), everyoneReadSDDL, true)
	rep, err := InitLayout(l)
	if err != nil {
		t.Fatal(err)
	}
	var modes []InitChange
	for _, c := range rep.Changes {
		if c.Action == "mode" {
			modes = append(modes, c)
		}
	}
	if len(modes) != 1 || modes[0].Key != "config.json" || modes[0].Note != "not private → private" {
		t.Fatalf("changes %+v", rep.Changes)
	}
	if err := fsperm.CheckPrivate(l.ConfigPath()); err != nil {
		t.Fatal(err)
	}
	rep, err = InitLayout(l)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Changes {
		if c.Action == "mode" {
			t.Fatalf("a private config.json reported as changed: %+v", c)
		}
	}
}

// Section 3: a pull into a directory of this user works and the file is private; a pull subdirectory
// that other users can write to, or a junction in its place, is refused; an out_dir that is a junction
// is refused; a pull into a shared out_dir makes its own subdirectory private.
func TestWindowsPull(t *testing.T) {
	atts := []*interactions.Attachment{pullAtt("report.md", []byte("peer content"))}

	t.Run("own directory", func(t *testing.T) {
		out := longTempDir(t)
		res, err := pullInto(out, "ix_win_000001", atts)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 {
			t.Fatalf("results %+v", res)
		}
		if b, err := os.ReadFile(res[0].Path); err != nil || string(b) != "peer content" {
			t.Fatalf("read %q, %v", b, err)
		}
		if err := fsperm.CheckPrivate(res[0].Path); err != nil {
			t.Fatal(err)
		}
		// Pulled again: the same content is already there.
		res, err = pullInto(out, "ix_win_000001", atts)
		if err != nil || len(res) != 1 || !res[0].AlreadyPresent {
			t.Fatalf("second pull: %+v, %v", res, err)
		}
	})

	t.Run("shared out_dir", func(t *testing.T) {
		out := filepath.Join(longTempDir(t), "shared")
		if err := os.Mkdir(out, 0o700); err != nil {
			t.Fatal(err)
		}
		setDACL(t, out, everyoneWriteSDDL, true)
		res, err := pullInto(out, "ix_win_000002", atts)
		if err != nil {
			t.Fatal(err)
		}
		if err := fsperm.CheckPrivate(filepath.Dir(res[0].Path)); err != nil {
			t.Fatalf("the pull subdirectory is not private: %v", err)
		}
		if err := fsperm.CheckPrivate(res[0].Path); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("subdirectory writable by others", func(t *testing.T) {
		out := longTempDir(t)
		sub := filepath.Join(out, "anet-ix_win_00000")
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		setDACL(t, sub, everyoneWriteSDDL, true)
		_, err := pullInto(out, "ix_win_000003", atts)
		if err == nil || !strings.Contains(err.Error(), "writable by other users") {
			t.Fatalf("error %v", err)
		}
		if entries, _ := os.ReadDir(sub); len(entries) != 0 {
			t.Fatal("a file was written into the refused directory")
		}
	})

	t.Run("subdirectory is a junction", func(t *testing.T) {
		out := longTempDir(t)
		elsewhere := longTempDir(t)
		makeJunction(t, filepath.Join(out, "anet-ix_win_00000"), elsewhere)
		if _, err := pullInto(out, "ix_win_000004", atts); err == nil {
			t.Fatal("a junction as the pull subdirectory was accepted")
		}
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
			t.Fatal("a file was written through the junction")
		}
	})

	t.Run("out_dir is a junction", func(t *testing.T) {
		base := longTempDir(t)
		elsewhere := longTempDir(t)
		link := filepath.Join(base, "out")
		makeJunction(t, link, elsewhere)
		_, err := pullInto(link, "ix_win_000005", atts)
		if err == nil || !strings.Contains(err.Error(), "changed while it was being opened") {
			t.Fatalf("error %v", err)
		}
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
			t.Fatal("a file was written through the junction")
		}
	})
}

// The launcher page is written into the runtime dir without following a reparse point: an existing
// name is never opened (O_EXCL) and the page is private.
func TestWindowsOpenNoFollowCreatesPrivateFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rt")
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "page.html")
	f, err := fsperm.OpenNoFollow(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := fsperm.CheckPrivate(p); err != nil {
		t.Fatal(err)
	}
	if _, err := fsperm.OpenNoFollow(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("an existing name was opened: %v", err)
	}
}

// modules.p2p is refused at start on Windows with config_invalid.
func TestWindowsP2PConfigRefused(t *testing.T) {
	r := checkP2PConfig(Config{}, map[string]any{"modules": map[string]any{"p2p": map[string]any{"socket": `C:\peer.sock`}}})
	if r == nil || r.Key != "config_invalid" || r.Args["key"] != "modules.p2p" {
		t.Fatalf("refusal %+v", r)
	}
	if r := checkP2PConfig(Config{}, map[string]any{"modules": map[string]any{}}); r != nil {
		t.Fatalf("a configuration without modules.p2p refused: %+v", r)
	}
}

//go:build linux

package autoreply

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ANetResearch/ANet/internal/execwork"
)

// BU-8: what the disk watch adds up is the work dir and the run directories of the processes still
// running: sandboxPlan.command registers its run directory, whose tmp/ is the agent's /tmp, and Close
// takes it off. Without bubblewrap: nothing is started.
func TestTheDiskWatchSeesTheRunDirectory(t *testing.T) {
	plan := &sandboxPlan{Bwrap: "/usr/bin/bwrap", AID: "EaidDiskWatchRunDir01", Home: filepath.Join(t.TempDir(), "home")}
	agent := writeStub(t, "#!/bin/sh\necho hi\n")
	l, err := plan.command(t.TempDir(), []string{agent}, []string{"PATH=/usr/bin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	live := plan.liveRunDirs()
	if len(live) != 1 || !strings.HasPrefix(filepath.Base(live[0]), runDirPrefix(plan.AID)+"-") {
		l.Close()
		t.Fatalf("the running process's run directories: %v", live)
	}
	if n := dirsSize(append(plan.liveRunDirs(), t.TempDir()), 1<<20); n > 1<<20 {
		l.Close()
		t.Fatalf("%d bytes before the agent wrote anything", n)
	}
	if err := os.WriteFile(filepath.Join(live[0], "tmp", "hog"), make([]byte, 4<<20), 0o600); err != nil {
		l.Close()
		t.Fatal(err)
	}
	if n := dirsSize(append(plan.liveRunDirs(), t.TempDir()), 1<<20); n <= 1<<20 {
		t.Errorf("%d bytes: the watch does not see what the agent wrote to /tmp", n)
	}
	l.Close()
	if live := plan.liveRunDirs(); len(live) != 0 {
		t.Fatalf("run directories still watched after Close: %v", live)
	}
	if _, err := os.Stat(live[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the run directory survived Close: %v", err)
	}
}

// BU-8: a file counts by the space it holds, not only by its size: an agent that allocates blocks past
// the end of a file (fallocate --keep-size) does not slip under the limit with a file of size zero.
func TestDirsSizeCountsAllocatedBlocks(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "held"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const held = 4 << 20
	if err := syscall.Fallocate(int(f.Fd()), 0x01 /* FALLOC_FL_KEEP_SIZE */, 0, held); err != nil {
		t.Skipf("the file system here cannot allocate past a file's end: %v", err)
	}
	if fi, _ := f.Stat(); fi.Size() != 0 {
		t.Fatalf("size %d after fallocate --keep-size", fi.Size())
	}
	if n := dirsSize([]string{dir}, 1<<40); n < held {
		t.Fatalf("%d bytes for a file holding %d", n, held)
	}
}

// SB-9: before a daemon's first sandboxed run, the run directories and socket directories its
// identity's earlier runs left are removed — read-only leftovers included — and another identity's
// are not; it happens once.
func TestSandboxLeftoversAreSweptBeforeTheFirstRun(t *testing.T) {
	d := newBareDaemon(t)
	prefix := runDirPrefix(d.AID())
	mine := filepath.Join(sandboxRunRoot(), prefix+"-leftover")
	other := filepath.Join(sandboxRunRoot(), "zzzzzzzz-leftover")
	sock := filepath.Join(os.TempDir(), sandboxSockDirPrefix+prefix+"-leftover")
	for _, dir := range []string{mine, other, sock} {
		ro := filepath.Join(dir, "tmp", "ro")
		if err := os.MkdirAll(ro, 0o700); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(ro, "f"), []byte("x"), 0o400)
		_ = os.Chmod(ro, 0o500)
	}
	t.Cleanup(func() { execwork.RemoveTree(other) })
	d.sweepSandboxLeftovers()
	for _, gone := range []string{mine, sock} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the sweep: %v", gone, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another identity's run directory went: %v", err)
	}
	// The temp dir is shared and the prefix is public: a directory named like ours that this user
	// does not own is not swept.
	theirs := filepath.Join(os.TempDir(), sandboxSockDirPrefix+prefix+"-theirs")
	if err := os.MkdirAll(theirs, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { execwork.RemoveTree(theirs) })
	leftoverOwner = func() int { return os.Getuid() + 1 }
	removeSandboxLeftovers(d.AID())
	leftoverOwner = os.Getuid
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("a directory of another user's went: %v", err)
	}
	again := filepath.Join(sandboxRunRoot(), prefix+"-new")
	if err := os.MkdirAll(again, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { execwork.RemoveTree(again) })
	d.sweepSandboxLeftovers()
	if _, err := os.Stat(again); err != nil {
		t.Errorf("a second sweep removed a run directory made after the first: %v", err)
	}
}

// noSandbox makes the host's sandbox unavailable for the test, and returns the local reason a refused
// run is recorded with: no bubblewrap.
func noSandbox(t *testing.T) string {
	t.Helper()
	bwrapLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { bwrapLookPath = exec.LookPath })
	return sandboxCodeNoBwrap
}

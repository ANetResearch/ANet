package daemon

import (
	"os"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// KEL-10: a second daemon on a data directory, and a command that looks at the lock, both see it held.
func TestDataDirLockIsHeldByARunningDaemon(t *testing.T) {
	d := newTestDaemon(t, "", true)
	if !DataLockHeld(d.layout) {
		t.Fatal("a running daemon does not hold the data directory lock")
	}
	if _, err := New(d.layout); err == nil || !isLockHeld(err) {
		t.Fatalf("a second daemon on the same directory: %v", err)
	}
	root := d.layout.Root
	d.Close()
	if DataLockHeld(Layout{Root: root}) {
		t.Fatal("the lock outlives the daemon")
	}
	if fi, err := os.Stat(Layout{Root: root}.LockPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file: %v %v", fi, err)
	}
	// A directory without a lock file is not held.
	if DataLockHeld(Layout{Root: t.TempDir()}) {
		t.Fatal("an empty directory is held")
	}
}

func isLockHeld(err error) bool { return err != nil && err.Error() == ErrDataLockHeld.Error() }

// A damaged identity file is identity_file_invalid at start and in the offline status: one sentence
// with the file's path, not the Go error (ux E2).
func TestDamagedIdentityFileIsSaidInOneSentence(t *testing.T) {
	layout := v02DataDir(t, false, func(l Layout) { writeIdentityTag(t, l, identityTagThisVersion) })
	before, _ := os.ReadFile(layout.IdentityPath())
	if err := CheckDataDirForStart(layout); !isRefusal(err, "identity_file_invalid") {
		t.Fatalf("CheckDataDirForStart: %v", err)
	}
	d, err := New(layout)
	if d != nil {
		d.Close()
	}
	if !isRefusal(err, "identity_file_invalid") {
		t.Fatalf("New: %v", err)
	}
	if after, _ := os.ReadFile(layout.IdentityPath()); string(after) != string(before) {
		t.Fatal("the damaged file changed")
	}
	if _, err := ReadIdentityStatus(layout, a2ashape.English); !isRefusal(err, "identity_file_invalid") {
		t.Fatalf("ReadIdentityStatus: %v", err)
	}
	if _, err := LoadOrGenerateIdentity(layout); !isRefusal(err, "identity_file_invalid") {
		t.Fatalf("LoadOrGenerateIdentity: %v", err)
	}
}

const identityTagThisVersion = "anet.identity/2"

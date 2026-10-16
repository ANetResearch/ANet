package daemon

// datalock.go is the data directory lock (KEL §8.1 KEL-10, FS-9 follow-up): a running daemon holds
// an exclusive flock on <data directory>/daemon.lock for as long as the process lives, and the
// operating system releases it when the process ends, however it ends. A command with an offline
// branch (`anet retention set`, the steps of `anet identity` that work on files) looks at the lock
// before it touches anything: a lock that is held while the control plane does not answer means a
// daemon is there and busy, and writing the files under it would be undone by it.

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrDataLockHeld is a data directory whose lock another process (or another daemon of this
// process) holds.
var ErrDataLockHeld = errors.New("anet: the data directory is in use by a running daemon")

// DataLock is a held data directory lock.
type DataLock struct{ f *os.File }

// LockPath is the lock file of the data directory.
func (l Layout) LockPath() string { return filepath.Join(l.Root, "daemon.lock") }

// AcquireDataLock takes the lock of l, creating the file (0600) when it is not there. It returns
// ErrDataLockHeld when a daemon holds it.
func AcquireDataLock(l Layout) (*DataLock, error) {
	if err := l.EnsureRoot(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(l.LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockData(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &DataLock{f: f}, nil
}

// Release gives the lock up. It is safe on nil and on a released lock.
func (k *DataLock) Release() {
	if k == nil || k.f == nil {
		return
	}
	_ = k.f.Close()
	k.f = nil
}

// DataLockHeld reports whether a daemon holds the lock of l. It takes the lock itself for the
// moment of the check, so a directory without a lock file, or one that cannot be opened, is not
// held.
func DataLockHeld(l Layout) bool {
	if _, err := os.Stat(l.LockPath()); err != nil {
		return false
	}
	k, err := AcquireDataLock(l)
	if errors.Is(err, ErrDataLockHeld) {
		return true
	}
	k.Release()
	return false
}

//go:build windows

package daemon

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockData takes an exclusive lock on the first byte of f without waiting (LockFileEx with
// LOCKFILE_EXCLUSIVE_LOCK|LOCKFILE_FAIL_IMMEDIATELY). The lock belongs to the handle, as a flock belongs
// to the open file description: a lock held through another handle, in this process or another, is
// ErrDataLockHeld. The system releases it when the handle is closed or the process ends.
func lockData(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &windows.Overlapped{})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION), errors.Is(err, windows.ERROR_IO_PENDING):
		return ErrDataLockHeld
	}
	return err
}

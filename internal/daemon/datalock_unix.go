//go:build unix

package daemon

import (
	"errors"
	"os"
	"syscall"
)

// lockData takes the exclusive, non-blocking flock on f. A lock held by another open file
// description, in this process or another, is ErrDataLockHeld.
func lockData(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return ErrDataLockHeld
		}
		return err
	}
}

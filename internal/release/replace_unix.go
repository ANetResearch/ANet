//go:build !windows

package release

import (
	"os"
	"path/filepath"
)

// updateTempPattern names the file Apply writes beside the binary before it
// is checked and renamed into place.
const updateTempPattern = ".anet-update-*"

// chmodTemp gives the staged binary the mode of the one it replaces.
func chmodTemp(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }

// replaceExe renames the checked binary tmp over exe.
func replaceExe(tmp, exe string) error {
	if err := os.Rename(tmp, exe); err != nil {
		return err
	}
	// The rename is durable once the directory entry is; without this a
	// crash right after could come back with the old name pointing nowhere
	// on some filesystems. Best effort: not every platform can fsync a
	// directory, and the rename has already happened.
	if d, derr := os.Open(filepath.Dir(exe)); derr == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// SweepAside removes the binaries an earlier update on Windows moved aside.
// Elsewhere a rename replaces a running binary directly and leaves none, so
// there is nothing to remove.
func SweepAside(exe string) {}

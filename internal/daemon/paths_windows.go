//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

// RuntimeDir is the per-user, HOME-independent runtime dir this process writes cross-process
// coordination files to: the single-daemon pointer and the multi-daemon identity registry (see
// DaemonsDir).
//
// On Windows it is <LocalAppData>\anet\run, where LocalAppData is read from the known folder
// (FOLDERID_LocalAppData) rather than from %LOCALAPPDATA%, so a CLI started from an agent's tool
// sandbox with a different environment resolves the same directory as the daemon. Every write and read
// goes through checkPrivateDir, which refuses a reparse point, a directory owned by another account, or
// one whose access control list grants access to any other account.
func RuntimeDir() string {
	return filepath.Join(localAppData(), "anet", "run")
}

// localAppData is the known folder FOLDERID_LocalAppData of this user. When the known folder cannot be
// read, os.UserCacheDir (%LOCALAPPDATA%) is used, and failing that the temporary directory; whichever
// is used is still checked by checkPrivateDir before anything in it is trusted.
func localAppData() string {
	if d, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0); err == nil && filepath.IsAbs(d) {
		return d
	}
	if d, err := os.UserCacheDir(); err == nil && filepath.IsAbs(d) {
		return d
	}
	return os.TempDir()
}

// legacyRuntimeDir is RuntimeDir on Windows: no earlier release used another location there.
func legacyRuntimeDir() string { return RuntimeDir() }

// runtimeDirCandidates lists every runtime dir a daemon of this user may have written to. On Windows
// that is RuntimeDir alone, and only when it passes checkPrivateDir.
func runtimeDirCandidates() []string {
	d := RuntimeDir()
	if checkPrivateDir(d) != nil {
		return nil
	}
	return []string{d}
}

// notPrivateDirError carries fsperm's account of why a directory is not private while also matching
// errNotPrivateDir, so that callers that test errors.Is(err, errNotPrivateDir) behave as on unix.
type notPrivateDirError struct{ err error }

func (e notPrivateDirError) Error() string   { return e.err.Error() }
func (e notPrivateDirError) Unwrap() []error { return []error{e.err, errNotPrivateDir} }

// checkPrivateDir verifies that dir is a directory that is not a symbolic link, junction or other
// name-surrogate reparse point, whose owner is this user (or SYSTEM, or the Administrators group), and
// whose access control list grants no right to any other account (fsperm.CheckPrivate).
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s: %w (symbolic link, junction or not a directory)", dir, errNotPrivateDir)
	}
	if err := fsperm.CheckPrivate(dir); err != nil {
		if errors.Is(err, fsperm.ErrNotPrivate) {
			return notPrivateDirError{err}
		}
		return err
	}
	return nil
}

// ensurePrivateDir creates dir (and missing parents) and then verifies it with checkPrivateDir. A
// missing leaf is created with a protected access control list that names only this user, SYSTEM and
// the Administrators group. An existing directory of this user whose access control list grants access
// to others is made private first (fsperm.MkdirAllPrivate); a reparse point or a directory of another
// account is refused.
func ensurePrivateDir(dir string) error {
	if err := fsperm.MkdirAllPrivate(dir); err != nil {
		return err
	}
	return checkPrivateDir(dir)
}

// EnsureRoot creates the root data dir, private to this user (it holds private keys).
func (l Layout) EnsureRoot() error { return fsperm.MkdirAllPrivate(l.Root) }

// protectTemp gives a temporary file the private access control list (this user, SYSTEM,
// Administrators) before it is renamed into place, so the file is private under its final name whatever
// the directory would have let it inherit.
func protectTemp(path string) error { return fsperm.Protect(path) }

// syncDir would make a rename in dir durable. Windows cannot flush a directory handle (FlushFileBuffers
// on a directory opened for reading fails with ERROR_ACCESS_DENIED); NTFS journals the rename as part
// of its metadata, so there is nothing further to do and syncDir reports success.
func syncDir(string) error { return nil }

// runtimeSensitivePaths are the per-user anet directories outside the data directory that a sandbox
// never binds: RuntimeDir and the <LocalAppData>\anet directory that holds it.
func runtimeSensitivePaths() []string {
	return []string{RuntimeDir(), filepath.Join(localAppData(), "anet")}
}

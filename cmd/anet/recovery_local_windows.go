//go:build windows

package main

import (
	"errors"
	"io/fs"
	"os"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

// mkdirDurable makes the missing directory of a writeFileDurable file private to this user (a
// protected ACL: this user, SYSTEM, Administrators). A directory that exists is used as it is, as
// on unix, where MkdirAll leaves an existing directory's mode alone.
func mkdirDurable(dir string) error {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return fsperm.MkdirAllPrivate(dir)
	}
	return os.MkdirAll(dir, 0o700)
}

// protectDurableTemp gives the temporary file a private ACL before anything is written to it when perm
// grants nothing to group or other (0600: a recovery file, a local key). Chmod on Windows only sets
// the read-only attribute and does not restrict who can read the file.
func protectDurableTemp(tmp string, perm os.FileMode) error {
	if perm&0o077 != 0 {
		return nil
	}
	return fsperm.Protect(tmp)
}

// renameDurable puts the temporary file in place, retrying while a reader holds the old file.
func renameDurable(tmp, path string) error { return fsperm.ReplaceFile(tmp, path) }

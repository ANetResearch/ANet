//go:build unix

package fsperm

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Inspect reads Info for path. It never follows a final link.
func Inspect(path string) (Info, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Info{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Info{}, &os.PathError{Op: "lstat", Path: path, Err: errors.New("no owner information")}
	}
	perm := fi.Mode().Perm()
	return Info{
		Link:        fi.Mode()&os.ModeSymlink != 0,
		OwnedBySelf: int64(st.Uid) == int64(os.Getuid()),
		Owner:       fmt.Sprintf("uid %d", st.Uid),
		OthersRead:  perm&0o077 != 0,
		OthersWrite: perm&0o022 != 0,
		Mode:        perm,
		dir:         fi.IsDir(),
		readWhy:     fmt.Sprintf("mode %04o", uint32(perm)),
	}, nil
}

// MkdirAllPrivate creates dir and missing parents.
// unix: exactly os.MkdirAll(dir, 0o700) (no check; callers that check on unix keep their own code).
// windows: every missing directory, the parents and the leaf, is created with CreateDirectory and a
// SECURITY_ATTRIBUTES whose descriptor is "O:<user>D:P(A;OICI;FA;;;<user>)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)";
// an existing leaf that is a Link or not OwnedBySelf is an error (ErrNotPrivate); one that is OwnedBySelf
// but OthersRead is Protect-ed; the result must pass CheckPrivate.
func MkdirAllPrivate(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// CreateTempPrivate is os.CreateTemp for a file that holds a secret.
// unix: exactly os.CreateTemp(dir, pattern) (the file is created 0600).
// windows: the file is created with the private descriptor (OpenNoFollow with O_CREATE|O_EXCL and 0o600).
func CreateTempPrivate(dir, pattern string) (*os.File, error) {
	return os.CreateTemp(dir, pattern)
}

// Protect makes path private. unix: os.Chmod(path, 0o700) for a directory, 0o600 otherwise.
// windows: SetSecurityInfo(DACL|PROTECTED_DACL), on the handle the check was made on (opened without
// following a reparse point), with user:F, SYSTEM:F, Administrators:F (OI|CI on directories).
// A Link, or a path not OwnedBySelf, is refused with ErrNotPrivate.
func Protect(path string) error {
	i, err := Inspect(path)
	if err != nil {
		return err
	}
	if err := refuseUnowned(path, i); err != nil {
		return err
	}
	if i.dir {
		return os.Chmod(path, 0o700)
	}
	return os.Chmod(path, 0o600)
}

// OpenNoFollow is os.OpenFile that refuses a final symbolic link or other name-surrogate reparse point
// (error: *os.PathError wrapping syscall.ELOOP).
// unix: os.OpenFile(name, flag|syscall.O_NOFOLLOW, perm).
// windows: CreateFile with FILE_FLAG_OPEN_REPARSE_POINT; access from O_RDONLY/O_WRONLY/O_RDWR (+O_APPEND
// as FILE_APPEND_DATA); disposition CREATE_NEW for O_CREATE|O_EXCL, OPEN_ALWAYS for O_CREATE,
// OPEN_EXISTING otherwise; share FILE_SHARE_READ|FILE_SHARE_WRITE; FILE_ATTRIBUTE_READONLY when
// O_CREATE and perm&0o200 == 0; with O_CREATE and perm&0o077 == 0 a new file gets the private
// descriptor "D:P(A;;FA;;;<user>)(A;;FA;;;SY)(A;;FA;;;BA)"; after open, FileAttributeTagInfo: a
// name-surrogate reparse point is closed and refused, a directory opened for writing is refused;
// O_TRUNC is applied after the check (File.Truncate(0)). Any other flag bit is an error.
func OpenNoFollow(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag|syscall.O_NOFOLLOW, perm)
}

// ReplaceFile renames oldpath over newpath. unix: os.Rename. windows: os.Rename, retried for at most
// 2 s (backoff 10 ms doubling, cap 200 ms) while it fails with ERROR_ACCESS_DENIED,
// ERROR_SHARING_VIOLATION or ERROR_LOCK_VIOLATION (a reader holds newpath; F10).
func ReplaceFile(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

// CurrentUser names this process's account for messages: unix "uid N"; windows the token user SID.
func CurrentUser() string {
	return fmt.Sprintf("uid %d", os.Getuid())
}

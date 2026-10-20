// Package fsperm decides whether a file or directory is private to the
// current user and makes it so, on unix and on Windows alike.
//
// On unix, private means what the data directory has always required: not a
// symbolic link, owned by this process's uid, and no permission bit for the
// group or for others (0700 for a directory, 0600 for a file).
//
// On Windows, private means: the path is not a name-surrogate reparse point
// (a symbolic link, a junction or a mount point); its owner is the user of
// this process's token, LocalSystem (S-1-5-18) or BUILTIN\Administrators
// (S-1-5-32-544); and every ACCESS_ALLOWED entry of its DACL, inherit-only
// entries included, names one of those three, OWNER RIGHTS (S-1-3-4) or
// CREATOR OWNER (S-1-3-0). A NULL DACL, a missing DACL or an entry type this
// package does not know makes the path not private (fail closed). Deny
// entries are ignored: they can only take access away. Protection writes a
// protected DACL granting full control to the user, LocalSystem and
// Administrators, inherited by the children of a directory.
//
// The package also opens files without following a final symbolic link or
// reparse point (OpenNoFollow, NoFollow), replaces files by rename in a way
// that tolerates a reader holding the target on Windows (ReplaceFile), and
// opens a file so that its path can still be replaced or removed while the
// handle is open (OpenReplaceable).
//
// It imports only the standard library and, in its Windows files,
// golang.org/x/sys/windows.
package fsperm

import (
	"errors"
	"fmt"
	"os"
)

// ErrNotPrivate is wrapped by CheckPrivate's error when the path is not private to this user.
var ErrNotPrivate = errors.New("not private to this user")

// whyLink is the reason CheckPrivate gives for a symbolic link or reparse point.
const whyLink = "symbolic link or reparse point"

// Info is the access to one path, read without following a final symbolic link or reparse point.
type Info struct {
	// Link: unix, the path is a symbolic link (Lstat ModeSymlink); windows, it is a name-surrogate
	// reparse point (symlink, junction, mount point; read from FileAttributeTagInfo on a handle opened
	// with FILE_FLAG_OPEN_REPARSE_POINT|FILE_FLAG_BACKUP_SEMANTICS).
	Link bool
	// OwnedBySelf: unix, Stat_t.Uid == os.Getuid(); windows, the owner SID is the process token user,
	// S-1-5-18 (SYSTEM) or S-1-5-32-544 (BUILTIN\Administrators).
	OwnedBySelf bool
	// Owner names the owner for messages: unix "uid N"; windows the SID string ("S-1-5-21-…").
	Owner string
	// OthersRead: unix, Mode().Perm()&0o077 != 0; windows, an ACCESS_ALLOWED ACE (any flags, inherit-only
	// included) grants any right to a SID outside {token user, S-1-5-18, S-1-5-32-544, S-1-3-4, S-1-3-0},
	// or the DACL is NULL, or an ACE type other than ACCESS_ALLOWED/ACCESS_DENIED (and their object/
	// callback forms; callback-allow counts as a grant) is present.
	OthersRead bool
	// OthersWrite: unix, Mode().Perm()&0o022 != 0; windows, such an ACE grants any of FILE_WRITE_DATA,
	// FILE_APPEND_DATA, FILE_WRITE_EA, FILE_DELETE_CHILD, FILE_WRITE_ATTRIBUTES, DELETE, WRITE_DAC,
	// WRITE_OWNER, GENERIC_WRITE, GENERIC_ALL, MAXIMUM_ALLOWED (or the DACL is NULL).
	OthersWrite bool
	// Mode for display: unix, Lstat Mode().Perm(); windows, synthetic: 0o700/0o600 (dir/file) when
	// !OthersRead, 0o755/0o644 when OthersRead && !OthersWrite, 0o777/0o666 when OthersWrite.
	Mode os.FileMode

	// dir records that the path is a directory (not following a final link).
	dir bool
	// readWhy explains OthersRead for CheckPrivate: "mode 0755" on unix, "access granted to <SID>"
	// (the first offending SID) on windows.
	readWhy string
}

// Private reports !i.Link && i.OwnedBySelf && !i.OthersRead.
func (i Info) Private() bool { return !i.Link && i.OwnedBySelf && !i.OthersRead }

// why is the reason i is not private, or "" when it is.
func (i Info) why() string {
	switch {
	case i.Link:
		return whyLink
	case !i.OwnedBySelf:
		return "owned by " + i.Owner
	case i.OthersRead:
		if i.readWhy != "" {
			return i.readWhy
		}
		return fmt.Sprintf("mode %04o", uint32(i.Mode.Perm()))
	}
	return ""
}

// CheckPrivate returns nil when Inspect(path).Private(), otherwise an error wrapping ErrNotPrivate whose
// text is "<path>: not private to this user (<why>)", why one of "symbolic link or reparse point",
// "owned by <Owner>", "mode %04o" (unix) or "access granted to <SID>" (windows, first offending SID).
func CheckPrivate(path string) error {
	i, err := Inspect(path)
	if err != nil {
		return err
	}
	if why := i.why(); why != "" {
		return notPrivate(path, why)
	}
	return nil
}

// notPrivate is the ErrNotPrivate error for path.
func notPrivate(path, why string) error {
	return fmt.Errorf("%s: %w (%s)", path, ErrNotPrivate, why)
}

// refuseUnowned is Protect's precondition: a link or a path this user does not own is not
// protected (and so not made to look private).
func refuseUnowned(path string, i Info) error {
	if i.Link {
		return notPrivate(path, whyLink)
	}
	if !i.OwnedBySelf {
		return notPrivate(path, "owned by "+i.Owner)
	}
	return nil
}

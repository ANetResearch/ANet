//go:build !windows

package daemon

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// pullSubdirTrouble says what disqualifies the opened pull subdirectory fi (at path) as a place to
// write peer files, or "" when it qualifies (pullDirTrouble, for this process's uid).
func pullSubdirTrouble(fi fs.FileInfo, _ string) string { return pullDirTrouble(fi, os.Getuid()) }

// createPullSubdir has nothing to do on unix: openDirIn creates the subdirectory with mode 0700, which
// makes it private whatever the directory around it allows.
func createPullSubdir(*os.Root, string, string) error { return nil }

// protectPulled has nothing to do on unix: a pulled file is created with mode 0600.
func protectPulled(string) error { return nil }

// pullDirTrouble says what disqualifies fi as the pull subdirectory, or "" when it qualifies: it must be
// a directory, not a symbolic link, owned by uid, and not writable by group or others. Anything else
// could be, or be changed by, another local user.
func pullDirTrouble(fi fs.FileInfo, uid int) string {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return "is a symbolic link"
	case !fi.IsDir():
		return "exists and is not a directory"
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Sprintf("is writable by others (mode %04o)", fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
		return fmt.Sprintf("belongs to uid %d, not to this user (uid %d)", st.Uid, uid)
	}
	return ""
}

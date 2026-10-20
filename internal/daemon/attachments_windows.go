//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

// Flags of GetFinalPathNameByHandleW (fileapi.h): the normalized name, on a drive letter.
const (
	fileNameNormalized = 0x0 // FILE_NAME_NORMALIZED
	volumeNameDOS      = 0x0 // VOLUME_NAME_DOS
)

// openExistingDir opens the existing directory p (absolute, clean, resolved through symbolic links when
// it was checked) and makes sure the directory opened is the one p names now, as the Linux version does
// with /proc/self/fd: the system's own name for the opened directory (GetFinalPathNameByHandle) must be
// p, compared without regard to case. A component swapped for a symbolic link or junction after the
// check leads the open elsewhere, and the name says so. The directory is then opened as an os.Root,
// which must be the same directory as the handle the name was read from.
//
// A directory reached through a mapped network drive or a substituted drive letter has a final name on
// another volume and is refused; pull into a directory on a local drive.
func openExistingDir(p string) (*os.Root, error) {
	if !filepath.IsAbs(p) {
		return nil, fmt.Errorf("%s is not an absolute path", p)
	}
	want := filepath.Clean(p)
	f, err := os.Open(want)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !held.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", want)
	}
	got, err := finalPathName(windows.Handle(f.Fd()))
	if err != nil {
		return nil, fmt.Errorf("%s: the system cannot name the opened directory: %w", want, err)
	}
	if !strings.EqualFold(got, want) {
		return nil, fmt.Errorf("%s changed while it was being opened (it led to %s)", want, got)
	}
	r, err := os.OpenRoot(want)
	if err != nil {
		return nil, err
	}
	opened, err := r.Stat(".")
	if err != nil {
		r.Close()
		return nil, err
	}
	if !os.SameFile(held, opened) {
		r.Close()
		return nil, fmt.Errorf("%s changed while it was being opened", want)
	}
	return r, nil
}

// finalPathName is the normalized name of the file h refers to, with the \\?\ prefix removed (and
// \\?\UNC\server\share turned into \\server\share).
func finalPathName(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalized|volumeNameDOS)
		if err != nil {
			return "", err
		}
		if n < uint32(len(buf)) {
			buf = buf[:n]
			break
		}
		// n is the size needed, including the terminating NUL.
		if n > 1<<16 {
			return "", errors.New("final path name too long")
		}
		buf = make([]uint16, n)
	}
	s := windows.UTF16ToString(buf)
	switch {
	case strings.HasPrefix(s, `\\?\UNC\`):
		s = `\\` + s[len(`\\?\UNC\`):]
	case strings.HasPrefix(s, `\\?\`):
		s = s[len(`\\?\`):]
	}
	return s, nil
}

// pullSubdirTrouble says what disqualifies the opened pull subdirectory fi (at path) as a place to write
// peer files, or "" when it qualifies. It must be a directory and not a symbolic link, junction or other
// name-surrogate reparse point; its owner must be this user (or SYSTEM, or the Administrators group);
// and its access control list must grant no other account a right that lets it change the directory
// (fsperm.Inspect). The access control list is read by path, so the path must still name the opened
// directory afterwards; anything else could be, or be changed by, another local user.
func pullSubdirTrouble(fi fs.FileInfo, path string) string {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return "is a symbolic link or junction"
	case !fi.IsDir():
		return "exists and is not a directory"
	}
	info, err := fsperm.Inspect(path)
	if err != nil {
		return fmt.Sprintf("cannot be inspected (%v)", err)
	}
	switch {
	case info.Link:
		return "is a symbolic link or junction"
	case !info.OwnedBySelf:
		return fmt.Sprintf("is owned by %s, not by this user (%s)", info.Owner, fsperm.CurrentUser())
	case info.OthersWrite:
		return "is writable by other users"
	}
	now, err := os.Lstat(path)
	if err != nil || !os.SameFile(now, fi) {
		return "changed while it was being opened"
	}
	return ""
}

// createPullSubdir creates the pull subdirectory name in parent (at path) and gives it the private
// access control list (this user, SYSTEM, Administrators), so that it is private whatever the
// directory around it would let it inherit, as mode 0700 makes it on unix. A subdirectory that already
// exists is left as it is, for pullSubdirTrouble to judge; one replaced between being created and being
// protected by a link or by another account's directory is refused by fsperm.Protect.
func createPullSubdir(parent *os.Root, path, name string) error {
	err := parent.Mkdir(name, 0o700)
	switch {
	case errors.Is(err, fs.ErrExist):
		return nil
	case err != nil:
		return err
	}
	return fsperm.Protect(path)
}

// protectPulled gives a pulled file the private access control list, as mode 0600 does on unix.
func protectPulled(path string) error { return fsperm.Protect(path) }

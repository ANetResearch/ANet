package release

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

// updateTempPattern names the file Apply writes beside the binary before it
// is checked and renamed into place. It ends in .exe so that the probe
// (`<file> version`) can start it.
const updateTempPattern = ".anet-update-*.exe"

// asideInfix separates the binary's name from the time it was moved aside:
// <exe>.old-<digits>.
const asideInfix = ".old-"

// renameFile is the rename of every step of replaceExe: fsperm.ReplaceFile,
// which retries for a bounded time while another process holds the file
// (a reader, or a scanner that opened it without FILE_SHARE_DELETE). A test
// replaces it to make one step fail.
var renameFile = fsperm.ReplaceFile

// chmodTemp gives the staged binary the mode of the one it replaces as far
// as Windows has one: Chmod there only sets or clears the read-only
// attribute, and whether the file runs does not depend on it, so a failure
// is not a reason to stop.
func chmodTemp(f *os.File, mode os.FileMode) error {
	_ = f.Chmod(mode)
	return nil
}

// replaceExe puts the checked binary tmp in place of exe.
//
// Windows does not let a running executable be replaced or deleted, but it
// lets it be renamed. So exe is first renamed aside, to
// <exe>.old-<unix nanoseconds> in the same directory, and tmp is then
// renamed to exe. If the second rename fails, the first is undone, and exe
// is the binary it was. The aside file stays until a later `anet update`
// (SweepAside): a daemon started from it may still be running.
func replaceExe(tmp, exe string) error {
	if _, err := os.Lstat(exe); errors.Is(err, fs.ErrNotExist) {
		return renameFile(tmp, exe)
	} else if err != nil {
		return err
	}
	aside := exe + asideInfix + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := os.Lstat(aside); err == nil {
		return fmt.Errorf("%s already exists", aside)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := renameFile(exe, aside); err != nil {
		return fmt.Errorf("cannot move the current binary aside: %w", err)
	}
	if err := renameFile(tmp, exe); err != nil {
		if rerr := renameFile(aside, exe); rerr != nil {
			return fmt.Errorf("%w: %v; the previous binary is now %s and could not be renamed back (%v): rename it to %s",
				ErrReplaceIncomplete, err, aside, rerr, exe)
		}
		return err
	}
	return nil
}

// SweepAside removes the binaries that earlier updates moved aside
// (<exe>.old-<digits>, regular files only). A file that is still in use,
// because a daemon started from it has not been restarted, cannot be
// removed and is left for the next run; every error is ignored.
//
// The directory is listed rather than globbed: the install directory may
// contain characters that a pattern would read as a character class.
func SweepAside(exe string) {
	dir, base := filepath.Dir(exe), filepath.Base(exe)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := base + asideInfix
	for _, e := range ents {
		name := e.Name()
		if len(name) <= len(prefix) || !strings.EqualFold(name[:len(prefix)], prefix) || !e.Type().IsRegular() {
			continue
		}
		if strings.Trim(name[len(prefix):], "0123456789") != "" {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

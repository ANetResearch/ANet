//go:build !no_mcp && windows

package agentwire

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

// sameBinaryName reports whether the base name of a command names the
// binary name. Windows file names are case-insensitive, and a command may
// be written with or without .exe in any case.
func sameBinaryName(base, name string) bool {
	if len(base) >= 4 && strings.EqualFold(base[len(base)-4:], ".exe") {
		base = base[:len(base)-4]
	}
	return strings.EqualFold(base, name)
}

// accessMode returns the access of path as permission bits, or 0 when it
// is absent. The bits come from the file's ACL (fsperm): 0600 when no
// other account can read it, 0644 when another can read it, 0666 when
// another can write it. A file whose ACL cannot be read counts as readable
// by others.
func accessMode(path string) fs.FileMode {
	if _, err := os.Stat(path); err != nil {
		return 0
	}
	info, err := fsperm.Inspect(path)
	if err != nil {
		return 0o644
	}
	return info.Mode
}

// chmodPrivate narrows the access of an existing file. A mode without
// group or other bits makes the ACL private to this user (fsperm.Protect);
// Chmod alone only sets the read-only attribute on Windows.
func chmodPrivate(path string, perm fs.FileMode) error {
	if err := os.Chmod(path, perm); err != nil {
		return err
	}
	if perm&0o077 != 0 {
		return nil
	}
	return fsperm.Protect(path)
}

// protectNew gives a file just created a private ACL, before anything is
// written to it, when its mode grants nothing to group or other.
func protectNew(path string, perm fs.FileMode) error {
	if perm&0o077 != 0 {
		return nil
	}
	return fsperm.Protect(path)
}

// replaceFile renames tmp over path, retrying while a reader (the tool
// itself) holds path open.
func replaceFile(tmp, path string) error { return fsperm.ReplaceFile(tmp, path) }

// errBatchShim is a tool CLI found as a .cmd or .bat file.
var errBatchShim = errors.New("the program is a batch file")

// toolCLI finds a tool's own command-line program, to run with arguments.
// A .cmd or .bat file (an npm shim) is not run: Windows starts it through
// cmd.exe, which reads characters such as & and % in the arguments (a data
// directory path) as its own syntax, and Go cannot quote an argument
// against that. The caller then edits the tool's file directly, as it does
// when the program is not installed.
func toolCLI(o *Options, name string) (string, error) {
	p, err := o.LookPath(name)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".cmd", ".bat":
		return "", errBatchShim
	}
	return p, nil
}

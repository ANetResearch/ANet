//go:build !no_mcp && !windows

package agentwire

import (
	"io/fs"
	"os"
	"strings"
)

// sameBinaryName reports whether the base name of a command names the
// binary name, with an optional .exe.
func sameBinaryName(base, name string) bool {
	base = strings.TrimSuffix(base, ".exe")
	return base == name
}

// accessMode returns the permission bits of path, or 0 when it is absent.
func accessMode(path string) fs.FileMode {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Mode().Perm()
}

// chmodPrivate narrows the mode of an existing file.
func chmodPrivate(path string, perm fs.FileMode) error { return os.Chmod(path, perm) }

// protectNew: a file is created with its mode on unix; nothing more to do.
func protectNew(string, fs.FileMode) error { return nil }

// replaceFile renames tmp over path.
func replaceFile(tmp, path string) error { return os.Rename(tmp, path) }

// toolCLI finds a tool's own command-line program, to run with arguments.
func toolCLI(o *Options, name string) (string, error) { return o.LookPath(name) }

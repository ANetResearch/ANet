//go:build !windows

package main

import (
	"fmt"
	"io"
	"path/filepath"
)

// resolveExe is the file `anet update` replaces: the binary this process
// runs from, with symbolic links resolved.
func resolveExe(exe string) (string, error) { return filepath.EvalSymlinks(exe) }

// printRestartHint says how to restart the daemons that still run the
// previous binary.
func printRestartHint(w io.Writer) {
	fmt.Fprintln(w, "    anet stop --all && anet up --all")
}

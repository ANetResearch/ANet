package main

import (
	"fmt"
	"io"
	"path/filepath"
)

// resolveExe is the file `anet update` replaces: the binary this process
// runs from, with symbolic links resolved. Resolving can fail on Windows
// where unix has no counterpart (a path under a mount point or a
// subst drive that the resolution cannot follow, or a directory this user
// may not traverse); the path the process was started from is then the
// file to replace, as it is when there is no link at all.
func resolveExe(exe string) (string, error) {
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		return p, nil
	}
	return exe, nil
}

// printRestartHint says how to restart the daemons that still run the
// previous binary: two commands, one per line, because Windows PowerShell
// 5.1 has no && operator.
func printRestartHint(w io.Writer) {
	fmt.Fprintln(w, "    anet stop --all")
	fmt.Fprintln(w, "    anet up --all")
}

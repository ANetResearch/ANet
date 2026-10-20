//go:build !windows

package main

import (
	"fmt"
	"os"
)

// filePrivacy is how doctor and identity status report a file's access: its mode, and whether no one
// but its owner can read it. fi is the os.Stat of path.
func filePrivacy(_ string, fi os.FileInfo) (mode string, private bool) {
	return fmt.Sprintf("%04o", fi.Mode().Perm()), fi.Mode().Perm()&0o077 == 0
}

// privateFileHint is the command that makes the file at path private to its owner.
func privateFileHint(path string) string { return "chmod 600 " + path }

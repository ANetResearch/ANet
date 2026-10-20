//go:build !windows

package main

import (
	"fmt"
	"os"
)

// checkTokenFileAccess refuses a token file that other users can access. fi is the Stat of the open
// file.
func checkTokenFileAccess(path string, fi os.FileInfo) error {
	if fi.Mode().Perm()&0o007 != 0 {
		return fmt.Errorf("token file %s is accessible to other users (mode %04o); chmod o-rwx", path, fi.Mode().Perm())
	}
	return nil
}

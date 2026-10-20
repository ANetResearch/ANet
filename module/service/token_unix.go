//go:build !no_service && !windows

package service

import (
	"fmt"
	"os"
)

// tokenFileAccess refuses a token file that other users can access, judged by the mode of the file that
// was opened (fi).
func tokenFileAccess(p string, fi os.FileInfo) error {
	if fi.Mode().Perm()&0o007 != 0 {
		return fmt.Errorf("token_file %s is accessible to other users (mode %04o); chmod o-rwx", p, fi.Mode().Perm())
	}
	return nil
}

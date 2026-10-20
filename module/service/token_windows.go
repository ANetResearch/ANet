//go:build !no_service && windows

package service

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

// tokenFileAccess refuses a token file whose access control list grants any other account access
// (fsperm.Inspect). Windows reports no access in the file mode, so the access control list is read by
// path: the path (resolved through symbolic links) must still name the file that was opened (fi), or
// the token is refused.
func tokenFileAccess(p string, fi os.FileInfo) error {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("token_file: %w", err)
	}
	info, err := fsperm.Inspect(real)
	if err != nil {
		return fmt.Errorf("token_file: %w", err)
	}
	now, err := os.Lstat(real)
	if err != nil || info.Link || !os.SameFile(now, fi) {
		return fmt.Errorf("token_file %s changed while it was being read; try again", p)
	}
	if info.OthersRead {
		return fmt.Errorf("token_file %s is readable by other users; restrict its ACL to this user", p)
	}
	return nil
}

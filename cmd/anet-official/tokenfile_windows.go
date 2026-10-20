//go:build windows

package main

import (
	"fmt"
	"os"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

// checkTokenFileAccess refuses a token file that other users can read, from its ACL (fsperm). fi is
// the Stat of the open file. The ACL is read from the path without following a link, so a path that
// is a symbolic link or other reparse point is refused (its own ACL says nothing about the file it
// names), and the path must still name the file that was opened.
func checkTokenFileAccess(path string, fi os.FileInfo) error {
	info, err := fsperm.Inspect(path)
	if err != nil {
		return fmt.Errorf("token file %s: its access cannot be read: %w", path, err)
	}
	if info.Link {
		return fmt.Errorf("token file %s is a symbolic link or reparse point; name the file itself", path)
	}
	if info.OthersRead {
		return fmt.Errorf("token file %s is readable by other users; restrict its ACL to this user", path)
	}
	if now, err := os.Lstat(path); err != nil || !os.SameFile(fi, now) {
		return fmt.Errorf("token file %s changed while it was being checked", path)
	}
	return nil
}

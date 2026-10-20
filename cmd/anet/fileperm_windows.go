//go:build windows

package main

import (
	"fmt"
	"os"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

// filePrivacy is how doctor and identity status report a file's access on Windows: the mode fsperm
// derives from the ACL, and whether the file is private to this user (not a link or reparse point,
// owned by this user, SYSTEM or Administrators, and no access for any other account). A file whose
// ACL cannot be read is not private.
func filePrivacy(path string, _ os.FileInfo) (mode string, private bool) {
	info, err := fsperm.Inspect(path)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%04o", info.Mode), info.Private()
}

// privateFileHint is the command that makes the file at path private to this user: inherited entries
// removed, full control for this user, SYSTEM and Administrators (by SID, so that it reads the same in
// PowerShell and cmd and in every display language). An explicit entry for another account that the
// file carries remains, and the next doctor run reports it.
func privateFileHint(path string) string {
	return `icacls "` + path + `" /inheritance:r /grant:r "*` + fsperm.CurrentUser() + `:F" "*S-1-5-18:F" "*S-1-5-32-544:F"`
}

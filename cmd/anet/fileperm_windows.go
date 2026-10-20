//go:build windows

package main

import (
	"fmt"
	"os"
	"strings"

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

// privateFileHint is the command that makes the file at path private to this user, valid on one line
// in PowerShell and in cmd: exactly three entries remain, full control for this user, SYSTEM and
// Administrators (named by SID, so that it reads the same in every display language).
//
// Why no other entry remains: /inheritance:r removes every inherited entry; /grant:r replaces every
// explicit entry of the three SIDs it names; and /remove names every other SID that an explicit entry
// of the file carries now (fsperm.ExplicitSIDs), which removes those entries, allow and deny alike.
// The three are never in /remove, so the order in which icacls applies the options cannot take their
// grants away. When the list cannot be read the command has no /remove, and an explicit entry for
// another account that the file carries remains; the next doctor run reports it.
func privateFileHint(path string) string {
	var b strings.Builder
	b.WriteString(`icacls "` + path + `" /inheritance:r`)
	user := fsperm.CurrentUser()
	if sids, err := fsperm.ExplicitSIDs(path); err == nil {
		first := true
		for _, sid := range sids {
			if sid == user || sid == sidLocalSystem || sid == sidAdministrators {
				continue
			}
			if first {
				b.WriteString(" /remove")
				first = false
			}
			b.WriteString(` "*` + sid + `"`)
		}
	}
	b.WriteString(` /grant:r "*` + user + `:F" "*` + sidLocalSystem + `:F" "*` + sidAdministrators + `:F"`)
	return b.String()
}

// The SIDs of LocalSystem and BUILTIN\Administrators.
const (
	sidLocalSystem    = "S-1-5-18"
	sidAdministrators = "S-1-5-32-544"
)

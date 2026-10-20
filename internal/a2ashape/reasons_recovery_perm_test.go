package a2ashape

import (
	"strings"
	"testing"
)

// identity.recovery_perm names the command that makes the recovery file
// private. The caller passes the platform's command in {command} (Windows: an
// icacls command); without one it is chmod 600 on the path, so the next step
// on Linux and macOS is what it was. doctor reports the file only when it is
// not private to its owner (a 0400 file is private), which is what the
// sentence says.
func TestRecoveryPermNamesThePlatformCommand(t *testing.T) {
	path := "/srv/u/.anet-recovery/x.anetrec"
	for _, lang := range []Language{English, Chinese} {
		if got := Next("identity.recovery_perm", lang, Args{"path": path}); got != "chmod 600 "+path {
			t.Errorf("%v: default next step %q", lang, got)
		}
		win := `icacls "C:\Users\a\.anet-recovery\x.anetrec" /inheritance:r /grant:r "a:F"`
		args := Args{"path": `C:\Users\a\.anet-recovery\x.anetrec`, "command": win}
		if got := Next("identity.recovery_perm", lang, args); got != win {
			t.Errorf("%v: next step %q, want %q", lang, got, win)
		}
		if s := Sentence("identity.recovery_perm", lang, args); !strings.Contains(s, win) || strings.Contains(s, "chmod") {
			t.Errorf("%v: sentence %q does not name only the given command", lang, s)
		}
	}
}

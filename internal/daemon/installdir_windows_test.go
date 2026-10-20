//go:build windows

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

// install.ps1 may install into <LocalAppData>\anet\bin. When <LocalAppData>\anet does not exist, the
// installer creates it with its own access control list: icacls /inheritance:r and full control for
// this user, SYSTEM and Administrators, inherited (Set-AnetPrivateAcl). The daemon's runtime
// directory <LocalAppData>\anet\run is then created below that existing directory: ensurePrivateDir
// accepts the parent as it is, creates run private, and leaves the installer's bin alone.
func TestWindowsRuntimeDirBesideTheInstallerBin(t *testing.T) {
	base := filepath.Join(t.TempDir(), "anet")
	bin := filepath.Join(base, "bin")
	icacls := filepath.Join(os.Getenv("SystemRoot"), "System32", "icacls.exe")
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	user := tu.User.Sid.String()
	for _, d := range []string{base, bin} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(icacls, d, "/inheritance:r", "/grant:r",
			"*"+user+":(OI)(CI)F", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F").CombinedOutput()
		if err != nil {
			t.Fatalf("icacls %s: %v\n%s", d, err, out)
		}
		if err := fsperm.CheckPrivate(d); err != nil {
			t.Fatalf("the installer's directory %s is not private for fsperm: %v", d, err)
		}
	}
	before, err := fsperm.ExplicitSIDs(bin)
	if err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(base, "run")
	if err := ensurePrivateDir(run); err != nil {
		t.Fatalf("ensurePrivateDir below the installer's directory: %v", err)
	}
	if err := checkPrivateDir(run); err != nil {
		t.Fatalf("the runtime directory: %v", err)
	}
	if err := checkPrivateDir(base); err != nil {
		t.Fatalf("the installer's parent after the runtime directory was made: %v", err)
	}
	after, err := fsperm.ExplicitSIDs(bin)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("the installer's bin changed: %v -> %v", before, after)
	}
}

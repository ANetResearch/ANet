//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// A token file private to this user is read; one whose access control list lets Everyone or the Users
// group read it is refused with the Windows sentence, as module/service refuses it.
func TestWindowsTokenFileACL(t *testing.T) {
	const tok = "0123456789abcdef0123456789abcdef"
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setFileDACL(t, p, "D:P(A;;FA;;;<user>)(A;;FA;;;SY)(A;;FA;;;BA)")
	if got, err := readToken(p); err != nil || got != tok {
		t.Fatalf("private token file: %q, %v", got, err)
	}
	for _, other := range []string{"(A;;FR;;;WD)", "(A;;FR;;;BU)", "(A;;FA;;;AU)"} {
		setFileDACL(t, p, "D:P(A;;FA;;;<user>)(A;;FA;;;SY)(A;;FA;;;BA)"+other)
		_, err := readToken(p)
		if err == nil || !strings.Contains(err.Error(), "is readable by other users; restrict its ACL to this user") {
			t.Fatalf("token file with %s: %v", other, err)
		}
	}
}

// A token file reached through a symbolic link is refused: the link's own access control list says
// nothing about the file it names.
func TestWindowsTokenFileLinkRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token")
	if err := os.WriteFile(target, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skip("creating a symbolic link needs SeCreateSymbolicLinkPrivilege (an elevated account or Developer Mode), which this account lacks")
		}
		t.Fatal(err)
	}
	if _, err := readToken(link); err == nil || !strings.Contains(err.Error(), "symbolic link or reparse point") {
		t.Fatalf("token file through a link: %v", err)
	}
}

// setFileDACL sets the access control list of path from sddl, "<user>" being this process's user SID.
func setFileDACL(t *testing.T, path, sddl string) {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString(strings.ReplaceAll(sddl, "<user>", u.User.Sid.String()))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

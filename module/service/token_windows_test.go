//go:build !no_service && windows

package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// A token file private to this user is read; one whose access control list lets Everyone read it is
// refused with the Windows sentence.
func TestWindowsTokenFileACL(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setFileDACL(t, p, "D:P(A;;FA;;;<user>)(A;;FA;;;SY)(A;;FA;;;BA)")
	if tok, err := readToken(p); err != nil || tok != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("private token file: %q, %v", tok, err)
	}
	setFileDACL(t, p, "D:P(A;;FA;;;<user>)(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;WD)")
	_, err := readToken(p)
	if err == nil || !strings.Contains(err.Error(), "is readable by other users; restrict its ACL to this user") {
		t.Fatalf("token file readable by Everyone: %v", err)
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

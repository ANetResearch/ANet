//go:build windows

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// setDACL replaces the access control list of path with the one in sddl ("D:..."), "<user>" replaced by
// this process's user SID. With protected false the entries the parent passes down are kept as well.
func setDACL(t *testing.T, path, sddl string, protected bool) {
	t.Helper()
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString(strings.ReplaceAll(sddl, "<user>", u.User.Sid.String()))
	if err != nil {
		t.Fatalf("SDDL %q: %v", sddl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if protected {
		info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		t.Fatalf("set the access control list of %s: %v", path, err)
	}
}

// privateSDDL names only this user, SYSTEM and the Administrators group (OI|CI).
const privateSDDL = "D:(A;OICI;FA;;;<user>)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// everyoneWriteSDDL adds write access for Everyone to privateSDDL.
const everyoneWriteSDDL = privateSDDL + "(A;OICI;FW;;;WD)"

// everyoneReadSDDL adds read access for Everyone to privateSDDL.
const everyoneReadSDDL = privateSDDL + "(A;OICI;FR;;;WD)"

// makeJunction makes link a junction (mount point reparse point) to the directory target. Junctions
// need no privilege, unlike symbolic links.
func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	target, err := filepath.Abs(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(link, 0o700); err != nil {
		t.Fatal(err)
	}
	subst, err := windows.UTF16FromString(`\??\` + target)
	if err != nil {
		t.Fatal(err)
	}
	printName, err := windows.UTF16FromString(target)
	if err != nil {
		t.Fatal(err)
	}
	// MountPointReparseBuffer: four USHORTs, then the substitute name and the print name, each with
	// its terminating NUL.
	pathBuf := append(append([]uint16{}, subst...), printName...)
	substLen := (len(subst) - 1) * 2
	printLen := (len(printName) - 1) * 2
	dataLen := 8 + len(pathBuf)*2
	buf := make([]byte, 8+dataLen)
	le := func(off int, v uint32, n int) {
		for i := 0; i < n; i++ {
			buf[off+i] = byte(v >> (8 * i))
		}
	}
	le(0, windows.IO_REPARSE_TAG_MOUNT_POINT, 4)
	le(4, uint32(dataLen), 2)
	le(6, 0, 2)
	le(8, 0, 2)                   // SubstituteNameOffset
	le(10, uint32(substLen), 2)   // SubstituteNameLength
	le(12, uint32(substLen+2), 2) // PrintNameOffset
	le(14, uint32(printLen), 2)   // PrintNameLength
	for i, c := range pathBuf {
		le(16+2*i, uint32(c), 2)
	}
	p, err := windows.UTF16PtrFromString(link)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatalf("open %s: %v", link, err)
	}
	defer windows.CloseHandle(h)
	var n uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &buf[0], uint32(len(buf)), nil, 0, &n, nil); err != nil {
		t.Fatalf("make %s a junction: %v", link, err)
	}
}

// longTempDir is t.TempDir() with its components in their long form (a CI runner's temporary
// directory can be spelled with 8.3 short names), as checkPullOutDir resolves an out_dir.
func longTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

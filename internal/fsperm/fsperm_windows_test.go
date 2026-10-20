//go:build windows

package fsperm

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const fileAllAccess = 0x1F01FF // FA

// winSetDACL replaces the DACL of path with the one in sddl, protected from inheritance.
func winSetDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("SDDL %q: %v", sddl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("SDDL %q: %v", sddl, err)
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	if err != nil {
		t.Fatalf("set the DACL of %s: %v", path, err)
	}
}

// winPrivateDACL is the SDDL DACL of a private directory plus extra entries.
func winPrivateDACL(extra string) string {
	u := CurrentUser()
	return "D:P(A;OICI;FA;;;" + u + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)" + extra
}

// winACEs reads the DACL entries of path and whether the DACL is protected.
func winACEs(t *testing.T, path string) ([]aclEntry, bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("DACL of %s: %v (nil: %v)", path, err, dacl == nil)
	}
	b, err := aclBytes(sd, dacl)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := parseACL(b)
	if err != nil {
		t.Fatal(err)
	}
	return ents, ctl&windows.SE_DACL_PROTECTED != 0
}

// winJunction creates a junction link pointing at the directory target with mklink.
func winJunction(t *testing.T, link, target string) {
	t.Helper()
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(filepath.Join(sys, "cmd.exe"), "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
}

// winSymlink creates a symbolic link or skips the test when this account may not.
func winSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("creating a symbolic link needs SeCreateSymbolicLinkPrivilege (an elevated window) or Developer Mode: %v", err)
	}
}

func winWrite(t *testing.T, p, data string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsCurrentUser(t *testing.T) {
	u := CurrentUser()
	if !strings.HasPrefix(u, "S-1-") {
		t.Fatalf("CurrentUser() = %q", u)
	}
	d := filepath.Join(t.TempDir(), "d")
	if err := MkdirAllPrivate(d); err != nil {
		t.Fatal(err)
	}
	if i, err := Inspect(d); err != nil || i.Owner != u {
		t.Errorf("owner of a new private directory: %+v, %v; want %s", i, err, u)
	}
}

func TestWindowsNoFollow(t *testing.T) {
	if NoFollow != 0 {
		t.Errorf("NoFollow = %#x, want 0", NoFollow)
	}
}

func TestWindowsInspectMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing", "x")
	if _, err := Inspect(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Inspect = %v, want fs.ErrNotExist", err)
	}
	if err := CheckPrivate(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("CheckPrivate = %v, want fs.ErrNotExist", err)
	}
}

func TestWindowsMkdirAllPrivate(t *testing.T) {
	d := filepath.Join(t.TempDir(), "a", "b")
	if err := MkdirAllPrivate(d); err != nil {
		t.Fatal(err)
	}
	i, err := Inspect(d)
	if err != nil {
		t.Fatal(err)
	}
	if !i.Private() || i.Link || !i.OwnedBySelf || i.OthersRead || i.OthersWrite || i.Mode != 0o700 {
		t.Errorf("Inspect = %+v", i)
	}
	ents, protected := winACEs(t, d)
	if !protected {
		t.Error("the DACL is not protected")
	}
	user, err := selfSID()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]byte{user, sidLocalSystem, sidAdmins}
	if len(ents) != len(want) {
		t.Fatalf("%d entries, want 3: %+v", len(ents), ents)
	}
	for k, e := range ents {
		if e.typ != aceAllowed || e.flags != 0x3 || e.mask != fileAllAccess || !sidIn(e.sid, want[k]) {
			t.Errorf("entry %d: type %d flags %#x mask %#x SID %s; want allow OI|CI FA %s",
				k, e.typ, e.flags, e.mask, sidString(e.sid), sidString(want[k]))
		}
	}
	// A file created inside inherits the private DACL.
	f := filepath.Join(d, "f")
	winWrite(t, f, "x")
	if fi, err := Inspect(f); err != nil || !fi.Private() || fi.Mode != 0o600 {
		t.Errorf("Inspect(child) = %+v, %v", fi, err)
	}
	// An existing private directory is accepted as it is.
	if err := MkdirAllPrivate(d); err != nil {
		t.Errorf("existing directory: %v", err)
	}
	// An existing file is not a directory.
	if err := MkdirAllPrivate(f); err == nil {
		t.Error("MkdirAllPrivate accepted a file")
	}
}

func TestWindowsEveryoneReadIsNotPrivate(t *testing.T) {
	d := filepath.Join(t.TempDir(), "d")
	if err := MkdirAllPrivate(d); err != nil {
		t.Fatal(err)
	}
	winSetDACL(t, d, winPrivateDACL("(A;;FR;;;WD)"))
	i, err := Inspect(d)
	if err != nil {
		t.Fatal(err)
	}
	if i.Private() || !i.OthersRead || i.OthersWrite || i.Mode != 0o755 {
		t.Errorf("Inspect = %+v", i)
	}
	err = CheckPrivate(d)
	if want := d + ": not private to this user (access granted to S-1-1-0)"; !errors.Is(err, ErrNotPrivate) || err.Error() != want {
		t.Errorf("CheckPrivate = %v, want %q", err, want)
	}
	if err := Protect(d); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(d); err != nil {
		t.Errorf("after Protect: %v", err)
	}
	if ents, protected := winACEs(t, d); len(ents) != 3 || !protected {
		t.Errorf("after Protect: %d entries, protected %v", len(ents), protected)
	}
	// MkdirAllPrivate repairs an existing directory of this user in the same way.
	winSetDACL(t, d, winPrivateDACL("(A;OICI;FR;;;BU)"))
	if err := MkdirAllPrivate(d); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(d); err != nil {
		t.Errorf("after MkdirAllPrivate: %v", err)
	}
}

func TestWindowsOthersWrite(t *testing.T) {
	root := t.TempDir()
	u := CurrentUser()
	for _, c := range []struct {
		ace         string
		read, write bool
		mode        os.FileMode
	}{
		{"(A;;FW;;;BU)", true, true, 0o666},
		{"(A;;FR;;;BU)", true, false, 0o644},
		{"(A;;0x10000;;;BU)", true, true, 0o666}, // DELETE
		{"(D;;FA;;;AN)", false, false, 0o600},    // a deny entry takes nothing away from privacy
		{"(A;;FA;;;OW)", false, false, 0o600},    // OWNER RIGHTS
	} {
		f := filepath.Join(root, "f")
		winWrite(t, f, "x")
		winSetDACL(t, f, "D:P(A;;FA;;;"+u+")(A;;FA;;;SY)(A;;FA;;;BA)"+c.ace)
		i, err := Inspect(f)
		if err != nil {
			t.Fatal(err)
		}
		if i.OthersRead != c.read || i.OthersWrite != c.write || i.Mode != c.mode || i.Private() == c.read {
			t.Errorf("%s: %+v", c.ace, i)
		}
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWindowsNullDACL(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	winWrite(t, f, "x")
	err := windows.SetNamedSecurityInfo(f, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("set a NULL DACL: %v", err)
	}
	i, err := Inspect(f)
	if err != nil {
		t.Fatal(err)
	}
	if !i.OthersRead || !i.OthersWrite || i.Private() {
		t.Errorf("NULL DACL: %+v", i)
	}
	if err := CheckPrivate(f); !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), "S-1-1-0") {
		t.Errorf("CheckPrivate = %v", err)
	}
	if err := Protect(f); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(f); err != nil {
		t.Errorf("after Protect: %v", err)
	}
}

func TestWindowsJunctionIsLink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	winSetDACL(t, target, winPrivateDACL("(A;OICI;FR;;;BU)"))
	link := filepath.Join(root, "junction")
	winJunction(t, link, target)

	i, err := Inspect(link)
	if err != nil {
		t.Fatal(err)
	}
	if !i.Link || i.Private() {
		t.Errorf("Inspect(junction) = %+v", i)
	}
	if err := CheckPrivate(link); !errors.Is(err, ErrNotPrivate) || err.Error() != link+": not private to this user (symbolic link or reparse point)" {
		t.Errorf("CheckPrivate(junction) = %v", err)
	}
	if err := Protect(link); !errors.Is(err, ErrNotPrivate) {
		t.Errorf("Protect(junction) = %v, want ErrNotPrivate", err)
	}
	if err := MkdirAllPrivate(link); !errors.Is(err, ErrNotPrivate) {
		t.Errorf("MkdirAllPrivate(junction) = %v, want ErrNotPrivate", err)
	}
	if ti, err := Inspect(target); err != nil || !ti.OthersRead {
		t.Errorf("the junction's target was changed: %+v, %v", ti, err)
	}
	for _, flag := range []int{os.O_RDONLY, os.O_WRONLY, os.O_RDWR | os.O_CREATE} {
		if f, err := OpenNoFollow(link, flag, 0o600); !errors.Is(err, syscall.ELOOP) {
			if err == nil {
				f.Close()
			}
			t.Errorf("OpenNoFollow(junction, %#x) = %v, want ELOOP", flag, err)
		}
	}
}

func TestWindowsOpenNoFollowFileSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	winWrite(t, target, "data")
	link := filepath.Join(root, "link")
	winSymlink(t, target, link)

	if i, err := Inspect(link); err != nil || !i.Link {
		t.Errorf("Inspect(symlink) = %+v, %v", i, err)
	}
	for _, flag := range []int{os.O_RDONLY, os.O_WRONLY | os.O_CREATE | os.O_TRUNC, os.O_RDWR | os.O_APPEND} {
		f, err := OpenNoFollow(link, flag, 0o600)
		var pe *os.PathError
		if err == nil {
			f.Close()
		}
		if !errors.Is(err, syscall.ELOOP) || !errors.As(err, &pe) {
			t.Errorf("flag %#x: %v, want a PathError wrapping ELOOP", flag, err)
		}
	}
	if b, _ := os.ReadFile(target); string(b) != "data" {
		t.Errorf("the target changed: %q", b)
	}
}

func TestWindowsOpenNoFollowThroughJunction(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	winWrite(t, filepath.Join(dir, "plain"), "plain")
	junction := filepath.Join(root, "junction")
	winJunction(t, junction, dir)
	// A junction in a parent component is followed, as a parent symbolic link is on unix.
	f, err := OpenNoFollow(filepath.Join(junction, "plain"), os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("a regular file behind a parent junction: %v", err)
	}
	f.Close()

	target := filepath.Join(root, "target")
	winWrite(t, target, "data")
	winSymlink(t, target, filepath.Join(dir, "link"))
	if f, err := OpenNoFollow(filepath.Join(junction, "link"), os.O_RDONLY, 0); !errors.Is(err, syscall.ELOOP) {
		if err == nil {
			f.Close()
		}
		t.Errorf("a final symbolic link behind a junction: %v, want ELOOP", err)
	}
}

func TestWindowsOpenNoFollowCreate(t *testing.T) {
	root := t.TempDir()
	// The directory grants Users read access to new children, so only an explicit descriptor makes
	// a new file private.
	winSetDACL(t, root, winPrivateDACL("(A;OICI;FR;;;BU)"))

	p := filepath.Join(root, "secret")
	f, err := OpenNoFollow(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("hello"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	i, err := Inspect(p)
	if err != nil {
		t.Fatal(err)
	}
	if !i.Private() || i.Mode != 0o600 {
		t.Errorf("a file created with 0o600: %+v", i)
	}
	if ents, protected := winACEs(t, p); len(ents) != 3 || !protected {
		t.Errorf("a file created with 0o600: %d entries, protected %v", len(ents), protected)
	}
	if _, err := OpenNoFollow(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); !errors.Is(err, fs.ErrExist) {
		t.Errorf("O_EXCL on an existing file: %v", err)
	}

	f, err = OpenNoFollow(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if fi, err := os.Stat(p); err != nil || fi.Size() != 0 {
		t.Errorf("O_TRUNC: %v, %v", fi, err)
	}

	f, err = OpenNoFollow(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("ab")
	f.Close()
	f, err = OpenNoFollow(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("cd")
	f.Close()
	if b, _ := os.ReadFile(p); string(b) != "abcd" {
		t.Errorf("O_APPEND: %q", b)
	}

	// Without the 0o077 bits clear the file inherits the directory's entries.
	open := filepath.Join(root, "shared")
	f, err = OpenNoFollow(open, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if i, err := Inspect(open); err != nil || !i.OthersRead {
		t.Errorf("a file created with 0o644: %+v, %v", i, err)
	}

	// A read-only file.
	ro := filepath.Join(root, "readonly")
	f, err = OpenNoFollow(ro, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if fi, err := os.Stat(ro); err != nil || fi.Mode().Perm()&0o200 != 0 {
		t.Errorf("a file created with 0o400 is writable: %v, %v", fi, err)
	}
	os.Chmod(ro, 0o600)

	// Directories: readable, never writable.
	if f, err := OpenNoFollow(root, os.O_RDONLY, 0); err != nil {
		t.Errorf("a directory for reading: %v", err)
	} else {
		f.Close()
	}
	for _, flag := range []int{os.O_WRONLY, os.O_RDWR, os.O_RDONLY | os.O_CREATE} {
		if f, err := OpenNoFollow(root, flag, 0o700); !errors.Is(err, syscall.EISDIR) {
			if err == nil {
				f.Close()
			}
			t.Errorf("a directory with flag %#x: %v, want EISDIR", flag, err)
		}
	}

	// Flags outside the supported set.
	for _, flag := range []int{os.O_RDONLY | os.O_SYNC, os.O_WRONLY | os.O_RDWR} {
		if f, err := OpenNoFollow(p, flag, 0); !errors.Is(err, syscall.EINVAL) {
			if err == nil {
				f.Close()
			}
			t.Errorf("flag %#x: %v, want EINVAL", flag, err)
		}
	}
	if _, err := OpenNoFollow(filepath.Join(root, "missing"), os.O_RDONLY, 0); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file: %v", err)
	}
}

func TestWindowsReplaceFileWhileReading(t *testing.T) {
	root := t.TempDir()
	tmp, dst := filepath.Join(root, "tmp"), filepath.Join(root, "dst")
	winWrite(t, dst, "old")
	winWrite(t, tmp, "new")
	p, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		t.Fatal(err)
	}
	// A reader as Go opens files: no FILE_SHARE_DELETE, so a rename over dst fails while it is open.
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		windows.CloseHandle(h)
		close(released)
	}()
	start := time.Now()
	err = ReplaceFile(tmp, dst)
	<-released
	if err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	t.Logf("replaced after %v", time.Since(start))
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Errorf("dst = %q", b)
	}
	if _, err := os.Lstat(tmp); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("tmp still exists: %v", err)
	}
	// A failure other than a held target is returned at once.
	start = time.Now()
	if err := ReplaceFile(filepath.Join(root, "missing"), dst); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing source: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("a missing source took %v", d)
	}
}

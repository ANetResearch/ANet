//go:build unix

package fsperm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func mkfile(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func TestInspectModes(t *testing.T) {
	root := t.TempDir()
	me := fmt.Sprintf("uid %d", os.Getuid())
	for _, c := range []struct {
		name        string
		dir         bool
		mode        os.FileMode
		read, write bool
	}{
		{"d0700", true, 0o700, false, false},
		{"d0755", true, 0o755, true, false},
		{"d0770", true, 0o770, true, true},
		{"f0600", false, 0o600, false, false},
		{"f0644", false, 0o644, true, false},
		{"f0666", false, 0o666, true, true},
		{"f0640", false, 0o640, true, false},
	} {
		p := filepath.Join(root, c.name)
		if c.dir {
			mkdir(t, p, c.mode)
		} else {
			mkfile(t, p, c.mode)
		}
		i, err := Inspect(p)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if i.Link || !i.OwnedBySelf || i.Owner != me || i.OthersRead != c.read || i.OthersWrite != c.write || i.Mode != c.mode {
			t.Errorf("%s: %+v", c.name, i)
		}
		if i.Private() == c.read {
			t.Errorf("%s: Private() = %v", c.name, i.Private())
		}
		err = CheckPrivate(p)
		if c.read {
			want := fmt.Sprintf("%s: not private to this user (mode %04o)", p, c.mode)
			if !errors.Is(err, ErrNotPrivate) || err.Error() != want {
				t.Errorf("%s: CheckPrivate = %v, want %q", c.name, err, want)
			}
		} else if err != nil {
			t.Errorf("%s: CheckPrivate = %v", c.name, err)
		}
	}
}

func TestInspectSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	mkfile(t, target, 0o600)
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	i, err := Inspect(link)
	if err != nil || !i.Link || i.Private() {
		t.Fatalf("Inspect(link) = %+v, %v", i, err)
	}
	if err := CheckPrivate(link); !errors.Is(err, ErrNotPrivate) || err.Error() != link+": not private to this user (symbolic link or reparse point)" {
		t.Errorf("CheckPrivate(link) = %v", err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Protect(link); !errors.Is(err, ErrNotPrivate) {
		t.Errorf("Protect(link) = %v, want ErrNotPrivate", err)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o644 {
		t.Errorf("Protect followed the link: target mode %04o", fi.Mode().Perm())
	}
}

func TestInspectOtherOwner(t *testing.T) {
	p := "/"
	if os.Getuid() == 0 {
		p = filepath.Join(t.TempDir(), "theirs")
		mkfile(t, p, 0o600)
		if err := os.Chown(p, 65534, 65534); err != nil {
			t.Skipf("chown to uid 65534: %v", err)
		}
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	uid := fi.Sys().(*syscall.Stat_t).Uid
	if int64(uid) == int64(os.Getuid()) {
		t.Skipf("%s is owned by this user", p)
	}
	i, err := Inspect(p)
	if err != nil || i.OwnedBySelf || i.Private() || i.Owner != fmt.Sprintf("uid %d", uid) {
		t.Fatalf("Inspect(%s) = %+v, %v", p, i, err)
	}
	want := fmt.Sprintf("%s: not private to this user (owned by uid %d)", p, uid)
	if err := CheckPrivate(p); !errors.Is(err, ErrNotPrivate) || err.Error() != want {
		t.Errorf("CheckPrivate = %v, want %q", err, want)
	}
	if err := Protect(p); !errors.Is(err, ErrNotPrivate) {
		t.Errorf("Protect = %v, want ErrNotPrivate", err)
	}
}

func TestInspectMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing")
	if _, err := Inspect(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Inspect = %v", err)
	}
	if err := CheckPrivate(p); !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrNotPrivate) {
		t.Errorf("CheckPrivate = %v", err)
	}
}

func TestProtect(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "d")
	mkdir(t, d, 0o755)
	f := filepath.Join(root, "f")
	mkfile(t, f, 0o666)
	for p, want := range map[string]os.FileMode{d: 0o700, f: 0o600} {
		if err := Protect(p); err != nil {
			t.Fatalf("Protect(%s): %v", p, err)
		}
		if fi, _ := os.Lstat(p); fi.Mode().Perm() != want {
			t.Errorf("%s: mode %04o, want %04o", p, fi.Mode().Perm(), want)
		}
		if err := CheckPrivate(p); err != nil {
			t.Errorf("CheckPrivate(%s) after Protect: %v", p, err)
		}
	}
}

func TestMkdirAllPrivate(t *testing.T) {
	d := filepath.Join(t.TempDir(), "a", "b", "c")
	if err := MkdirAllPrivate(d); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(d); err != nil {
		t.Errorf("new directory: %v", err)
	}
	if err := MkdirAllPrivate(d); err != nil {
		t.Errorf("existing directory: %v", err)
	}
}

func TestOpenNoFollow(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	mkfile(t, target, 0o600)
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []int{os.O_RDONLY, os.O_WRONLY | os.O_CREATE | os.O_TRUNC} {
		f, err := OpenNoFollow(link, flag, 0o600)
		var pe *os.PathError
		if err == nil {
			f.Close()
		}
		if !errors.Is(err, syscall.ELOOP) || !errors.As(err, &pe) {
			t.Errorf("flag %#x: err = %v, want a PathError wrapping ELOOP", flag, err)
		}
	}
	if b, _ := os.ReadFile(target); string(b) != "data" {
		t.Errorf("the target changed: %q", b)
	}

	p := filepath.Join(root, "new")
	f, err := OpenNoFollow(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("hello")
	f.Close()
	if err := CheckPrivate(p); err != nil {
		t.Errorf("created file: %v", err)
	}
	if _, err := OpenNoFollow(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); !errors.Is(err, fs.ErrExist) {
		t.Errorf("O_EXCL on an existing file: %v", err)
	}
	f, err = OpenNoFollow(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if fi, _ := os.Stat(p); fi.Size() != 0 {
		t.Errorf("O_TRUNC left %d bytes", fi.Size())
	}
}

func TestReplaceFile(t *testing.T) {
	root := t.TempDir()
	tmp, dst := filepath.Join(root, "tmp"), filepath.Join(root, "dst")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := ReplaceFile(tmp, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Errorf("dst = %q", b)
	}
	if _, err := os.Lstat(tmp); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("tmp still exists: %v", err)
	}
}

func TestCurrentUser(t *testing.T) {
	if got, want := CurrentUser(), fmt.Sprintf("uid %d", os.Getuid()); got != want {
		t.Errorf("CurrentUser() = %q, want %q", got, want)
	}
}

func TestNoFollow(t *testing.T) {
	if NoFollow != syscall.O_NOFOLLOW {
		t.Errorf("NoFollow = %#x", NoFollow)
	}
}

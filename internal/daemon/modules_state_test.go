package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// StateDir gives a module <data dir>/modules/<name>/, private to this user
// even when it already existed with a wider mode, and nothing for a name
// that is not a plain directory name.
func TestModuleStateDir(t *testing.T) {
	root := t.TempDir()
	h := moduleHost{&Daemon{layout: Layout{Root: root}}}
	want := filepath.Join(root, "modules", "a2a")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	got := h.StateDir("a2a")
	if got != want {
		t.Fatalf("StateDir = %q, want %q", got, want)
	}
	for _, d := range []string{got, filepath.Dir(got)} {
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Fatalf("state dir %s: %v, %v", d, fi, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "../x", "/abs"} {
		if d := h.StateDir(bad); d != "" {
			t.Errorf("StateDir(%q) = %q", bad, d)
		}
	}
	// The seam is offered, and it is the scoped one: without an agent
	// named it finds nothing.
	seam, ok := h.TaskSeam()
	if !ok || seam == nil {
		t.Fatal("TaskSeam not offered")
	}
	if _, err := seam.Get(context.Background(), "", "ix_any", nil); !errors.Is(err, a2ashape.ErrTaskNotFound) {
		t.Errorf("Get with no agent named: %v, want TaskNotFound", err)
	}
}

// A state directory that is a symbolic link — at either level — is not
// handed out: the module would write its token wherever the link points,
// and the target's mode would be changed through it.
func TestModuleStateDirRefusesSymlink(t *testing.T) {
	elsewhere := t.TempDir()
	if err := os.Chmod(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	h := moduleHost{&Daemon{layout: Layout{Root: root}}}
	if err := os.MkdirAll(filepath.Join(root, "modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, "modules", "a2a")); err != nil {
		t.Fatal(err)
	}
	if d := h.StateDir("a2a"); d != "" {
		t.Fatalf("StateDir through a symlinked module directory = %q", d)
	}

	root = t.TempDir()
	h = moduleHost{&Daemon{layout: Layout{Root: root}}}
	if err := os.Symlink(elsewhere, filepath.Join(root, "modules")); err != nil {
		t.Fatal(err)
	}
	if d := h.StateDir("a2a"); d != "" {
		t.Fatalf("StateDir through a symlinked modules directory = %q", d)
	}

	if fi, err := os.Stat(elsewhere); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("the link target was changed: %v, %v", fi, err)
	}
}

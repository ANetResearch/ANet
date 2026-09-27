package daemon

import (
	"os"
	"path/filepath"
	"testing"
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
	got := h.StateDir("a2a")
	if got != want {
		t.Fatalf("StateDir = %q, want %q", got, want)
	}
	fi, err := os.Stat(got)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir %v, %v", fi, err)
	}
	for _, bad := range []string{"", ".", "..", "a/b", "../x", "/abs"} {
		if d := h.StateDir(bad); d != "" {
			t.Errorf("StateDir(%q) = %q", bad, d)
		}
	}
	if _, ok := h.TaskSeam(); ok {
		t.Error("TaskSeam offered before it is implemented")
	}
}

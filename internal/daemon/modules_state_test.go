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

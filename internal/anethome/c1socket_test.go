package anethome

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultC1Socket(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/4242")
	if got := DefaultC1Socket(); got != "/run/user/4242/anetlink/c1.sock" {
		t.Fatalf("DefaultC1Socket = %q", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := DefaultC1Socket(); got != "" {
		t.Fatalf("without XDG_RUNTIME_DIR: %q", got)
	}
}

// TestOneDefaultC1Socket: the default C1 socket is defined here and nowhere
// else. The daemon's configuration check, the sandbox's never-bind list,
// doctor and provider/anetlink ask this package; a second place that joins
// the same path is a second definition that drifts.
func TestOneDefaultC1Socket(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") ||
			filepath.Base(p) == "anethome.go" && filepath.Base(filepath.Dir(p)) == "anethome" {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), `"anetlink", "c1.sock"`) {
			t.Errorf("%s joins the default C1 socket path itself", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

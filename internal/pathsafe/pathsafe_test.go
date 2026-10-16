package pathsafe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathWithin(t *testing.T) {
	for _, c := range []struct {
		p, root string
		want    bool
	}{
		{"/a/b", "/a", true}, {"/a", "/a", true}, {"/ab", "/a", false}, {"/a/../b", "/a", false}, {"/", "/a", false},
	} {
		if got := PathWithin(filepath.Clean(c.p), c.root); got != c.want {
			t.Errorf("PathWithin(%q, %q) = %v", c.p, c.root, got)
		}
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd":     "passwd",
		`C:\dir\file.txt`:      "file.txt",
		".envrc":               "_envrc",
		"invoice\u202Efdp.exe": "invoicefdp.exe",
		"CON.txt":              "_CON.txt",
		"":                     "attachment",
		"name. ":               "name",
		"a\x00b":               "ab",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SafeName(strings.Repeat("x", 500) + ".tar.gz"); len(got) > MaxSafeNameBytes || !strings.HasSuffix(got, ".gz") {
		t.Errorf("a long name: %d bytes, %q", len(got), got)
	}
}

func TestResolveThroughExistingFollowsLinksInThePrefixOnly(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(real)
	got, err := ResolveThroughExisting(filepath.Join(link, "not", "yet"))
	if err != nil || got != filepath.Join(resolved, "not", "yet") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestDetectMime(t *testing.T) {
	if got := DetectMime("a.png", nil); got != "image/png" {
		t.Errorf("by extension: %q", got)
	}
	if got := DetectMime("noext", []byte("<html><body>x</body></html>")); !strings.HasPrefix(got, "text/html") {
		t.Errorf("by content: %q", got)
	}
	if got := DetectMime("noext", nil); got != "application/octet-stream" {
		t.Errorf("empty: %q", got)
	}
}

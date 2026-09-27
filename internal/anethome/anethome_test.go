package anethome

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHome(t *testing.T) {
	t.Setenv("ANET_HOME", "/srv/anet-home")
	if h := Home(); h != "/srv/anet-home" {
		t.Fatalf("Home with ANET_HOME = %q", h)
	}
	t.Setenv("ANET_HOME", "")
	u := t.TempDir()
	t.Setenv("HOME", u)
	if h := Home(); h != filepath.Join(u, ".anet") {
		t.Fatalf("Home without ANET_HOME = %q, want %s/.anet", h, u)
	}
}

func TestNamesAndDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANET_HOME", home)
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"default", true}, {"coder", true}, {"a.b-c_d", true}, {"A1", true},
		{"", false}, {"ids", false}, {".hidden", false}, {"-x", false}, {"a/b", false}, {"..", false},
		{string(make([]byte, 65)), false},
	} {
		if got := ValidName(c.name); got != c.ok {
			t.Errorf("ValidName(%q) = %v", c.name, got)
		}
	}
	if Dir("") != home || Dir("default") != home {
		t.Fatalf("the default identity is the home itself: %q %q", Dir(""), Dir("default"))
	}
	coder := filepath.Join(home, "ids", "coder")
	if Dir("coder") != coder {
		t.Fatalf("Dir(coder) = %q", Dir("coder"))
	}
	for dir, want := range map[string]string{
		home:                        "default",
		home + "/":                  "default",
		coder:                       "coder",
		filepath.Join(coder, "x"):   "",
		filepath.Join(home, "ids"):  "",
		t.TempDir():                 "",
		filepath.Join(home, "ids/"): "",
	} {
		if got := NameForDir(dir); got != want {
			t.Errorf("NameForDir(%q) = %q, want %q", dir, got, want)
		}
	}
}

// The walk sees the home and every valid-named directory under ids — the
// set both port allocators skip — and nothing else.
func TestIdentities(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANET_HOME", home)
	if got := Identities(); !reflect.DeepEqual(got, []Identity{{Name: "default", Dir: home}}) {
		t.Fatalf("without ids/: %+v", got)
	}
	ids := filepath.Join(home, "ids")
	for _, d := range []string{"b", "a", ".tmp", "ids"} {
		if err := os.MkdirAll(filepath.Join(ids, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ids, "c"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	want := []Identity{
		{Name: "default", Dir: home},
		{Name: "a", Dir: filepath.Join(ids, "a")},
		{Name: "b", Dir: filepath.Join(ids, "b")},
	}
	if got := Identities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Identities = %+v, want %+v", got, want)
	}
}

// The local A2A interface's files are in the module's state directory, the
// path the daemon's Host.StateDir("a2a") makes.
func TestModuleDirs(t *testing.T) {
	if got := ModuleDir("/d", "x402"); got != "/d/modules/x402" {
		t.Fatalf("ModuleDir = %q", got)
	}
	if got := A2ADir("/d"); got != "/d/modules/a2a" {
		t.Fatalf("A2ADir = %q", got)
	}
}

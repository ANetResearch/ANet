//go:build windows

package anethome

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWindowsValidName(t *testing.T) {
	for _, name := range []string{
		"con", "CON", "Con.txt", "prn", "aux.x", "nul", "NUL.a.b", "com0", "com1", "COM9.log",
		"lpt0", "LPT1", "lpt9.txt", "a.", "coder.",
	} {
		if ValidName(name) {
			t.Errorf("ValidName(%q) = true", name)
		}
	}
	for _, name := range []string{"COM¹", "com²", "LPT³.txt", "con ", "con .txt", "x "} {
		if osValidName(name) {
			t.Errorf("osValidName(%q) = true", name)
		}
	}
	for _, name := range []string{"default", "coder", "console", "con-x", "com10", "lpt", "a.b", "A1", "nulx.txt"} {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) = false", name)
		}
	}
}

func TestWindowsNameTaken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANET_HOME", home)
	if NameTaken("coder") {
		t.Error("taken without an ids directory")
	}
	ids := filepath.Join(home, IDsDir)
	if err := os.MkdirAll(filepath.Join(ids, "Coder"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ids, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"Coder": true, "coder": true, "CODER": true, "FILE": true, "other": false} {
		if got := NameTaken(name); got != want {
			t.Errorf("NameTaken(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestWindowsIdentitiesCaseFold(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANET_HOME", home)
	ids := filepath.Join(home, IDsDir)
	for _, d := range []string{"Bob", "carol", ".tmp"} {
		if err := os.MkdirAll(filepath.Join(ids, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	want := []Identity{
		{Name: "default", Dir: home},
		{Name: "Bob", Dir: filepath.Join(ids, "Bob")},
		{Name: "carol", Dir: filepath.Join(ids, "carol")},
	}
	if got := Identities(); !reflect.DeepEqual(got, want) {
		t.Errorf("Identities = %+v, want %+v", got, want)
	}
	// A case-sensitive directory could hold both "Bob" and "bob"; only the first is listed.
	if !listedAlready(want[1:], "BOB") || listedAlready(want[1:], "dave") {
		t.Error("listedAlready does not fold case")
	}
}

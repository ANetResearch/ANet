//go:build !no_mcp && windows

package agentwire

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

// Windows names are case-insensitive and .exe is optional, in any case.
func TestWindowsIsAnetCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		`C:\x\ANET.EXE`: true,
		`C:\Users\A B\AppData\Local\Programs\anet\anet.exe`: true,
		`C:\x\Anet`:      true,
		`anet`:           true,
		`C:/x/anet.Exe`:  true,
		`C:\x\anet.exe2`: false,
		`C:\x\anetd.exe`: false,
		`C:\x\anet.cmd`:  false,
		`C:\x\exe`:       false,
	} {
		if got := isAnetCommand(cmd); got != want {
			t.Errorf("isAnetCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// A tool CLI that is a batch file is not run (cmd.exe would parse the arguments); an .exe is.
func TestWindowsToolCLIRefusesBatchShim(t *testing.T) {
	for p, ok := range map[string]bool{
		`C:\npm\claude.cmd`:           false,
		`C:\npm\claude.BAT`:           false,
		`C:\Program Files\claude.exe`: true,
	} {
		o := &Options{LookPath: func(string) (string, error) { return p, nil }}
		got, err := toolCLI(o, "claude")
		if ok != (err == nil) || (ok && got != p) {
			t.Errorf("%s: %q %v", p, got, err)
		}
		if !ok && !errors.Is(err, errBatchShim) {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// A file written with a private mode has a private ACL; its mode reads as 0600, and an absent file
// as 0. Narrowing an existing file's mode to 0600 makes its ACL private.
func TestWindowsPrivateWrites(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := writeAtomic(p, []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckPrivate(p); err != nil {
		t.Fatal(err)
	}
	if m := accessMode(p); m != 0o600 {
		t.Fatalf("mode %o", m)
	}
	if m := accessMode(filepath.Join(dir, "absent")); m != 0 {
		t.Fatalf("absent: mode %o", m)
	}
	q := filepath.Join(dir, "other.yaml")
	if err := os.WriteFile(q, []byte("b: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := chmodPrivate(q, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckPrivate(q); err != nil {
		t.Fatal(err)
	}
	o := &Options{Now: func() time.Time { return time.Unix(0, 0) }}
	bak, err := backup(o, q, []byte("secret"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckPrivate(bak); err != nil {
		t.Fatalf("backup: %v", err)
	}
}

// winSetDACL replaces the DACL of path with sddl ("<user>" is this process's user SID), protected from
// inheritance.
func winSetDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(strings.ReplaceAll(sddl, "<user>", fsperm.CurrentUser()))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// A tool configuration that is private stays private when wire rewrites it, in a directory whose
// inheritable access control list lets Everyone read: its mode is read from its ACL (0600), not from the
// Windows mode bits (0666), and the temporary file and the backup are private from the moment they exist.
// A configuration others can read is rewritten as it was, not narrowed.
func TestWindowsRewriteKeepsAPrivateACL(t *testing.T) {
	dir := t.TempDir()
	winSetDACL(t, dir, "D:P(A;OICI;FA;;;<user>)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FR;;;WD)")
	o := &Options{Now: func() time.Time { return time.Unix(0, 0) }}

	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.Protect(p); err != nil {
		t.Fatal(err)
	}
	c := change{path: p, before: []byte("{}\n"), after: []byte("{\"a\": 1}\n"), mode: 0o644}
	bak, err := c.apply(o)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "{\"a\": 1}\n" {
		t.Fatalf("content %q", b)
	}
	if err := fsperm.CheckPrivate(p); err != nil {
		t.Errorf("the rewritten private configuration: %v", err)
	}
	if bak == "" {
		t.Fatal("no backup")
	}
	if err := fsperm.CheckPrivate(bak); err != nil {
		t.Errorf("the backup: %v", err)
	}

	q := filepath.Join(dir, "shared.json")
	if err := os.WriteFile(q, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c = change{path: q, before: []byte("{}\n"), after: []byte("{\"b\": 2}\n"), mode: 0o600}
	if _, err := c.apply(o); err != nil {
		t.Fatal(err)
	}
	if i, err := fsperm.Inspect(q); err != nil || !i.OthersRead {
		t.Errorf("a configuration others could read was narrowed: %+v, %v", i, err)
	}
}

// Trae on Windows: the entry goes into %APPDATA%\Trae\User\mcp.json with the
// drive path of anet, the rule into %USERPROFILE%\.trae\user_rules, and
// unwire takes both out again.
func TestWindowsTraeWireAndUnwire(t *testing.T) {
	setTraeGOOS(t, "windows")
	h := newHost(t)
	appData := filepath.Join(h.home, "AppData", "Roaming")
	h.env["APPDATA"] = appData
	o := h.opts()
	o.Bin = filepath.Join(h.home, "AppData", "Local", "Programs", "anet", "anet.exe")
	r := one(t, h.wireWith(o, ToolTrae), Written)
	if len(r.Changes) != 2 || !h.exists(`AppData/Roaming/Trae/User/mcp.json`) || !h.exists(".trae/user_rules/anet.md") {
		t.Fatalf("changes %v", r.Changes)
	}
	one(t, h.wireWith(o, ToolTrae), UpToDate)
	one(t, h.unwireWith(o, ToolTrae), Removed)
	if got := h.snapshot(); len(got) != 0 {
		t.Fatalf("unwire left %v", keys(got))
	}
}

// ZCode's program is run with fixed arguments only, so a zcode.cmd found on
// PATH is run as it is (unlike the batch shims toolCLI refuses).
func TestWindowsZCodeBatchFileIsUsed(t *testing.T) {
	o := &Options{LookPath: func(string) (string, error) { return `C:\Users\A B\AppData\Roaming\npm\zcode.cmd`, nil }}
	if got := zcodeCLI(o); got != `C:\Users\A B\AppData\Roaming\npm\zcode.cmd` {
		t.Fatalf("zcodeCLI = %q", got)
	}
	for _, argv := range [][]string{{"plugins", "marketplace", "add", zcodeMarketplaceURL}, {"plugins", "install", zcodePluginID}} {
		for _, a := range argv {
			if strings.ContainsAny(a, "&|<>^%!\"() ") {
				t.Fatalf("argument %q holds a character cmd.exe reads", a)
			}
		}
	}
}

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/module"
)

// TestEveryKnownModuleIsWiredConsistently (MOD-1, §7.1): the name table, the
// switch files, the declarations, the route list and the doctor register
// say the same thing about every module.
func TestEveryKnownModuleIsWiredConsistently(t *testing.T) {
	compiled := module.Compiled()
	for _, k := range module.Known() {
		switch {
		case k.Tag == "":
			// No tag yet: compiled in by a declaration that carries none,
			// whatever the tags are.
			if !slices.Contains(compiled, k.Name) {
				t.Errorf("%s has no tag and is not declared compiled", k.Name)
			}
		case k.Additive:
			b, err := os.ReadFile("module_" + k.Name + ".go")
			if err != nil || !strings.Contains(string(b), "//go:build "+k.Tag+"\n") {
				t.Errorf("%s: module_%s.go missing or not tagged %q: %v", k.Name, k.Name, k.Tag, err)
			}
		default:
			b, err := os.ReadFile("module_" + k.Name + ".go")
			if err != nil || !strings.Contains(string(b), "//go:build !"+k.Tag+"\n") {
				t.Errorf("%s: module_%s.go missing or not tagged !%s: %v", k.Name, k.Name, k.Tag, err)
			}
		}
		for _, id := range k.DoctorIDs {
			if !doctorIDKnown(id) {
				t.Errorf("%s: doctor id %q is not in the register", k.Name, id)
			}
		}
	}
	// The additive names are shell and nothing else.
	for _, k := range module.Known() {
		if k.Additive != (k.Name == "shell") {
			t.Errorf("%s: additive = %v", k.Name, k.Additive)
		}
	}
	// The route list: every owner is a module of the table, and every route
	// of the table is in the list (R-13).
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "daemon", "testdata", "control_routes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	owned := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		pat, owner, ok := strings.Cut(l, "\t")
		if !ok {
			t.Fatalf("control_routes.txt line %q", l)
		}
		owned[pat] = owner
		if _, known := module.Lookup(owner); owner != "kernel" && !known {
			t.Errorf("route %s is owned by %q, which is not a module", pat, owner)
		}
		if m, ok := module.RouteOwner(pat); (owner != "kernel") != ok || (ok && m != owner) {
			t.Errorf("route %s: the file says %q, the table %q", pat, owner, m)
		}
	}
	for _, k := range module.Known() {
		for _, r := range k.Routes {
			if _, ok := owned[r]; !ok {
				t.Errorf("%s: route %s is not in control_routes.txt", k.Name, r)
			}
		}
	}
}

// MOD-F3: the command that belongs to a module is refused before anything
// is contacted when config.json turns the module off, with the one sentence
// and exit status 2.
func TestRequireModuleReadsWithout(t *testing.T) {
	layout := daemonLayout(t)
	if err := requireModule(layout, "mcp"); err != nil && (module.Availability{}).Available("mcp") {
		t.Fatalf("nothing turned off: %v", err)
	}
	if err := os.WriteFile(layout.ConfigPath(), []byte(`{"without":["mcp"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := requireModule(layout, "mcp")
	if err == nil {
		t.Skip("this binary does not contain mcp")
	}
	var sb strings.Builder
	if code := exitStatus(err, &sb); code != 2 {
		t.Errorf("exit status %d, want 2", code)
	}
	wantFix := "anet init --with mcp"
	if !slices.Contains(module.Compiled(), "mcp") {
		wantFix = "install.sh" // not in this binary at all: another build is the fix
	}
	for _, want := range []string{"MCP", wantFix} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("sentence %q lacks %q", sb.String(), want)
		}
	}
	// A configuration that does not parse turns nothing off (the daemon
	// refuses to start on it and says why).
	if err := os.WriteFile(layout.ConfigPath(), []byte(`{nope`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireModule(layout, "mcp"); err != nil && (module.Availability{}).Available("mcp") {
		t.Errorf("an unreadable config.json turned a module off: %v", err)
	}
	// Without a config.json it does not create one.
	empty := daemonLayout(t)
	_ = requireModule(empty, "mcp")
	if _, err := os.Stat(empty.ConfigPath()); err == nil {
		t.Error("the check created config.json")
	}
}

func daemonLayout(t *testing.T) daemon.Layout { return daemon.NewLayout(t.TempDir()) }

// MOD-F2 (5): the English sentences the modules' absence used to have are
// gone from the sources; module_absent is the one sentence. (The payment
// module's two, errNoPayments and errNoPaymentModule, went with MOD-2.)
func TestOldAbsenceSentencesAreGone(t *testing.T) {
	old := []string{"was compiled with -tags no_mcp", "is not compiled into this build", "is configured but not compiled",
		"is part of this build but is not configured", "no MCP server to register", "built without the anetlink module",
		"this build has no payment support", "this build has no payment module"}
	err := filepath.WalkDir(filepath.Join("..", ".."), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, s := range old {
			if strings.Contains(string(b), s) {
				t.Errorf("%s still says %q", p, s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// needModule skips a test of what doctor says about a module's block when
// that module is not in this build: doctor leaves the blocks of modules that
// are not available to the modules row (R-5: a skip names the tag).
func needModule(t *testing.T, name string) {
	t.Helper()
	if !(module.Availability{}).Available(name) {
		t.Skipf("needs the %s module: build without -tags no_%s", name, name)
	}
}

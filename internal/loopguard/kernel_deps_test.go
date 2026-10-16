package loopguard_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

const root = "github.com/ANetResearch/ANet"

// depsOf is the non-test dependency closure of a package of this module, as
// `go list -deps` prints it. A package that is not in this tree yet (the
// kernel packages MOD-3 and MOD-5 add) is skipped by the caller.
func depsOf(t *testing.T, pkg, tags string) ([]string, bool) {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	out, err := exec.Command(gobin, "list", "-tags", tags, "-deps", root+"/"+pkg).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "no required module provides package") ||
			strings.Contains(string(out), "is not in std") || strings.Contains(string(out), "cannot find package") ||
			strings.Contains(string(out), "directory not found") || strings.Contains(string(out), "no Go files") {
			return nil, false
		}
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}
	return strings.Fields(string(out)), true
}

// forbidden are the packages of the optional modules, from the name table
// (the list scripts/tagcheck.sh counts symbols of).
func forbidden() map[string]string {
	f := map[string]string{}
	for _, k := range module.Known() {
		for _, p := range k.Packages {
			f[root+"/"+p] = k.Name
		}
	}
	return f
}

// TestKernelImportsNoModule (MOD-P2): the kernel does not import a package of
// an optional module. The kernel is the daemon and the packages it shares with
// modules; a module reaches it only through module.Host. Hand check, recorded
// with the package: adding `import _ ".../module/service"` to a file of
// internal/daemon turns this red.
func TestKernelImportsNoModule(t *testing.T) {
	kernel := []string{"internal/daemon", "internal/a2ashape", "internal/netcard", "internal/localday", "internal/execwork",
		"internal/ixstate", "internal/pathsafe", "provider", "module"}
	bad := forbidden()
	checked := 0
	for _, pkg := range kernel {
		deps, ok := depsOf(t, pkg, "")
		if !ok {
			continue // not in this tree yet
		}
		checked++
		for _, d := range deps {
			if m, hit := bad[d]; hit {
				t.Errorf("%s imports %s, a package of the %s module", pkg, d, m)
			}
		}
	}
	if checked < 4 {
		t.Fatalf("only %d kernel packages were found", checked)
	}
}

// TestModulesDoNotImportTheKernel: no package of an optional module reaches
// internal/daemon, so a module is built and tested without a daemon.
func TestModulesDoNotImportTheKernel(t *testing.T) {
	for _, k := range module.Known() {
		for _, p := range k.Packages {
			tags := ""
			if k.Additive {
				tags = k.Tag // an additive module's packages are empty without its tag
			}
			deps, ok := depsOf(t, p, tags)
			if !ok {
				continue
			}
			for _, d := range deps {
				if d == root+"/internal/daemon" {
					t.Errorf("%s (module %s) imports the daemon", p, k.Name)
				}
			}
		}
	}
}

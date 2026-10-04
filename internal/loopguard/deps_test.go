package loopguard

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const (
	modPath = "github.com/ANetResearch/ANet"
	sdkPath = "github.com/a2aproject/"
)

// goListDeps is the dependency closure of pkg as `go list -deps` prints it
// (non-test imports, the closure SI-8 is stated over).
func goListDeps(t *testing.T, pkg string) []string {
	t.Helper()
	return goListDepsFor(t, "", pkg)
}

// goListDepsFor is goListDeps for the platform goos ("" for this one).
func goListDepsFor(t *testing.T, goos, pkg string) []string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	cmd := exec.Command(gobin, "list", "-deps", pkg)
	if goos != "" {
		cmd.Env = append(os.Environ(), "GOOS="+goos)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}
	return strings.Fields(string(out))
}

// The shared guard packages are imported by both the daemon and module/a2a
// (backendconn: by module/service, module/a2a and cmd/anet's doctor),
// so they may import neither: not the daemon (module/a2a would pull the
// kernel in, and the two would be one package again) and not the A2A SDK
// (the daemon would carry it, SI-8). The daemon, which now imports both,
// and the MCP server stay free of the SDK too (A2A-DESIGN SI-8 names both).
func TestSharedPackagesStayFreeOfTheDaemonAndTheSDK(t *testing.T) {
	for _, pkg := range []string{modPath + "/internal/loopguard", modPath + "/internal/anethome", modPath + "/internal/localpeer",
		modPath + "/internal/backendconn", modPath + "/internal/peercred"} {
		for _, d := range goListDeps(t, pkg) {
			if d == modPath+"/internal/daemon" || strings.HasPrefix(d, sdkPath) || strings.HasPrefix(d, modPath+"/module") {
				t.Errorf("%s depends on %s", pkg, d)
			}
		}
	}
	for _, pkg := range []string{modPath + "/internal/daemon", modPath + "/internal/mcpserv"} {
		for _, d := range goListDeps(t, pkg) {
			if strings.HasPrefix(d, sdkPath) || d == modPath+"/module/a2a" {
				t.Errorf("SI-8: %s depends on %s", pkg, d)
			}
		}
	}
}

// LEC PC-1: internal/peercred reads the kernel's word on a Unix socket peer
// with the standard library and golang.org/x/sys/unix alone, on every
// platform it builds for; backendconn, which the modules and doctor share,
// adds only localpeer and peercred to that. And the test helper that puts
// another account on the far end of a socket (peercredtest, which runs
// sudo) is in no binary.
func TestPeercredDependsOnTheStandardLibraryAndXSysOnly(t *testing.T) {
	std := func(d string) bool {
		first, _, _ := strings.Cut(d, "/")
		return !strings.Contains(first, ".") || strings.HasPrefix(d, "vendor/") || strings.HasPrefix(d, "golang.org/x/sys/")
	}
	allowed := map[string][]string{
		modPath + "/internal/peercred":    {modPath + "/internal/peercred"},
		modPath + "/internal/backendconn": {modPath + "/internal/backendconn", modPath + "/internal/peercred", modPath + "/internal/localpeer"},
	}
	for pkg, own := range allowed {
		for _, goos := range []string{"", "linux", "darwin", "freebsd", "openbsd"} {
			for _, d := range goListDepsFor(t, goos, pkg) {
				if !std(d) && !slices.Contains(own, d) {
					t.Errorf("%s (GOOS=%s) depends on %s: the standard library, golang.org/x/sys and %v only", pkg, goos, d, own[1:])
				}
			}
		}
	}
	for _, bin := range []string{"/cmd/anet", "/tools/anetpeer", "/cmd/anet-official"} {
		for _, d := range goListDeps(t, modPath+bin) {
			if d == modPath+"/internal/peercred/peercredtest" {
				t.Errorf("%s carries the sudo test helper %s", bin, d)
			}
		}
	}
}

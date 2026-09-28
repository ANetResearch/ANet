package loopguard

import (
	"os/exec"
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
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	out, err := exec.Command(gobin, "list", "-deps", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}
	return strings.Fields(string(out))
}

// The shared guard packages are imported by both the daemon and module/a2a,
// so they may import neither: not the daemon (module/a2a would pull the
// kernel in, and the two would be one package again) and not the A2A SDK
// (the daemon would carry it, SI-8). The daemon, which now imports both,
// and the MCP server stay free of the SDK too (A2A-DESIGN SI-8 names both).
func TestSharedPackagesStayFreeOfTheDaemonAndTheSDK(t *testing.T) {
	for _, pkg := range []string{modPath + "/internal/loopguard", modPath + "/internal/anethome", modPath + "/internal/localpeer"} {
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

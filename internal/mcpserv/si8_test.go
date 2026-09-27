//go:build !no_mcp

package mcpserv

import (
	"os/exec"
	"strings"
	"testing"
)

// SI-8 (A2A-DESIGN §1): the MCP server and the daemon do not link a2a-go.
// They speak A2A through internal/a2ashape, which restates the shapes
// without importing the SDK, so a build without the local A2A interface
// (no_a2a) carries no a2a-go at all. The SDK is allowed in tests only —
// contract_test.go decodes with it — and `go list -deps` leaves test
// imports out, so this is the check CI would run, as a test.
func TestSI8NoA2AGoInTheClosure(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	out, err := exec.Command(gobin, "list", "-deps", ".", "../daemon").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.Contains(pkg, "a2aproject") {
			t.Errorf("%s is in the dependency closure of internal/mcpserv or internal/daemon", pkg)
		}
	}
}

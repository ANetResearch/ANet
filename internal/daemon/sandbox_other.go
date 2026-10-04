//go:build !linux

package daemon

// sandbox_other.go: the bubblewrap sandbox exists only on Linux. On other platforms every sandboxed run
// fails closed until that platform has its own sandbox (macOS Seatbelt: LEC §4.3.8).

func (p *sandboxPlan) prepare() error {
	return sandboxUnavailable("the sandbox is implemented only on Linux (bubblewrap)")
}

func (p *sandboxPlan) command(workDir string, argv, env []string) (*sandboxLaunch, error) {
	return nil, sandboxUnavailable("the sandbox is implemented only on Linux (bubblewrap)")
}

// sandboxKind: no sandbox runs here.
const sandboxKind = ""

// sandboxLimits: no sandbox, no limits to report.
func sandboxLimits() string { return "" }

// removeSandboxLeftovers: no sandbox runs here, so none left anything.
func removeSandboxLeftovers(string) {}

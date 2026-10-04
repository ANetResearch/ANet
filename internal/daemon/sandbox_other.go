//go:build !linux

package daemon

// sandbox_other.go: the bubblewrap sandbox exists only on Linux. On other platforms every sandboxed run
// fails closed until that platform has its own sandbox (macOS Seatbelt: LEC §4.3.8).

func (p *sandboxPlan) prepare() error {
	return sandboxUnavailable("the sandbox is implemented only on Linux (bubblewrap)")
}

func (p *sandboxPlan) command(workDir string, argv, env []string, stdin []byte) (*sandboxLaunch, error) {
	return nil, sandboxUnavailable("the sandbox is implemented only on Linux (bubblewrap)")
}

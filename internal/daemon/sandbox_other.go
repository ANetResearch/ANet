//go:build !linux

package daemon

// sandbox_other.go: the §6 sandbox exists only on Linux (bubblewrap). On other platforms every
// sandboxed run fails closed; A2A-DESIGN §21 item 14 lists non-Linux sandboxes as out of scope.

func (p *sandboxPlan) prepare() error {
	return sandboxUnavailable("the sandbox is implemented only on Linux (bubblewrap)")
}

func (p *sandboxPlan) wrap(workDir, bin string, args []string) (string, []string, error) {
	return "", nil, sandboxUnavailable("the sandbox is implemented only on Linux (bubblewrap)")
}

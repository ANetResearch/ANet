//go:build unix && !linux

package daemon

import "runtime"

// runSandboxChecks on a Unix without its own probe yet: every check reports failure, so the sandbox is
// never taken for working. The macOS checks are LEC MS-4.
func runSandboxChecks(spec sandboxCheckSpec) sandboxCheckReport {
	return sandboxCheckReport{Detail: map[string]string{"platform": "no sandbox self-check on " + runtime.GOOS}}
}

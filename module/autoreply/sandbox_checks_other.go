//go:build unix && !linux && !darwin

package autoreply

import "runtime"

// runSandboxChecks on a Unix without a sandbox: every check reports failure, so the sandbox is never
// taken for working (LEC OP-1). The Linux checks are SB-2 (sandbox_linux.go), the macOS ones MS-4
// (sandbox_darwin.go).
func runSandboxChecks(spec sandboxCheckSpec) sandboxCheckReport {
	return sandboxCheckReport{Detail: map[string]string{"platform": "no sandbox self-check on " + runtime.GOOS}}
}

// usable: a probe spec here needs no more than its version and a port.
func (s sandboxCheckSpec) usable() bool { return s.TCP != "" }

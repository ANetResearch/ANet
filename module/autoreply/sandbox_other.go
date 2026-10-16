//go:build !linux && !darwin

package autoreply

// sandbox_other.go: there is a sandbox on Linux (bubblewrap, sandbox_linux.go) and on macOS (Seatbelt,
// sandbox_darwin.go) only. Everywhere else every sandboxed run fails closed with platform_unsupported
// (LEC OP-1).

// sandboxCodePlatformUnsupported is the local reason (ux §1.4 H) a sandboxed run is refused with here.
const sandboxCodePlatformUnsupported = "platform_unsupported"

func (p *sandboxPlan) prepare() error {
	return &sandboxProbeError{Code: sandboxCodePlatformUnsupported, Detail: "the sandbox runs only on Linux (bubblewrap) and macOS (Seatbelt)"}
}

func (p *sandboxPlan) command(workDir string, argv, env []string, stdin []byte) (*sandboxLaunch, error) {
	return nil, p.prepare()
}

// sandboxKind: no sandbox runs here.
const sandboxKind = ""

// sandboxLimits: no sandbox, no limits to report.
func sandboxLimits() string { return "" }

// removeSandboxLeftovers: no sandbox runs here, so none left anything.
func removeSandboxLeftovers(string) {}

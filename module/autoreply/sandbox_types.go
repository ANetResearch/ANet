package autoreply

// sandbox_types.go holds the sandbox declarations that every platform's build references, including
// those with no sandbox at all (sandbox_other.go), so that they compile everywhere. The sandbox itself
// (sandbox_init.go, sandbox_linux.go, sandbox_darwin.go) stays unix-only.

import "fmt"

// sandboxBridgeListen is the sandbox-side address of the egress endpoint (LEC §2.6.1).
const sandboxBridgeListen = "127.0.0.1:3128"

// sandboxProbeError is a sandbox that cannot be used here, with the local reason code (UX §1.4 H:
// no_bwrap, userns_blocked, userns_unavailable, netns_failed, seccomp_failed) and a detail for the
// operator. errors.Is reports it as errSandboxUnavailable, so every caller fails the turn closed.
type sandboxProbeError struct {
	Code   string
	Detail string
}

func (e *sandboxProbeError) Error() string {
	return fmt.Sprintf("%v: %s: %s", errSandboxUnavailable, e.Code, e.Detail)
}

func (e *sandboxProbeError) Unwrap() error { return errSandboxUnavailable }

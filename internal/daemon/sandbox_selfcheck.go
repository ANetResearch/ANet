package daemon

import "errors"

// SandboxCheck is this host's sandbox, as doctor reports it (LEC §5.5, ux §5.2 sandbox): which sandbox
// the platform has and, when it cannot be used, the local reason (ux §1.4 H) and a detail for the
// operator.
type SandboxCheck struct {
	// Kind is "bwrap" (Linux), "seatbelt" (macOS) or "" where there is none.
	Kind string `json:"kind"`
	// Reason is "" when the sandbox is usable, else its code: platform_unsupported, no_bwrap,
	// userns_blocked, userns_unavailable, netns_failed, seccomp_failed, no_sandbox_exec or
	// fs_isolation_failed.
	Reason string `json:"reason,omitempty"`
	// Detail is what the self-check saw, for the operator; it is not a sentence of the reason table.
	Detail string `json:"detail,omitempty"`
}

// SandboxSelfCheck runs this host's sandbox self-check in the calling process (SB-2 on Linux, MS-4 on
// macOS), or reuses its result for five minutes, as a sandboxed run would before starting. doctor calls
// it when the daemon cannot tell it; its probe files are made under the system temp dir only and removed
// before it returns (LEC §5.5).
func SandboxSelfCheck() SandboxCheck {
	c := SandboxCheck{Kind: sandboxKind}
	err := (&sandboxPlan{}).prepare()
	var pe *sandboxProbeError
	switch {
	case err == nil:
	case errors.As(err, &pe):
		c.Reason, c.Detail = pe.Code, pe.Detail
	default:
		c.Reason, c.Detail = "platform_unsupported", err.Error()
	}
	return c
}

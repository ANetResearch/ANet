package main

import (
	"slices"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// sandboxPlatformReasons are the reasons the sandbox item of doctor reports (ux §5.2 sandbox): the ones
// that are about this host's sandbox itself. The agent's and the model service's own reasons are
// sandbox.agent's and sandbox.upstream's.
var sandboxPlatformReasons = []string{"platform_unsupported", "no_bwrap", "userns_blocked", "userns_unavailable",
	"netns_failed", "seccomp_failed", "fs_isolation_failed", "no_sandbox_exec"}

// sandboxCheck is doctor's sandbox item (ux §5.2, LEC §5.5): shown only when untrusted or trusted peers
// run in the sandbox (UX-K6); ok with sandbox.ready, or sandbox.ready_egress when hosts are allowed; fail
// with the reason the sandbox cannot be used — on Linux bubblewrap's (SB-2), on macOS sandbox-exec's
// (MS-4), elsewhere platform_unsupported. The running daemon's result (GET /status
// auto_reply.sandbox) is used when it has one; otherwise doctor runs the self-check itself, with its
// probe files under the system temp dir only. What the self-check saw goes to the --json report
// (sandbox.detail), since the sentence asks for the details doctor holds.
func sandboxCheck(say func(id, status, key string, args a2ashape.Args), rep *doctorReport, env doctorEnv,
	layout daemon.Layout, ar *daemon.AutoReplyConfig) {
	if ar == nil || (ar.UntrustedMode() != daemon.UntrustedSandbox && ar.TrustedMode() != daemon.TrustedSandbox) {
		return
	}
	c, ok := sandboxFromStatus(env, rep, layout)
	if !ok {
		if env.sandbox == nil {
			return
		}
		c = env.sandbox()
	}
	rep.Sandbox = &c
	if c.Reason != "" {
		say("sandbox", stFail, c.Reason, nil)
		return
	}
	var hosts []string
	if ar.Sandbox != nil {
		hosts = ar.Sandbox.EgressAllow
	}
	if len(hosts) == 0 {
		say("sandbox", stOK, "sandbox.ready", nil)
		return
	}
	say("sandbox", stOK, "sandbox.ready_egress", a2ashape.Args{"n": len(hosts), "hosts": strings.Join(hosts, ", ")})
}

// sandboxFromStatus is the running daemon's own sandbox result, from GET /status auto_reply.sandbox (LEC
// §2.3.3), when it holds one this item reports: ready, or unavailable for one of sandboxPlatformReasons.
func sandboxFromStatus(env doctorEnv, rep *doctorReport, layout daemon.Layout) (daemon.SandboxCheck, bool) {
	if !rep.Control.Running || env.status == nil {
		return daemon.SandboxCheck{}, false
	}
	st, err := env.status(rep.Control.Addr, layout.ControlTokenPath())
	if err != nil {
		return daemon.SandboxCheck{}, false
	}
	ar, _ := st["auto_reply"].(map[string]any)
	sb, _ := ar["sandbox"].(map[string]any)
	state, _ := sb["state"].(string)
	reason, _ := sb["reason"].(string)
	kind, _ := sb["kind"].(string)
	switch {
	case state == "ready":
		return daemon.SandboxCheck{Kind: kind}, true
	case state == "unavailable" && slices.Contains(sandboxPlatformReasons, reason):
		return daemon.SandboxCheck{Kind: kind, Reason: reason}, true
	}
	return daemon.SandboxCheck{}, false
}

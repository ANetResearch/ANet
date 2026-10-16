//go:build !no_autoreply

package main

import (
	"encoding/json"
	"runtime"
	"slices"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/module/autoreply"
)

// sandboxPlatformReasons are the reasons the sandbox item of doctor reports (ux §5.2 sandbox): the ones
// that are about this host's sandbox itself. The agent's and the model service's own reasons are
// sandbox.agent's and sandbox.upstream's.
var sandboxPlatformReasons = []string{"platform_unsupported", "no_bwrap", "userns_blocked", "userns_unavailable",
	"netns_failed", "seccomp_failed", "fs_isolation_failed", "no_sandbox_exec", "sandbox_start_failed"}

// sandboxSelfCheckReasons are the reasons that have a .doctor variant: in doctor the sentence carries what
// the self-check saw (ux §1.4 H); everywhere else it asks to run doctor.
var sandboxSelfCheckReasons = []string{"netns_failed", "seccomp_failed", "fs_isolation_failed"}

// sandboxCheck is doctor's sandbox item (ux §5.2, LEC §5.5): shown only when untrusted or trusted peers
// run in the sandbox (UX-K6); ok with sandbox.ready, or sandbox.ready_egress when hosts are allowed; fail
// with the reason the sandbox cannot be used — on Linux bubblewrap's (SB-2), on macOS sandbox-exec's
// (MS-4), elsewhere platform_unsupported. The running daemon's result (GET /status
// auto_reply.sandbox) is used when it has one; otherwise doctor runs the self-check itself, with its
// probe files under the system temp dir only. What the self-check saw goes to the --json report
// (sandbox.detail), since the sentence asks for the details doctor holds.
func sandboxCheck(say func(id, status, key string, args a2ashape.Args), rep *doctorReport, env doctorEnv,
	layout daemon.Layout, ar *autoreply.AutoReplyConfig) {
	// The openai backend runs no local program: nothing is sandboxed, and /status says sandbox.off, so
	// doctor must not report a sandbox that runs (T-1 EXE F-8).
	if ar == nil || ar.Backend != "exec" || (ar.UntrustedMode() != autoreply.UntrustedSandbox && ar.TrustedMode() != autoreply.TrustedSandbox) {
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
		key, args := c.Reason, a2ashape.Args(nil)
		switch {
		case slices.Contains(sandboxSelfCheckReasons, c.Reason) && c.Detail != "":
			key, args = c.Reason+".doctor", a2ashape.Args{"detail": c.Detail}
		case c.Reason == "sandbox_start_failed":
			args = a2ashape.Args{"error": c.Detail}
		}
		say("sandbox", stFail, key, args)
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
func sandboxFromStatus(env doctorEnv, rep *doctorReport, layout daemon.Layout) (sandboxReport, bool) {
	if !rep.Control.Running || env.status == nil {
		return sandboxReport{}, false
	}
	st, err := env.status(rep.Control.Addr, layout.ControlTokenPath())
	if err != nil {
		return sandboxReport{}, false
	}
	ar, _ := st["auto_reply"].(map[string]any)
	sb, _ := ar["sandbox"].(map[string]any)
	state, _ := sb["state"].(string)
	reason, _ := sb["reason"].(string)
	kind, _ := sb["kind"].(string)
	detail, _ := sb["detail"].(string)
	switch {
	case state == "ready":
		return sandboxReport{Kind: kind}, true
	case state == "unavailable" && slices.Contains(sandboxPlatformReasons, reason):
		return sandboxReport{Kind: kind, Reason: reason, Detail: detail}, true
	}
	return sandboxReport{}, false
}

// autoReplyCheck is doctor's auto_reply item: off, or the backend and agent or model, with how trusted
// peers' tasks run when the backend runs a local agent (ux §5.2 auto_reply).
func autoReplyCheck(rep *doctorReport, ar *autoreply.AutoReplyConfig, lang a2ashape.Language) {
	if ar == nil {
		rep.Checks = append(rep.Checks, sayCheck("auto_reply", stInfo, "autoreply.off", nil, lang))
		return
	}
	name := ar.Model
	if rep.AutoReply.Backend == "exec" {
		name = ar.Agent
	}
	c := sayCheck("auto_reply", stOK, "autoreply.summary", a2ashape.Args{"backend": rep.AutoReply.Backend, "name": name}, lang)
	if rep.AutoReply.Backend == "exec" {
		key := "sandbox.trusted_direct"
		if ar.TrustedMode() == autoreply.TrustedSandbox {
			key = "sandbox.trusted_sandbox"
		}
		c.Detail += " " + a2ashape.Sentence(key, lang, nil)
	}
	rep.Checks = append(rep.Checks, c)
}

// sandboxDetailChecks adds the items that follow sandbox, shown when it is (ux §5.2, UX-K6): the agent
// and the model service, the hosts the sandbox may reach, today's budget, and the resource limits.
func sandboxDetailChecks(add func(id, status, detail, hint string), say func(id, status, key string, args a2ashape.Args),
	rep *doctorReport, env doctorEnv, layout daemon.Layout, ar *autoreply.AutoReplyConfig, lang a2ashape.Language) {
	if ar == nil || (ar.UntrustedMode() != autoreply.UntrustedSandbox && ar.TrustedMode() != autoreply.TrustedSandbox) {
		return
	}
	if ar.Backend == "exec" {
		key, args := ar.SandboxUpstreamProblem()
		code, _, _ := strings.Cut(key, ".")
		switch {
		case code == "agent_unsupported":
			say("sandbox.agent", stFail, key, args)
		case key != "":
			add("sandbox.agent", stOK, "", "")
			say("sandbox.upstream", stFail, key, args)
		default:
			add("sandbox.agent", stOK, "", "")
			add("sandbox.upstream", stOK, "", "")
		}
	}
	// sandbox.egress: the list as the daemon would check it, then the proxy variables the sandbox does
	// not use.
	var hosts []string
	if ar.Sandbox != nil {
		hosts = ar.Sandbox.EgressAllow
	}
	switch _, ref := autoreply.CheckEgressAllow(hosts, ar.APIBase); {
	case ref != nil:
		say("sandbox.egress", stFail, ref.Key, ref.Args)
	case len(hosts) > 0:
		say("sandbox.egress", stInfo, "sandbox.ready_egress", a2ashape.Args{"n": len(hosts), "hosts": strings.Join(hosts, ", ")})
	default:
		add("sandbox.egress", stOK, "", "")
	}
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"} {
		if env.env(k) != "" {
			say("sandbox.egress", stWarn, "sandbox.proxy_env", a2ashape.Args{"key": k})
			break
		}
	}
	// sandbox.budget: today's count is the daemon's.
	if st, ok := runtimeStatusQuiet(env, rep, layout); ok {
		ar, _ := st["auto_reply"].(map[string]any)
		sb, _ := ar["sandbox"].(map[string]any)
		b, _ := sb["budget"].(map[string]any)
		num := func(k string) int { f, _ := b[k].(float64); return int(f) }
		c := sayCheck("sandbox.budget", stOK, "sandbox.budget", a2ashape.Args{"n": num("today"), "daily": num("runs_per_day"),
			"per_peer": num("runs_per_peer_per_day")}, lang)
		c.Detail += " " + a2ashape.Sentence("sandbox.worst_case", lang, a2ashape.Args{"daily": num("runs_per_day")})
		rep.Checks = append(rep.Checks, c)
	} else {
		say("sandbox.budget", stUnknown, doctorDaemonDown, nil)
	}
	if runtime.GOOS == "linux" {
		if autoreply.SandboxLimits() == "no_cgroup" {
			say("sandbox.limits", stWarn, "no_cgroup_limits", nil)
		} else {
			add("sandbox.limits", stOK, "", "")
		}
	}
}

// sandboxSelfCheck is this host's sandbox self-check as the report holds it.
func sandboxSelfCheck() sandboxReport { return sandboxReport(autoreply.SandboxSelfCheck()) }

// autoReplyReport fills the auto_reply member of the report from the configuration's block.
func autoReplyReport(rep *doctorReport, cfg daemon.Config) {
	ar := autoreply.ParseAutoReply(cfg.AutoReply) // the block, decoded (the daemon keeps it raw)
	if ar == nil {
		return
	}
	rep.AutoReply.Configured = true
	rep.AutoReply.Backend = ar.Backend
	if rep.AutoReply.Backend == "" {
		rep.AutoReply.Backend = "openai"
	}
	rep.AutoReply.APIKeyConfigured = strings.TrimSpace(ar.APIKey) != ""
}

// autoReplyChecks adds doctor's auto_reply item and, when the sandbox is in use, the sandbox items.
func autoReplyChecks(add func(id, status, detail, hint string), say func(id, status, key string, args a2ashape.Args),
	rep *doctorReport, env doctorEnv, layout daemon.Layout, cfg daemon.Config, lang a2ashape.Language) {
	arCfg := autoreply.ParseAutoReply(cfg.AutoReply)
	autoReplyCheck(rep, arCfg, lang)
	sandboxCheck(say, rep, env, layout, arCfg)
	sandboxDetailChecks(add, say, rep, env, layout, arCfg, lang)
}

// autoReplyBackend is the backend the auto_reply block names ("" when it names none, or there is no
// block): doctor's preview item asks whether an openai auto-reply makes the previews.
func autoReplyBackend(raw json.RawMessage) (backend string, configured bool) {
	ar := autoreply.ParseAutoReply(raw)
	if ar == nil {
		return "", false
	}
	return ar.Backend, true
}

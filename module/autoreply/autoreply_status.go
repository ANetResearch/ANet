package autoreply

// autoreply_status.go — the auto-reply block of GET /status and POST /autoreply (LEC §2.3.3). One
// function, (*Daemon).autoReplyView, makes it; nothing else in the kernel reads its members.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/execwork"
)

// Values of auto_reply.sandbox.state.
const (
	sandboxStateReady       = "ready"
	sandboxStateUnavailable = "unavailable"
	sandboxStateOff         = "off"
)

// autoReplyView is the auto-reply configuration as /status and /autoreply show it: what it does, never
// the key (only whether one is set, AV-8), the model service address, and the sandbox with its state,
// reason and sentence, CONNECT allowlist, limits and today's budget.
func (m *autoReplyModule) autoReplyView(ar *AutoReplyConfig) map[string]any {
	backend := ar.Backend
	if backend == "" {
		backend = "openai"
	}
	entry := map[string]any{"backend": backend, "untrusted": ar.UntrustedMode(), "trusted": ar.TrustedMode(),
		"admitted": ar.AdmittedMode(), "api_key_set": ar.APIKey != ""}
	switch backend {
	case "exec":
		entry["agent"] = ar.Agent
		if ar.WorkDir != "" {
			entry["work_dir"] = ar.WorkDir
		}
		// The model service address of the sandboxed agent: --egress-allow checks a host against it.
		if ar.APIBase != "" {
			entry["api_base"] = ar.APIBase
		}
	default:
		entry["model"] = ar.Model
		entry["api_base"] = ar.APIBase
	}
	entry["sandbox"] = m.sandboxView(ar)
	return entry
}

// sandboxView is auto_reply.sandbox: state "off" when no run is sandboxed (untrusted=off or a budget of
// 0, and trusted=direct, or a backend that starts no process), "unavailable" with the reason when one
// would be but cannot, else "ready".
func (m *autoReplyModule) sandboxView(ar *AutoReplyConfig) map[string]any {
	lang := a2ashape.Lang()
	v := map[string]any{"egress_allow": egressAllowOf(ar), "kind": "", "limits": "", "budget": m.SandboxBudget()}
	key, state := "sandbox.off", sandboxStateOff
	var args a2ashape.Args
	untrusted := (ar.UntrustedMode() == UntrustedSandbox || ar.AdmittedMode() == AdmittedSandbox) && !ar.BudgetOff()
	if ar.Backend == "exec" && (untrusted || ar.TrustedMode() == TrustedSandbox) {
		v["kind"], v["limits"] = sandboxKind, sandboxLimits()
		key, state, args = m.sandboxProblem(ar)
	}
	// The reason is the sentence's key where the sentence is a status one (sandbox.off, sandbox.ready,
	// sandbox.ready_egress), else the code of the refusal (a variant such as .combo is not part of it).
	reason := a2ashape.ReasonCode(key)
	if strings.HasPrefix(key, "sandbox.") {
		reason = key
	}
	v["state"], v["reason"] = state, reason
	if d, _ := args["detail"].(string); d != "" && state == sandboxStateUnavailable {
		v["detail"] = d // what the self-check saw, for the operator's doctor
	}
	v["message"] = a2ashape.Sentence(key, lang, args)
	if next := a2ashape.Next(key, lang, args); next != "" {
		v["next"] = next
	}
	return v
}

// sandboxProblem is the sentence of a sandbox that runs: the first thing that stops it (the platform's
// self-check, then the agent and the model service, in resolveEgressUpstream's order), or sandbox.ready
// and sandbox.ready_egress.
func (m *autoReplyModule) sandboxProblem(ar *AutoReplyConfig) (key, state string, args a2ashape.Args) {
	if c := SandboxSelfCheck(); c.Reason != "" {
		return c.Reason, sandboxStateUnavailable, a2ashape.Args{"error": c.Detail, "detail": c.Detail}
	}
	if key, args := ar.SandboxUpstreamProblem(); key != "" {
		return key, sandboxStateUnavailable, args
	}
	if n := len(ar.Sandbox.egressHosts()); n > 0 {
		return "sandbox.ready_egress", sandboxStateReady, a2ashape.Args{"n": n, "hosts": strings.Join(ar.Sandbox.egressHosts(), ", ")}
	}
	return "sandbox.ready", sandboxStateReady, nil
}

// SandboxUpstreamProblem is the first thing about the agent or its model service that stops a sandboxed
// run, as the reason-table key (agent_unsupported, agent_unsupported.combo, no_api_key, no_api_base,
// no_model, or api_base_insecure) with its arguments; "" when there is none. It depends on the
// configuration alone, so doctor can say it without the daemon.
func (c AutoReplyConfig) SandboxUpstreamProblem() (key string, args a2ashape.Args) {
	_, err := resolveEgressUpstream(c)
	if err == nil {
		return "", nil
	}
	var pe *sandboxProbeError
	if !errors.As(err, &pe) {
		return "sandbox_start_failed", a2ashape.Args{"error": err.Error()}
	}
	args = a2ashape.Args{"name": c.Agent, "agents": strings.Join(sandboxAgentIDs(), ", ")}
	key = pe.Code
	var ce *egressComboError
	if errors.As(err, &ce) {
		key, args["why"] = ce.reasonKey(), ce.Why
	}
	return key, args
}

// egressHosts is the CONNECT allowlist, nil-safe.
func (s *AutoReplySandboxConfig) egressHosts() []string {
	if s == nil {
		return nil
	}
	return s.EgressAllow
}

// sandboxAgentIDs are the agents the sandbox runs, for the {agents} of agent_unsupported.
func sandboxAgentIDs() []string {
	var ids []string
	for _, a := range agentRegistry() {
		if a.sandbox != nil {
			ids = append(ids, a.id)
		}
	}
	sort.Strings(ids)
	return ids
}

// AutoReplyTestResult is one path of POST /autoreply-test (LEC §2.3.5): "direct" (the configured backend
// as a trusted peer's turn would run it) or "sandbox" (as an untrusted peer's would).
type AutoReplyTestResult struct {
	Path       string `json:"path"`
	OK         bool   `json:"ok"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message"`
	ReplyChars int    `json:"reply_chars,omitempty"`
	// Egress is what the sandboxed run did through this node: model requests, CONNECTs let through and
	// refused.
	Egress *autoReplyTestEgress `json:"egress,omitempty"`
}

type autoReplyTestEgress struct {
	ModelRequests  int `json:"model_requests"`
	ConnectAllowed int `json:"connect_allowed"`
	ConnectRefused int `json:"connect_refused"`
}

// TestAutoReplyPaths runs the enabled paths, in order: direct, then sandbox when untrusted or trusted is
// sandbox and the backend starts a local process. A path that fails is a result, not an error; the error
// is only that auto-reply is not configured.
func (m *autoReplyModule) TestAutoReplyPaths(ctx context.Context, prompt string) ([]AutoReplyTestResult, error) {
	arp := m.current()
	if arp == nil {
		return nil, errors.New(a2ashape.Sentence("autoreply.off", a2ashape.Lang(), nil))
	}
	cfg := *arp
	if strings.TrimSpace(prompt) == "" {
		prompt = autoReplyDefaultTestPrompt
	}
	lang := a2ashape.Lang()
	fail := func(path, reason, why string) AutoReplyTestResult {
		return AutoReplyTestResult{Path: path, Reason: reason,
			Message: a2ashape.Sentence("autoreply_test.fail", lang, a2ashape.Args{"path": path, "why": why})}
	}
	var out []AutoReplyTestResult
	reply, err := m.TestAutoReply(ctx, prompt)
	if err != nil {
		out = append(out, fail("direct", "", err.Error()))
	} else {
		n := utf8.RuneCountInString(reply)
		out = append(out, AutoReplyTestResult{Path: "direct", OK: true, ReplyChars: n,
			Message: a2ashape.Sentence("autoreply_test.pass_direct", lang, a2ashape.Args{"n": n})})
	}
	if cfg.Backend != "exec" || (cfg.UntrustedMode() != UntrustedSandbox && cfg.TrustedMode() != TrustedSandbox) {
		return out, nil
	}
	// The sandbox path: the state first (a sandbox that cannot run says why), then one real run.
	if key, state, args := m.sandboxProblem(&cfg); state == sandboxStateUnavailable {
		out = append(out, fail("sandbox", a2ashape.ReasonCode(key), a2ashape.Sentence(key, lang, args)))
		return out, nil
	}
	timeout := autoReplyDefaultAPITimeout
	if cfg.APITimeoutSeconds > 0 {
		timeout = time.Duration(cfg.APITimeoutSeconds) * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := m.runSandboxed(rctx, sandboxRequest{Cfg: cfg, RC: replyContext{Role: "inbound", Goal: prompt},
		Turns: []chatTurn{{Role: "user", Text: prompt}}})
	if dir, derr := execSandboxInteractionDir(m.env.DataDir, m.env.AID, ""); derr == nil {
		execwork.RemoveTree(dir)
	}
	if err != nil {
		reason, why := "", err.Error()
		var pe *sandboxProbeError
		if errors.As(err, &pe) {
			reason = pe.Code
			why = a2ashape.Sentence(pe.Code, lang, a2ashape.Args{"name": cfg.Agent, "detail": pe.Detail, "error": pe.Detail})
		}
		out = append(out, fail("sandbox", reason, why))
		return out, nil
	}
	e := &autoReplyTestEgress{}
	if res.Egress != nil {
		e.ModelRequests = res.Egress.ModelRequests
		for _, n := range res.Egress.Connect.Allowed {
			e.ConnectAllowed += n
		}
		for _, n := range res.Egress.Connect.Refused {
			e.ConnectRefused += n
		}
	}
	n := utf8.RuneCountInString(res.Reply)
	out = append(out, AutoReplyTestResult{Path: "sandbox", OK: true, ReplyChars: n, Egress: e,
		Message: a2ashape.Sentence("autoreply_test.pass_sandbox", lang, a2ashape.Args{"n": e.ModelRequests, "m": e.ConnectAllowed})})
	return out, nil
}

// SandboxLimits is auto_reply.sandbox.limits on this host: "full", "no_cgroup" (Linux without a user
// systemd) or "rlimit_only" (macOS); "" where there is no sandbox. doctor asks it directly.
func SandboxLimits() string { return sandboxLimits() }

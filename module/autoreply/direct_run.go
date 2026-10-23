package autoreply

// direct_run.go — what a direct run of the exec backend (a trusted or an admitted peer's) gets besides the
// agent itself (design v0.3.9 §C to §E): its key of the model proxy and its token of the turn tools, in its
// environment as ANET_MODEL_* and ANET_TURN_*, and the runner when one is configured. A sandboxed run is
// unchanged: it has its own egress endpoint (egress.go) and no turn tools.

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/turntools"
	"github.com/ANetResearch/ANet/module"
)

// directReply runs one direct turn: with the run's place in the model proxy and on the runner when
// auto_reply.runner is set, else through the replier as the node's user.
func (m *autoReplyModule) directReply(ctx context.Context, cfg AutoReplyConfig, replier autoReplier, th module.Turn,
	rc replyContext, turns []chatTurn) (string, error) {
	run, ticket := m.beginDirectRun(cfg, th)
	defer ticket.release(true)
	defer run.end()
	rc.RunEnv = run.env()
	if cfg.Runner != nil {
		return m.runnerReply(ctx, cfg, th, rc, turns)
	}
	return replier.Reply(ctx, rc, turns)
}

// beginDirectRun mints the run's key of the model proxy, when the proxy has upstreams, and its turn
// token, when the turn is an inbound one of a trusted or an admitted peer and the service module has
// agent_tool capabilities. An admitted peer's run takes its place in the admitted budget first; when the
// day's budget is used up the run is still given a key, and each of its model requests is answered
// model_budget_spent, which the agent reads as the model service's error. Nil when the run gets neither.
func (m *autoReplyModule) beginDirectRun(cfg AutoReplyConfig, th module.Turn) (*proxyRun, *budgetTicket) {
	p := m.currentProxy()
	if p == nil {
		return nil, nil
	}
	admitted := th.Admitted && !th.Trusted
	turn := th.Role == roleInbound && (th.Trusted || admitted) && m.svc != nil && len(m.svc.AgentTools()) > 0
	model := p.hasUpstreams()
	if !model && !turn {
		return nil, nil
	}
	timeout := autoReplyDefaultAPITimeout
	if cfg.APITimeoutSeconds > 0 {
		timeout = time.Duration(cfg.APITimeoutSeconds) * time.Second
	}
	spec := proxyRunSpec{Peer: th.Peer, Ix: th.Ix, Admitted: admitted, Model: model, Turn: turn,
		MaxRequests: trustedRequestsPerRun, Timeout: timeout}
	var ticket *budgetTicket
	if model && admitted {
		daily, perPeer, perRun := cfg.admittedBudgets()
		var verdict budgetVerdict
		ticket, verdict, _ = m.admitted.reserveAtOnce(th.Peer, daily, perPeer, 0, 0, m.admittedRunsOnChain)
		spec.MaxRequests = perRun
		if verdict != budgetGranted || perRun <= 0 {
			spec.Refusal = "model_budget_spent"
			log.Printf("anet: auto-reply %s: the admitted budget is used up; the run's model requests are refused", th.Ix)
		}
	}
	run, err := p.begin(spec)
	if err != nil {
		log.Printf("anet: auto-reply %s: model proxy: %v", th.Ix, err)
		ticket.release(false)
		return nil, nil
	}
	return run, ticket
}

// admittedRunsOnChain is the admitted peers' runs on this node's evidence chain recorded at or after
// since, for the admitted budget (BU-2's replay): a direct run for a peer not on the trust list, which
// only inbound.admit lets in, that was not turned away before it started.
func (m *autoReplyModule) admittedRunsOnChain(since time.Time) ([]budgetRun, error) {
	if m.ev == nil {
		return nil, nil
	}
	evs, err := m.ev.ReadOwnEvidence(EvAutoReplyInvoked, since)
	if err != nil {
		return nil, err
	}
	var out []budgetRun
	for _, e := range evs {
		p := e.Payload
		if trusted, _ := p["trusted"].(bool); trusted {
			continue
		}
		if mode, _ := p["mode"].(string); mode != modeDirect {
			continue
		}
		switch exit, _ := p["exit"].(string); exit {
		case exitBudget, exitSandboxUnavailable:
			continue
		}
		peer, _ := p["peer_aid"].(string)
		out = append(out, budgetRun{peer: peer, start: time.UnixMilli(e.At.UnixMilli() - int64Of(p["duration_ms"]))})
	}
	return out, nil
}

// withRunEnv is a direct run's environment: env, which execEnv built without any ANET_* variable of the
// daemon's, and the run's own ANET_TURN_* and ANET_MODEL_* variables from extra (the one exception to
// that rule, design v0.3.9 §E). Nothing else in extra is added.
func withRunEnv(env, extra []string) []string {
	for _, kv := range extra {
		k, v, ok := strings.Cut(kv, "=")
		if ok && runnerEnvName(k) && runnerEnvValue(v) &&
			(strings.HasPrefix(k, turntools.EnvTurnPrefix) || strings.HasPrefix(k, turntools.EnvModelPrefix)) {
			env = append(env, kv)
		}
	}
	return env
}

// --- the proxy's lifecycle ---

// currentProxy is the model proxy in force, nil when none is configured.
func (m *autoReplyModule) currentProxy() *modelProxy {
	m.proxyMu.Lock()
	defer m.proxyMu.Unlock()
	return m.proxy
}

// ensureModelProxy starts, keeps or stops the model proxy for cfg: kept when its block is unchanged,
// started again when it changed (a run under way loses its key then), stopped when there is none.
func (m *autoReplyModule) ensureModelProxy(cfg AutoReplyConfig) error {
	m.proxyMu.Lock()
	defer m.proxyMu.Unlock()
	if cfg.ModelProxy == nil || cfg.Backend != "exec" {
		if m.proxy != nil {
			m.proxy.Close()
			m.proxy = nil
			log.Printf("anet: auto-reply: model proxy off")
		}
		return nil
	}
	if m.proxy != nil && m.proxy.sameConfig(*cfg.ModelProxy) {
		return nil
	}
	if m.proxy != nil {
		m.proxy.Close()
		m.proxy = nil
	}
	p, err := newModelProxy(*cfg.ModelProxy)
	if err != nil {
		return err
	}
	p.setService(m.svc)
	m.proxy = p
	log.Printf("anet: auto-reply: model proxy on %s (%d upstream(s))", p.addr, len(p.ups))
	return nil
}

// closeModelProxy stops the model proxy (auto-reply off, or the module stopping).
func (m *autoReplyModule) closeModelProxy() {
	m.proxyMu.Lock()
	defer m.proxyMu.Unlock()
	if m.proxy != nil {
		m.proxy.Close()
		m.proxy = nil
	}
}

// runnerView is the runner and the model proxy as /status and /autoreply show them: the socket and the
// account, the listener and the upstreams' names; never a key or a key file.
func (m *autoReplyModule) runnerView(ar *AutoReplyConfig, entry map[string]any) {
	if r := ar.Runner; r != nil {
		entry["runner"] = map[string]any{"socket": r.Socket, "expected_user": r.ExpectedUser}
	}
	if pc := ar.ModelProxy; pc != nil {
		names := make([]string, 0, len(pc.Upstreams))
		for _, u := range pc.Upstreams {
			names = append(names, u.Name)
		}
		v := map[string]any{"listen": pc.Listen, "upstreams": names}
		if p := m.currentProxy(); p != nil {
			v["address"] = p.addr
		}
		daily, perPeer, perRun := ar.admittedBudgets()
		v["admitted_budget"] = map[string]any{"runs_per_day": daily, "runs_per_peer_per_day": perPeer,
			"requests_per_run": perRun, "today": m.admitted.today(m.admittedRunsOnChain)}
		entry["model_proxy"] = v
	}
}

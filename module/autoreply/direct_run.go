package autoreply

// direct_run.go — what a direct run of the exec backend (a trusted or an admitted peer's) gets besides the
// agent itself (design v0.3.9 §C to §E): its key of the model proxy and its token of the turn tools, in its
// environment as ANET_MODEL_* and ANET_TURN_*, and the runner when one is configured. A sandboxed run is
// unchanged: it has its own egress endpoint (egress.go) and no turn tools.
//
// A direct run runs beside the scan of owed turns and may outlive it (startDirect): at most
// auto_reply.runner.max_jobs at once (directRunsAtOnceDefault without one), and one per peer, so that one
// peer's long task holds up neither the other peers' turns nor the runner's other jobs. An admitted
// peer's run takes its place in the admitted budget before it starts; once the day's budget is used up
// the task is refused, as a sandboxed run's is, and no agent starts.

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANet/internal/turntools"
	"github.com/ANetResearch/ANet/module"
)

// directReply runs one direct turn: with the run's place in the model proxy and on the runner when
// auto_reply.runner is set, else through the replier as the node's user. ticket is the run's place in
// the admitted budget (startDirect), nil for a run that does not count.
func (m *autoReplyModule) directReply(ctx context.Context, cfg AutoReplyConfig, replier autoReplier, th module.Turn,
	rc replyContext, turns []chatTurn, ticket *budgetTicket) (string, error) {
	run := m.beginDirectRun(cfg, th, ticket)
	defer run.end()
	rc.RunEnv = run.env()
	if cfg.Runner != nil {
		return m.runnerReply(ctx, cfg, th, rc, turns)
	}
	return replier.Reply(ctx, rc, turns)
}

// beginDirectRun mints the run's key of the model proxy, when the proxy has upstreams, and its turn
// token, when the turn is an inbound one of a trusted or an admitted peer and the service module has
// agent_tool capabilities. An admitted peer's run took its place in the admitted budget before it was
// started (startDirect) and gets requests_per_run model requests; one that has no place (the proxy
// came with a configuration change after the run was started) is given a key whose every request is
// answered model_budget_spent, which the agent reads as the model service's error. Nil when the run
// gets neither.
func (m *autoReplyModule) beginDirectRun(cfg AutoReplyConfig, th module.Turn, ticket *budgetTicket) *proxyRun {
	p := m.currentProxy()
	if p == nil {
		return nil
	}
	admitted := th.Admitted && !th.Trusted
	turn := th.Role == roleInbound && (th.Trusted || admitted) && m.svc != nil && len(m.svc.AgentTools()) > 0
	model := p.hasUpstreams()
	if !model && !turn {
		return nil
	}
	timeout := autoReplyDefaultAPITimeout
	if cfg.APITimeoutSeconds > 0 {
		timeout = time.Duration(cfg.APITimeoutSeconds) * time.Second
	}
	spec := proxyRunSpec{Peer: th.Peer, Ix: th.Ix, Admitted: admitted, Model: model, Turn: turn,
		MaxRequests: trustedRequestsPerRun, Timeout: timeout}
	if model && admitted {
		_, _, perRun := cfg.admittedBudgets()
		spec.MaxRequests = perRun
		if ticket == nil || perRun <= 0 {
			spec.Refusal = "model_budget_spent"
			log.Printf("anet: auto-reply %s: the run has no place in the admitted budget; its model requests are refused", th.Ix)
		}
	}
	run, err := p.begin(spec)
	if err != nil {
		log.Printf("anet: auto-reply %s: model proxy: %v", th.Ix, err)
		return nil
	}
	return run
}

// --- direct runs beside the scan ---

// directRunsAtOnceDefault bounds the direct runs under way at once when auto_reply.runner sets no
// max_jobs, or no runner is configured: the default of the runner's --max-jobs.
const directRunsAtOnceDefault = 4

// directRunsAtOnce is how many direct runs may be under way at once: auto_reply.runner.max_jobs, which
// the operator sets to the runner's --max-jobs, or directRunsAtOnceDefault.
func (c AutoReplyConfig) directRunsAtOnce() int {
	if r := c.Runner; r != nil && r.MaxJobs > 0 {
		return r.MaxJobs
	}
	return directRunsAtOnceDefault
}

// admittedCounted reports whether th's direct run counts against the admitted budget: an admitted peer
// that is not trusted, while the model proxy forwards to an upstream.
func (m *autoReplyModule) admittedCounted(th module.Turn) bool {
	if !th.Admitted || th.Trusted {
		return false
	}
	p := m.currentProxy()
	return p != nil && p.hasUpstreams()
}

// startDirect starts th's direct run beside the scan (autoReplyOnce). The run takes a place among the
// direct runs first: none is free, or the peer's own run is still under way, and the turn waits for the
// next scan. An admitted peer's run then takes its place in the admitted budget: a budget of 0 leaves
// the task in the inbox, as the sandbox's does (BU-3); a budget used up for the day refuses the task with
// anet.reason=budget and the time until the next local midnight before anything starts (BU-4).
func (m *autoReplyModule) startDirect(ctx context.Context, cfg AutoReplyConfig, replier autoReplier, gate execGate, th module.Turn,
	turns []chatTurn, scan uint64) error {
	if !m.direct.take(scan, th.Peer, th.Ix, cfg.directRunsAtOnce()) {
		return nil
	}
	var ticket *budgetTicket
	if m.admittedCounted(th) {
		daily, perPeer, perRun := cfg.admittedBudgets()
		if perRun <= 0 {
			daily = 0 // no model request allowed: no run either
		}
		var verdict budgetVerdict
		var wait time.Duration
		ticket, verdict, wait = m.admitted.reserveAtOnce(th.Peer, daily, perPeer, 0, 0, m.admittedRunsOnChain)
		switch verdict {
		case budgetOff, budgetBusy:
			m.direct.giveBack(th.Peer)
			return nil
		case budgetSpent:
			m.direct.giveBack(th.Peer)
			return m.refuseTurn(ctx, th, autoReplyRun{Agent: gate.agent(cfg), InteractionID: th.Ix,
				Peer: th.Peer, Trusted: gate.trusted, Mode: gate.mode, Exit: exitBudget}, wait)
		}
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.direct.done(th.Peer, th.Ix)
		if err := m.replyTurn(ctx, cfg, replier, gate, th, turns, ticket); err != nil && !errors.Is(err, module.ErrTurnGone) {
			log.Printf("anet: auto-reply %s: %v", th.Ix, err)
		}
	}()
	return nil
}

// directRuns holds the direct runs under way, which outlive the scan that started them. A scan lists
// the owed turns before it starts any: a run that ends after the list was made may have answered a turn
// the list still shows as owed, so that scan leaves the turn to the next one, whose list is made after
// the answer. The zero value is ready.
type directRuns struct {
	mu      sync.Mutex
	running int
	peers   map[string]bool // peers with a run under way
	// seq counts the scans and the runs that ended; scan is the seq of the latest scan, ended the seq at
	// which each interaction's last run ended, kept until a scan begins after it.
	seq   uint64
	scan  uint64
	ended map[string]uint64
	runs  sync.WaitGroup // the runs under way, for the tests
}

// beginScan is a new scan's place in the order, taken before the scan lists the owed turns. A scan
// that began earlier starts nothing more.
func (d *directRuns) beginScan() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	d.scan = d.seq
	for ix, at := range d.ended {
		if at < d.scan {
			delete(d.ended, ix)
		}
	}
	return d.scan
}

// take reserves a place for a run of peer on ix for the scan: false when the scan is not the latest,
// ix's run ended after the scan made its list, the peer has a run under way, or atOnce runs are.
func (d *directRuns) take(scan uint64, peer, ix string, atOnce int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if scan != d.scan || d.ended[ix] > scan || d.peers[peer] || d.running >= atOnce {
		return false
	}
	if d.peers == nil {
		d.peers = map[string]bool{}
	}
	d.peers[peer] = true
	d.running++
	d.runs.Add(1)
	return true
}

// giveBack returns a place taken for a run that did not start.
func (d *directRuns) giveBack(peer string) {
	d.mu.Lock()
	d.running--
	delete(d.peers, peer)
	d.mu.Unlock()
	d.runs.Done()
}

// done ends the run of peer on ix, after its answer was sent.
func (d *directRuns) done(peer, ix string) {
	d.mu.Lock()
	d.running--
	delete(d.peers, peer)
	d.seq++
	if d.ended == nil {
		d.ended = map[string]uint64{}
	}
	d.ended[ix] = d.seq
	d.mu.Unlock()
	d.runs.Done()
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
		m.proxy.setAccounts(runnerAccounts(cfg.Runner))
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
	p.setAccounts(runnerAccounts(cfg.Runner))
	m.proxy = p
	log.Printf("anet: auto-reply: model proxy on %s (%d upstream(s))", p.addr, len(p.ups))
	return nil
}

// runnerAccounts is the account of the runner, whose runs' connections the model proxy takes besides
// the node's own: expected_user, or the owner of the runner's socket when it is not set. Nil without a
// runner.
func runnerAccounts(r *AutoReplyRunnerConfig) func() []int {
	if r == nil {
		return nil
	}
	rules, err := runnerPolicy(r).Resolve()
	if err != nil {
		return nil // refused when the configuration was checked
	}
	if uid, ok := rules.ExpectedUID(); ok {
		return func() []int { return []int{uid} }
	}
	sock := strings.TrimSpace(r.Socket)
	return func() []int {
		if _, owner, err := rules.CheckPath(sock); err == nil {
			return []int{owner}
		}
		return nil
	}
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

// runnerView is the runner and the model proxy as /status and /autoreply show them: the socket, the
// account and the direct runs at once, the listener and the upstreams' names; never a key or a key file.
func (m *autoReplyModule) runnerView(ar *AutoReplyConfig, entry map[string]any) {
	if r := ar.Runner; r != nil {
		entry["runner"] = map[string]any{"socket": r.Socket, "expected_user": r.ExpectedUser, "max_jobs": ar.directRunsAtOnce()}
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

package daemon

// status_summary.go — /status.summary (ux §4.7, UX-C6): the status summary
// is made here and nowhere else. hStatus gathers the facts each area owns
// (the identity, the hub join, the outbox, the inbound policy, the sandbox,
// the spending limits, retention, the clock and the preview setting) into an
// a2ashape.StatusInput; a2ashape.StatusSummary decides which lines show and
// words them. `anet status` prints the lines, and MCP node_status passes the
// whole /status on, so the three say the same thing.

import (
	"encoding/json"
	"math"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// statusProfile is /status.profile: the profile this node publishes. It was
// /status.summary before the status summary took that name (ux §4.7).
type statusProfile struct {
	Summary string `json:"summary"`
}

// statusWire is the part of /status the wire registry lists (ANetCore
// wire/registry.json, route /status).
type statusWire struct {
	Summary []a2ashape.SummaryLine `json:"summary"`
	Profile statusProfile          `json:"profile"`
}

// asMap is v as the generic JSON object /status carries it; nil when v is
// not an object. A member another area builds (auto_reply, delivery) is read
// through its wire form, so this file does not depend on its Go type.
func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// num is the member k of m as a whole number; 0 when it is none.
func num(m map[string]any, k string) int {
	f, _ := m[k].(float64)
	if f < 0 || f != f || f > math.MaxInt32 {
		return 0
	}
	return int(f)
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// statusSummary is /status.summary for the members out already holds.
func (d *Daemon) statusSummary(out map[string]any, lang a2ashape.Language) []a2ashape.SummaryLine {
	cfg := d.config()
	var in a2ashape.StatusInput
	d.IdentityStatus().FillSummary(&in)

	hub := d.viewOfJoin(d.joinSnapshot(), time.Now(), lang)
	in.Hub = a2ashape.Said{Key: hub.key, Args: hub.args}
	in.HubRegistered = hub.State == JoinRegistered
	if in.IdentityState == IdentityDeactivated {
		// The join record still says "registered", which is true of the hub and no longer of this node.
		in.Hub = a2ashape.Said{Key: reasonDeactivated}
		in.HubRegistered = false
	}
	if rec := d.joinSnapshot(); rec != nil && hub.key == reasonPending && rec.NextAttemptAt > 0 {
		in.HubThen = &a2ashape.Said{Key: "hub.next_check", Args: a2ashape.Args{"time": time.UnixMilli(rec.NextAttemptAt), "now": time.Now()}}
	}

	in.Delivery = deliverySaid(asMap(out["delivery"]))

	in.Inbound = d.inboundSaid(cfg, asMap(out["auto_reply"]) != nil)
	in.InboundThen = d.admitSaid(cfg)

	if ar := asMap(out["auto_reply"]); ar != nil && (str(ar, "untrusted") == untrustedSandbox || str(ar, "trusted") == trustedSandbox ||
		str(ar, "admitted") == trustedSandbox) {
		in.Sandbox, in.SandboxBudget = sandboxSaid(asMap(ar["sandbox"]))
	}

	in.Payments = d.SpendStatus().limitsSaid()

	in.Retention = a2ashape.Said{Key: "retention.summary", Args: a2ashape.Args{"retention": d.ContentRetention().String()}}
	if _, state := clockGate(d.clockObservation(), 0, d.nowMS(), d.monoMS()); state == clockSkewed || state == clockUnverified {
		in.Clock = d.clockSaid(state)
	}
	if n := d.localStateLost(); n != nil {
		if n.Repeated {
			in.LocalStateRepeated = n.said()
		} else {
			in.LocalStateDetected = n.said()
		}
	}

	if p := asMap(out["preview"]); p != nil {
		switch str(p, "mode") {
		case "on":
			in.Preview = &a2ashape.Said{Key: "preview.on"}
		case "with_reasoning":
			in.Preview = &a2ashape.Said{Key: "preview.with_reasoning"}
		}
	}
	sum := a2ashape.StatusSummary(in, lang)
	if sum == nil {
		sum = []a2ashape.SummaryLine{}
	}
	return sum
}

// clockSaid is the clock sentence of the privacy line (FS-U7): clock.skewed
// with how far apart the clocks are, or clock.unverified.
func (d *Daemon) clockSaid(state string) *a2ashape.Said {
	if state == clockSkewed {
		off := min(absMS(d.clockObservation().offset), uint64(math.MaxInt64/int64(time.Millisecond)))
		return &a2ashape.Said{Key: "clock.skewed", Args: a2ashape.Args{"dur": time.Duration(off) * time.Millisecond}}
	}
	return &a2ashape.Said{Key: "clock.unverified"}
}

// inboundSaid is the inbound line (ux §4.7): the policy, how many peers the
// allow list names, how many capabilities are public and whether auto-reply
// is on. Auto-reply is on when /status has an auto_reply member (it is
// absent when auto-reply is not configured or not in this build).
func (d *Daemon) inboundSaid(cfg Config, autoReplyOn bool) a2ashape.Said {
	in := cfg.inbound()
	auto := "off"
	if autoReplyOn {
		auto = "on"
	}
	args := a2ashape.Args{"m": len(in.PublicCapabilities), "auto_reply": auto}
	switch in.Policy {
	case PolicyOpen:
		return a2ashape.Said{Key: "inbound.summary.open", Args: args}
	case PolicyApprove:
		k := 0
		if items, err := d.PendingList(); err == nil {
			k = len(items)
		}
		args["k"] = k
		return a2ashape.Said{Key: "inbound.summary.approve", Args: args}
	}
	ps := d.readPeers()
	n := 0
	for aid := range ps.allow {
		if ps.allowed(aid) {
			n++
		}
	}
	for aid := range ps.trust {
		if !ps.allow[aid] && ps.allowed(aid) {
			n++
		}
	}
	args["n"] = n
	return a2ashape.Said{Key: "inbound.summary.closed", Args: args}
}

// sandboxSaid is the sandbox line and the budget sentence after it, from
// /status.auto_reply.sandbox: its message and next are already worded.
func sandboxSaid(sb map[string]any) (state, budget *a2ashape.Said) {
	if sb == nil || str(sb, "message") == "" {
		return nil, nil
	}
	reason := str(sb, "reason")
	state = &a2ashape.Said{Key: reason, Text: str(sb, "message"), NextText: str(sb, "next")}
	if str(sb, "state") == "unavailable" {
		state.Lvl = a2ashape.LevelFail
	}
	if b := asMap(sb["budget"]); b != nil && str(sb, "state") == "ready" {
		budget = &a2ashape.Said{Key: "sandbox.budget", Args: a2ashape.Args{
			"n": num(b, "today"), "daily": num(b, "runs_per_day"), "per_peer": num(b, "runs_per_peer_per_day")}}
	}
	return state, budget
}

// deliverySaid is the delivery line from /status.delivery (DLV-P-4): shown
// when a message is waiting for confirmation, or stuck; its message is
// already worded. Nil when the member is absent or nothing waits.
func deliverySaid(dl map[string]any) *a2ashape.Said {
	if dl == nil || str(dl, "message") == "" {
		return nil
	}
	if num(dl, "not_sent")+num(dl, "awaiting_confirmation")+num(dl, "queued_at_hub")+num(dl, "stuck")+num(dl, "unconfirmed_24h") == 0 {
		return nil
	}
	lvl := a2ashape.Level(str(dl, "level"))
	return &a2ashape.Said{Key: "outbox.ok", Text: str(dl, "message"), NextText: str(dl, "next"), Lvl: lvl}
}

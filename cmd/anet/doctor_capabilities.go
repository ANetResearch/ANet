package main

// doctor_capabilities.go — the doctor items of the correctness-and-use area
// (ux §5.2): capabilities.files, capabilities.inflight,
// capabilities.allow_only, preview and payments. They read config.json and
// the peer lists, as doctor does, so they run with the daemon down; the
// payments line takes today's remainder from the running daemon when there
// is one.

import (
	"encoding/json"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// inflightTimeoutOver is the timeout above which a capability that takes one
// call at a time is filled by a single call (capabilities.inflight_low).
const inflightTimeoutOver = 60 * time.Second

// serviceDefaultTimeout is module/service's bound on a call when neither the
// capability nor the module sets timeout_ms.
const serviceDefaultTimeout = 2 * time.Minute

// serviceCaps reads the service module's capabilities from config.json: the
// ids it serves and the timeout of each call (the capability's own
// timeout_ms, else the module's, else the default).
func serviceCaps(cfg daemon.Config) map[string]time.Duration {
	raw, ok := cfg.Modules["service"]
	if !ok {
		return nil
	}
	var m struct {
		TimeoutMS    int `json:"timeout_ms"`
		Capabilities []struct {
			ID        string `json:"id"`
			TimeoutMS int    `json:"timeout_ms"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	out := map[string]time.Duration{}
	for _, c := range m.Capabilities {
		d := serviceDefaultTimeout
		switch {
		case c.TimeoutMS > 0:
			d = time.Duration(c.TimeoutMS) * time.Millisecond
		case m.TimeoutMS > 0:
			d = time.Duration(m.TimeoutMS) * time.Millisecond
		}
		out[c.ID] = d
	}
	return out
}

// capabilitiesChecks is capabilities.files, .inflight and .allow_only for
// the public capabilities in st.
func capabilitiesChecks(say func(id, status, key string, args a2ashape.Args), st daemon.PolicyState) {
	service := serviceCaps(st.Config)
	lists := map[string]bool{}
	for _, l := range [][]string{st.Allow, st.Trust} {
		for _, aid := range l {
			lists[aid] = true
		}
	}
	for _, d := range st.Deny {
		delete(lists, d)
	}
	for _, c := range st.Inbound.PublicCapabilities {
		args := a2ashape.Args{"name": c.ID}
		if c.Files != nil && c.Files.Count > 0 {
			if _, served := service[c.ID]; !served {
				say("capabilities.files", stFail, "capabilities.files_not_service", args)
			}
		}
		if timeout, served := service[c.ID]; served && timeout > inflightTimeoutOver && c.MaxInflight < 2 {
			say("capabilities.inflight", stWarn, "capabilities.inflight_low", args)
		}
		if c.AllowOnly {
			if len(lists) == 0 {
				say("capabilities.allow_only", stWarn, "capabilities.allow_only_empty", args)
			} else {
				say("capabilities.allow_only", stInfo, "capabilities.allow_only", a2ashape.Args{"name": c.ID, "n": len(lists)})
			}
		}
	}
}

// previewCheck is preview: nothing while stream_preview is off; on or
// with_reasoning otherwise, and preview.no_source when no openai auto-reply
// makes the previews (they then come only from /tasks/preview).
func previewCheck(say func(id, status, key string, args a2ashape.Args), cfg daemon.Config) {
	switch cfg.StreamPreview {
	case "on":
		say("preview", stInfo, "preview.on", nil)
	case "with_reasoning":
		say("preview", stWarn, "preview.with_reasoning", nil)
	default:
		return
	}
	if ar := cfg.AutoReply; ar == nil || (ar.Backend != "" && ar.Backend != "openai") {
		say("preview", stInfo, "preview.no_source", nil)
	}
}

// paymentsCheck is payments: the limits at a glance (limits_summary, or
// its .off variant), and a warning when automatic or agent payments are on
// with no payee to pay. spend is the running daemon's /status.payments, nil
// when it is down; the day's remainders then are the limits themselves.
func paymentsCheck(say func(id, status, key string, args a2ashape.Args), st daemon.PolicyState, spend map[string]any) {
	p := st.Payments
	left, agentLeft := p.DailyMax, min(p.AgentDailyMax, p.DailyMax)
	if spend != nil {
		if v, ok := spend["left_today"].(float64); ok && v >= 0 {
			left = uint64(v)
		}
		if v, ok := spend["agent_left_today"].(float64); ok && v >= 0 {
			agentLeft = uint64(v)
		}
	}
	key := "limits_summary"
	if p.AutoMax == 0 && p.AgentMax == 0 {
		key += ".off"
	}
	say("payments", stInfo, key, a2ashape.Args{"auto_max": p.AutoMax, "agent_max": p.AgentMax, "agent_left": agentLeft,
		"explicit_max": p.ExplicitMax, "left": left, "n": len(st.Payees)})
	if (p.AutoMax > 0 || p.AgentMax > 0) && len(st.Payees) == 0 {
		say("payments", stWarn, "payments.no_payees", nil)
	}
}

package main

// doctor_hub.go is doctor's hub item (ux §5.2 hub, hub.probation): the join state and its sentence. It
// is the state /status.hub gives, so the sentence is the one `anet status` and MCP show (UX-K7): from the
// running daemon when it answers, else read from hub_join.json by the same code (daemon.ReadHubView).

import (
	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// hubFinding is the hub object of /status, as doctor reads it.
type hubFinding struct{ URL, State, Reason, Message, Next string }

// hubLevel is the status of a join state's finding (ux §5.2): ok when registered, info when there is no
// hub yet, warn while the node waits for an admission or retries, fail when the hub refuses or the wire
// versions do not meet or the identity is stopped or deactivated.
func hubLevel(f hubFinding) string {
	switch f.Reason {
	case "wire_too_old", "wire_too_new", "identity_stopped", "identity_deactivated":
		return stFail
	case "clock_skew":
		return stWarn
	}
	switch f.State {
	case daemon.JoinRegistered:
		return stOK
	case daemon.JoinNone:
		return stInfo
	case daemon.JoinPendingAdmission, daemon.JoinRetrying:
		return stWarn
	case daemon.JoinRefused:
		return stFail
	}
	return stUnknown
}

// hubCheck adds hub and, while the node is on probation, hub.probation.
func hubCheck(add func(id, status, detail, hint string), say func(id, status, key string, args a2ashape.Args),
	rep *doctorReport, env doctorEnv, layout daemon.Layout, lang a2ashape.Language) {
	var f hubFinding
	live := false
	if rep.Control.Running && env.status != nil {
		if st, err := env.status(rep.Control.Addr, layout.ControlTokenPath()); err == nil {
			if h, _ := st["hub"].(map[string]any); h != nil {
				str := func(k string) string { s, _ := h[k].(string); return s }
				f = hubFinding{str("url"), str("state"), str("reason"), str("message"), str("next")}
				live = f.State != ""
			}
		}
	}
	if !live {
		v, err := daemon.ReadHubView(layout, lang)
		if err != nil {
			add("hub", stUnknown, "hub_join.json cannot be read: "+err.Error(), noHint)
			return
		}
		f = hubFinding{v.URL, v.State, v.Reason, v.Message, v.Next}
	}
	rep.Hub.URL = f.URL
	rep.Hub.Registered = f.State == daemon.JoinRegistered
	if rep.Hub.Registered {
		// registered and registered.probation are both a registered node: the second is its own item.
		say("hub", stOK, "registered", a2ashape.Args{"hub": f.URL})
		if f.Reason == "registered.probation" {
			rep.Checks = append(rep.Checks, doctorCheck{ID: "hub.probation", Status: stInfo, Reason: f.Reason, Detail: f.Message, Hint: noHint})
		}
		return
	}
	c := doctorCheck{ID: "hub", Status: hubLevel(f), Reason: f.Reason, Detail: f.Message, Hint: f.Next}
	if c.Hint == "" && c.Status != stOK {
		c.Hint = noHint
	}
	rep.Checks = append(rep.Checks, c)
}

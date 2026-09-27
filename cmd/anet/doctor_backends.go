package main

// doctor_backends.go is doctor's reading of the provider-side A2A backends
// (modules.a2a.backends, A2A-DESIGN §11.6): who they take work from, and
// the two combinations the daemon refuses to start with, which the policy
// read (daemon.ReadPolicy) cannot see because they live in a module's
// block.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// a2aBackend is one entry of modules.a2a.backends as doctor reads it,
// without depending on the a2a module's types.
type a2aBackend struct {
	Match           string `json:"match"`
	URL             string `json:"url"`
	AcceptUntrusted bool   `json:"accept_untrusted"`
	Toolless        bool   `json:"toolless"`
}

// a2aBackends reads modules.a2a.backends; nil when there are none or the
// block does not parse (the daemon reports that itself).
func a2aBackends(cfg daemon.Config) []a2aBackend {
	raw, ok := cfg.Modules["a2a"]
	if !ok {
		return nil
	}
	var m struct {
		Backends []a2aBackend `json:"backends"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m.Backends
}

// a2aBackendChecks adds the a2a.backends checks: a fail for what the daemon
// refuses to start with, a warning for every backend that serves peers not
// on the trust list, and what the trusted-only ones will receive.
func a2aBackendChecks(add func(id, status, detail, hint string), policy string, cfg daemon.Config, trusted int) {
	bs := a2aBackends(cfg)
	if len(bs) == 0 {
		return
	}
	var trustedOnly []string
	for _, b := range bs {
		switch {
		case b.AcceptUntrusted && !b.Toolless:
			add("a2a.backends", stFail, "A2A backend "+b.URL+" has accept_untrusted without toolless: true; the daemon "+
				"refuses to start, because a backend that serves peers nobody vouched for must not be able to act on this machine",
				"set toolless: true only if the agent behind it has no tools, shell or file access; otherwise remove accept_untrusted")
		case b.AcceptUntrusted && policy == daemon.PolicyOpen:
			add("a2a.backends", stFail, "A2A backend "+b.URL+" has accept_untrusted and inbound.policy is open: anyone's "+
				"task would reach it, and the daemon refuses to start", "change one of the two (anet inbound policy approve|closed)")
		case b.AcceptUntrusted:
			add("a2a.backends", stWarn, "A2A backend "+b.URL+" takes tasks from peers not on the trust list "+
				"(accept_untrusted, toolless): allowed and approved peers reach the agent behind it",
				"keep that agent toolless and on a profile of its own; see A2A-DESIGN §11.6")
		default:
			trustedOnly = append(trustedOnly, b.URL)
		}
	}
	if len(trustedOnly) == 0 {
		return
	}
	list := strings.Join(trustedOnly, ", ")
	if trusted == 0 {
		add("a2a.backends", stInfo, "A2A backend "+list+" answers only peers on the trust list, and it is empty: "+
			"nothing is forwarded", "anet peers trust <aid>")
		return
	}
	add("a2a.backends", stOK, fmt.Sprintf("A2A backend %s answers text tasks from the %d peer(s) on the trust list; "+
		"the auto-reply agent does not see those tasks", list, trusted), "")
}

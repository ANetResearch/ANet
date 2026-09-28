package main

// doctor_backends.go is doctor's reading of the provider-side A2A backends
// (modules.a2a.backends, A2A-DESIGN §11.6): who they take work from, and
// the two combinations the daemon refuses to start with, which the policy
// read (daemon.ReadPolicy) cannot see because they live in a module's
// block.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/backendconn"
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
// on the trust list, and what the trusted-only ones will receive. Only a
// match "*" backend receives anything: a task the daemon hands a backend is
// a text task, and a text task names no skill.
func a2aBackendChecks(add func(id, status, detail, hint string), policy string, cfg daemon.Config, trusted int) {
	bs := a2aBackends(cfg)
	if len(bs) == 0 {
		return
	}
	var trustedOnly []string
	star := false
	for _, b := range bs {
		if strings.TrimSpace(b.Match) == "*" {
			star = true
		}
	}
	if !star {
		add("a2a.backends", stInfo, "no A2A backend has match \"*\": text tasks name no skill, so none is forwarded "+
			"and they are answered as without backends", "set match \"*\" on the backend that should answer text tasks")
	} else {
		a2aRetryCheck(add, cfg)
	}
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
		case strings.TrimSpace(b.Match) == "*":
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

// backendTransportChecks adds how the daemon reaches the services behind
// modules.service and the A2A backends (docs/notes/0030 N1,
// internal/backendconn): a URL the daemon refuses to start with is a fail;
// a TCP backend (allow_tcp) a warning, since a Unix socket is the form whose
// far side can be checked; a socket whose path the daemon would refuse to
// connect through a fail, and one that is not there (the backend is down) a
// warning. The path is judged as this process's user sees it, which is the
// daemon's when doctor runs as the node's user.
func backendTransportChecks(add func(id, status, detail, hint string), cfg daemon.Config) {
	type group struct {
		pol  backendconn.Policy
		raw  string // one URL of the group
		what []string
	}
	var order []string
	groups := map[string]*group{}
	put := func(key string, pol backendconn.Policy, raw, what string) {
		g, ok := groups[key]
		if !ok {
			g = &group{pol: pol, raw: raw}
			groups[key] = g
			order = append(order, key)
		}
		g.what = append(g.what, what)
	}
	keyOf := func(raw string) string {
		t, err := backendconn.Parse(raw)
		switch {
		case err != nil:
			return "bad:" + raw
		case t.Unix():
			return "unix:" + t.Socket
		default:
			return "tcp:" + t.URL.Scheme + "://" + t.URL.Host
		}
	}
	if raw, ok := cfg.Modules["service"]; ok {
		var m struct {
			backendconn.Policy
			Capabilities []struct {
				ID  string `json:"id"`
				URL string `json:"url"`
			} `json:"capabilities"`
		}
		if json.Unmarshal(raw, &m) == nil {
			for _, c := range m.Capabilities {
				put("service "+keyOf(c.URL), m.Policy, c.URL, "service capability "+c.ID)
			}
		}
	}
	if raw, ok := cfg.Modules["a2a"]; ok {
		var m struct {
			Backends []struct {
				backendconn.Policy
				Match string `json:"match"`
				URL   string `json:"url"`
			} `json:"backends"`
		}
		if json.Unmarshal(raw, &m) == nil {
			for i, b := range m.Backends {
				put(fmt.Sprintf("a2a %d", i), b.Policy, b.URL, fmt.Sprintf("A2A backend (match %q)", strings.TrimSpace(b.Match)))
			}
		}
	}
	const id = "backend.transport"
	for _, k := range order {
		g := groups[k]
		sort.Strings(g.what)
		who := strings.Join(g.what, ", ")
		rules, err := g.pol.Resolve()
		if err != nil {
			add(id, stFail, who+": "+err.Error()+"; the daemon refuses to start", "")
			continue
		}
		t, err := backendconn.Parse(g.raw)
		if err != nil {
			add(id, stFail, who+": "+err.Error()+"; the daemon refuses to start", "write unix:///path/to/socket[:/request/path]")
			continue
		}
		if err := rules.Admit(t); err != nil {
			add(id, stFail, who+": "+err.Error()+"; the daemon refuses to start",
				"serve it on a Unix socket (unix:///path/to/socket), or set allow_tcp: true")
			continue
		}
		if !t.Unix() {
			add(id, stWarn, who+": reached over TCP at "+t.URL.Host+" (allow_tcp). The daemon sends nothing to a loopback "+
				"listener another user holds, which on a system without a socket table (all but Linux) is every loopback listener; "+
				"while the backend is down another local user can take its port",
				"serve it on a Unix socket in a directory only its user can write: unix:///path/to/socket")
			continue
		}
		real, owner, err := rules.CheckPath(t.Socket)
		switch {
		case errors.Is(err, backendconn.ErrRefused):
			add(id, stFail, who+": "+err.Error()+"; every call is refused",
				"the socket's directory, and those above it, must be writable only by root, this user or the backend's user (or socket_group)")
		case err != nil:
			add(id, stWarn, who+": "+err.Error(), "start the backend")
		default:
			add(id, stOK, fmt.Sprintf("%s: socket %s (owner uid %d) passes the path checks; the listener is checked on each connection", who, real, owner), "")
		}
	}
}

// a2aRetryCheck reports modules.a2a.retry (A2A-DESIGN §11.6): how long a
// forward that failed for a reason a retry may fix is tried again. Read
// here rather than through module/a2a, which a no_a2a build does not have;
// the defaults are the module's.
func a2aRetryCheck(add func(id, status, detail, hint string), cfg daemon.Config) {
	var m struct {
		Retry struct {
			MaxInterval string `json:"max_interval"`
			GiveUpAfter string `json:"give_up_after"`
		} `json:"retry"`
	}
	_ = json.Unmarshal(cfg.Modules["a2a"], &m)
	read := func(key, v string, def time.Duration) (time.Duration, bool) {
		if strings.TrimSpace(v) == "" {
			return def, true
		}
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil || d < 0 {
			add("a2a.backends.retry", stFail, fmt.Sprintf("modules.a2a.retry.%s %q is not a duration; the daemon refuses to start", key, v),
				`write a Go duration such as "2m"`)
			return 0, false
		}
		return d, true
	}
	maxInterval, ok1 := read("max_interval", m.Retry.MaxInterval, 2*time.Minute)
	giveUp, ok2 := read("give_up_after", m.Retry.GiveUpAfter, 10*time.Minute)
	if !ok1 || !ok2 {
		return
	}
	if giveUp == 0 {
		add("a2a.backends.retry", stInfo, "a forward the backend could not take is not tried again (retry.give_up_after 0): "+
			"the task stays in the inbox until the daemon restarts or the requester writes again", "")
		return
	}
	detail := fmt.Sprintf("a forward that fails because the backend is not reachable or answers 5xx is tried again for up to %s, "+
		"waiting at most %s between attempts (modules.a2a.retry)", giveUp, max(maxInterval, 5*time.Second))
	if giveUp >= 15*time.Minute {
		add("a2a.backends.retry", stInfo, detail+"; requesters fail a task they hear nothing about for 15m by default "+
			"(no_response_after), so later attempts may answer tasks they have given up on", "")
		return
	}
	add("a2a.backends.retry", stOK, detail, "")
}

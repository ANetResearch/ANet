//go:build !no_a2a

package a2a

// backend.go is the provider side (A2A-DESIGN §11.6): text tasks delegated
// to this node, handed to a local A2A server — an agent that already speaks
// A2A answering work that arrives over the network.
//
//	"modules": {"a2a": {"backends": [{
//	    "match": "*", "url": "http://127.0.0.1:9900",
//	    "token_file": "/path/to/token", "accept_untrusted": false, "toolless": false}]}}
//
// The rules, all of them about who may reach a local agent:
//
//   - Only text tasks the kernel has accepted, and only from peers on the
//     trust list, are forwarded; everything else stays in the inbox for
//     the operator (MCP reply_task, the CLI).
//   - accept_untrusted: true forwards work from peers that are not trusted
//     as well, and is accepted only together with toolless: true — the
//     operator's statement that the backend cannot act on the machine (no
//     tools, no shell, no files). The module declares such a backend to
//     the kernel (Host.DeclareUntrustedBackend), whose configuration check
//     refuses it together with inbound.policy=open (§5.1).
//   - What is forwarded carries anet.peer_aid, anet.trusted and the
//     a2a.serviceParameters the requester sent, restored as headers. All
//     requests use one token, so a backend cannot tell peers apart by its
//     credential and must read anet.peer_aid.
//   - Each forwarded task is recorded: anet.backend.forwarded{backend,
//     interaction_id, peer_aid, trusted}.
//
// TODO(A2A-DESIGN §11.6): the forwarding itself. It needs a kernel seam this
// work package does not have: TaskSeam reaches only this node's OUTBOUND
// tasks (by design, §11.2), and forwarding needs the inbound side — a
// stream of accepted inbound text tasks with the peer's trust, and a way to
// answer one (the /tasks/reply path, including completion). The shape that
// fits the existing seams is an optional Host extension, type-asserted like
// module.ProxyCardSigner:
//
//	type InboundTasks interface {
//	    // WatchInbound delivers accepted inbound text tasks and their
//	    // follow-up messages, with anet.peer_aid and anet.trusted set;
//	    // untrusted peers only when the module declared an untrusted
//	    // backend.
//	    WatchInbound(ctx context.Context) (<-chan module.Task, error)
//	    // Reply answers one: a message, input-required, or completion.
//	    Reply(ctx context.Context, taskID string, msg a2ashape.Message, state a2ashape.TaskState) (module.Task, error)
//	}
//
// With it, Start runs one loop per backend: a2aclient (JSON-RPC, bearer
// from token_file) SendMessage with the task's history, the reply mapped
// back through Reply, the evidence event written with RecordEvidence.
// Until then a configured backend is validated and declared, and logged as
// not yet forwarding.

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
)

// Backend is one provider-side A2A backend.
type Backend struct {
	// Match is "*" (every accepted text task) or the skill id a task names.
	Match string `json:"match"`
	// URL is the backend's A2A base URL: http on loopback, or https.
	URL string `json:"url"`
	// TokenFile holds the bearer token the daemon presents to the backend.
	TokenFile string `json:"token_file,omitempty"`
	// AcceptUntrusted forwards tasks from peers not on the trust list; it
	// needs Toolless.
	AcceptUntrusted bool `json:"accept_untrusted,omitempty"`
	// Toolless is the operator's statement that the backend has no tools:
	// it cannot run commands, read files or reach the network on a task's
	// behalf.
	Toolless bool `json:"toolless,omitempty"`
}

func (c Config) validate() error {
	seen := map[string]bool{}
	for i, b := range c.Backends {
		where := fmt.Sprintf("a2a: backends[%d]", i)
		m := strings.TrimSpace(b.Match)
		if m == "" {
			return errors.New(where + ": match is required (\"*\" or a skill id)")
		}
		if seen[m] {
			return fmt.Errorf("%s: a second backend for match %q", where, m)
		}
		seen[m] = true
		if err := checkBackendURL(b.URL); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if b.AcceptUntrusted && !b.Toolless {
			return errors.New(where + ": accept_untrusted needs toolless: true — a backend that serves peers " +
				"nobody vouched for must not be able to act on this machine")
		}
	}
	return nil
}

// declaresUntrusted reports a backend that takes work from untrusted peers.
func (c Config) declaresUntrusted() bool {
	for _, b := range c.Backends {
		if b.AcceptUntrusted {
			return true
		}
	}
	return false
}

// logBackends says what the configured backends will do in this build.
func (c Config) logBackends() {
	for _, b := range c.Backends {
		who := "trusted peers"
		if b.AcceptUntrusted {
			who = "any admitted peer (toolless; inbound.policy=open is refused with it)"
		}
		log.Printf("anet: a2a: backend %s (match %s, %s) is configured but not forwarding yet: "+
			"accepted tasks stay in the inbox", b.URL, b.Match, who)
	}
}

// checkBackendURL accepts http on a loopback host, or https: a task's text
// goes there, and in the clear only on this machine.
func checkBackendURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("url %q is not an http(s) URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if ip := net.ParseIP(h); (ip != nil && ip.IsLoopback()) || strings.EqualFold(h, "localhost") {
			return nil
		}
		return fmt.Errorf("url %q: plain http only to a loopback host; use https", raw)
	}
	return fmt.Errorf("url %q is not an http(s) URL", raw)
}

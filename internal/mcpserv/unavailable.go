//go:build !no_mcp

package mcpserv

// unavailable.go is the server without a node: `anet mcp` serves even when the
// node of its identity could not be started (no node set up yet, a
// configuration the daemon refuses, a daemon that does not answer), so that
// the host of a coding agent lists the tools and the agent can find out what
// is wrong instead of seeing no anet at all. Every tool is listed; `guide`
// works as it always does (local text), `node_status` reports the reason and
// the command that fixes it, and every other tool answers with the same reason
// as a tool error. The Control that `anet mcp` passes in keeps looking for the
// daemon at each call, so that the tools work as soon as the node runs,
// without a new session.

import (
	"context"
	"errors"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// NodeUnavailable is why the server has no node to forward a call to. A
// Control returns it from Call while the node is unavailable.
type NodeUnavailable struct {
	// Reason is the reason code (daemon_not_running, daemon_not_answering,
	// config_invalid, ...).
	Reason string
	// Message is the sentence of the reason table, in English (text handed
	// to a model is English, a2ashape.Lang).
	Message string
	// Next is the command that fixes it, as the operator would type it.
	Next string
	// Detail is what happened when the node was started, when it was: the
	// last lines of its log, or how long it was waited for. Empty otherwise.
	Detail string
}

// Error is the sentence with its reason code in brackets, the command on a
// line of its own (as the command line prints a refusal), then the detail.
func (u *NodeUnavailable) Error() string {
	var b strings.Builder
	b.WriteString(u.Message)
	b.WriteString(a2ashape.CodeSuffix(u.Reason, ""))
	if u.Next != "" {
		b.WriteString("\n→ ")
		b.WriteString(u.Next)
	}
	if u.Detail != "" {
		b.WriteString("\n")
		b.WriteString(u.Detail)
	}
	return b.String()
}

// status is the answer of node_status while the node is unavailable: the same
// facts as the error of the other tools, as named members, so that the agent
// reports the state of the node rather than a failed call.
func (u *NodeUnavailable) status() map[string]any {
	out := map[string]any{
		"running": false,
		"reason":  u.Reason,
		"message": u.Message,
		"next":    u.Next,
		"summary": "The anet node of this identity is not running: " + u.Message + a2ashape.CodeSuffix(u.Reason, ""),
		"setup":   "Run the command in next (through the plugin runner when one is given), then call node_status again; no new session is needed. The guide topic setup describes the whole setup.",
	}
	if u.Detail != "" {
		out["detail"] = u.Detail
	}
	return out
}

// unavailableOf is the NodeUnavailable in err's chain, or nil.
func unavailableOf(err error) *NodeUnavailable {
	var u *NodeUnavailable
	if errors.As(err, &u) {
		return u
	}
	return nil
}

// Options are the settings of NewWith.
type Options struct {
	// Unavailable, when set, is why no node answered when the server
	// started. The instructions then begin with it. The Control is still
	// used for every call: it answers with a NodeUnavailable until the node
	// runs.
	Unavailable *NodeUnavailable
}

// unavailableInstructions is the start of the instructions of a server that
// started without its node.
func unavailableInstructions(u *NodeUnavailable) string {
	return "The anet node of this identity is not running, so the anet tools cannot act yet: " +
		u.Message + a2ashape.CodeSuffix(u.Reason, "") + " The command that fixes it: " + u.Next + ". " +
		"Until the node runs, node_status reports this reason and the command, every other tool except " +
		"guide answers with the same reason, and guide works as usual. Run the command (through the " +
		"plugin runner when one is given) and call node_status again; the tools then work in this " +
		"session. "
}

// nodeStatusAnswer is node_status: the daemon's /status, or the reason the
// node is unavailable.
func nodeStatusAnswer(ctx context.Context, c Control) (any, error) {
	_, out, err := forward(ctx, c, "/status", map[string]any{})
	if u := unavailableOf(err); u != nil {
		return u.status(), nil
	}
	return out, err
}

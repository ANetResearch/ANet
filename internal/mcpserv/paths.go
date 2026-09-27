//go:build !no_mcp

package mcpserv

import (
	"context"
	"fmt"
)

// allowedPaths is every control-plane route the MCP surface may call.
//
// The control token is the node's full authority: a process holding it can
// call any route (A2A-DESIGN §21 item 13). The MCP server holds it on
// behalf of a model, so it confines itself. In particular it never calls
// the three payment routes that are not the agent tier (§8.6):
//
//	/tasks/pay-manual            task-manual: only `anet pay`, after a TTY confirmation
//	/x402-authorize, /delegate   gateway: an authorization handed out, or pay:true
//	/redeem                      redeem: credit taken out of the hub
//
// A payment an agent makes goes through /tasks/pay, whose route fixes the
// purpose at task-agent and so the agent limits. Everything else here reads,
// or sends a task message that spends nothing.
var allowedPaths = map[string]bool{
	"/agents/list":     true,
	"/agents/card":     true,
	"/tasks/send":      true,
	"/tasks/get":       true,
	"/tasks/list":      true,
	"/tasks/wait":      true,
	"/tasks/cancel":    true,
	"/tasks/reply":     true,
	"/tasks/pay":       true,
	"/balance":         true,
	"/evidence":        true,
	"/status":          true,
	"/inbound/pending": true,
}

// guarded is the Control every tool gets: calls outside allowedPaths are
// refused here, before they reach the daemon.
type guarded struct{ c Control }

func (g guarded) Call(ctx context.Context, path string, body, out any) error {
	if !allowedPaths[path] {
		return fmt.Errorf("anet mcp: %s is not a route this surface may call", path)
	}
	return g.c.Call(ctx, path, body, out)
}

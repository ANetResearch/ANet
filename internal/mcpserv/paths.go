//go:build !no_mcp

package mcpserv

import (
	"context"
	"encoding/json"
	"fmt"
)

// allowedPaths is every control-plane route the MCP surface may call.
//
// The control token is the node's full authority: a process holding it can
// call any route (A2A-DESIGN §21 item 13). The MCP server holds it on
// behalf of a model, so it confines itself. In particular it never calls
// the payment routes that are not the agent tier (§8.6):
//
//	/tasks/pay-manual            task-manual: only `anet pay`, after a TTY confirmation
//	/redeem                      redeem: credit taken out of the hub
//
// A payment an agent makes goes through /tasks/pay, whose route fixes the
// purpose at task-agent and so the agent limits. Everything else here reads,
// or sends a task message that spends nothing.
//
// Each route has the check its request body must pass (CUX-S6): anyBody for
// the routes whose daemon-side handler reads the body, emptyBody for
// /payments/status, which the daemon also answers with the hub's identity
// when asked with {"hub": true}, a call this surface has no use for.
var allowedPaths = map[string]bodyCheck{
	"/agents/list":     anyBody,
	"/agents/card":     anyBody,
	"/tasks/send":      anyBody,
	"/tasks/get":       anyBody,
	"/tasks/list":      anyBody,
	"/tasks/wait":      anyBody,
	"/tasks/cancel":    anyBody,
	"/tasks/reply":     anyBody,
	"/tasks/pay":       anyBody,
	"/balance":         anyBody,
	"/payments/status": emptyBody,
	"/evidence":        anyBody,
	"/status":          anyBody,
	"/inbound/pending": anyBody,
}

// guarded is the Control every tool gets: calls outside allowedPaths are
// refused here, before they reach the daemon.
type guarded struct{ c Control }

// bodyCheck judges a request body before it is sent.
type bodyCheck func(body any) error

func anyBody(any) error { return nil }

// emptyBody accepts the JSON object {} and nothing else.
func emptyBody(body any) error {
	b, err := json.Marshal(body)
	if err != nil || string(b) != "{}" {
		return fmt.Errorf("anet mcp: this route takes an empty request body")
	}
	return nil
}

func (g guarded) Call(ctx context.Context, path string, body, out any) error {
	check, ok := allowedPaths[path]
	if !ok {
		return fmt.Errorf("anet mcp: %s is not a route this surface may call", path)
	}
	if err := check(body); err != nil {
		return err
	}
	return g.c.Call(ctx, path, body, out)
}

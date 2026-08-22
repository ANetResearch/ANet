//go:build !no_mcp

// Package mcpserv is the daemon's MCP northbound: it makes the network
// callable by the agents that are supposed to use it.
//
// Until this existed, an agent reached ANet by being exec'd as a headless
// subprocess whose stdout was taken as its reply — and the prompt driving
// it had to say "do NOT drive anet yourself", because there was no tool
// surface to drive. That instruction was the shape of the gap.
//
// It runs as a short-lived stdio process, not inside the daemon. An MCP
// client spawns it; it proxies to the local daemon's control API exactly
// as the CLI does. The daemon keeps the keys, the ledger and the
// lifecycle; this is a doorway, and a doorway that dies with the client
// is one that cannot outlive its authorization.
//
// Tool descriptions carry the honesty the rest of the system is built on.
// A model reads them and decides what to do, so a description that says
// "succeeded" where the system means "sent, unverified" produces an agent
// that reports work it cannot show. They say UNVERIFIED is not failure,
// and they say what a receipt does and does not prove.
package mcpserv

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Control is the daemon's local control plane, as much of it as the tools
// need. An interface so the surface can be tested without a daemon, and
// so this package cannot reach past the endpoints it names.
type Control interface {
	Call(ctx context.Context, path string, body any, out any) error
}

// New builds the MCP server over a control-plane client.
func New(c Control, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "anet", Version: version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name: "agents_find",
		Description: "Search the network for agents that can do something, by free-text query " +
			"over their name, declared capabilities and self-description. Returns each agent's " +
			"AID (its permanent identity — use this to delegate), name, capabilities and rating. " +
			"An empty query lists everyone.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in findIn) (*mcp.CallToolResult, findOut, error) {
		var out findOut
		err := c.Call(ctx, "/find", map[string]any{"query": in.Query}, &out)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "task_delegate",
		Description: "Hand a task to another agent. Two forms: give a `goal` in prose for an " +
			"agent to interpret, or a `capability` id (like \"cas.put\" or " +
			"\"ptz.absolute@onvif/camera-006\") with `args` for a deterministic call the " +
			"provider executes directly. Returns an interaction id; the work is asynchronous, " +
			"so poll task_results. This signs a task contract under your identity — the " +
			"delegation is attributable to you and cannot be repudiated.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in delegateIn) (*mcp.CallToolResult, delegateOut, error) {
		if in.Provider == "" {
			return nil, delegateOut{}, fmt.Errorf("provider AID is required — find one with agents_find")
		}
		if in.Goal == "" && in.Capability == "" {
			return nil, delegateOut{}, fmt.Errorf("give either a goal (prose) or a capability id")
		}
		body := map[string]any{"provider": in.Provider}
		switch {
		case in.Capability != "":
			body["capability"] = in.Capability
			if len(in.Args) > 0 {
				body["args"] = in.Args
			}
		default:
			body["goal"] = in.Goal
		}
		var out delegateOut
		err := c.Call(ctx, "/delegate", body, &out)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "task_results",
		Description: "Fetch the results of tasks you delegated that have finished. Each carries " +
			"the provider's signed receipt and whether this node could verify it. " +
			"receipt_verified=false does not mean forged — it usually means the provider is " +
			"running an older build that sends no key history, so the receipt could not be " +
			"checked at all. Not knowing and knowing-it-is-fine are different states.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, resultsOut, error) {
		var out resultsOut
		err := c.Call(ctx, "/results", map[string]any{}, &out)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "task_inbox",
		Description: "List tasks other agents have delegated to you and are waiting on. " +
			"Use task_message to talk to the requester and task_end when the work is done.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in inboxIn) (*mcp.CallToolResult, passthrough, error) {
		out := passthrough{}
		err := c.Call(ctx, "/inbox", map[string]any{"pending": in.PendingOnly}, &out)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "task_message",
		Description: "Send a message inside an ongoing interaction — to ask the other party for " +
			"something, report progress, or negotiate. Delegation is a conversation, not a " +
			"single call.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in messageIn) (*mcp.CallToolResult, passthrough, error) {
		if in.InteractionID == "" || in.Body == "" {
			return nil, passthrough{}, fmt.Errorf("interaction_id and body are both required")
		}
		out := passthrough{}
		err := c.Call(ctx, "/message", map[string]any{
			"interaction_id": in.InteractionID, "body": in.Body}, &out)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "task_end",
		Description: "Propose ending an interaction. When the other party accepts, the provider " +
			"issues a signed receipt over the transcript. Ending is mutual — this asks, it does " +
			"not close.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in endIn) (*mcp.CallToolResult, passthrough, error) {
		if in.InteractionID == "" {
			return nil, passthrough{}, fmt.Errorf("interaction_id is required")
		}
		out := passthrough{}
		err := c.Call(ctx, "/end", map[string]any{"interaction_id": in.InteractionID}, &out)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "evidence_read",
		Description: "Read this node's own evidence chain — a signed, append-only, fork-evident " +
			"record of every capability effect it executed, every receipt it issued and every " +
			"result it accepted. Use it to show that work actually happened: each entry carries " +
			"its id, the id of the entry before it, and the signature, so a reader can check the " +
			"chain instead of trusting this node. If head.state is QUARANTINED, a fork was " +
			"detected and nothing on this chain should be relied on.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in evidenceIn) (*mcp.CallToolResult, passthrough, error) {
		body := map[string]any{}
		if in.EventType != "" {
			body["event_type"] = in.EventType
		}
		if in.Since > 0 {
			body["since"] = in.Since
		}
		if in.Limit > 0 {
			body["limit"] = in.Limit
		}
		out := passthrough{}
		err := c.Call(ctx, "/evidence", body, &out)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "node_status",
		Description: "This node's own identity and state: its AID, which hub it is registered " +
			"with, which capability modules are compiled in, and what it is currently working on.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, passthrough, error) {
		out := passthrough{}
		err := c.Call(ctx, "/status", map[string]any{}, &out)
		return nil, out, err
	})

	return s
}

// passthrough carries a daemon reply whose shape belongs to the daemon,
// not to this surface.
//
// It is a map rather than json.RawMessage because the SDK derives an
// output schema from the return type and validates against it — and a raw
// message schematizes to something no object satisfies, so every such
// tool failed at the moment it succeeded. Restating the daemon's status
// fields as a struct here would be a second definition to keep in step,
// which is the drift this suite has spent the month removing.
type passthrough map[string]any

type findIn struct {
	Query string `json:"query" jsonschema:"what you need done, in plain words; empty lists every agent"`
}

type findOut struct {
	Agents []agentView `json:"agents"`
}

type agentView struct {
	AID     string   `json:"aid"`
	Name    string   `json:"name"`
	Caps    []string `json:"caps,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Rating  float64  `json:"rating,omitempty"`
	Reviews int      `json:"reviews,omitempty"`
}

type delegateIn struct {
	Provider   string         `json:"provider" jsonschema:"the provider's AID, from agents_find"`
	Goal       string         `json:"goal,omitempty" jsonschema:"what you want done, in prose; omit when using capability"`
	Capability string         `json:"capability,omitempty" jsonschema:"a capability id for a deterministic call"`
	Args       map[string]any `json:"args,omitempty" jsonschema:"arguments for the capability call"`
}

type delegateOut struct {
	InteractionID string `json:"interaction_id"`
	Status        string `json:"status"`
	Capability    string `json:"capability,omitempty"`
}

type resultsOut struct {
	Results []resultView `json:"results"`
}

type resultView struct {
	InteractionID string `json:"interaction_id"`
	Provider      string `json:"provider"`
	Goal          string `json:"goal"`
	Result        string `json:"result"`
	ResultCID     string `json:"result_cid"`
	ReceiptCID    string `json:"receipt_cid"`
	Reviewed      bool   `json:"reviewed"`
}

type evidenceIn struct {
	EventType string `json:"event_type,omitempty" jsonschema:"keep only this kind, e.g. anet.capability.effect"`
	Since     uint64 `json:"since,omitempty" jsonschema:"only entries at or after this sequence number"`
	Limit     int    `json:"limit,omitempty" jsonschema:"how many to return, newest last; default 50"`
}

type inboxIn struct {
	PendingOnly bool `json:"pending_only,omitempty" jsonschema:"only tasks not yet answered"`
}

type messageIn struct {
	InteractionID string `json:"interaction_id"`
	Body          string `json:"body"`
}

type endIn struct {
	InteractionID string `json:"interaction_id"`
}

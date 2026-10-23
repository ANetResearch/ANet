//go:build !no_mcp

package main

// turnmcp_test.go — `anet turn-mcp` against a node's turn tools (internal/turntools): the run's tools are
// listed with their schemas, a call goes to the node with the run's token and its answer or refusal comes
// back, and without the run's variables the server serves no tools and says why. The node's side is a
// stand-in that answers as module/autoreply's turn tools do for one run (turn_tools.go; the real one is
// exercised end to end in module/autoreply runner_e2e_test.go).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANet/internal/turntools"
)

// fakeTurnNode answers the turn tools for the token tok, whose caller is caller, and records the calls
// as a ServiceCaller would see them.
type fakeTurnNode struct {
	tok, caller string
	mu          sync.Mutex
	calls       []string // caller capability args
}

func (f *fakeTurnNode) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+f.tok {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(turntools.CallResponse{Error: &turntools.CallError{Code: "turn_unauthorized", Message: "anet: no token"}})
			return
		}
		switch r.URL.Path {
		case turntools.RoutePrefix + turntools.ToolsPath:
			_ = json.NewEncoder(w).Encode(turntools.ToolsResponse{Tools: []turntools.Tool{
				{Name: "board_post", Capability: "board.post", Description: "Post a request for help.",
					InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`)},
				{Name: "org_me", Capability: "org.me", InputSchema: json.RawMessage(`{"type":"object"}`)},
			}})
		case turntools.RoutePrefix + turntools.CallPath:
			var req turntools.CallRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.calls = append(f.calls, f.caller+" "+req.Capability+" "+string(req.Args))
			f.mu.Unlock()
			if req.Capability == "org.me" {
				_ = json.NewEncoder(w).Encode(turntools.CallResponse{Error: &turntools.CallError{Code: "not_member", Message: "the caller is not a member"}})
				return
			}
			_ = json.NewEncoder(w).Encode(turntools.CallResponse{Result: json.RawMessage(`{"id":"item-7"}`)})
		default:
			http.NotFound(w, r)
		}
	})
}

func connectTurnMCP(t *testing.T, env map[string]string) *mcp.ClientSession {
	t.Helper()
	srv := newTurnMCPServer(context.Background(), func(k string) string { return env[k] })
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "test-agent"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestTurnMCPListsAndCallsTheRunsTools(t *testing.T) {
	node := &fakeTurnNode{tok: "ttk-test", caller: "aidMember"}
	hs := httptest.NewServer(node.handler())
	defer hs.Close()
	sess := connectTurnMCP(t, map[string]string{turntools.EnvURL: hs.URL + turntools.RoutePrefix, turntools.EnvToken: node.tok})
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 2 || res.Tools[0].Name != "board_post" || res.Tools[0].Description != "Post a request for help." {
		t.Fatalf("tools %+v", res.Tools)
	}
	schema, _ := json.Marshal(res.Tools[0].InputSchema)
	if !strings.Contains(string(schema), `"required":["title"]`) {
		t.Errorf("the schema was not passed on: %s", schema)
	}
	out, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "board_post", Arguments: map[string]any{"title": "help"}})
	if err != nil || out.IsError || toolText(out) != `{"id":"item-7"}` {
		t.Fatalf("call: %v %+v", err, out)
	}
	out, err = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "org_me", Arguments: map[string]any{}})
	if err != nil || !out.IsError || !strings.Contains(toolText(out), "the caller is not a member [not_member]") {
		t.Fatalf("a refusal: %v %+v", err, out)
	}
	if got := strings.Join(node.calls, "\n"); got != `aidMember board.post {"title":"help"}`+"\n"+`aidMember org.me {}` {
		t.Errorf("calls the node saw:\n%s", got)
	}
}

// Without the run's variables — or with the literal ${…} an agent's configuration passes through for a
// variable that is not set — the server starts, serves no tools, and its instructions say why.
func TestTurnMCPWithoutARunServesNoTools(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unset":      {},
		"unexpanded": {turntools.EnvURL: "${ANET_TURN_URL}", turntools.EnvToken: "${ANET_TURN_TOKEN}"},
		"not local":  {turntools.EnvURL: "http://example.org/anet-turn/v1", turntools.EnvToken: "t"},
	} {
		sess := connectTurnMCP(t, env)
		res, err := sess.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Tools) != 0 {
			t.Errorf("%s: %d tools", name, len(res.Tools))
		}
		if ins := sess.InitializeResult().Instructions; !strings.Contains(ins, "no turn tools") {
			t.Errorf("%s: instructions %q", name, ins)
		}
	}
	// A token the node does not know (the run ended): no tools, and the node's sentence.
	node := &fakeTurnNode{tok: "ttk-live"}
	hs := httptest.NewServer(node.handler())
	defer hs.Close()
	sess := connectTurnMCP(t, map[string]string{turntools.EnvURL: hs.URL + turntools.RoutePrefix, turntools.EnvToken: "ttk-ended"})
	if ins := sess.InitializeResult().Instructions; !strings.Contains(ins, "[turn_unauthorized]") {
		t.Errorf("an ended run: instructions %q", ins)
	}
}

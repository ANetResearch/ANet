//go:build !no_mcp

package mcpserv

// ux_contract_test.go is UX-T8 (ux §6.1): the MCP surface is the 14 tools of
// §4.4, none of them named for what an agent must not do (UX-M2), the routes
// they may call are the registered ones (UX-M1), and a tool error is the
// control plane's error and nothing more (UX-M4).

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolNamesAreTheFourteenOfUX44(t *testing.T) {
	got := listTools(t, connect(t, &fakeControl{}))
	var names []string
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	want := append([]string(nil), theTools...)
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools %v, want %v", names, want)
	}
	if len(names) != 14 {
		t.Fatalf("%d tools, want 14", len(names))
	}
	// UX-M2: no tool is named for identity, rotation, recovery, deactivation,
	// retention, previews, auto-reply, the hub or payees.
	for _, n := range names {
		for _, bad := range []string{"identity", "rotate", "recover", "deactivate", "retention", "preview", "autoreply", "hub", "payees"} {
			if strings.Contains(n, bad) {
				t.Errorf("tool %q contains %q (UX-M2)", n, bad)
			}
		}
	}
}

func TestAllowedPathsAreTheRegisteredOnes(t *testing.T) {
	want := []string{"/agents/list", "/agents/card", "/tasks/send", "/tasks/get", "/tasks/list", "/tasks/wait", "/tasks/cancel",
		"/tasks/reply", "/tasks/pay", "/balance", "/payments/status", "/evidence", "/status", "/inbound/pending"}
	sort.Strings(want)
	var got []string
	for p := range allowedPaths {
		got = append(got, p)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("allowedPaths %v, want %v", got, want)
	}
}

func TestPaymentsStatusTakesOnlyAnEmptyObject(t *testing.T) {
	g := guarded{&fakeControl{}}
	for _, ok := range []any{map[string]any{}, struct{}{}} {
		if err := g.Call(context.Background(), "/payments/status", ok, nil); err != nil {
			t.Errorf("{} refused: %v", err)
		}
	}
	for _, bad := range []any{map[string]any{"hub": true}, nil, []any{}, "x"} {
		if err := g.Call(context.Background(), "/payments/status", bad, nil); err == nil {
			t.Errorf("%#v accepted", bad)
		}
	}
}

// UX-M4: whatever the control plane's error says is what the tool says, with
// or without a reason or a code on it.
func TestToolErrorIsTheControlError(t *testing.T) {
	for _, e := range []*DaemonError{
		{Status: 409, Message: "A sentence. Next step.", Reason: "hub_pending_admission"},
		{Status: 404, Message: "No task t_1 here.", Code: "TaskNotFoundError", Reason: "no_local_task"},
		{Status: 400, Message: "plain error", Code: "InvalidParamsError", Reason: "not_a_table_code"},
	} {
		sess := connect(t, &fakeControl{err: e})
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "node_status"})
		msg, isErr := resultText(t, res, err)
		if !isErr || msg != e.Message {
			t.Errorf("tool error %q, want %q", msg, e.Message)
		}
	}
}

// node_status hands on /status as it is, summary included.
func TestNodeStatusPassesTheSummaryOn(t *testing.T) {
	const status = `{"aid":"a","summary":[{"topic":"hub","level":"ok","text":"Registered."},{"topic":"inbound","level":"ok","text":"Takes tasks only from the 0 peers on the allow list; 0 public capabilities; auto-reply off."}]}`
	sess := connect(t, &fakeControl{reply: map[string]string{"/status": status}})
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "node_status"})
	msg, isErr := resultText(t, res, err)
	if isErr || !strings.Contains(msg, `"summary":[{"topic":"hub","level":"ok","text":"Registered."}`) {
		t.Fatalf("node_status %q", msg)
	}
}

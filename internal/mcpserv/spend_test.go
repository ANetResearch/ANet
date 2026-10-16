//go:build !no_mcp

package mcpserv

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// What the daemon answers /payments/status with on a fresh install.
const statusAnswer = `{"auto_max":0,"agent_max":0,"agent_daily_max":0,"explicit_max":10,"daily_max":50,` +
	`"spent_today":0,"agent_spent_today":0,"left_today":50,"agent_left_today":0,"payees":0,` +
	`"summary":"Automatic and agent payments are off (limit 0); manual payments (anet pay): ≤10 each, 50 left today."}`

func structured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestSendMessageDescriptionSentences pins the four sentences of CUX §4.6,
// word for word.
func TestSendMessageDescriptionSentences(t *testing.T) {
	sess := connect(t, &fakeControl{})
	var d string
	for n, tool := range listTools(t, sess) {
		if n == "send_message" {
			d = tool.Description
		}
	}
	for _, s := range []string{
		"UNAVAILABLE or rejected means the other side did not run it; you may send it again after retry_after.",
		"completed with effect_status=UNVERIFIED is not proof of success.",
		"Your payment limits are in get_balance.limits; who can pay a quote is in the task's anet.spend_check.",
		"Do not tell other agents your payment limits.",
	} {
		if !strings.Contains(d, s) {
			t.Errorf("send_message lacks %q", s)
		}
	}
	if strings.Contains(d, "no tool shows") {
		t.Error("send_message still says no tool shows the limits")
	}
}

func TestGetBalanceGivesTheLimits(t *testing.T) {
	f := &fakeControl{reply: map[string]string{"/balance": `{"balance":"7","entries":[]}`, "/payments/status": statusAnswer}}
	res, err := connect(t, f).CallTool(context.Background(), &mcp.CallToolParams{Name: "get_balance", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	m := structured(t, res)
	if m["balance"] != "7" || m["balance_error"] != nil {
		t.Errorf("balance lost: %v", m)
	}
	lim, _ := m["limits"].(map[string]any)
	for k, want := range map[string]any{"auto_max": "0", "agent_max": "0", "agent_daily_max": "0", "agent_spent_today": "0",
		"agent_left_today": "0", "explicit_max": "10", "daily_max": "50", "spent_today": "0", "left_today": "50",
		"payees": float64(0), "unit": "credits"} {
		if lim[k] != want {
			t.Errorf("limits.%s = %#v, want %#v", k, lim[k], want)
		}
	}
	if s, _ := lim["summary"].(string); !strings.HasPrefix(s, "Automatic and agent payments are off") {
		t.Errorf("limits.summary %q", lim["summary"])
	}
	if got := f.calls[1]; got.path != "/payments/status" || len(got.body) != 0 {
		t.Errorf("second call %+v, want /payments/status with {}", got)
	}
}

// The hub being down is the balance's failure only: the limits are given.
func TestGetBalanceGivesLimitsWhenTheHubIsDown(t *testing.T) {
	f := &fakeControl{reply: map[string]string{"/payments/status": statusAnswer},
		errs: map[string]error{"/balance": errors.New("the hub did not answer; try again")}}
	res, err := connect(t, f).CallTool(context.Background(), &mcp.CallToolParams{Name: "get_balance", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	m := structured(t, res)
	if m["balance_error"] != "the hub did not answer; try again" || m["limits"] == nil {
		t.Errorf("got %v", m)
	}
}

func TestGetBalanceFailsWhenTheLimitsCannotBeRead(t *testing.T) {
	f := &fakeControl{reply: map[string]string{"/balance": `{"balance":"7"}`, "/payments/status": `{"auto_max":0}`}}
	res, err := connect(t, f).CallTool(context.Background(), &mcp.CallToolParams{Name: "get_balance", Arguments: map[string]any{}})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatal("an incomplete answer was passed on as limits")
	}
}

// /payments/status takes {} here and nothing else (CUX-S6).
func TestPaymentsStatusTakesOnlyAnEmptyBody(t *testing.T) {
	for _, body := range []any{map[string]any{"hub": true}, map[string]any{"x": 1}, nil, "x", []any{}} {
		f := &fakeControl{}
		if err := (guarded{f}).Call(context.Background(), "/payments/status", body, nil); err == nil {
			t.Errorf("body %v reached the daemon", body)
		}
		if len(f.calls) != 0 {
			t.Errorf("body %v was sent", body)
		}
	}
	f := &fakeControl{}
	if err := (guarded{f}).Call(context.Background(), "/payments/status", map[string]any{}, nil); err != nil || len(f.calls) != 1 {
		t.Errorf("{} refused: %v", err)
	}
}

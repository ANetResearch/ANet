//go:build !no_mcp

package mcpserv

// balance.go is get_balance (CUX-S5, CUX-S6): the hub's account of the
// balance, and beside it the limits this node pays within, read from
// /payments/status with the empty body the guard allows. The limits do not
// depend on the hub, so they are given when the balance cannot be read; the
// balance's failure is then balance_error.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// balanceLimits is get_balance's `limits`. The amounts are decimal strings
// (core §2.8); payees counts the payee list.
type balanceLimits struct {
	AutoMax         string      `json:"auto_max"`
	AgentMax        string      `json:"agent_max"`
	AgentDailyMax   string      `json:"agent_daily_max"`
	AgentSpentToday string      `json:"agent_spent_today"`
	AgentLeftToday  string      `json:"agent_left_today"`
	ExplicitMax     string      `json:"explicit_max"`
	DailyMax        string      `json:"daily_max"`
	SpentToday      string      `json:"spent_today"`
	LeftToday       string      `json:"left_today"`
	Payees          json.Number `json:"payees"`
	Unit            string      `json:"unit"`
	Summary         string      `json:"summary"`
}

// limitsFrom reads /payments/status into balanceLimits. Every member must
// be there: an answer that lacks one is not the daemon's.
func limitsFrom(raw json.RawMessage) (balanceLimits, error) {
	var st struct {
		AutoMax         json.Number `json:"auto_max"`
		AgentMax        json.Number `json:"agent_max"`
		AgentDailyMax   json.Number `json:"agent_daily_max"`
		AgentSpentToday json.Number `json:"agent_spent_today"`
		AgentLeftToday  json.Number `json:"agent_left_today"`
		ExplicitMax     json.Number `json:"explicit_max"`
		DailyMax        json.Number `json:"daily_max"`
		SpentToday      json.Number `json:"spent_today"`
		LeftToday       json.Number `json:"left_today"`
		Payees          json.Number `json:"payees"`
		Summary         string      `json:"summary"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return balanceLimits{}, fmt.Errorf("the daemon's payment limits did not read: %w", err)
	}
	for _, n := range []json.Number{st.AutoMax, st.AgentMax, st.AgentDailyMax, st.AgentSpentToday, st.AgentLeftToday,
		st.ExplicitMax, st.DailyMax, st.SpentToday, st.LeftToday, st.Payees} {
		if n == "" {
			return balanceLimits{}, fmt.Errorf("the daemon's payment limits are incomplete")
		}
	}
	return balanceLimits{AutoMax: st.AutoMax.String(), AgentMax: st.AgentMax.String(),
		AgentDailyMax: st.AgentDailyMax.String(), AgentSpentToday: st.AgentSpentToday.String(),
		AgentLeftToday: st.AgentLeftToday.String(), ExplicitMax: st.ExplicitMax.String(),
		DailyMax: st.DailyMax.String(), SpentToday: st.SpentToday.String(), LeftToday: st.LeftToday.String(),
		Payees: st.Payees, Unit: "credits", Summary: st.Summary}, nil
}

// getBalance calls /balance, then /payments/status.
func getBalance(ctx context.Context, c Control) (*mcp.CallToolResult, any, error) {
	out := map[string]json.RawMessage{}
	var raw json.RawMessage
	if err := c.Call(ctx, "/balance", map[string]any{}, &raw); err != nil {
		msg, _ := json.Marshal(err.Error())
		out["balance_error"] = msg
	} else if json.Unmarshal(raw, &out) != nil || out == nil {
		out = map[string]json.RawMessage{}
		msg, _ := json.Marshal("the daemon returned an answer to /balance that did not read")
		out["balance_error"] = msg
	}
	var st json.RawMessage
	if err := c.Call(ctx, "/payments/status", map[string]any{}, &st); err != nil {
		return nil, nil, err
	}
	limits, err := limitsFrom(st)
	if err != nil {
		return nil, nil, err
	}
	lb, err := json.Marshal(limits)
	if err != nil {
		return nil, nil, err
	}
	out["limits"] = lb
	body, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return nil, json.RawMessage(body), nil
}

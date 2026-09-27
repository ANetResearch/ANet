package main

// payments.go holds `anet payments limits` (A2A-DESIGN §8.6): show the
// spending limits, or change them after a confirmation typed on the
// terminal. Every change needs it, lowering included: the rule is that a
// limit is set by a person, and one that only lowers is still a person's
// decision about their money.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// limitFlags pairs each flag of `payments limits` with its config key.
var limitFlags = []struct{ flag, key, what string }{
	{"auto-max", "auto_max", "largest payment the daemon makes on its own"},
	{"agent-max", "agent_max", "largest payment an agent may submit (MCP, local A2A)"},
	{"agent-daily-max", "agent_daily_max", "agent and automatic payments per 24 hours"},
	{"explicit-max", "explicit_max", "largest payment you confirm yourself (anet pay, redeem, gateway)"},
	{"daily-max", "daily_max", "all payments per 24 hours"},
}

// runPayments is `anet payments limits [--auto-max N] [--agent-max N]
// [--agent-daily-max N] [--explicit-max N] [--daily-max N]`.
func runPayments(c *client, rest []string) error {
	pos, flags := splitFlags(rest)
	usage := "payments limits [--auto-max N] [--agent-max N] [--agent-daily-max N] [--explicit-max N] [--daily-max N]"
	if len(pos) == 0 || (pos[0] != "limits" && pos[0] != "show") {
		return fmt.Errorf("%s", usage)
	}
	body := map[string]any{}
	for _, f := range limitFlags {
		v, ok := flags[f.flag]
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return fmt.Errorf("--%s takes a whole number of credits (0 turns that tier off)", f.flag)
		}
		body[f.key] = n
	}
	if len(body) == 0 {
		return c.do("/payments/limits", map[string]any{})
	}
	if err := ttyConfirm(func() (string, error) { return limitsPrompt(c, body) }); err != nil {
		return err
	}
	return c.do("/payments/limits", body)
}

// limitsPrompt states each change as current → new, from the daemon's
// current limits.
func limitsPrompt(c *client, body map[string]any) (string, error) {
	cur := map[string]uint64{}
	b, code, err := c.fetch("/payments/limits", map[string]any{})
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("payments limits: daemon returned %d: %s", code, strings.TrimSpace(string(b)))
	}
	var out struct {
		Payments map[string]json.RawMessage `json:"payments"`
	}
	if json.Unmarshal(b, &out) == nil {
		for k, v := range out.Payments {
			var n uint64
			if json.Unmarshal(v, &n) == nil {
				cur[k] = n
			}
		}
	}
	var sb strings.Builder
	sb.WriteString("Change the spending limits of this node?")
	for _, f := range limitFlags {
		v, ok := body[f.key]
		if !ok {
			continue
		}
		from := "?"
		if n, ok := cur[f.key]; ok {
			from = strconv.FormatUint(n, 10)
		}
		fmt.Fprintf(&sb, "\n  %-16s %s → %d   (%s)", f.key, from, v, f.what)
	}
	return sb.String(), nil
}

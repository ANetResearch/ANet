package main

// pay.go holds `anet pay` and `anet payments` (A2A-DESIGN §8.6).
//
// Paying a quote by hand is the manual tier: it reads a confirmation from
// /dev/tty and refuses without a terminal, like the granting commands in
// peers.go, and only then calls /tasks/pay-manual. So is raising or
// lowering a spending limit. An agent driving this CLI through a tool call
// has no terminal; its payments go through /tasks/pay (the agent tier),
// bounded by agent_max and agent_daily_max. The check runs in this
// process: anything that can read the control token can call the routes
// directly (§21 item 13).

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// runPay is `anet pay <interaction_id> [--option N] [--reject]`.
func runPay(c *client, rest []string) error {
	pos, flags := splitFlags(rest)
	if len(pos) == 0 || strings.TrimSpace(pos[0]) == "" {
		return fmt.Errorf("pay <interaction_id> [--option N] [--reject]")
	}
	ix := strings.TrimSpace(pos[0])
	if flags["reject"] == "true" {
		return c.do("/tasks/pay-manual", map[string]any{"task_id": ix, "decision": "reject"})
	}
	accepts, err := quotedOptions(c, ix)
	if err != nil {
		return err
	}
	i := 0
	if v, ok := flags["option"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > len(accepts) {
			return fmt.Errorf("pay: --option must be 1..%d", len(accepts))
		}
		i = n - 1
	} else if len(accepts) > 1 {
		var b strings.Builder
		fmt.Fprintf(&b, "the provider offers %d ways to pay; choose one with --option N:\n", len(accepts))
		for n, a := range accepts {
			fmt.Fprintf(&b, "  %d. %s\n", n+1, describeOption(a))
		}
		return fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
	}
	if err := confirmOnTTY(fmt.Sprintf("Pay for task %s: %s", ix, describeOption(accepts[i]))); err != nil {
		return err
	}
	return c.do("/tasks/pay-manual", map[string]any{"task_id": ix, "decision": "submit", "accept": accepts[i]})
}

// quotedOptions reads the options of the latest quote on a task, exactly
// as the provider wrote them: the daemon compares the chosen one with the
// quote it stored.
func quotedOptions(c *client, ix string) ([]json.RawMessage, error) {
	b, code, err := c.fetch("/thread", map[string]any{"interaction_id": ix})
	if err != nil {
		return nil, err
	}
	var out struct {
		Error  string `json:"error"`
		Thread struct {
			Role     string `json:"role"`
			Messages []struct {
				Metadata json.RawMessage `json:"metadata"`
			} `json:"messages"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(b, &out); err != nil || code != 200 {
		if out.Error != "" {
			return nil, fmt.Errorf("%s", out.Error)
		}
		return nil, fmt.Errorf("pay: the daemon answered %d", code)
	}
	if out.Thread.Role != "outbound" {
		return nil, fmt.Errorf("pay: %s is not a task you delegated", ix)
	}
	for i := len(out.Thread.Messages) - 1; i >= 0; i-- {
		var meta struct {
			Required *struct {
				Accepts []json.RawMessage `json:"accepts"`
			} `json:"x402.payment.required"`
		}
		if json.Unmarshal(out.Thread.Messages[i].Metadata, &meta) == nil && meta.Required != nil &&
			len(meta.Required.Accepts) > 0 {
			return meta.Required.Accepts, nil
		}
	}
	return nil, fmt.Errorf("pay: the provider of %s has not asked to be paid", ix)
}

// describeOption renders one quoted option for the confirmation prompt.
func describeOption(raw json.RawMessage) string {
	var o struct {
		Amount  string `json:"amount"`
		Asset   string `json:"asset"`
		PayTo   string `json:"payTo"`
		Network string `json:"network"`
		Scheme  string `json:"scheme"`
	}
	_ = json.Unmarshal(raw, &o)
	return fmt.Sprintf("%s %s to %s on %s (%s)", o.Amount, o.Asset, o.PayTo, o.Network, o.Scheme)
}

// runPayments is `anet payments [show]` and
// `anet payments set <key>=<n>... [--payees-file PATH]`.
func runPayments(c *client, rest []string) error {
	pos, flags := splitFlags(rest)
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch sub {
	case "", "show":
		return c.do("/payments/status", map[string]any{})
	case "set":
		set := map[string]uint64{}
		for _, kv := range pos[1:] {
			k, v, ok := strings.Cut(kv, "=")
			n, err := strconv.ParseUint(v, 10, 64)
			if !ok || err != nil {
				return fmt.Errorf("payments set: %q is not <limit>=<whole number>", kv)
			}
			set[k] = n
		}
		body := map[string]any{"set": set}
		what := make([]string, 0, len(set)+1)
		for k, v := range set {
			what = append(what, fmt.Sprintf("%s=%d", k, v))
		}
		if pf, ok := flags["payees-file"]; ok {
			body["payees_file"] = pf
			what = append(what, "payees_file="+strconv.Quote(pf))
		}
		if len(what) == 0 {
			return fmt.Errorf("payments set <limit>=<n>... [--payees-file PATH]")
		}
		if err := confirmOnTTY("Change this node's spending limits: " + strings.Join(what, ", ")); err != nil {
			return err
		}
		return c.do("/payments/limits", body)
	default:
		return fmt.Errorf("payments [show] | payments set <limit>=<n>... [--payees-file PATH]")
	}
}

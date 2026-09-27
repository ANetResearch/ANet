package main

// pay.go holds `anet pay`, `anet redeem`, `anet payments` and `anet payees`
// (A2A-DESIGN §8.6).
//
// Paying a quote by hand is the manual tier: it reads a confirmation from
// /dev/tty and refuses without a terminal, like the granting commands in
// peers.go (tty.go), and only then calls /tasks/pay-manual. So is a
// redemption (the manual tier's other payment, to the hub), raising or
// lowering a spending limit, and putting a payee on the list. Without a
// terminal nothing is asked of the daemon at all: the quote, the hub and
// the current limits the prompt shows are read only once a terminal is
// open (ttyConfirm). An agent driving this CLI through a tool call has no
// terminal; its payments go through /tasks/pay (the agent tier), bounded
// by agent_max and agent_daily_max. The check runs in this process:
// anything that can read the control token can call the routes directly
// (§21 item 13).

import (
	"encoding/json"
	"fmt"
	"sort"
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
	var chosen json.RawMessage
	if err := ttyConfirm(func() (string, error) {
		accepts, err := quotedOptions(c, ix)
		if err != nil {
			return "", err
		}
		i := 0
		if v, ok := flags["option"]; ok {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > len(accepts) {
				return "", fmt.Errorf("pay: --option must be 1..%d", len(accepts))
			}
			i = n - 1
		} else if len(accepts) > 1 {
			var b strings.Builder
			fmt.Fprintf(&b, "the provider offers %d ways to pay; choose one with --option N:\n", len(accepts))
			for n, a := range accepts {
				fmt.Fprintf(&b, "  %d. %s\n", n+1, describeOption(a))
			}
			return "", fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
		}
		chosen = accepts[i]
		return fmt.Sprintf("Pay for task %s: %s", printable(ix, 128), describeOption(chosen)), nil
	}); err != nil {
		return err
	}
	return c.do("/tasks/pay-manual", map[string]any{"task_id": ix, "decision": "submit", "accept": chosen})
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
	// The provider wrote these: made safe to show on a terminal.
	return fmt.Sprintf("%s %s to %s on %s (%s)", printable(o.Amount, 40), printable(o.Asset, 40),
		printable(o.PayTo, 256), printable(o.Network, 256), printable(o.Scheme, 40))
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
		pf, setPayees := flags["payees-file"]
		if setPayees {
			body["payees_file"] = pf
		}
		if len(set) == 0 && !setPayees {
			return fmt.Errorf("payments set <limit>=<n>... [--payees-file PATH]")
		}
		if err := ttyConfirm(func() (string, error) { return limitsPrompt(c, set, pf, setPayees) }); err != nil {
			return err
		}
		return c.do("/payments/limits", body)
	default:
		return fmt.Errorf("payments [show] | payments set <limit>=<n>... [--payees-file PATH]")
	}
}

// limitsPrompt states each change as current → new, from the daemon's
// current limits (/payments/status), in a fixed order.
func limitsPrompt(c *client, set map[string]uint64, payeesFile string, setPayees bool) (string, error) {
	b, code, err := c.fetch("/payments/status", map[string]any{})
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("payments: the daemon answered %d: %s", code, strings.TrimSpace(string(b)))
	}
	cur := map[string]json.RawMessage{}
	_ = json.Unmarshal(b, &cur)
	from := func(k string) string {
		if v, ok := cur[k]; ok {
			return printable(string(v), 256)
		}
		return "?"
	}
	var sb strings.Builder
	sb.WriteString("Change the spending limits of this node?")
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, "\n  %-16s %s → %d", printable(k, 64), from(k), set[k])
	}
	if setPayees {
		fmt.Fprintf(&sb, "\n  %-16s %s → %s", "payees_file", from("payees_file"), strconv.Quote(payeesFile))
	}
	return sb.String(), nil
}

// runRedeem is `anet redeem <amount> [--ref <reference>]`: give credit back
// to the hub, which destroys it and signs for what it took. It is a
// payment to the hub (purpose redeem: explicit_max and daily_max, no payee
// list), so it is confirmed on the terminal with the amount and the payee —
// the hub's AID, the key the authorization is signed to — before anything
// is signed.
func runRedeem(c *client, rest []string) error {
	pos, flags := splitFlags(rest)
	if len(pos) < 1 {
		return fmt.Errorf("redeem <amount> [--ref <reference>]")
	}
	n, err := strconv.ParseUint(pos[0], 10, 64)
	if err != nil {
		return fmt.Errorf("redeem: amount must be a whole number of credits: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("redeem: the amount must be above 0")
	}
	ref := strings.TrimSpace(flags["ref"])
	if ref == "" && len(pos) > 1 {
		ref = strings.Join(pos[1:], " ")
	}
	if err := ttyConfirm(func() (string, error) { return redeemPrompt(c, n, ref) }); err != nil {
		return err
	}
	return c.do("/redeem", map[string]any{"amount": n, "reference": ref})
}

// redeemPrompt names the amount, the payee and the limits it falls under,
// from /payments/status with the hub. Without a hub AID there is no payee
// to name and nothing the daemon could sign to, so it ends without asking.
func redeemPrompt(c *client, amount uint64, ref string) (string, error) {
	b, code, err := c.fetch("/payments/status", map[string]any{"hub": true})
	if err != nil {
		return "", err
	}
	var st struct {
		Hub         string `json:"hub"`
		HubAID      string `json:"hub_aid"`
		HubError    string `json:"hub_error"`
		ExplicitMax uint64 `json:"explicit_max"`
		DailyMax    uint64 `json:"daily_max"`
		Spent24h    uint64 `json:"spent_24h"`
		Error       string `json:"error"`
	}
	if json.Unmarshal(b, &st) != nil || code != 200 {
		if st.Error != "" {
			return "", fmt.Errorf("redeem: %s", st.Error)
		}
		return "", fmt.Errorf("redeem: the daemon answered %d", code)
	}
	switch {
	case st.Hub == "":
		return "", fmt.Errorf("redeem: this node has no hub (anet hub-register <url>); nothing was signed")
	case st.HubAID == "":
		return "", fmt.Errorf("redeem: the identity of the hub %s is not known (%s); nothing was signed",
			printable(st.Hub, 256), printable(st.HubError, 256))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Redeem %d credits: give them back to your hub, which destroys them and signs for what it took.", amount)
	fmt.Fprintf(&sb, "\n  payee      %s (the hub at %s)", printable(st.HubAID, 256), printable(st.Hub, 256))
	if ref != "" {
		fmt.Fprintf(&sb, "\n  reference  %s", printable(ref, 256))
	}
	fmt.Fprintf(&sb, "\n  limits     explicit_max %d per payment; daily_max %d (%d signed in the last 24 hours)",
		st.ExplicitMax, st.DailyMax, st.Spent24h)
	if amount > st.ExplicitMax || st.Spent24h+amount > st.DailyMax {
		sb.WriteString("\n  above these limits the daemon refuses it; `anet payments set` changes them")
	}
	sb.WriteString("\nThis cannot be undone.")
	return sb.String(), nil
}

// runPayees is `anet payees list`, `anet payees add <aid>` and
// `anet payees remove <aid>`: the payee list of the spending policy
// (payments.payees_file). Adding widens what this node can pay, so it is
// confirmed on the terminal; removing narrows it and is not.
func runPayees(c *client, rest []string) error {
	pos, _ := splitFlags(rest)
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	aid := ""
	if len(pos) > 1 {
		aid = strings.TrimSpace(pos[1])
	}
	switch sub {
	case "", "list", "ls":
		return c.do("/payees/list", map[string]any{})
	case "add", "allow":
		if aid == "" {
			return fmt.Errorf("payees add <aid>")
		}
		if err := ttyConfirm(func() (string, error) { return payeePrompt(c, aid) }); err != nil {
			return err
		}
		return c.do("/payees/add", map[string]any{"aid": aid})
	case "remove", "rm":
		if aid == "" {
			return fmt.Errorf("payees remove <aid>")
		}
		return c.do("/payees/remove", map[string]any{"aid": aid})
	}
	return fmt.Errorf("payees list | add <aid> | remove <aid>")
}

// payeePrompt says what putting aid on the payee list allows: payments to
// it within each tier's limits, which it lists as they stand.
func payeePrompt(c *client, aid string) (string, error) {
	b, code, err := c.fetch("/payments/status", map[string]any{})
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("payees: the daemon answered %d: %s", code, strings.TrimSpace(string(b)))
	}
	var st struct {
		AutoMax       uint64 `json:"auto_max"`
		AgentMax      uint64 `json:"agent_max"`
		AgentDailyMax uint64 `json:"agent_daily_max"`
		ExplicitMax   uint64 `json:"explicit_max"`
		DailyMax      uint64 `json:"daily_max"`
	}
	_ = json.Unmarshal(b, &st)
	return fmt.Sprintf("Allow this node to pay %s?%s\n"+
		"  Payments to it are then possible within the spending limits:\n"+
		"    automatic   up to %d each (auto_max)\n"+
		"    agents      up to %d each, %d a day (agent_max, agent_daily_max)\n"+
		"    you         up to %d each, %d a day in all (explicit_max, daily_max)",
		printable(aid, 256), peerNote(c, aid), st.AutoMax, st.AgentMax, st.AgentDailyMax,
		st.ExplicitMax, st.DailyMax), nil
}

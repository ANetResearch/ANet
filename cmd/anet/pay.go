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
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// runPay is `anet pay <task> [--option N] [--reject]`. The confirmation is
// ux §1.4 M4: the quote (amount, payee by its full AID, its name and the
// skill), the price the payee signed, the manual limit, then confirm.yes
// (UX-R12).
func runPay(c *client, rest []string) error {
	const use = "anet pay <task> [--option N] [--reject]"
	pos, flags := splitFlags(rest)
	if len(pos) == 0 || strings.TrimSpace(pos[0]) == "" {
		return usageError{use}
	}
	ix := strings.TrimSpace(pos[0])
	if flags["reject"] == "true" {
		return c.do("/tasks/pay-manual", map[string]any{"task_id": ix, "decision": "reject"})
	}
	var chosen json.RawMessage
	if err := ttyConfirm("pay", func() (string, error) {
		accepts, err := quotedOptions(c, ix)
		if err != nil {
			return "", err
		}
		i := 0
		if v, ok := flags["option"]; ok {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > len(accepts) {
				return "", usageError{fmt.Sprintf("%s  (--option is 1 to %d)", use, len(accepts))}
			}
			i = n - 1
		} else if len(accepts) > 1 {
			var b strings.Builder
			fmt.Fprintf(&b, "%s  (the provider offers %d ways to pay; choose one with --option N:", use, len(accepts))
			for n, a := range accepts {
				fmt.Fprintf(&b, "\n  %d. %s", n+1, describeOption(a))
			}
			b.WriteString(")")
			return "", usageError{b.String()}
		}
		chosen = accepts[i]
		return payPrompt(c, ix, chosen), nil
	}); err != nil {
		return err
	}
	return c.do("/tasks/pay-manual", map[string]any{"task_id": ix, "decision": "submit", "accept": chosen})
}

// payPrompt is the explanation of `anet pay` (ux §1.4 M4): pay.prompt_quote,
// the signed price line and pay.prompt_limits.
func payPrompt(c *client, ix string, option json.RawMessage) string {
	lang := a2ashape.Lang()
	var o struct {
		Amount string `json:"amount"`
		PayTo  string `json:"payTo"`
	}
	_ = json.Unmarshal(option, &o)
	// The provider wrote these: made safe to show on a terminal.
	quote := a2ashape.Args{"amount": printable(o.Amount, 40), "peer": printable(o.PayTo, 256)}
	// One /agents/card for the payee's name and the prices it signed: from
	// the daemon's cache, or asked of the payee within 10 seconds (HPP
	// §4.5).
	card, haveCard := payeeCard(c, o.PayTo)
	if name := card.name(); haveCard && name != "" {
		quote["name"] = name
	}
	skill := taskSkill(c, ix)
	if skill != "" {
		quote["skill"] = printable(skill, 128)
	}
	lines := []string{a2ashape.Sentence("pay.prompt_quote", lang, quote)}
	lines = append(lines, signedPriceLine(card, haveCard, skill, o.Amount, lang))
	if st, ok := spendStatus(c); ok {
		lines = append(lines, a2ashape.Sentence("pay.prompt_limits", lang,
			a2ashape.Args{"single": st.ExplicitMax, "left": st.left()}))
	}
	return strings.Join(lines, "\n")
}

// payeeCard is /agents/card for aid, with the prices it signed; false when
// it cannot be read or is not the payee's.
func payeeCard(c *client, aid string) (cardAnswer, bool) {
	fc := *c
	// The daemon waits up to 10 seconds for the payee's answer.
	fc.timeout = 20 * time.Second
	b, code, err := fc.fetch("/agents/card", map[string]any{"aid": aid})
	var ra cardAnswer
	if err != nil || code != 200 || json.Unmarshal(b, &ra) != nil || ra.AID != aid || ra.Verification != "VERIFIED" {
		return cardAnswer{}, false
	}
	return ra, true
}

// name is the name on a verified card, made safe for a terminal.
func (ra cardAnswer) name() string {
	var card struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(ra.Card, &card)
	return printable(card.Name, 80)
}

// signedPriceLine is the second line of `anet pay`'s prompt (ux §3.2 text
// E): the price the payee signed for the task's skill — one the card does
// not list is 0, as for an automatic payment (HPP PAY-D2 step 4) — and
// whether this quote is above it; or that there is no signed price (a text
// task, or no extended card was had). The manual tier pays either way: the
// line is for the person deciding.
func signedPriceLine(ra cardAnswer, have bool, skill, amount string, lang a2ashape.Language) string {
	if !have || skill == "" || len(ra.Prices) == 0 {
		return a2ashape.Sentence("pay.prompt_no_price", lang, nil)
	}
	price := uint64(0)
	for _, p := range ra.Prices {
		if p.Skill == skill {
			n, err := strconv.ParseUint(p.Amount, 10, 64)
			if err != nil {
				return a2ashape.Sentence("pay.prompt_no_price", lang, nil)
			}
			price = n
			break
		}
	}
	key := "pay.prompt_price"
	if q, err := strconv.ParseUint(amount, 10, 64); err != nil || q > price {
		key = "pay.prompt_price_higher"
	}
	return a2ashape.Sentence(key, lang, a2ashape.Args{"price": price})
}

// taskSkill is the capability a task calls (metadata anet.skill), "" for a
// text task or when the task cannot be read.
func taskSkill(c *client, ix string) string {
	b, code, err := c.fetch("/tasks/get", map[string]any{"task_id": ix, "history_length": 0, "max_task_bytes": cliTaskBytes})
	if err != nil || code != 200 {
		return ""
	}
	var t struct {
		Metadata map[string]any `json:"metadata"`
	}
	if json.Unmarshal(b, &t) != nil {
		return ""
	}
	s, _ := t.Metadata[a2ashape.KeySkill].(string)
	return s
}

// spendLimits is what /payments/status says of the limits and today's
// spending (HPP §2.7).
type spendLimits struct {
	AutoMax       uint64 `json:"auto_max"`
	AgentMax      uint64 `json:"agent_max"`
	AgentDailyMax uint64 `json:"agent_daily_max"`
	ExplicitMax   uint64 `json:"explicit_max"`
	DailyMax      uint64 `json:"daily_max"`
	// LeftToday is what the manual tier may still pay today, as the
	// daemon counted it (daily_max − spent_today): the {left} of the
	// prompts, never worked out here.
	LeftToday uint64 `json:"left_today"`
}

// left is what may still be paid today under daily_max.
func (s spendLimits) left() uint64 { return s.LeftToday }

// spendStatus reads /payments/status; false when it cannot be read.
func spendStatus(c *client) (spendLimits, bool) {
	var st spendLimits
	b, code, err := c.fetch("/payments/status", map[string]any{})
	if err != nil || code != 200 || json.Unmarshal(b, &st) != nil {
		return spendLimits{}, false
	}
	return st, true
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
// `anet payments set <limit>=<n>...`.
func runPayments(c *client, rest []string) error {
	const use = "anet payments [show] | anet payments set <limit>=<n> ..."
	pos, _ := splitFlags(rest)
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch sub {
	case "", "show":
		return showPayments(c)
	case "set":
		set := map[string]uint64{}
		for _, kv := range pos[1:] {
			k, v, ok := strings.Cut(kv, "=")
			n, err := strconv.ParseUint(v, 10, 64)
			if !ok || err != nil {
				return usageError{fmt.Sprintf("anet payments set <limit>=<whole number> ...  (%q is not one)", printable(kv, 64))}
			}
			set[k] = n
		}
		if len(set) == 0 {
			return usageError{"anet payments set <limit>=<n> ..."}
		}
		if err := ttyConfirm("payment_limits", func() (string, error) { return limitsPrompt(c, set) }); err != nil {
			return err
		}
		return c.do("/payments/limits", map[string]any{"set": set})
	default:
		return usageError{use}
	}
}

// showPayments is `anet payments`: the answer as JSON, then the limits at a
// glance (limits_summary, or its .off variant) on stderr, the very sentence
// MCP's get_balance.limits.summary carries (ux D5).
func showPayments(c *client) error {
	out, code, err := c.fetch("/payments/status", map[string]any{})
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if err := printAnswer(out, code, false); err != nil {
		return err
	}
	var st struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal(out, &st) == nil && st.Summary != "" {
		fmt.Fprintln(errW(), printable(st.Summary, 1024))
	}
	return nil
}

// limitsPrompt is limits_confirm, then one limits_confirm.line per change,
// current → new, from the daemon's current limits (/payments/status), in a
// fixed order (ux §1.4 M1).
func limitsPrompt(c *client, set map[string]uint64) (string, error) {
	b, code, err := c.fetch("/payments/status", map[string]any{})
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", answerError(code, b)
	}
	cur := map[string]json.RawMessage{}
	_ = json.Unmarshal(b, &cur)
	lang := a2ashape.Lang()
	lines := []string{a2ashape.Sentence("limits_confirm", lang, nil)}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args := a2ashape.Args{"key": printable(k, 64), "to": set[k]}
		if v, ok := cur[k]; ok {
			args["from"] = printable(string(v), 32)
		}
		lines = append(lines, "  "+a2ashape.Sentence("limits_confirm.line", lang, args))
	}
	return strings.Join(lines, "\n"), nil
}

// runRedeem is `anet redeem <amount> [--ref <reference>]`: give credit back
// to the hub, which destroys it and signs for what it took. It is a
// payment to the hub (purpose redeem: explicit_max and daily_max, no payee
// list), so it is confirmed on the terminal with redeem_confirm — the
// amount, the hub and the manual limit — before anything is signed.
//
// The payee is the hub's AID, sent as pay_to: the daemon signs only to that
// hub, refusing when the node settles on another by then (a hub-register in
// between), so what the operator confirmed and what is signed cannot
// differ. A node without the payment module says so before anything is
// asked (runClient).
func runRedeem(c *client, rest []string) error {
	const use = "anet redeem <amount> [--ref R]"
	pos, flags := splitFlags(rest)
	if len(pos) < 1 {
		return usageError{use}
	}
	n, err := strconv.ParseUint(pos[0], 10, 64)
	if err != nil || n == 0 {
		return usageError{use + "  (the amount is a whole number of credits above 0)"}
	}
	ref := strings.TrimSpace(flags["ref"])
	if ref == "" && len(pos) > 1 {
		ref = strings.Join(pos[1:], " ")
	}
	var payee string
	if err := ttyConfirm("redeem", func() (string, error) {
		prompt, hubAID, err := redeemPrompt(c, n)
		payee = hubAID
		return prompt, err
	}); err != nil {
		return err
	}
	return c.do("/redeem", map[string]any{"amount": n, "reference": ref, "pay_to": payee})
}

// redeemPrompt is redeem_confirm, from /payments/status with the hub, and
// the payee (the hub's AID) it will be signed to. Without a hub AID there is
// no payee and nothing the daemon could sign to, so it ends without asking.
func redeemPrompt(c *client, amount uint64) (string, string, error) {
	b, code, err := c.fetch("/payments/status", map[string]any{"hub": true})
	if err != nil {
		return "", "", err
	}
	var st struct {
		spendLimits
		Hub      string `json:"hub"`
		HubAID   string `json:"hub_aid"`
		HubError string `json:"hub_error"`
		Error    string `json:"error"`
	}
	if json.Unmarshal(b, &st) != nil || code != 200 {
		return "", "", answerError(code, b)
	}
	switch {
	case st.Hub == "":
		return "", "", fmt.Errorf("redeem: this node has no hub (anet hub-register <url>); nothing was signed")
	case st.HubAID == "":
		return "", "", fmt.Errorf("redeem: the identity of the hub %s is not known (%s); nothing was signed",
			printable(st.Hub, 256), printable(st.HubError, 256))
	}
	return a2ashape.Sentence("redeem_confirm", a2ashape.Lang(), a2ashape.Args{"amount": amount,
		"hub": printable(st.Hub, 256), "single": st.ExplicitMax, "left": st.left()}), st.HubAID, nil
}

// runPayees is `anet payees list`, `anet payees add <aid>` and
// `anet payees remove <aid>`: the payee list of the spending policy
// (payees.allow). Adding widens what this node can pay, so it is confirmed
// on the terminal; removing narrows it and is not.
func runPayees(c *client, rest []string) error {
	const use = "anet payees list|add|remove [<aid>]"
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
			return usageError{"anet payees add <aid>"}
		}
		if err := ttyConfirm("payees", func() (string, error) { return payeePrompt(c, aid) }); err != nil {
			return err
		}
		return c.do("/payees/add", map[string]any{"aid": aid})
	case "remove", "rm":
		if aid == "" {
			return usageError{"anet payees remove <aid>"}
		}
		return c.do("/payees/remove", map[string]any{"aid": aid})
	}
	return usageError{use}
}

// payeePrompt is payee_confirm: putting aid on the payee list lets the
// automatic and agent tiers pay it, within their limits as they stand.
func payeePrompt(c *client, aid string) (string, error) {
	b, code, err := c.fetch("/payments/status", map[string]any{})
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", answerError(code, b)
	}
	var st spendLimits
	_ = json.Unmarshal(b, &st)
	args := a2ashape.Args{"peer": aid, "auto_max": st.AutoMax, "agent_max": st.AgentMax, "daily": st.AgentDailyMax}
	if name := peerName(c, aid); name != "" {
		args["name"] = name
	}
	return a2ashape.Sentence("payee_confirm", a2ashape.Lang(), args), nil
}

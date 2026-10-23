package main

// peers.go holds `anet peers` and `anet inbound` (A2A-DESIGN §5). The
// commands that grant — allow and trust, approving a held delegation,
// loosening the inbound policy — confirm on the terminal first (tty.go),
// each with its line of the reason table's M group (ux UX-R12).

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// runPeers is `anet peers list [--json]` and `anet peers
// allow|trust|deny|remove <aid>`.
func runPeers(c *client, rest []string) error {
	const use = "anet peers list [--json] | allow|trust|deny|remove <aid>"
	pos, flags := splitFlags(rest)
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
		return peersList(c, flags["json"] == "true")
	case "allow", "trust":
		if aid == "" {
			return usageError{"anet peers " + sub + " <aid>"}
		}
		if err := ttyConfirm("trust", func() (string, error) { return peerPrompt(c, sub, aid), nil }); err != nil {
			return err
		}
		return c.do("/peers/"+sub, map[string]any{"aid": aid})
	case "deny", "remove", "rm":
		if aid == "" {
			return usageError{"anet peers " + sub + " <aid>"}
		}
		if sub == "rm" {
			sub = "remove"
		}
		return c.do("/peers/"+sub, map[string]any{"aid": aid})
	}
	return usageError{use}
}

// peersList is `anet peers list` (ux UX-C1, a command for a person): a
// table of the peers on the lists — the whole AID (UX-G8a), the lists it is
// on, and whether this node's messages to it carry this node's identity
// (sealed-sender §5.2, SSR-U3) — or with --json the daemon's answer as it
// came. The sentence of the last column is the control plane's
// sender_hidden_text, word for word (UX-C5, UX-T1).
func peersList(c *client, asJSON bool) error {
	if asJSON {
		return c.do("/peers/list", map[string]any{})
	}
	out, code, err := c.fetch("/peers/list", map[string]any{})
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if code != 200 {
		return answerError(code, out)
	}
	var list struct {
		Peers []peerRow `json:"peers"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return answerError(code, out)
	}
	writePeersTable(outW(), list.Peers, a2ashape.Lang())
	return nil
}

// peerRow is one /peers/list peer, as the table reads it.
type peerRow struct {
	AID              string   `json:"aid"`
	Lists            []string `json:"lists"`
	SenderHidden     bool     `json:"sender_hidden"`
	SenderHiddenText string   `json:"sender_hidden_text"`
}

// writePeersTable writes the `anet peers list` table in the language's
// column titles: the AID first and whole, so it can be copied to any
// command that takes one (UX-G8a), and the sentence last, where a long one
// runs on without pushing anything else off the line.
func writePeersTable(w io.Writer, peers []peerRow, lang a2ashape.Language) {
	const none = "—"
	rows := [][]string{{a2ashape.Label("peers_list.aid", lang), a2ashape.Label("peers_list.lists", lang),
		a2ashape.Label("peers_list.sender", lang)}}
	for _, p := range peers {
		var lists []string
		for _, l := range p.Lists {
			if l = printable(l, 16); l != "" {
				lists = append(lists, l)
			}
		}
		listCol := none
		if len(lists) > 0 {
			listCol = strings.Join(lists, ",")
		}
		aid := printable(p.AID, 128)
		if aid == "" {
			aid = none
		}
		rows = append(rows, []string{aid, listCol, oneLine(p.SenderHiddenText, maxLineRunes)})
	}
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, cell := range r[:len(r)-1] {
			widths[i] = max(widths[i], displayWidth(cell))
		}
	}
	for _, r := range rows {
		var b strings.Builder
		for i, cell := range r {
			b.WriteString(cell)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-displayWidth(cell)+2))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

// peerPrompt is the confirmation of `anet peers allow|trust <aid>`:
// allow_confirm, or for trust the trust_confirm line of how a trusted
// peer's tasks run on this node (ux §1.4 M1, M3).
func peerPrompt(c *client, sub, aid string) string {
	args := a2ashape.Args{"peer": aid}
	if name := peerName(c, aid); name != "" {
		args["name"] = name
	}
	key := "allow_confirm"
	if sub == "trust" {
		// Without auto-reply (no auto_reply block, or the module is not available) nothing runs a local
		// agent for the peer: trust only decides whether local backends take its tasks (MOD-3).
		switch trustedMode(c) {
		case "sandbox":
			key = "trust_confirm.sandbox"
		case "direct":
			key = "trust_confirm.direct"
		case "":
			key = "trust_confirm.no_agent"
		}
	}
	return a2ashape.Sentence(key, a2ashape.Lang(), args)
}

// trustedMode is how this node runs a trusted peer's tasks (auto_reply.trusted:
// direct or sandbox), as /status says it; "" when auto-reply is off or it does not say.
func trustedMode(c *client) string {
	b, code, err := c.fetch("/status", nil)
	if err != nil || code != 200 {
		return ""
	}
	var st struct {
		AutoReply *struct {
			Trusted string `json:"trusted"`
		} `json:"auto_reply"`
	}
	if json.Unmarshal(b, &st) != nil || st.AutoReply == nil {
		return ""
	}
	return st.AutoReply.Trusted
}

// runInbound is `anet inbound policy [closed|approve|open]`,
// `anet inbound list` (alias pending), `anet inbound approve|reject <id>` and
// `anet inbound admit set <capability> [--cache-seconds N]|off`.
func runInbound(c *client, rest []string) error {
	const use = "anet inbound policy [closed|approve|open] | list | approve <id> | reject <id> | " +
		"admit set <capability> [--cache-seconds N] | admit off"
	pos, flags := splitFlags(rest)
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	arg := ""
	if len(pos) > 1 {
		arg = strings.TrimSpace(pos[1])
	}
	switch sub {
	case "", "policy":
		if arg == "" {
			return c.do("/inbound/policy", map[string]any{})
		}
		switch arg {
		case "closed":
		case "approve", "open":
			if err := confirmOnTTY("inbound_policy", a2ashape.Sentence("inbound.confirm_"+arg, a2ashape.Lang(), nil)); err != nil {
				return err
			}
		default:
			return usageError{"anet inbound policy closed|approve|open"}
		}
		return c.do("/inbound/policy", map[string]any{"policy": arg})
	case "pending", "list", "ls":
		return c.do("/inbound/pending", map[string]any{})
	case "approve":
		if arg == "" {
			return usageError{"anet inbound approve <id>"}
		}
		if err := ttyConfirm("approve", func() (string, error) { return approvePrompt(c, arg) }); err != nil {
			return err
		}
		return c.do("/inbound/approve", map[string]any{"interaction_id": arg})
	case "reject":
		if arg == "" {
			return usageError{"anet inbound reject <id>"}
		}
		return c.do("/inbound/reject", map[string]any{"interaction_id": arg})
	case "admit":
		return inboundAdmit(c, pos[1:], flags)
	}
	return usageError{use}
}

// inboundAdmit is `anet inbound admit set <capability> [--cache-seconds N]`
// and `anet inbound admit off` (design v0.3.9 §A): which internal capability
// of the service module decides whether a peer on no list may send text
// tasks. The daemon checks that the capability is one; the answer is the
// inbound state, admit included.
func inboundAdmit(c *client, pos []string, flags map[string]string) error {
	const use = "anet inbound admit set <capability> [--cache-seconds N] | anet inbound admit off"
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch {
	case sub == "off" && len(pos) == 1:
		if _, ok := flags["cache-seconds"]; ok {
			return usageError{use}
		}
		return c.do("/inbound/policy", map[string]any{"admit": nil})
	case sub == "set" && len(pos) == 2 && strings.TrimSpace(pos[1]) != "":
		admit := map[string]any{"capability": strings.TrimSpace(pos[1])}
		if v, ok := flags["cache-seconds"]; ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 1 || n > 300 {
				return usageError{"--cache-seconds takes a whole number from 1 to 300"}
			}
			admit["cache_seconds"] = n
		}
		return c.do("/inbound/policy", map[string]any{"admit": admit})
	}
	return usageError{use}
}

// approvePrompt is approve_confirm for a held task: who sent it, by its
// full AID and its verified name (A2A-DESIGN §5.3), never what it asks. An
// item the daemon does not list is an error, so nobody is asked to approve
// nothing; when the list cannot be read the prompt names the task alone and
// the daemon has the last word.
func approvePrompt(c *client, ix string) (string, error) {
	args := a2ashape.Args{"task": ix}
	b, code, err := c.fetch("/inbound/pending", map[string]any{})
	if err == nil && code == 200 {
		var out struct {
			Pending []struct {
				InteractionID string `json:"interaction_id"`
				Requester     string `json:"requester"`
			} `json:"pending"`
		}
		if json.Unmarshal(b, &out) == nil {
			found := false
			for _, p := range out.Pending {
				if p.InteractionID != ix {
					continue
				}
				found = true
				args["peer"] = printable(p.Requester, 128)
				if name := peerName(c, p.Requester); name != "" {
					args["name"] = name
				}
			}
			if !found {
				return "", fmt.Errorf("no held delegation %s (see `anet inbound list`)", printable(ix, 128))
			}
		}
	}
	return a2ashape.Sentence("approve_confirm", a2ashape.Lang(), args), nil
}

// peerName is a peer's name for a confirmation: the name on its network
// card, which the daemon takes from the directory without naming the peer to
// the hub and verifies against the peer's key history (/agents/card,
// sealed-sender SSR-D16). "" (shown as unnamed) when the card does not
// verify or there is none: a name nobody signed is not shown. The AID
// beside it is what is being granted. It asks for no prices, so the peer
// is not contacted (prices: false; HPP PAY-C10).
func peerName(c *client, aid string) string {
	fc := *c
	fc.timeout = 10 * time.Second
	b, code, err := fc.fetch("/agents/card", map[string]any{"aid": aid, "prices": false})
	if err != nil || code != 200 {
		return ""
	}
	var out struct {
		AID          string          `json:"aid"`
		Card         json.RawMessage `json:"card"`
		Verification string          `json:"verification"`
	}
	if json.Unmarshal(b, &out) != nil || out.AID != aid || out.Verification != "VERIFIED" {
		return ""
	}
	var card struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(out.Card, &card)
	return printable(card.Name, 80)
}

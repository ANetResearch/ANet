package main

// peers.go holds `anet peers` and `anet inbound` (A2A-DESIGN §5). The
// commands that grant — allow and trust, approving a held delegation,
// loosening the inbound policy — confirm on the terminal first (tty.go),
// each with its line of the reason table's M group (ux UX-R12).

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// runPeers is `anet peers list|allow|trust|deny|remove [<aid>]`.
func runPeers(c *client, rest []string) error {
	const use = "anet peers list|allow|trust|deny|remove [<aid>]"
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
		return c.do("/peers/list", map[string]any{})
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
		key = "trust_confirm.direct"
		if trustedMode(c) == "sandbox" {
			key = "trust_confirm.sandbox"
		}
	}
	return a2ashape.Sentence(key, a2ashape.Lang(), args)
}

// trustedMode is how this node runs a trusted peer's tasks (auto_reply.trusted:
// direct or sandbox), as /status says it; "" when it does not say.
//
// v030-todo(EXE-7): /status.auto_reply carries trusted; until it does the
// prompt says direct, the stronger of the two statements.
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
// `anet inbound list` (alias pending) and `anet inbound approve|reject <id>`.
func runInbound(c *client, rest []string) error {
	const use = "anet inbound policy [closed|approve|open] | list | approve <id> | reject <id>"
	pos, _ := splitFlags(rest)
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
// beside it is what is being granted.
func peerName(c *client, aid string) string {
	fc := *c
	fc.timeout = 10 * time.Second
	b, code, err := fc.fetch("/agents/card", map[string]any{"aid": aid})
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

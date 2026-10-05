package main

// peers.go holds `anet peers` and `anet inbound` (A2A-DESIGN §5). The
// commands that grant — allow and trust, approving a held delegation,
// loosening the inbound policy — confirm on the terminal first (tty.go).

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// errAcceptOn is the answer to `anet accept on` and to
// `hub-register --accept-delegations true`.
var errAcceptOn = errors.New("`accept on` was removed: it let anyone delegate to this node.\n" +
	"Inbound delegations are governed by the inbound policy:\n" +
	"  closed   (the default) only peers on your allow list; add one with `anet peers allow <aid>`\n" +
	"  approve  anyone else is held until you run `anet inbound approve <id>`\n" +
	"  open     anyone may send a natural-language task (capability calls still need inbound.public_capabilities)\n" +
	"Set it with `anet inbound policy <closed|approve|open>`.")

// runPeers is `anet peers list|allow|trust|deny|remove [<aid>]`.
func runPeers(c *client, rest []string) error {
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
			return fmt.Errorf("peers %s <aid>", sub)
		}
		what := "delegate tasks to this node"
		if sub == "trust" {
			what = "delegate tasks to this node AND drive its local agent through exec auto-reply"
		}
		if err := ttyConfirm(func() (string, error) {
			return fmt.Sprintf("Allow %s to %s?%s", printable(aid, 256), what, peerNote(c, aid)), nil
		}); err != nil {
			return err
		}
		return c.do("/peers/"+sub, map[string]any{"aid": aid})
	case "deny", "remove", "rm":
		if aid == "" {
			return fmt.Errorf("peers %s <aid>", sub)
		}
		if sub == "rm" {
			sub = "remove"
		}
		return c.do("/peers/"+sub, map[string]any{"aid": aid})
	}
	return fmt.Errorf("peers list|allow|trust|deny|remove [<aid>]")
}

// runInbound is `anet inbound policy [closed|approve|open]`,
// `anet inbound list` (alias pending) and `anet inbound approve|reject <id>`.
func runInbound(c *client, rest []string) error {
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
			if err := confirmOnTTY(fmt.Sprintf("Set the inbound policy to %s? (%s)", arg, policyNote(arg))); err != nil {
				return err
			}
		default:
			return fmt.Errorf("inbound policy closed|approve|open")
		}
		return c.do("/inbound/policy", map[string]any{"policy": arg})
	case "pending", "list", "ls":
		return c.do("/inbound/pending", map[string]any{})
	case "approve":
		if arg == "" {
			return fmt.Errorf("inbound approve <interaction_id>")
		}
		if err := ttyConfirm(func() (string, error) {
			note, err := pendingNote(c, arg)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Approve the held delegation %s?%s", printable(arg, 128), note), nil
		}); err != nil {
			return err
		}
		return c.do("/inbound/approve", map[string]any{"interaction_id": arg})
	case "reject":
		if arg == "" {
			return fmt.Errorf("inbound reject <interaction_id>")
		}
		return c.do("/inbound/reject", map[string]any{"interaction_id": arg})
	}
	return fmt.Errorf("inbound policy [closed|approve|open] | list | approve <id> | reject <id>")
}

func policyNote(p string) string {
	if p == "open" {
		return "anyone may then send this node natural-language tasks"
	}
	return "delegations from peers not on the allow list are then held for your approval"
}

// pendingNote describes a held item for the confirmation prompt: who sent
// it, when, and how large, not what it asks (A2A-DESIGN §5.3). An item the
// daemon does not list is an error, so nobody is asked to approve nothing;
// when the list cannot be read the prompt goes without the description and
// the daemon has the last word.
func pendingNote(c *client, ix string) (string, error) {
	b, code, err := c.fetch("/inbound/pending", map[string]any{})
	if err != nil || code != 200 {
		return "", nil
	}
	var out struct {
		Pending []struct {
			InteractionID string `json:"interaction_id"`
			Requester     string `json:"requester"`
			ArrivedAt     int64  `json:"arrived_at"`
			Capability    string `json:"capability"`
			Bytes         int64  `json:"bytes"`
			Attachments   int    `json:"attachments"`
		} `json:"pending"`
	}
	if json.Unmarshal(b, &out) != nil {
		return "", nil
	}
	for _, p := range out.Pending {
		if p.InteractionID != ix {
			continue
		}
		s := fmt.Sprintf("\n  from      %s\n  size      %d bytes", printable(p.Requester, 128), p.Bytes)
		if p.ArrivedAt > 0 {
			s += "\n  arrived   " + time.UnixMilli(p.ArrivedAt).Local().Format(time.RFC3339)
		}
		if p.Attachments > 0 {
			s += fmt.Sprintf("\n  files     %d attachment(s)", p.Attachments)
		}
		if p.Capability != "" {
			s += "\n  calls     " + printable(p.Capability, 128)
		}
		return s + "\nApproving runs it as if this peer were on your allow list, this once.", nil
	}
	return "", fmt.Errorf("no held delegation %s (see `anet inbound list`)", ix)
}

// peerNote describes a peer for the confirmation prompt from its network
// card, which the daemon takes from the directory without naming the peer
// to the hub and verifies against the peer's key history (/agents/card,
// sealed-sender SSR-D16). A card is what the peer says of itself, signed by
// it; the AID above it is what is being granted.
func peerNote(c *client, aid string) string {
	fc := *c
	fc.timeout = 10 * time.Second
	b, code, err := fc.fetch("/agents/card", map[string]any{"aid": aid})
	if err != nil || code != 200 {
		return ""
	}
	var out struct {
		AID               string          `json:"aid"`
		Card              json.RawMessage `json:"card"`
		Verification      string          `json:"verification"`
		VerificationError string          `json:"verificationError"`
	}
	if json.Unmarshal(b, &out) != nil || out.AID != aid {
		return ""
	}
	switch out.Verification {
	case "VERIFIED":
		var card struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(out.Card, &card)
		s := "\n  network card (the agent's own words, signed by it):"
		if card.Name != "" {
			s += "\n    name     " + printable(card.Name, 80)
		}
		if card.Description != "" {
			s += "\n    about    " + printable(card.Description, 160)
		}
		return s
	case "UNVERIFIED":
		return "\n  its network card does not verify here (" + printable(out.VerificationError, 80) + "); nothing of it is shown"
	}
	if out.VerificationError == daemon.CardUnknownAgent {
		return "\n  no hub knows this AID"
	}
	return "\n  this agent publishes no network card"
}

// runAccept is the wire-1 `anet accept <on|off>` (A2A-DESIGN §5.1). "on"
// let anyone delegate and has no safe equivalent, so it is refused with the
// policies and the allow-list command; "off" sets policy closed.
func runAccept(c *client, rest []string) error {
	arg := ""
	if len(rest) > 0 {
		arg = rest[0]
	}
	b, err := parseBool(arg)
	if err != nil || b {
		return errAcceptOn
	}
	return c.do("/accept", map[string]any{"enabled": false})
}

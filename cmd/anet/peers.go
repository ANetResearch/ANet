package main

// peers.go holds `anet peers` and `anet inbound` (A2A-DESIGN §5), and the
// terminal confirmation that granting commands require.
//
// Granting — putting a peer on the allow or trust list, approving a held
// delegation, loosening the inbound policy — reads a confirmation from
// /dev/tty and refuses when there is no terminal. An agent that drives
// this CLI through a tool call has no terminal and cannot grant on its own.
// The check runs in this process: the daemon only sees the control token
// and cannot tell whether a caller went through a terminal, and anything
// that can read the token can call the routes directly (§21 item 13).

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// errAcceptOn is the answer to `anet accept on` and to
// `hub-register --accept-delegations true`.
var errAcceptOn = errors.New("`accept on` was removed: it let anyone delegate to this node.\n" +
	"Inbound delegations are governed by the inbound policy:\n" +
	"  closed   (the default) only peers on your allow list; add one with `anet peers allow <aid>`\n" +
	"  approve  anyone else is held until you run `anet inbound approve <id>`\n" +
	"  open     anyone may send a natural-language task (capability calls still need inbound.public_capabilities)\n" +
	"Set it with `anet inbound policy <closed|approve|open>`.")

// errNoTTY is returned by a granting command run without a terminal.
var errNoTTY = errors.New("this command grants access and needs a confirmation typed on a terminal (/dev/tty); " +
	"none is available to this process. Run it yourself in a terminal, or edit the peers files directly " +
	"(see `anet peers list` for their paths)")

// openTTY opens the controlling terminal. Tests replace it.
var openTTY = func() (io.ReadWriteCloser, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }

// confirmOnTTY writes prompt to the terminal and reads one line from it. It
// returns nil only when the answer is "yes".
func confirmOnTTY(prompt string) error {
	tty, err := openTTY()
	if err != nil {
		return errNoTTY
	}
	defer tty.Close()
	if _, err := fmt.Fprintf(tty, "%s\nType yes to confirm: ", prompt); err != nil {
		return errNoTTY
	}
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && line == "" {
		return errNoTTY
	}
	if strings.TrimSpace(strings.ToLower(line)) != "yes" {
		return fmt.Errorf("not confirmed; nothing changed")
	}
	return nil
}

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
		if err := confirmOnTTY(fmt.Sprintf("Allow %s to %s?", aid, what)); err != nil {
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
// `anet inbound pending` and `anet inbound approve|reject <id>`.
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
	case "pending":
		return c.do("/inbound/pending", map[string]any{})
	case "approve":
		if arg == "" {
			return fmt.Errorf("inbound approve <interaction_id>")
		}
		if err := confirmOnTTY(fmt.Sprintf("Approve the held delegation %s?%s", arg, pendingNote(c, arg))); err != nil {
			return err
		}
		return c.do("/inbound/approve", map[string]any{"interaction_id": arg})
	case "reject":
		if arg == "" {
			return fmt.Errorf("inbound reject <interaction_id>")
		}
		return c.do("/inbound/reject", map[string]any{"interaction_id": arg})
	}
	return fmt.Errorf("inbound policy [closed|approve|open] | pending | approve <id> | reject <id>")
}

func policyNote(p string) string {
	if p == "open" {
		return "anyone may then send this node natural-language tasks"
	}
	return "delegations from peers not on the allow list are then held for your approval"
}

// pendingNote describes a held item for the confirmation prompt: who sent
// it and when, not what it asks.
func pendingNote(c *client, ix string) string {
	b, code, err := c.fetch("/inbound/pending", map[string]any{})
	if err != nil || code != 200 {
		return ""
	}
	var out struct {
		Pending []struct {
			InteractionID string `json:"interaction_id"`
			Requester     string `json:"requester"`
			Capability    string `json:"capability"`
			Bytes         int64  `json:"bytes"`
		} `json:"pending"`
	}
	if json.Unmarshal(b, &out) != nil {
		return ""
	}
	for _, p := range out.Pending {
		if p.InteractionID == ix {
			s := fmt.Sprintf(" It is from %s, %d bytes", p.Requester, p.Bytes)
			if p.Capability != "" {
				s += ", a call to " + p.Capability
			}
			return s + "."
		}
	}
	return ""
}

// runHubRegister is `anet hub-register <url> [--name N] [--caps a,b]
// [--token INVITE]`. The wire-1 --accept-delegations flag is still read:
// true is refused like `accept on`, false asks for what the closed policy
// does (A2A-DESIGN §5.1).
func runHubRegister(c *client, rest []string) error {
	pos, flags := splitFlags(rest)
	if len(pos) < 1 || pos[0] == "" {
		return fmt.Errorf("hub-register <url> [--name NAME] [--caps a,b] [--token INVITE]")
	}
	body := map[string]any{"hub": pos[0], "name": flags["name"]}
	// Only sent when given. A hub that admits openly has no use for it, and
	// sending an empty string would make the two cases look different on
	// the wire when they are not.
	if v := strings.TrimSpace(flags["token"]); v != "" {
		body["token"] = v
	}
	if v := flags["caps"]; v != "" {
		body["caps"] = strings.Split(v, ",")
	}
	if v, ok := flags["accept-delegations"]; ok {
		b, err := parseBool(v)
		if err != nil {
			return fmt.Errorf("--accept-delegations takes true or false")
		}
		if b {
			return errAcceptOn
		}
		body["accept_delegations"] = false
	}
	return c.do("/hub-register", body)
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

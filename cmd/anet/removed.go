package main

// removed.go refuses the commands and flags 0.3.0 removed (ux §4.3) before
// anything else happens: the CLI prints cli_removed with what to do instead
// and exits 2, without reading the data directory or contacting the daemon.
// The removed code itself goes with the area that owned it; this table keeps
// a person who types the old form from getting a silent no-op or an unrelated
// error until v0.3.x ends (ux §4.3).

import (
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// removedCLI is one removed command, or one removed flag of a command.
type removedCLI struct {
	cmd  string // the verb as typed (aliases are listed on their own)
	flag string // "" for the whole command
	// what is {what}: the command as ux §4.3 names it.
	what string
	// instead is the id of its {instead} phrase (a2ashape, cli_removed).
	instead string
}

// removedCommands is ux §4.3. `anet autoreply set --api-key` is not here: it
// is api_key_on_argv, the key having already been typed where others can
// read it (LEC, EXE-5).
var removedCommands = []removedCLI{
	{cmd: "x402-authorize", what: "anet x402-authorize", instead: "x402_authorize"},
	{cmd: "pay-header", what: "anet pay-header", instead: "x402_authorize"},
	{cmd: "delegate", flag: "pay", what: "anet delegate --pay", instead: "delegate_pay"},
	{cmd: "profile", flag: "pricing", what: "anet profile set --pricing", instead: "profile_pricing"},
	{cmd: "hub-register", flag: "token-file", what: "anet hub-register --token-file", instead: "hub_register_token"},
	{cmd: "hub-register", flag: "token", what: "anet hub-register --token", instead: "hub_register_token"},
	{cmd: "hub-register", flag: "accept-delegations", what: "anet hub-register --accept-delegations", instead: "hub_register_accept_delegations"},
	{cmd: "p2p-advertise", what: "anet p2p-advertise", instead: "p2p_advertise"},
	{cmd: "accept", what: "anet accept", instead: "accept"},
	{cmd: "accept-end", what: "anet accept-end", instead: "accept_end"},
	{cmd: "install", what: "anet install", instead: "install"},
	{cmd: "evidence", what: "anet evidence", instead: "evidence"},
	{cmd: "autoreply", flag: "require-image", what: "anet autoreply set --require-image", instead: "require_image"},
	{cmd: "auto-reply", flag: "require-image", what: "anet auto-reply set --require-image", instead: "require_image"},
	{cmd: "autoreply", flag: "max-auto-replies", what: "anet autoreply set --max-auto-replies", instead: "fixed_auto_reply"},
	{cmd: "auto-reply", flag: "max-auto-replies", what: "anet auto-reply set --max-auto-replies", instead: "fixed_auto_reply"},
	{cmd: "autoreply", flag: "poll-interval", what: "anet autoreply set --poll-interval", instead: "fixed_auto_reply"},
	{cmd: "auto-reply", flag: "poll-interval", what: "anet auto-reply set --poll-interval", instead: "fixed_auto_reply"},
	{cmd: "payments", flag: "payees-file", what: "anet payments set --payees-file", instead: "payments_payees_file"},
}

// refuseRemoved is the cli_removed sentence for an invocation that uses a
// removed command or flag, and true; "" and false for any other. A flag
// counts in either form (--flag, --flag=value) anywhere before a bare "--".
func refuseRemoved(cmd string, rest []string, lang a2ashape.Language) (string, bool) {
	flags := map[string]bool{}
	for _, a := range rest {
		if a == "--" {
			break
		}
		if name, ok := strings.CutPrefix(a, "--"); ok {
			name, _, _ = strings.Cut(name, "=")
			flags[name] = true
		}
	}
	for _, r := range removedCommands {
		if r.cmd != cmd || (r.flag != "" && !flags[r.flag]) {
			continue
		}
		return a2ashape.Sentence("cli_removed", lang, a2ashape.Args{"what": r.what, "instead": r.instead}), true
	}
	return "", false
}

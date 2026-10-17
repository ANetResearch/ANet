package main

// hubjoin.go is `anet hub-register` and `anet hub-leave` (v0.3.0 LEC CL-4,
// AD-8; ux §3.1, §3.4 (a) and (g)). The daemon decides and words every
// outcome; the CLI asks it what a hub-register would do (dry_run), brings
// what a first join needs (next_digest, KEL D-10, D-11), and prints the
// daemon's sentences: the recovery reminder first when there is one, then
// the join state, each followed by its command on a "  → " line.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// hubRegisterTimeout bounds `anet hub-register`'s wait for the daemon: the
// hub's identity (up to 30 seconds), an attempt of the background loop the
// daemon may have to wait for (60 seconds), this join attempt (up to 60
// seconds, proof of work included, LEC AD-1) and the profile published
// after it (30 seconds). Running out says the daemon cannot be reached,
// which would be wrong while it is still working.
const hubRegisterTimeout = 4 * time.Minute

// errExit1 ends a command with exit status 1 after it printed what to say.
var errExit1 = errors.New("")

// usageError is a command used wrongly: its usage line, exit status 2
// (ux UX-C4).
type usageError struct{ usage string }

func (e usageError) Error() string { return "usage: " + e.usage }

// hubRegisterAnswer is the /hub-register answer the CLI prints (LEC
// §2.3.2): the state's sentence and command, and the recovery reminder.
type hubRegisterAnswer struct {
	Hub      string `json:"hub"`
	State    string `json:"state"`
	Message  string `json:"message"`
	Next     string `json:"next"`
	Reason   string `json:"reason"`
	HubCode  string `json:"hub_code"`
	Recovery *struct {
		State   string `json:"state"`
		Message string `json:"message"`
		Next    string `json:"next"`
	} `json:"recovery"`
	// The dry_run answer.
	Action         string `json:"action"`
	RecoveryNeeded bool   `json:"recovery_needed"`
}

// hubCLI is where hub-register and hub-leave write, and what they ask of
// their environment; tests replace them.
type hubCLI struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	// terminal reports whether a terminal is there to ask a passphrase on.
	terminal func() bool
	lang     a2ashape.Language
}

func defaultHubCLI() hubCLI {
	return hubCLI{stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv, terminal: haveTTY, lang: a2ashape.Lang()}
}

// haveTTY reports whether this process can open its controlling terminal.
func haveTTY() bool {
	t, err := openTTY()
	if err != nil {
		return false
	}
	_ = t.Close()
	return true
}

// warnInviteEnv says once that ANET_INVITE is ignored (LEC AV-5, ux §2.3):
// the config_key_removed sentence, on stderr, and the command goes on.
func (h hubCLI) warnInviteEnv() {
	if h.getenv(daemon.InviteEnv) == "" {
		return
	}
	fmt.Fprintln(h.stderr, a2ashape.Sentence("config_key_removed", h.lang,
		a2ashape.Args{"key": daemon.InviteEnv, "instead": "invite_env"}))
}

// say prints a sentence and, when there is one, its command on the next
// line (ux UX-R2).
func say(w io.Writer, sentence, next string) {
	if sentence != "" {
		fmt.Fprintln(w, sentence)
	}
	if next != "" {
		fmt.Fprintln(w, nextPrefix+next)
	}
}

// refusedBy prints a control-plane refusal (ux UX-C3): the sentence and its
// command on stderr, no JSON, exit status 1. A body that is not a refusal
// is shown as it came.
func (h hubCLI) refusedBy(code int, body []byte) error {
	var e a2ashape.ErrorBody
	if json.Unmarshal(body, &e) != nil || e.Error == "" {
		fmt.Fprintf(h.stderr, "error: the daemon answered %d: %s\n", code, strings.TrimSpace(string(body)))
		return errExit1
	}
	say(h.stderr, e.Error+a2ashape.CodeSuffix(e.Reason, e.HubCode), e.Next)
	return errExit1
}

// runHubRegister is `anet hub-register <url> [--name N] [--caps a,b]
// [--recovery-file F] [--new-recovery]` (LEC CL-4):
//
//  1. the removed flags were refused before this (removed.go); ANET_INVITE
//     is ignored with one line;
//  2. dry_run asks the daemon what this would do; a hub that cannot be
//     reached is said at once, before any passphrase (UX-O1);
//  3. a first join brings next_digest (and unprotected without a
//     terminal); switching hubs is a key rotation (KEL L-4);
//  4. the answer: the recovery reminder and its command first, then the
//     join state's sentence and its command. Exit 0 for registered, waiting
//     for admission and retrying; 1 for refused (ux UX-C4).
func runHubRegister(c *client, rest []string) error {
	return defaultHubCLI().hubRegister(c, rest)
}

func (h hubCLI) hubRegister(c *client, rest []string) error {
	const use = "anet hub-register <url> [--name N] [--caps a,b] [--recovery-file F] [--new-recovery]"
	pos, flags := splitFlags(rest)
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		return usageError{use}
	}
	h.warnInviteEnv()
	hub := strings.TrimRight(strings.TrimSpace(pos[0]), "/")
	cc := *c
	cc.timeout = hubRegisterTimeout
	b, code, err := cc.fetch("/hub-register", map[string]any{"hub": hub, "dry_run": true})
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if code != 200 {
		return h.refusedBy(code, b)
	}
	var dry hubRegisterAnswer
	if err := json.Unmarshal(b, &dry); err != nil {
		return fmt.Errorf("hub-register: the daemon's answer does not read: %v", err)
	}
	body := map[string]any{"hub": hub}
	if v := flags["name"]; v != "" {
		body["name"] = v
	}
	if v := flags["caps"]; v != "" {
		body["caps"] = strings.Split(v, ",")
	}
	// Switching hubs is a key rotation (KEL L-4); without a terminal the daemon answers it with
	// hub_switch_requires_rotation, and nothing here is asked.
	if dry.Action == "switch" && h.terminal() {
		return h.switchHub(c, hub, flags)
	}
	var afterJoin func()
	if dry.Action == "join" && dry.RecoveryNeeded {
		digest, unprotected, done, err := joinRecovery(h, c.dataDir, hub, flags)
		if err != nil {
			return err
		}
		afterJoin = done
		body["next_digest"] = base64.StdEncoding.EncodeToString(digest)
		if unprotected {
			body["unprotected"] = true
		}
	}
	b, code, err = cc.fetch("/hub-register", body)
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if code != 200 {
		return h.refusedBy(code, b)
	}
	var out hubRegisterAnswer
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("hub-register: the daemon's answer does not read: %v", err)
	}
	if afterJoin != nil {
		afterJoin()
	}
	if r := out.Recovery; r != nil && r.Message != "" {
		say(h.stdout, r.Message, r.Next)
	}
	if out.State == daemon.JoinRefused {
		say(h.stdout, out.Message+a2ashape.CodeSuffix(out.Reason, out.HubCode), out.Next)
		return errExit1
	}
	say(h.stdout, out.Message, out.Next)
	return nil
}

// runHubLeave is `anet hub-leave [<url>]` (LEC AD-8): a refusal is said as
// the daemon words it (witness_hub_leave, no_hub_yet), on stderr.
func runHubLeave(c *client, rest []string) error {
	h := defaultHubCLI()
	pos, _ := splitFlags(rest)
	hub := ""
	if len(pos) > 0 {
		hub = strings.TrimSpace(pos[0])
	}
	b, code, err := c.fetch("/hub-leave", map[string]any{"hub": hub})
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if code != 200 {
		return h.refusedBy(code, b)
	}
	var pretty strings.Builder
	var v any
	if json.Unmarshal(b, &v) == nil {
		enc := json.NewEncoder(&pretty)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		fmt.Fprint(h.stdout, pretty.String())
		return nil
	}
	fmt.Fprintln(h.stdout, string(b))
	return nil
}

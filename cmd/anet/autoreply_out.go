package main

// autoreply_out.go is what `anet autoreply set|show|test` print and the terminal confirmation of
// `--trusted direct` (v0.3.0 LEC CL-1 to CL-3, §5.2; ux §3.3). The sentences are the daemon's, taken from
// its answer (UX-C5): this file only arranges them. JSON goes to stdout; the lines for a person go to
// stderr (UX-C1), except `show` and `test`, whose lines are the answer.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// autoReplyNow is the auto-reply block of GET /status, nil when there is none.
func autoReplyNow(c *client) (map[string]any, error) {
	b, code, err := c.fetch("/status", nil)
	if err != nil {
		return nil, diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if code != 200 {
		return nil, answerError(code, b)
	}
	var st struct {
		AutoReply map[string]any `json:"auto_reply"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("status: %v", err)
	}
	return st.AutoReply, nil
}

// confirmTrustedDirect asks, on the terminal, before trusted peers go from the sandbox back to direct runs
// (trusted_direct_confirm). Nothing is asked when the daemon already runs them direct.
func confirmTrustedDirect(c *client) error {
	cur, err := autoReplyNow(c)
	if err != nil {
		return err
	}
	if cur == nil || cur["trusted"] != "sandbox" {
		return nil
	}
	err = confirmOnTTY("trust", a2ashape.Sentence("trusted_direct_confirm", a2ashape.Lang(), nil))
	if err == errNoTTY || err == errDeclined {
		return errExit1 // said already (tty.go)
	}
	return err
}

// needBackendFirst says, for --egress-allow on a node with no auto-reply, that the backend comes first:
// a list of hosts for a sandbox that does not exist yet would only be refused by the daemon with a rule
// about the backend.
func needBackendFirst(lang a2ashape.Language) {
	rule := "set the backend first, with anet autoreply set --backend exec --agent <name>"
	if lang == a2ashape.Chinese {
		rule = "请先配置后端,例如 anet autoreply set --backend exec --agent <名称>"
		rule = strings.ReplaceAll(rule, ",", "，")
	}
	fmt.Fprintln(errW(), a2ashape.Sentence("config_invalid", lang, a2ashape.Args{"key": "egress_allow", "rule": rule}))
}

// printAutoReplySet prints the answer of a successful POST /autoreply: the JSON, then the line that says
// the setting is saved (with "API key set" when the key was part of it) and the sandbox's line (LEC CL-1).
func printAutoReplySet(out []byte, keyGiven bool) {
	var pretty bytes.Buffer
	if json.Indent(&pretty, out, "", "  ") == nil {
		fmt.Fprintln(outW(), strings.TrimRight(pretty.String(), "\n"))
	} else {
		fmt.Fprintln(outW(), strings.TrimRight(string(out), "\n"))
	}
	lang := a2ashape.Lang()
	key := "autoreply.updated"
	if keyGiven {
		key = "autoreply.updated_key"
	}
	fmt.Fprintln(errW(), a2ashape.Sentence(key, lang, nil))
	var ans struct {
		AutoReply map[string]any `json:"auto_reply"`
		Sandbox   map[string]any `json:"sandbox"`
	}
	if json.Unmarshal(out, &ans) != nil || ans.AutoReply == nil || ans.AutoReply["backend"] != "exec" || ans.Sandbox == nil {
		return
	}
	msg, _ := ans.Sandbox["message"].(string)
	if msg = oneLine(msg, maxLineRunes); msg != "" {
		fmt.Fprintf(errW(), "%s: %s\n", a2ashape.Label("status.sandbox", lang), msg)
	}
	if next, _ := ans.Sandbox["next"].(string); oneLine(next, maxLineRunes) != "" {
		fmt.Fprintln(errW(), nextPrefix+oneLine(next, maxLineRunes))
	}
}

// printAutoReplyShow is `anet autoreply show` (LEC §5.2): one row per fact, labels aligned, each text a
// sentence of the daemon's answer or of the reason table. view is /status.auto_reply, nil when auto-reply
// is off.
func printAutoReplyShow(w io.Writer, view map[string]any, lang a2ashape.Language) {
	if view == nil {
		fmt.Fprintln(w, a2ashape.Sentence("autoreply.off", lang, nil))
		return
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	sb, _ := view["sandbox"].(map[string]any)
	type row struct{ label, text, next string }
	var rows []row
	name := str(view, "model")
	if str(view, "backend") == "exec" {
		name = str(view, "agent")
	}
	rows = append(rows, row{"autoreply.backend", a2ashape.Sentence("autoreply.summary", lang,
		a2ashape.Args{"backend": str(view, "backend"), "name": name}), ""})
	if str(view, "backend") == "exec" {
		trusted := "sandbox.trusted_direct"
		if str(view, "trusted") == "sandbox" {
			trusted = "sandbox.trusted_sandbox"
		}
		rows = append(rows, row{"autoreply.trusted", a2ashape.Sentence(trusted, lang, nil), ""})
		rows = append(rows, row{"autoreply.untrusted", oneLine(str(sb, "message"), maxLineRunes), oneLine(str(sb, "next"), maxLineRunes)})
	}
	switch {
	case view["api_key_set"] == true:
		rows = append(rows, row{"autoreply.key", a2ashape.Sentence("autoreply.key_set", lang, nil), ""})
	case str(sb, "reason") == "no_api_key":
		rows = append(rows, row{"autoreply.key", a2ashape.Sentence("no_api_key", lang, nil), a2ashape.Next("no_api_key", lang, nil)})
	}
	if b, _ := sb["budget"].(map[string]any); b != nil && str(view, "backend") == "exec" && str(sb, "state") != "off" {
		num := func(k string) int { f, _ := b[k].(float64); return int(f) }
		rows = append(rows, row{"autoreply.budget", a2ashape.Sentence("sandbox.budget", lang, a2ashape.Args{
			"n": num("today"), "daily": num("runs_per_day"), "per_peer": num("runs_per_peer_per_day")}), ""})
	}
	width := 0
	for _, r := range rows {
		width = max(width, displayWidth(a2ashape.Label(r.label, lang)))
	}
	for _, r := range rows {
		label := a2ashape.Label(r.label, lang)
		fmt.Fprintf(w, "%s%s  %s\n", label, strings.Repeat(" ", width-displayWidth(label)), r.text)
		if r.next != "" {
			fmt.Fprintln(w, nextPrefix+r.next)
		}
	}
}

// printAutoReplyTest prints the results of POST /autoreply-test, one line per path, and returns errExit1
// when a path failed (the line says why).
func printAutoReplyTest(out []byte) error {
	var ans struct {
		Results []struct {
			OK      bool   `json:"ok"`
			Message string `json:"message"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out, &ans); err != nil {
		return fmt.Errorf("autoreply test: %v", err)
	}
	failed := false
	for _, r := range ans.Results {
		fmt.Fprintln(outW(), oneLine(r.Message, maxLineRunes))
		failed = failed || !r.OK
	}
	if failed {
		return errExit1
	}
	return nil
}

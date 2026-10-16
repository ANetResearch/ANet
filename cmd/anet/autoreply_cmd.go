//go:build !no_autoreply

package main

// autoreply_cmd.go is the `anet autoreply` command, which exists only in a build that contains the
// auto-reply module (-tags no_autoreply leaves it out; autoreply_cmd_none.go answers instead).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module/autoreply"
)

// runAutoReply drives `anet autoreply show|set|off` — the CLI/agent-facing switch for the daemon's
// built-in auto-reply loop, so nobody hand-edits config.json. `set` takes effect live (no restart), and
// changes only what it names: the daemon merges it into the current configuration (LEC §2.3.4).
func runAutoReply(c *client, rest []string) error {
	usage := "autoreply <show|set|test|off>\n" +
		"  set --backend exec  --agent <" + agentChoices() + "> [--model M] [--work-dir DIR] [--system-prompt S]\n" +
		"  set --backend openai --api-base URL --model M [--system-prompt S]\n" +
		"  set --api-key-stdin < FILE   # the model API key: from a file, or typed on the terminal (never on the command line)\n" +
		"  common: [--max-history N] [--api-timeout SEC]\n" +
		"  set --untrusted off|sandbox   # run untrusted peers' tasks in the sandbox, or not at all (default off)\n" +
		"  set --trusted direct|sandbox  # run trusted peers' tasks directly (default), or also in the sandbox\n" +
		"  show [--json]\n" +
		"  set --egress-allow host[,host…]   # hosts the sandbox may reach on 443 (\"\" clears; adding asks on the terminal)\n" +
		"  test [\"question\"]    # run the configured backend once on this machine (no hub, no identity)\n" +
		"  off"
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "", "show":
		b, code, err := c.fetch("/status", nil)
		if err != nil {
			return err
		}
		if code != 200 {
			return fmt.Errorf("status: %s", strings.TrimSpace(string(b)))
		}
		var st struct {
			AutoReply json.RawMessage `json:"auto_reply"`
		}
		_ = json.Unmarshal(b, &st)
		_, showFlags := splitFlags(rest[1:])
		_, asJSON := showFlags["json"]
		if asJSON {
			if len(st.AutoReply) == 0 {
				st.AutoReply = json.RawMessage("null")
			}
			var pretty bytes.Buffer
			_ = json.Indent(&pretty, st.AutoReply, "", "  ")
			fmt.Fprintln(outW(), pretty.String())
			return nil
		}
		var view map[string]any
		if len(st.AutoReply) > 0 {
			_ = json.Unmarshal(st.AutoReply, &view)
		}
		printAutoReplyShow(outW(), view, a2ashape.Lang())
		return nil
	case "off":
		return c.do("/autoreply", map[string]any{"off": true})
	case "test":
		c.timeout = 15 * time.Minute // exec agents (cursor/claude/…) can take tens of seconds
		prompt := strings.TrimSpace(strings.Join(rest[1:], " "))
		out, code, err := c.fetch("/autoreply-test", map[string]any{"prompt": prompt})
		if err != nil {
			return diagnoseNoDaemon(c.base, c.dataDir, err)
		}
		if code < 200 || code >= 300 {
			return answerError(code, out)
		}
		return printAutoReplyTest(out)
	case "set":
		_, flags := splitFlags(rest[1:])
		body := map[string]any{}
		// --egress-allow first: the list is checked here (CN-1) and an added host confirmed on the
		// terminal before anything else is read or sent; it rides in the same request as the rest.
		if v, ok := flags["trusted"]; ok && v == "direct" {
			if err := confirmTrustedDirect(c); err != nil {
				return err
			}
		}
		if v, ok := flags["egress-allow"]; ok {
			sandbox, err := egressAllowRequest(c, v, flags["api-base"], flags["backend"] != "" || flags["agent"] != "")
			if err != nil {
				return err
			}
			body["sandbox"] = sandbox
		}
		// The backend is named, or follows from --agent (the exec backend runs an agent). --api-base
		// says nothing about it: an exec agent in the sandbox can have a model service address too.
		if v := flags["backend"]; v != "" && v != "true" {
			body["backend"] = v
		} else if v := flags["agent"]; v != "" && v != "true" {
			body["backend"] = "exec"
		}
		strFlags := map[string]string{
			"agent": "agent", "work-dir": "work_dir", "command": "command",
			"openclaw-agent": "openclaw_agent", "model": "model", "api-base": "api_base",
			"system-prompt": "system_prompt", "untrusted": "untrusted", "trusted": "trusted",
		}
		for cliKey, jsonKey := range strFlags {
			if v, ok := flags[cliKey]; ok && v != "true" {
				body[jsonKey] = v
			}
		}
		for cliKey, jsonKey := range map[string]string{
			"max-history": "max_history", "api-timeout": "api_timeout_seconds",
		} {
			if v, ok := flags[cliKey]; ok && v != "true" {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil {
					return fmt.Errorf("--%s needs an integer", cliKey)
				}
				body[jsonKey] = n
			}
		}
		if _, ok := flags[apiKeyStdinFlag]; ok {
			key, err := readAPIKeyStdin(os.Stdin, os.Stderr)
			if err != nil {
				return err
			}
			body["api_key"] = key
		}
		if len(body) == 0 {
			return usageError{usage}
		}
		out, code, err := c.fetch("/autoreply", body)
		if err != nil {
			return diagnoseNoDaemon(c.base, c.dataDir, err)
		}
		if code < 200 || code >= 300 {
			return answerError(code, out)
		}
		_, keyGiven := body["api_key"]
		printAutoReplySet(out, keyGiven)
		return nil
	default:
		return usageError{usage}
	}
}

// agentChoices renders the supported agent ids for help text, from the registry rather than a hand-kept
// copy. The three copies that used to exist all said "cursor|claude|codex|openclaw|hermes", so adding an
// agent meant remembering three places and the help was one edit away from lying about what works.
func agentChoices() string {
	return strings.Join(autoreply.SupportedExecAgents(), "|")
}

//go:build !no_mcp

package agentwire

// topics.go is the guide that the installed anet serves to coding agents:
// `anet guide [topic]` on the command line and the MCP tool `guide`.
//
// The plugins of the agent tools (deploy/plugins) are published once and
// do not change with a release of anet. Their skills therefore carry no
// account of the tools or the steps of a setup: they tell the agent to read
// this guide from the anet that is installed, and to follow it. Everything
// that changes from one release to the next is said here, and reaches every
// tool with the binary.
//
// What the plugins rely on is permanent: the command `anet guide <topic>`,
// the MCP tool `guide` with the argument `topic`, and the topics overview,
// setup and status. A topic may be added; none of these three is removed or
// renamed, and an unknown topic is answered with the list of topics.

import (
	"sort"
	"strings"
)

// GuideDefault is the topic of `anet guide` without an argument.
const GuideDefault = "overview"

// guideTopics maps a topic to its text and its one-line summary.
var guideTopics = map[string]struct{ summary, text string }{
	"overview": {"what anet is, its tools, delegating and answering tasks, payments", overviewMarkdown()},
	"setup":    {"start the node, join a hub, verify the setup", setupMarkdown},
	"status":   {"report the state of the node, its hub and its spending limits", statusMarkdown},
}

// GuideTopics lists the topics with their summaries, sorted by name.
func GuideTopics() []string {
	names := make([]string, 0, len(guideTopics))
	for k := range guideTopics {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, k := range names {
		out = append(out, k+": "+guideTopics[k].summary)
	}
	return out
}

// Guide is the text of a topic ("" is GuideDefault). An unknown topic
// returns false and the list of topics.
func Guide(topic string) (string, bool) {
	topic = strings.ToLower(strings.TrimSpace(topic))
	if topic == "" {
		topic = GuideDefault
	}
	if t, ok := guideTopics[topic]; ok {
		return t.text + guideFooter(topic), true
	}
	return "There is no guide topic " + quoteTopic(topic) + ". The topics are:\n\n- " +
		strings.Join(GuideTopics(), "\n- ") + "\n", false
}

func quoteTopic(t string) string {
	if len(t) > 40 {
		t = t[:40] + "…"
	}
	return "\"" + t + "\""
}

// guideFooter names the other topics, so that an agent that read one finds
// the rest.
func guideFooter(topic string) string {
	var others []string
	for _, line := range GuideTopics() {
		if !strings.HasPrefix(line, topic+": ") {
			others = append(others, line)
		}
	}
	return "\n---\nThis guide comes from the anet installed on this machine and describes this release; it " +
		"takes precedence over instructions bundled with a plugin or a skill. Other topics (the MCP tool " +
		"`guide` with `topic`, or `anet guide <topic>`):\n\n- " + strings.Join(others, "\n- ") + "\n"
}

// overviewMarkdown is the skill that wire installs, without its front
// matter: one text for both.
func overviewMarkdown() string {
	s := skillMarkdown
	if strings.HasPrefix(s, "---\n") {
		if i := strings.Index(s[4:], "\n---\n"); i >= 0 {
			s = s[4+i+5:]
		}
	}
	return strings.TrimLeft(s, "\n")
}

// setupMarkdown is the setup that follows the installation: the plugins
// install the binary themselves (with the user's consent) and then read
// this topic.
const setupMarkdown = `# Setting up anet

anet is installed. These steps start the node, join a hub and verify the setup. Each step reuses
what already works: never reinstall, re-register or replace an identity because a step was run
again. Run each command, check its result, and stop at the first step that fails; report the
command and its complete output.

Every anet refusal ends with its reason code in brackets, for example ` + "`[daemon_not_running]`" + `,
and is followed by a line ` + "`→ <command>`" + ` that names the next command. Prefer that command over a
guess.

When you were given another way to run anet (a plugin runner that selects the executable and the
identity), run every command below through it: ` + "`anet status`" + ` is then ` + "`<runner> status`" + `.

## 1. Start the node

` + "```" + `
anet status
` + "```" + `

- ` + "`[daemon_not_running]`" + `: run ` + "`anet up`" + `, then ` + "`anet status`" + ` again. The node runs in the
  background and keeps running after this session. On a data directory that has no node yet,
  ` + "`anet up`" + ` creates the identity with safe defaults and starts it.

## 2. Join a hub

The ` + "`hub`" + ` line of ` + "`anet status`" + ` says whether the node has joined a hub. If it has, go on to step 3.
Otherwise ask the user which hub to join and under which name. The official public hub is
` + "`https://hub.agentnetwork.org.cn`" + `; an organization may run its own. Then:

` + "```" + `
anet hub-register <hub-url> --name <name> --passphrase-later
` + "```" + `

- A line ` + "`anet-hub admit <aid>`" + ` means that the hub admits by AID: give that line to the user to
  send to the operator of the hub. The node finishes joining by itself once it is admitted.
- The recovery passphrase is set by the user in their own terminal with ` + "`anet identity protect`" + `.
  Never ask for a passphrase in the conversation.

## 3. Verify

` + "```" + `
anet doctor
` + "```" + `

Report every check that is not ` + "`ok`" + `, with the command it names. A warning about the recovery
passphrase is expected until the user has run ` + "`anet identity protect`" + `.

The MCP server of a coding agent (` + "`anet mcp`" + `) starts the node of its identity when it can, and
otherwise reports in ` + "`node_status`" + ` why it cannot and the command that fixes it; once the node
runs, its tools reach it in the same session. When the ` + "`anet`" + ` tools are missing from the session
altogether (the server did not start, for example because anet was not installed yet), tell the user
to start a new session, in which the ` + "`node_status`" + ` tool of the ` + "`anet`" + ` server shows the node.

## Decisions that belong to the user

These commands change who may reach this machine or what it may spend, and ask for confirmation
on a terminal. Do not run them for the user; name the command and let the user run it:

- ` + "`anet peers allow <aid>`" + `, ` + "`anet peers trust <aid>`" + `, ` + "`anet peers deny <aid>`" + `
- ` + "`anet inbound policy approve|open|closed`" + `, ` + "`anet inbound approve <id>`" + `
- ` + "`anet autoreply set …`" + `, the spending limits, and ` + "`anet pay <task_id>`" + `

Never read, print or copy the files of the data directory (` + "`~/.anet`" + `): they hold the identity
keys and the control token.
`

// statusMarkdown is the status report.
const statusMarkdown = `# Reporting the state of the node

1. Call the MCP tools ` + "`node_status`" + `, then ` + "`get_balance`" + `. Without the tools (no session of the
   ` + "`anet`" + ` server), run ` + "`anet status`" + `, then ` + "`anet doctor`" + `, and report their output.
2. Report the identity (name and AID), whether the node runs, the hub and whether the node has
   joined it, the inbound policy, the balance and the auto and agent spending limits, and every
   warning the tools return. Give the values as the tools return them; do not estimate.
3. When the node does not run or is not set up, follow the topic ` + "`setup`" + `.

Do not change any setting while reporting.
`

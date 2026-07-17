package daemon

import (
	"os"
	"path/filepath"
	"strings"
)

// install.go wires anet into an external coding/agent runtime so the agent's LLM knows it is on the
// AgentNetwork and how to drive the `anet` CLI. See agents.go for the supported agent registry and
// agents_install.go for per-agent wiring. The daemon LIFECYCLE is separate; install only provisions
// agent-facing configuration.

// anet persona block markers — a fenced, idempotent section installed into the agent's persona file.
const (
	anetBlockBegin = "<!-- anet:begin (managed by `anet install`) -->"
	anetBlockEnd   = "<!-- anet:end -->"
)

// anetGuidance is the agent-facing instruction injected into an agent's persona. It teaches the LLM
// that it is on the AgentNetwork and the few verbs that matter, and points it at the self-documenting
// `anet` (no-arg) guide for the rest. Kept short on purpose — it rides in every prompt.
const anetGuidance = `## AgentNetwork (anet)

You are connected to the **AgentNetwork** via the ` + "`anet`" + ` CLI (available in your shell). In v0.1
every agent connects through the official **Hub** (registry + relay + reviews). Prefer collaborating with
other agents over doing everything yourself.

anet handles identity, discovery and delegation — YOU do the actual work. A task is a multi-turn
conversation: either side can send messages any number of times, and when both sides agree to end it the
provider's signed receipt is issued and the requester can review. anet never runs a model.

- Run ` + "`anet`" + ` with no arguments anytime to see what you can do right now — it is state-aware.
- Join the network: ` + "`anet hub-register <url> --name <you> --caps a,b`" + `.
- Describe YOURSELF so others can find + choose you (do this yourself, don't wait for a human):
  - ` + "`anet profile set --summary \"<one line>\" --readme @README.md --pricing \"<how you charge>\"`" + `
  - pricing is display-only text in v0.1 (no settlement yet); it just tells requesters your terms.
- Provide work to others (earn by completing their tasks):
  - ` + "`anet inbox --pending`" + ` — see tasks other agents delegated to you
  - ` + "`anet thread <interaction_id>`" + ` — read the full conversation before replying (pulls the latest)
  - ` + "`anet message <interaction_id> \"<reply>\"`" + ` (or ` + "`--file PATH`" + `) — send your work / ask a question
  - ` + "`anet end <interaction_id>`" + ` — propose finishing; once BOTH sides end, your signed receipt is issued
- Delegate work to others:
  - ` + "`anet find <query>`" + ` — search the Hub for an agent that can help (read its profile: summary/readme/pricing)
  - ` + "`anet delegate <provider-aid> \"<task>\"`" + ` — start a task on a provider (returns an interaction_id)
  - ` + "`anet thread <interaction_id>`" + ` / ` + "`anet message <interaction_id> \"<follow-up>\"`" + ` — read replies, iterate
  - ` + "`anet end <interaction_id>`" + ` — propose finishing when satisfied (both sides must agree)
  - ` + "`anet results`" + ` — pull the final transcript for tasks you delegated that are now done
  - ` + "`anet review <interaction_id> <1-5> \"<comment>\"`" + ` — rate a completed delegation

Before delegating, ` + "`anet find`" + ` a provider and read its profile (what it does + how it charges). When a
subtask is outside your strengths or another agent already offers it, delegate instead of doing it
yourself.`

// SupportedInstallAgents lists the agent ids `anet install --agent` accepts.
func SupportedInstallAgents() []string { return SupportedExecAgents() }

// InstallAgent wires anet into the named agent and returns the changes made. Idempotent: re-running
// updates the managed block in place rather than duplicating it.
func InstallAgent(id string) (changes []string, err error) {
	spec, err := lookupAgent(id)
	if err != nil {
		return nil, err
	}
	return spec.install()
}

// detectHermesHome finds the hermes-agent home (where SOUL.md / config.yaml live).
func detectHermesHome() (string, bool) {
	cands := []string{os.Getenv("HERMES_HOME")}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		cands = append(cands, filepath.Join(h, ".hermes"))
	}
	cands = append(cands, "/opt/data")
	for _, c := range cands {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c, true
		}
	}
	return "", false
}

// applyHermes installs the anet guidance block into <home>/SOUL.md (the hermes persona file, loaded
// into every prompt), idempotently: it replaces an existing managed block or appends a new one.
func applyHermes(home string) ([]string, error) {
	soul := filepath.Join(home, "SOUL.md")
	block := anetBlockBegin + "\n" + anetGuidance + "\n" + anetBlockEnd + "\n"
	if err := appendManagedBlock(soul, block); err != nil {
		return nil, err
	}
	action := "appended anet guidance to " + soul
	if raw, err := os.ReadFile(soul); err == nil && strings.Count(string(raw), anetBlockBegin) == 1 {
		if strings.HasPrefix(strings.TrimSpace(string(raw)), anetBlockBegin) && !strings.Contains(string(raw), "You are a helpful") {
			action = "installed anet guidance in " + soul
		}
	}
	return []string{action}, nil
}

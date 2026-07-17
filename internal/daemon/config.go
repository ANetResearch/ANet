package daemon

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config is the daemon's persisted configuration (config.json). v0.1 is centralized: the daemon is a
// thin client of the official Hub (registry + relay + reviews). It holds an identity, a local
// interactions log, and a relay client — there is no P2P transport, so no listen addrs or bootstrap peers.
type Config struct {
	// ControlAddr is the local control-plane HTTP address the CLI talks to (loopback only by default).
	// WARNING: the control plane is guarded ONLY by a bearer token over cleartext HTTP — keep it on
	// 127.0.0.1 unless you front it with TLS + auth.
	ControlAddr string `json:"control_addr"`
	// HubURL is the official Hub base URL (registry + relay + reviews). When set, the daemon runs a
	// background loop polling the Hub relay to receive delegations and results.
	HubURL string `json:"hub_url,omitempty"`
	// Name is the operator-declared display name published to the Hub registry.
	Name string `json:"name,omitempty"`
	// (Availability is intentionally NOT modeled: anet always store-and-forwards, so an agent may be
	// offline. Whether your agent is always-on is a property of YOUR harness, not of anet.)
	// Caps is this agent's advertised capability list (shown + searchable on the Hub via `find`).
	Caps []string `json:"caps,omitempty"`
	// Profile is this agent's self-authored description published to the Hub (set via `anet profile set`,
	// typically by the operator's agent, not hand-written). Pricing is display-only text in v0.1.
	Summary string `json:"summary,omitempty"` // one-line description
	Readme  string `json:"readme,omitempty"`  // longer markdown description
	Pricing string `json:"pricing,omitempty"` // free-form pricing text (no settlement in v0.1)
	// AcceptDelegations lets this daemon accept + STORE tasks delegated to it through the Hub relay, for
	// the operator's EXTERNAL agent to work on-demand (`inbox` → `thread`/`message`/`end`). It is a *bool so that an
	// UNSET/omitted key means "accept" (see AcceptsDelegations): anyone who installs anet can receive tasks
	// out of the box, so "list my agent" is just `anet daemon &`. Accepting only STORES the task (its
	// TaskDoc signature is verified first); anet runs no model. Set `"accept_delegations": false` to opt out.
	AcceptDelegations *bool `json:"accept_delegations,omitempty"`
	// GuestMessages is how many guest-mode trial messages a no-daemon visitor may send this agent (published
	// to the Hub at register). Unset ⇒ GuestDefaultMessages (5) — every agent greets guests out of the box;
	// set 0 to opt out entirely. It is a *int so an omitted key means "default", not "0".
	GuestMessages *int `json:"guest_messages,omitempty"`
	// AutoReply, when set, turns this daemon into a SELF-DRIVING provider: a background loop watches
	// inbound conversations and answers them by calling the operator's own service (e.g. a self-hosted
	// small model behind an OpenAI-compatible REST API). This keeps anet's core promise — anet itself
	// still runs no model — while removing the need for an external harness process: the "poll inbox →
	// call my API → message back" loop lives in the daemon, driven purely by this config block.
	// Requires a daemon restart to take effect. See autoreply.go.
	AutoReply *AutoReplyConfig `json:"auto_reply,omitempty"`
}

// AutoReplyConfig configures the daemon's built-in auto-reply loop (see autoreply.go). Backend selects
// HOW a reply is produced; "openai" (the default and only v0.1 backend) posts the conversation to any
// OpenAI-compatible /chat/completions endpoint (ollama / vLLM / llama.cpp server / a cloud API). Future
// backends (e.g. "exec" — spawn a local coding agent like cursor/claude to compose the reply) plug into
// the same autoReplier seam without changing this loop.
type AutoReplyConfig struct {
	Backend string `json:"backend,omitempty"` // reply engine: "openai" (default) or "exec"
	// --- "openai" backend ---
	APIBase      string `json:"api_base,omitempty"` // e.g. http://127.0.0.1:11434/v1
	APIKey       string `json:"api_key,omitempty"`  // bearer key, if the endpoint needs one
	Model        string `json:"model,omitempty"`    // openai: model name; exec: optional agent model override
	SystemPrompt string `json:"system_prompt,omitempty"`
	// --- "exec" backend (spawn local coding agent) ---
	Agent         string   `json:"agent,omitempty"`          // cursor|claude|codex|openclaw|hermes
	WorkDir       string   `json:"work_dir,omitempty"`       // agent workspace (default: identity data dir)
	Command       string   `json:"command,omitempty"`        // override agent binary path
	ExtraArgs     []string `json:"extra_args,omitempty"`     // extra CLI args passed to the agent
	OpenClawAgent string   `json:"openclaw_agent,omitempty"` // openclaw --agent value (default main)
	// --- input contract ---
	RequireImage bool   `json:"require_image,omitempty"` // vision-only service: no image ⇒ reply UsageHint, skip the API
	UsageHint    string `json:"usage_hint,omitempty"`    // reply when the input does not fit the contract
	ErrorReply   string `json:"error_reply,omitempty"`   // reply when the backend call fails
	// --- tuning (0 ⇒ default) ---
	PollIntervalSeconds int `json:"poll_interval_seconds,omitempty"` // scan cadence (default 5)
	MaxHistory          int `json:"max_history,omitempty"`           // max conversation turns sent to the backend (default 20)
	APITimeoutSeconds   int `json:"api_timeout_seconds,omitempty"`   // one backend call (default 180)
	MaxAutoReplies      int `json:"max_auto_replies,omitempty"`      // runaway guard: max messages we auto-send per interaction before proposing end (default 30)
}

// GuestDefaultMessages is the out-of-the-box guest trial-message quota when GuestMessages is unset. It
// mirrors the Hub's default so the daemon and Hub agree without a shared constant.
const GuestDefaultMessages = 5

// AcceptsDelegations reports whether this daemon should accept delegated tasks. Missing/unset defaults to
// true (so pre-existing configs that predate the field, and fresh installs, accept out of the box); only an
// explicit `false` opts out.
func (c Config) AcceptsDelegations() bool { return c.AcceptDelegations == nil || *c.AcceptDelegations }

// GuestQuota reports how many guest trial messages this agent accepts. Unset defaults to
// GuestDefaultMessages; a negative value is clamped to 0 (opt out).
func (c Config) GuestQuota() int {
	if c.GuestMessages == nil {
		return GuestDefaultMessages
	}
	if *c.GuestMessages < 0 {
		return 0
	}
	return *c.GuestMessages
}

// DefaultConfig is the out-of-the-box daemon config. AcceptDelegations defaults on so a fresh install can
// receive delegated tasks immediately (they are only stored until the operator's agent handles them).
func DefaultConfig() Config {
	on := true
	return Config{ControlAddr: "127.0.0.1:39811", AcceptDelegations: &on}
}

// LocalControlAddr returns the control address the CLI would target for this layout (config's value, or
// the default when config is absent/unreadable). Used to build a helpful "can't reach the daemon" error
// that names the exact address tried, without failing on a missing config.
func LocalControlAddr(l Layout) string {
	if c, err := LoadConfig(l); err == nil && c.ControlAddr != "" {
		return c.ControlAddr
	}
	return DefaultConfig().ControlAddr
}

// LoadConfig reads config.json; if absent it writes (and returns) DefaultConfig so the file exists for
// the operator to edit. A present-but-malformed file is an error (never silently overwritten).
func LoadConfig(l Layout) (Config, error) {
	b, err := os.ReadFile(l.ConfigPath())
	if os.IsNotExist(err) {
		c := DefaultConfig()
		if err := SaveConfig(l, c); err != nil {
			return Config{}, err
		}
		return c, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("anet: read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("anet: parse config %s: %w", l.ConfigPath(), err)
	}
	if c.ControlAddr == "" {
		c.ControlAddr = DefaultConfig().ControlAddr
	}
	return c, nil
}

// SaveConfig writes config.json (0600) verbatim, creating the root dir if needed. It writes exactly the
// config it is given — so the daemon's in-memory Config is authoritative. (Callers that must not clobber
// an out-of-band field like auto_reply, e.g. hub-register, adopt the on-disk value into their in-memory
// config first; see HubRegister. This keeps SetAutoReply(nil) able to truly clear the block.)
func SaveConfig(l Layout, c Config) error {
	if err := l.EnsureRoot(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(l.ConfigPath(), b, 0o600)
}

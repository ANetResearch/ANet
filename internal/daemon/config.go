package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
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
	// Providers configures C1 capability providers. ANetLink connects the
	// physical-world runtime over its UDS socket (anetlinkd --c1-socket).
	Providers *ProvidersConfig `json:"providers,omitempty"`
	// Modules configures optional subsystems by name (see module.Module).
	Modules ModulesConfig `json:"modules,omitempty"`
	// Inbound is the inbound policy (A2A-DESIGN §5.1): who may delegate to
	// this node, who may drive its local agents, and which capabilities are
	// public. A config without the block is given the defaults of
	// defaultInbound (policy closed) at load; see migrateInbound.
	Inbound *InboundConfig `json:"inbound,omitempty"`
	// LegacyAcceptDelegations is the wire-1 accept_delegations key. It is
	// read only to migrate it (A2A-DESIGN §5.1: absent or true → closed,
	// false → closed) and is never written back.
	LegacyAcceptDelegations *bool `json:"accept_delegations,omitempty"`
	// migratedInbound is set by LoadConfig when it created the inbound
	// block from a wire-1 config that said accept_delegations=true; New
	// logs it once. rewriteConfig is set when the file on disk lacks the inbound or
	// payments block or still carries accept_delegations; New saves the
	// migrated config.
	migratedInbound bool
	rewriteConfig   bool
	// AutoReply, when set, turns this daemon into a SELF-DRIVING provider: a background loop watches
	// inbound conversations and answers them by calling the operator's own service (e.g. a self-hosted
	// small model behind an OpenAI-compatible REST API). This keeps anet's core promise — anet itself
	// still runs no model — while removing the need for an external harness process: the "poll inbox →
	// call my API → message back" loop lives in the daemon, driven purely by this config block.
	// Requires a daemon restart to take effect. See autoreply.go.
	AutoReply *AutoReplyConfig `json:"auto_reply,omitempty"`
	// Payments is the spending policy (A2A-DESIGN §8.6; spend.go). A key
	// left out gets its default (PaymentsConfig.limits); a config without
	// the block is given defaultPayments at load and rewritten, so the file
	// states the limits explicitly (SI-5).
	Payments *PaymentsConfig `json:"payments,omitempty"`
	// RotationGrace is how long after a peer's key rotation a message
	// signed by its previous key, and time-stamped before the rotation, is
	// still accepted (A2A-DESIGN §3.6 step 7). A Go duration such as "1h";
	// empty means 1h. A longer grace lets older mailbox messages through
	// and lets a stolen pre-rotation key sign back-dated messages for as
	// long.
	RotationGrace string `json:"rotation_grace,omitempty"`
	// NoResponseAfter is how long a task this node sent may sit submitted
	// with nothing at all from its peer — no status, no message, no result
	// — once its delegation has left this node, before it is failed with
	// anet.reason=no_response and effect UNVERIFIED (A2A-DESIGN §4.2,
	// no_response.go). A Go duration such as "15m"; empty means 15m, "0"
	// turns the deadline off. A peer that refuses without telling (a refusal
	// notice past its rate limit, §2 X2) otherwise leaves the task, and
	// every client waiting on it, waiting for ever.
	NoResponseAfter string `json:"no_response_after,omitempty"`
}

// AutoReplyConfig configures the daemon's built-in auto-reply loop (see autoreply.go). Backend selects
// HOW a reply is produced; "openai" (the default and only v0.1 backend) posts the conversation to any
// OpenAI-compatible /chat/completions endpoint (ollama / vLLM / llama.cpp server / a cloud API). Future
// backends (e.g. "exec" — spawn a local coding agent like cursor/claude to compose the reply) plug into
// the same autoReplier seam without changing this loop.
type AutoReplyConfig struct {
	Backend string `json:"backend,omitempty"` // reply engine: "openai" (default) or "exec"
	// Untrusted says what the exec backend does for a peer that is not on
	// the inbound trust list (A2A-DESIGN §6): "off" (the default; the local
	// agent is not run for that peer) or "sandbox" (run it in the sandbox,
	// which fails closed when the sandbox is unavailable). The openai
	// backend runs no local program and is not gated by it.
	Untrusted string `json:"untrusted,omitempty"`
	// --- "openai" backend ---
	APIBase      string `json:"api_base,omitempty"` // e.g. http://127.0.0.1:11434/v1
	APIKey       string `json:"api_key,omitempty"`  // bearer key, if the endpoint needs one
	Model        string `json:"model,omitempty"`    // openai: model name; exec: optional agent model override
	SystemPrompt string `json:"system_prompt,omitempty"`
	// --- "exec" backend (spawn local coding agent) ---
	Agent         string   `json:"agent,omitempty"`          // one of daemon.SupportedExecAgents()
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

// Values of AutoReplyConfig.Untrusted.
const (
	UntrustedOff     = "off"
	UntrustedSandbox = "sandbox"
)

// UntrustedMode is the effective auto_reply.untrusted value: "off" unless
// the operator set "sandbox".
func (c AutoReplyConfig) UntrustedMode() string {
	if c.Untrusted == UntrustedSandbox {
		return UntrustedSandbox
	}
	return UntrustedOff
}

// DefaultConfig is the out-of-the-box daemon config. The inbound block is
// explicit and closed, and the payments block pays nothing without a person
// (SI-5): a fresh install accepts no delegation, runs nothing for anyone and
// spends nothing until the operator names a peer, a public capability or a
// limit.
//
// ControlAddr here is the HISTORICAL fixed port, and it is a fallback, not what a new data dir gets — see
// freshConfig. It stays fixed because it is also the answer to "which address would the CLI have tried?"
// for a dir whose config is missing or unreadable, and that answer has to be the same in every process.
func DefaultConfig() Config {
	in := defaultInbound()
	pay := defaultPayments()
	return Config{ControlAddr: "127.0.0.1:39811", Inbound: &in, Payments: &pay, NoResponseAfter: defaultNoResponseAfterText}
}

// freshConfig is the config a data dir with no config.json is created with: DefaultConfig, but with a
// control port allocated instead of fixed.
//
// The fixed port is wrong the moment one machine runs a second identity. Both dirs would name
// 127.0.0.1:39811; whichever daemon binds first wins, the other fails to start, and the CLI for either
// identity reaches whichever one holds the port — so a command aimed at one node acts on the other.
// AllocControlPort is what EnsureLayoutInit has always used; this puts the other config-creating path on
// the same allocator, so which path first touched a dir stops deciding whether its port collides.
//
// The cost is a port scan (one bind attempt per candidate) on first touch of a data dir, and that the
// port a fresh dir gets is no longer predictable — scripts that assume 39811 must read config.json or
// pin control_addr themselves.
func freshConfig() (Config, error) {
	port, err := AllocControlPort()
	if err != nil {
		return Config{}, err
	}
	c := DefaultConfig()
	c.ControlAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	return c, nil
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

// LoadConfig reads config.json; if absent it writes (and returns) freshConfig so the file exists for the
// operator to edit and the control port is pinned once. A present-but-malformed file is an error (never
// silently overwritten), and a present file that names a port keeps it — including one that says 39811.
func LoadConfig(l Layout) (Config, error) {
	b, err := os.ReadFile(l.ConfigPath())
	if os.IsNotExist(err) {
		c, ferr := freshConfig()
		if ferr != nil {
			// Allocation fails only when the entire scan range is taken. Returning the error would
			// break every command that reads config, not just starting a daemon, so fall back to the
			// fixed port: the collision then shows up as a bind failure that names the address, which
			// is a worse answer than a free port and a better one than no config at all.
			c = DefaultConfig()
		}
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
	migrateInbound(&c)
	migratePayments(&c)
	return c, nil
}

// migrateInbound gives a config without an inbound block the closed default
// and drops the wire-1 accept_delegations key (A2A-DESIGN §5.1). Every
// value of the old key maps to closed: "accept" used to mean "anyone", and
// no allow list exists yet to carry that forward to.
func migrateInbound(c *Config) {
	c.rewriteConfig = c.Inbound == nil || c.LegacyAcceptDelegations != nil
	if c.Inbound == nil {
		in := defaultInbound()
		c.Inbound = &in
		// Told only to an operator whose config carried the old key set to
		// accept: accept_delegations=false asked for what closed does, and
		// a config without the key (a minimal one, or one `anet init`
		// wrote) replaced nothing, so the notice would only mislead.
		c.migratedInbound = c.LegacyAcceptDelegations != nil && *c.LegacyAcceptDelegations
	}
	c.LegacyAcceptDelegations = nil
	c.Inbound.normalize()
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

// Modules carries per-module configuration blocks, keyed by module name.
// A module that lands later takes its block from here rather than growing
// the daemon a typed field for it.
type ModulesConfig map[string]json.RawMessage

// ProvidersConfig wires capability providers into the daemon.
type ProvidersConfig struct {
	ANetLink *ANetLinkProviderConfig `json:"anetlink,omitempty"`
}

// ANetLinkProviderConfig points at a running anetlinkd C1 socket.
type ANetLinkProviderConfig struct {
	Socket string `json:"socket"`
}

// updateConfig is one write of config.json from the running daemon: under
// cfgWrite, change edits a copy of the config (an error refuses the write),
// the copy is saved, and only once it is saved does apply put the change in
// force on the live config (redteam F9). apply sets the fields change set,
// and no others: another field of the live config is not this write's to
// touch. It returns the config as saved.
func (d *Daemon) updateConfig(change func(*Config) error, apply func(*Config)) (Config, error) {
	d.cfgWrite.Lock()
	defer d.cfgWrite.Unlock()
	next := d.config()
	if err := change(&next); err != nil {
		return Config{}, err
	}
	if err := SaveConfig(d.layout, next); err != nil {
		return Config{}, err
	}
	d.mu.Lock()
	apply(&d.cfg)
	d.mu.Unlock()
	return next, nil
}

package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
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
	// Sandbox configures the sandbox's network (LEC §3.2, ux §2.2); absent, it has none beyond the
	// model route.
	Sandbox *AutoReplySandboxConfig `json:"sandbox,omitempty"`
}

// AutoReplySandboxConfig is auto_reply.sandbox.
type AutoReplySandboxConfig struct {
	// EgressAllow is the CONNECT allowlist (LEC CN-1): the hosts, each by its exact name, that a
	// sandboxed agent may reach on port 443. Empty, the default: none. Each one is a way for the
	// untrusted peer's task to send data out, so adding one is confirmed on a terminal (UX-C12);
	// removing one is not.
	EgressAllow []string `json:"egress_allow,omitempty"`
}

// empty reports a sandbox block that sets nothing.
func (s *AutoReplySandboxConfig) empty() bool {
	return s == nil || len(s.EgressAllow) == 0
}

// The egress allowlist is checked when the daemon starts (CN-1, LEC §3.2): a config.json edited by hand
// to hold an address, a wildcard or a model host does not start.
func init() { configChecks = append(configChecks, checkEgressAllowConfig) }

func checkEgressAllowConfig(c Config, _ map[string]any) *a2ashape.Refusal {
	if c.AutoReply == nil || c.AutoReply.Sandbox == nil {
		return nil
	}
	_, ref := checkEgressAllow(c.AutoReply.Sandbox.EgressAllow, c.AutoReply.APIBase)
	return ref
}

// withSandboxRequest is the configuration a POST /autoreply asks for, as far as auto_reply.sandbox goes
// (LEC §2.3.4; the rest of the request is SetAutoReply's): a request that names nothing but sandbox
// changes only that block of the current configuration, and in it only the members it names; another
// request keeps the current block unless it names one. The allowlist is checked against the result's
// api_base and kept in its normalized form (CN-1).
func withSandboxRequest(cur *AutoReplyConfig, req AutoReplyConfig, members map[string]json.RawMessage) (*AutoReplyConfig, *a2ashape.Refusal) {
	sandbox, named := members["sandbox"]
	onlySandbox := named
	for k := range members {
		if k != "sandbox" && k != "off" {
			onlySandbox = false
		}
	}
	var next AutoReplyConfig
	switch {
	case onlySandbox && cur != nil:
		next = *cur
	case !onlySandbox:
		next = req
	}
	var block AutoReplySandboxConfig
	if cur != nil && cur.Sandbox != nil {
		block = *cur.Sandbox
		block.EgressAllow = slices.Clone(block.EgressAllow)
	}
	if named {
		var in map[string]json.RawMessage
		_ = json.Unmarshal(sandbox, &in)
		if _, ok := in["egress_allow"]; ok && req.Sandbox != nil {
			block.EgressAllow = req.Sandbox.EgressAllow
		}
	}
	norm, ref := checkEgressAllow(block.EgressAllow, next.APIBase)
	if ref != nil {
		return nil, ref
	}
	block.EgressAllow = norm
	next.Sandbox = nil
	if !block.empty() {
		next.Sandbox = &block
	}
	return &next, nil
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

// --- What the daemon refuses to start with (ux UX-D3, §2.3) ---

// RemovedSetting is a config.json key that 0.3.0 removed (ux §2.3), as found
// in a file: where it is, and the id of the {instead} phrase that says what
// to do instead (a2ashape, config_key_removed).
type RemovedSetting struct {
	// Path names the key as the user finds it in the file, with the index of
	// an array element: "inbound.allow_file",
	// "inbound.public_capabilities[2].evidence".
	Path string
	// Key is the entry of ux §2.3 it matched, "[]" standing for any element:
	// "inbound.public_capabilities[].evidence".
	Key     string
	Instead string
	// Pending is the work item that still writes this key itself
	// ("v030-todo(<item>)"); until it deletes its code, the key does not stop
	// the daemon. Empty: the key is refused.
	Pending string
}

// removedSettings is ux §2.3: each removed key, where it sits in config.json
// ("name[]" is every element of that array) and its {instead} phrase.
var removedSettings = []struct{ key, instead string }{
	{"pricing", "pricing"},
	{"payments.publish_prices", "publish_prices"},
	{"modules.x402.voucher_addr", "voucher"},
	{"modules.x402.voucher_url", "voucher"},
	{"modules.x402.witness_hub", "witness_hub"},
	{"modules.x402.witness_every_seconds", "witness_hub"},
	{"providers", "providers"},
	{"accept_delegations", "accept_delegations"},
	{"rotation_grace", "rotation_grace"},
	{"inbound.allow_file", "list_files"},
	{"inbound.deny_file", "list_files"},
	{"inbound.trust_file", "list_files"},
	{"inbound.reject_notice", "reject_notice"},
	{"inbound.pending", "pending"},
	{"inbound.public_capabilities[].evidence", "evidence"},
	{"auto_reply.require_image", "require_image"},
	{"auto_reply.usage_hint", "require_image"},
	{"auto_reply.error_reply", "error_reply"},
	{"auto_reply.poll_interval_seconds", "fixed_auto_reply"},
	{"auto_reply.max_auto_replies", "fixed_auto_reply"},
	{"payments.payees_file", "payees_file"},
	{"modules.anetlink.timeout_s", "anetlink_timeout"},
	{"modules.service.expected_uid", "expected_uid"},
	{"modules.a2a.backends[].expected_uid", "expected_uid"},
	{"modules.anetlink.expected_uid", "expected_uid"},
	{"modules.p2p.dial_timeout_ms", "p2p_timeouts"},
	{"modules.p2p.reach_timeout_ms", "p2p_timeouts"},
}

// InviteEnv is the one removed item that is not a key: the environment
// variable that carried an invite. It does not stop the daemon (it is
// ignored); doctor's config.removed warns about it (ux §2.3).
const InviteEnv = "ANET_INVITE"

// removedStillWritten are the removed keys this binary still writes itself
// (the defaults of a fresh config.json, or a command that is still there), or
// whose replacement does not work yet. A daemon refusing them would refuse
// its own files, so each waits for the work item that deletes its code,
// which also deletes its line here (R-5).
var removedStillWritten = map[string]string{
	// The defaults write the list files and the queue and notice limits.
	"inbound.allow_file":    "v030-todo(UX-4)",
	"inbound.deny_file":     "v030-todo(UX-4)",
	"inbound.trust_file":    "v030-todo(UX-4)",
	"inbound.reject_notice": "v030-todo(UX-4)",
	"inbound.pending":       "v030-todo(UX-4)",
	// The defaults write the payee list's file.
	"payments.payees_file": "v030-todo(PAY-12)",
	// `anet autoreply set --require-image|--usage-hint|--error-reply|
	// --poll-interval|--max-auto-replies` still write these.
	"auto_reply.require_image":         "v030-todo(UX-9)",
	"auto_reply.usage_hint":            "v030-todo(UX-9)",
	"auto_reply.error_reply":           "v030-todo(UX-9)",
	"auto_reply.poll_interval_seconds": "v030-todo(UX-9)",
	"auto_reply.max_auto_replies":      "v030-todo(UX-9)",
	// expected_user does not take a numeric uid yet, so the advice to use it
	// instead would not work.
	"modules.service.expected_uid":        "v030-todo(EXE-10)",
	"modules.a2a.backends[].expected_uid": "v030-todo(EXE-10)",
	"modules.anetlink.expected_uid":       "v030-todo(EXE-10)",
}

// FindRemovedSettings lists the removed keys a config.json holds, in the
// order of ux §2.3; a key that is there with any value counts, null too. A
// file that is not a JSON object is an error.
func FindRemovedSettings(raw []byte) ([]RemovedSetting, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var top any
	if err := dec.Decode(&top); err != nil {
		return nil, err
	}
	if _, ok := top.(map[string]any); !ok {
		return nil, errors.New("config.json is not a JSON object")
	}
	var out []RemovedSetting
	for _, rs := range removedSettings {
		for _, path := range findKey(top, strings.Split(rs.key, "."), "") {
			out = append(out, RemovedSetting{Path: path, Key: rs.key, Instead: rs.instead, Pending: removedStillWritten[rs.key]})
		}
	}
	return out, nil
}

// findKey is where the dotted key segs sits under v: one path per match,
// with the index of each array element it passes through, spelled as the
// file spells it. A key matches whatever its case: encoding/json reads
// "Rotation_Grace" into the field of "rotation_grace" (it folds case like
// strings.EqualFold), so a removed key in another case would otherwise still
// take effect, or be ignored without a word (UX-D3).
func findKey(v any, segs []string, at string) []string {
	obj, ok := v.(map[string]any)
	if !ok || len(segs) == 0 {
		return nil
	}
	name, each := strings.CutSuffix(segs[0], "[]")
	var names []string
	for k := range obj {
		if strings.EqualFold(k, name) {
			names = append(names, k)
		}
	}
	slices.Sort(names)
	var out []string
	for _, k := range names {
		path := k
		if at != "" {
			path = at + "." + k
		}
		out = append(out, findUnder(obj[k], segs, each, path)...)
	}
	return out
}

// findUnder continues findKey below the value of the key segs[0] matched.
func findUnder(child any, segs []string, each bool, path string) []string {
	if !each {
		if len(segs) == 1 {
			return []string{path}
		}
		return findKey(child, segs[1:], path)
	}
	arr, ok := child.([]any)
	if !ok {
		return nil
	}
	var out []string
	for i, el := range arr {
		p := path + "[" + strconv.Itoa(i) + "]"
		if len(segs) == 1 {
			out = append(out, p)
			continue
		}
		out = append(out, findKey(el, segs[1:], p)...)
	}
	return out
}

// ConfigCheck is one validation of config.json that refuses to start: it
// returns config_invalid (or nil) for a value the daemon cannot run with.
// The area that owns a setting appends its check to configChecks; the
// daemon, doctor and the deployment-template test all run them through
// CheckConfigForStart (CUX-V10).
type ConfigCheck func(c Config, raw map[string]any) *a2ashape.Refusal

// configChecks are the validations of config.json.
var configChecks []ConfigCheck

// ConfigRefusal is why the daemon will not start with a config.json: one
// refusal per problem, so that every key can be fixed at once. A data
// directory an earlier anet wrote is refused the same way, with the one
// refusal identity_v02 (CheckDataDirForStart).
type ConfigRefusal struct {
	Refusals []*a2ashape.Refusal
}

// Sentences are the refusals' sentences in a language, one each.
func (e *ConfigRefusal) Sentences(lang a2ashape.Language) []string {
	out := make([]string, len(e.Refusals))
	for i, r := range e.Refusals {
		out[i] = a2ashape.Sentence(r.Key, lang, r.Args)
	}
	return out
}

// Error is the refusals' sentences in this process's language, one a line.
func (e *ConfigRefusal) Error() string {
	return strings.Join(e.Sentences(a2ashape.Lang()), "\n")
}

// CheckConfigForStart is the check the daemon makes before it starts: no
// removed key (config_key_removed) and every ConfigCheck. A missing
// config.json passes (the daemon writes the defaults). The error is a
// *ConfigRefusal, or the error reading or parsing the file.
func CheckConfigForStart(l Layout) error {
	raw, err := os.ReadFile(l.ConfigPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("anet: read config: %w", err)
	}
	found, err := FindRemovedSettings(raw)
	if err != nil {
		return fmt.Errorf("anet: parse config %s: %w", l.ConfigPath(), err)
	}
	var refusals []*a2ashape.Refusal
	for _, f := range found {
		if f.Pending == "" {
			refusals = append(refusals, RemovedSettingRefusal(f))
		}
	}
	if len(configChecks) > 0 {
		var c Config
		var m map[string]any
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("anet: parse config %s: %w", l.ConfigPath(), err)
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("anet: parse config %s: %w", l.ConfigPath(), err)
		}
		for _, check := range configChecks {
			if r := check(c, m); r != nil {
				refusals = append(refusals, r)
			}
		}
	}
	if len(refusals) > 0 {
		return &ConfigRefusal{Refusals: refusals}
	}
	return nil
}

// RemovedSettingRefusal is config_key_removed for a key found in a file.
func RemovedSettingRefusal(f RemovedSetting) *a2ashape.Refusal {
	return a2ashape.Refuse("config_key_removed", a2ashape.Args{"key": f.Path, "instead": f.Instead})
}

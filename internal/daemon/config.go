package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
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
	// typically by the operator's agent, not hand-written). It carries no price (0.3.0: the setting
	// "pricing" is a removed key, refused at start).
	Summary string `json:"summary,omitempty"` // one-line description
	Readme  string `json:"readme,omitempty"`  // longer markdown description
	// Modules configures optional subsystems by name (see module.Module).
	Modules ModulesConfig `json:"modules,omitempty"`
	// Without names the optional modules this node turns off (MOD-P5): a
	// module in the list is not built, not registered and not started, and
	// everything that depends on it answers module_absent. Empty is the
	// default. Only an offline command writes it (`anet init --without`,
	// `--with`): no control route, MCP tool or local A2A method does, and a
	// write the daemon makes of this file takes the value from the disk, not
	// from memory (updateConfig).
	Without []string `json:"without,omitempty"`
	// Inbound is the inbound policy (A2A-DESIGN §5.1): who may delegate to
	// this node, who may drive its local agents, and which capabilities are
	// public. A config without the block is given the defaults of
	// defaultInbound (policy closed) at load; see fillInbound.
	Inbound *InboundConfig `json:"inbound,omitempty"`
	// rewriteConfig is set when the file on disk lacks the inbound or
	// payments block; New saves the config with the block written out, so
	// the file states the defaults explicitly (SI-5).
	rewriteConfig bool
	// AutoReply is the auto_reply block, kept raw: the kernel neither parses nor interprets it. The
	// auto-reply module (autoreply_config.go) reads it, checks it through the ConfigSpec it registers
	// and writes it only through ModuleConfigHost.UpdateModuleConfig (K-6); the kernel carries it
	// byte for byte through every other write of config.json (carryDiskConfig).
	AutoReply json.RawMessage `json:"auto_reply,omitempty"`
	// Payments is the spending policy (A2A-DESIGN §8.6; spend.go). A key
	// left out gets its default (PaymentsConfig.limits); a config without
	// the block is given defaultPayments at load and rewritten, so the file
	// states the limits explicitly (SI-5).
	Payments *PaymentsConfig `json:"payments,omitempty"`
	// NoResponseAfter is how long a task this node sent may sit submitted
	// with nothing at all from its peer — no status, no message, no result
	// — once its delegation has left this node, before it is failed with
	// anet.reason=no_response and effect UNVERIFIED (A2A-DESIGN §4.2,
	// no_response.go). A Go duration such as "15m"; empty means 15m, "0"
	// turns the deadline off. A peer that refuses without telling (a refusal
	// notice past its rate limit, §2 X2) otherwise leaves the task, and
	// every client waiting on it, waiting for ever.
	NoResponseAfter string `json:"no_response_after,omitempty"`
	// ContentRetention is how long the content of a finished task that is
	// not public is kept on this node (FS §4.6.2, ux §2.2): "<N>d" for N
	// from 1 to 3650, or "forever". A fresh config.json says "30d", which is
	// also what a config without the key gets. A public task is kept 7 days,
	// or this when it is shorter, whatever it says. `anet retention set`
	// changes it, after a confirmation typed on a terminal; a value that is
	// neither a number of days nor forever stops the daemon from starting
	// (retention_invalid).
	ContentRetention RetentionValue `json:"content_retention,omitempty"`
	// StreamPreview is whether this node, as a provider, sends the requester
	// a snapshot of its reply while it writes it (CUX §2.10, CUX-P4): "off"
	// (the default), "on", or "with_reasoning", which also sends the
	// reasoning to a requester that asks for it. A fresh config.json says
	// "off"; a config without the key gets it. `anet preview` changes it;
	// any other value stops the daemon from starting (config_invalid).
	StreamPreview string `json:"stream_preview,omitempty"`
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
	return Config{ControlAddr: "127.0.0.1:39811", Inbound: &in, Payments: &pay, NoResponseAfter: defaultNoResponseAfterText,
		ContentRetention: RetentionValue(interactions.DefaultRetention().String()),
		StreamPreview:    streamPreviewOff}
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
	if ref := withoutTypeRefusal(b); ref != nil {
		return Config{}, &ConfigRefusal{Refusals: []*a2ashape.Refusal{ref}}
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("anet: parse config %s: %w", l.ConfigPath(), err)
	}
	if c.ControlAddr == "" {
		c.ControlAddr = DefaultConfig().ControlAddr
	}
	fillInbound(&c)
	migratePayments(&c)
	return c, nil
}

// fillInbound gives a config without an inbound block the closed default
// (A2A-DESIGN §5.1). The wire-1 accept_delegations key is not read: 0.3.0
// refuses to start with it (ux §2.3), and migrates nothing.
func fillInbound(c *Config) {
	c.rewriteConfig = c.Inbound == nil
	if c.Inbound == nil {
		in := defaultInbound()
		c.Inbound = &in
	}
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
	// K-2: what the kernel does not own comes from the disk. `without` and
	// the blocks the kernel does not parse (modules.*, auto_reply) are
	// whatever config.json says now, not what this process read at start; a
	// kernel write changes its own keys and carries the rest through, so
	// that a hand edit made while the daemon runs is not undone by the next
	// peers or policy write. An unreadable file refuses the write rather
	// than overwrite what could not be read.
	if err := d.carryDiskConfig(&next); err != nil {
		return Config{}, err
	}
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

// carryDiskConfig sets what the kernel does not own in next — `without` and
// the blocks it does not parse — to what config.json says now (K-2). The
// caller holds cfgWrite and calls it before it changes its own keys; every
// write of config.json the daemon makes does (updateConfig, and the policy,
// limit and hub writes beside it).
func (d *Daemon) carryDiskConfig(next *Config) error {
	disk, err := readConfigFile(d.layout)
	if err != nil {
		return err
	}
	if disk != nil {
		next.Without, next.Modules, next.AutoReply = disk.Without, disk.Modules, disk.AutoReply
	}
	return nil
}

// readConfigFile is config.json as it is on disk now; nil when there is none.
// It does not create the file (LoadConfig does).
func readConfigFile(l Layout) (*Config, error) {
	b, err := os.ReadFile(l.ConfigPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("anet: read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("anet: parse config %s: %w", l.ConfigPath(), err)
	}
	return &c, nil
}

// WithoutInFile is the `without` list of config.json as it is on disk now,
// without creating the file. The CLI asks it before it contacts the daemon
// (MOD-F3); an absent file has none. A list that does not decode is an
// error: a CLI that cannot tell what is turned off does not guess.
func WithoutInFile(l Layout) ([]string, error) {
	c, err := readConfigFile(l)
	if err != nil || c == nil {
		return nil, err
	}
	return c.Without, nil
}

// AvailabilityOf is the Availability value of a configuration: its `without`
// list over what this binary contains. A list that does not validate gives
// the value with nothing turned off; the start check says why it is invalid.
func AvailabilityOf(c Config) module.Availability {
	av, err := module.NewAvailability(c.Without)
	if err != nil {
		return module.Availability{}
	}
	return av
}

// ModuleRefusals are the problems of a configuration that concern the
// optional modules, one refusal each: an invalid `without`, a block for a
// module that does not exist or whose block is not under modules, and a
// block for a module that is not available (MOD-F1). The same list is what
// the daemon refuses to start with, what `anet up` and the daemon log say,
// and what doctor's modules row says.
func ModuleRefusals(c Config) []*a2ashape.Refusal {
	var out []*a2ashape.Refusal
	av, err := module.NewAvailability(c.Without)
	if err != nil {
		if r, ok := err.(*a2ashape.Refusal); ok {
			out = append(out, r)
		}
		av = module.Availability{}
	}
	names := make([]string, 0, len(c.Modules))
	for n := range c.Modules {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		info, ok := module.Lookup(n)
		if _, test := rowOf(n); test && !ok {
			continue // a fake module of this package's tests (moduleRowForTest)
		}
		switch {
		case !ok && removedModuleBlock(n):
			// A block of a module that no longer exists is config_key_removed,
			// said once by the removed-settings check.
		case !ok:
			out = append(out, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "modules." + cleanKey(n),
				"rule": ruleText("没有这个模块", "there is no such module")}))
		case info.ConfigKey == "":
			out = append(out, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "modules." + n,
				"rule": ruleText("这个模块没有配置块", "this module takes no configuration block")}))
		case info.ConfigKey != "modules."+n:
			out = append(out, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "modules." + n,
				"rule": "top_level_key", "m": n}))
		default:
			if r := av.Absent(n, "configured"); r != nil {
				r.Args["key"] = info.ConfigKey
				out = append(out, r)
			}
		}
	}
	if blockOrNil(c.AutoReply) != nil {
		if r := av.Absent("autoreply", "configured"); r != nil {
			r.Args["key"] = "auto_reply"
			out = append(out, r)
		}
	}
	if c.streamPreview() != streamPreviewOff {
		// stream_preview on or with_reasoning is the preview module's
		// configuration (MOD-F1): off is the default and needs nothing.
		if r := av.Absent("preview", "configured"); r != nil {
			r.Args["key"] = "stream_preview"
			out = append(out, r)
		}
	}
	return out
}

// removedModuleBlock reports a modules.<name> block that ux §2.3 removed.
func removedModuleBlock(name string) bool {
	for _, r := range removedSettings {
		if r.key == "modules."+name {
			return true
		}
	}
	return false
}

// cleanKey shortens a name found in a file for a sentence.
func cleanKey(s string) string {
	if r := []rune(s); len(r) > 32 {
		return string(r[:32]) + "…"
	}
	return s
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
	{"modules.taskboard", "taskboard"},
}

// InviteEnv is the one removed item that is not a key: the environment
// variable that carried an invite. It does not stop the daemon (it is
// ignored); doctor's config.removed warns about it (ux §2.3).
const InviteEnv = "ANET_INVITE"

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
			out = append(out, RemovedSetting{Path: path, Key: rs.key, Instead: rs.instead})
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

// Unwrap lists the refusals, so that errors.As finds the first of them.
func (e *ConfigRefusal) Unwrap() []error {
	out := make([]error, len(e.Refusals))
	for i, r := range e.Refusals {
		out[i] = r
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
		refusals = append(refusals, RemovedSettingRefusal(f))
	}
	{
		var c Config
		var m map[string]any
		if ref := withoutTypeRefusal(raw); ref != nil {
			return &ConfigRefusal{Refusals: append(refusals, ref)}
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("anet: parse config %s: %w", l.ConfigPath(), err)
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("anet: parse config %s: %w", l.ConfigPath(), err)
		}
		refusals = append(refusals, ModuleRefusals(c)...)           // MOD-F1, K-2
		refusals = append(refusals, moduleConfigRefusals(c, "")...) // the modules' own ConfigSpec.Check (K-6)
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

// --- content_retention (FS §4.6.2, §5.1) ---

// RetentionValue is content_retention as config.json holds it. Any JSON
// value decodes into it, so that one that is not a setting — a number, a
// list, "30" without its d — is refused with retention_invalid, whose
// sentence says how to write it, rather than making the whole file
// unreadable. A value that is not a string is kept as its JSON text, which
// is never a setting.
type RetentionValue string

// UnmarshalJSON keeps any JSON value: a string as it is, null as absent,
// anything else as its JSON text.
func (v *RetentionValue) UnmarshalJSON(p []byte) error {
	var s string
	if err := json.Unmarshal(p, &s); err == nil {
		*v = RetentionValue(s)
		return nil
	}
	*v = RetentionValue(p)
	return nil
}

// contentRetention is the setting in force: the configured one, 30 days
// when config.json has none. A value that is not a setting stops the daemon
// from starting, so a running daemon holds one only if something wrote it
// past the checks; it then keeps every task that is not public, since
// nothing should go that the operator may have meant to keep.
func (c Config) contentRetention() interactions.Retention {
	if c.ContentRetention == "" {
		return interactions.DefaultRetention()
	}
	if r, ok := interactions.ParseRetention(string(c.ContentRetention)); ok {
		return r
	}
	return interactions.RetentionForever
}

// The setting is checked when the daemon starts (FS §5.1): a config.json
// edited by hand to an unreadable retention does not start, and says how to
// write it (retention_invalid). An absent key, or null, is the default.
func init() { configChecks = append(configChecks, checkContentRetention) }

func checkContentRetention(_ Config, raw map[string]any) *a2ashape.Refusal {
	v, ok := raw["content_retention"]
	if !ok || v == nil {
		return nil
	}
	if s, isText := v.(string); isText {
		if _, ok := interactions.ParseRetention(s); ok {
			return nil
		}
	}
	return a2ashape.Refuse("retention_invalid", nil)
}

// ParseContentRetention reads a content_retention value as the daemon does:
// "<N>d" (1 to 3650) or "forever".
func ParseContentRetention(s string) (interactions.Retention, bool) {
	return interactions.ParseRetention(s)
}

// ContentRetentionInFile is the content_retention l's config.json sets, read
// without creating or changing anything: the setting, and the text the file
// holds ("" when it has none, or no config.json). A value that is not a
// setting is retention_invalid.
func ContentRetentionInFile(l Layout) (interactions.Retention, string, *a2ashape.Refusal, error) {
	raw, err := os.ReadFile(l.ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return interactions.DefaultRetention(), "", nil, nil
	}
	if err != nil {
		return interactions.Retention{}, "", nil, fmt.Errorf("anet: read config: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return interactions.Retention{}, "", nil, fmt.Errorf("anet: parse config %s: not a JSON object", l.ConfigPath())
	}
	v, ok := m["content_retention"]
	if !ok || v == nil {
		return interactions.DefaultRetention(), "", nil, nil
	}
	text, _ := json.Marshal(v)
	if s, isText := v.(string); isText {
		if r, ok := interactions.ParseRetention(s); ok {
			return r, s, nil, nil
		}
		return interactions.Retention{}, s, a2ashape.Refuse("retention_invalid", nil), nil
	}
	return interactions.Retention{}, string(text), a2ashape.Refuse("retention_invalid", nil), nil
}

// SetContentRetentionInFile writes content_retention = r into l's
// config.json while no daemon runs (`anet retention set` with the daemon
// stopped, FS §5.2 step 5): that key alone changes, every other key stays as
// it is written, and the file is replaced at once (0600). A data directory
// without a config.json gets one first, as every command gives it. The
// daemon reads the new value when it next starts.
func SetContentRetentionInFile(l Layout, r interactions.Retention) error {
	if !r.Valid() {
		return fmt.Errorf("anet: content_retention: %w", interactions.ErrBadInput)
	}
	if _, err := os.Stat(l.ConfigPath()); errors.Is(err, os.ErrNotExist) {
		if _, err := LoadConfig(l); err != nil {
			return err
		}
	}
	raw, err := os.ReadFile(l.ConfigPath())
	if err != nil {
		return fmt.Errorf("anet: read config: %w", err)
	}
	cur, err := decodeObject(raw)
	if err != nil {
		return fmt.Errorf("anet: parse config %s: %w", l.ConfigPath(), err)
	}
	cur["content_retention"] = r.String()
	out, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(l.ConfigPath(), out, 0o600)
}

// --- stream_preview (CUX §2.10) ---

// The settings of stream_preview.
const (
	streamPreviewOff = "off"
	streamPreviewOn  = previewModeOn
	streamPreviewWR  = previewModeWithReasoning
)

// validStreamPreview reports whether v is a setting of stream_preview.
func validStreamPreview(v string) bool {
	switch v {
	case streamPreviewOff, streamPreviewOn, streamPreviewWR:
		return true
	}
	return false
}

// streamPreview is the setting in force: the configured one, off when
// config.json has none or a running daemon holds a value that is not a
// setting (the start check refuses one).
func (c Config) streamPreview() string {
	if validStreamPreview(c.StreamPreview) {
		return c.StreamPreview
	}
	return streamPreviewOff
}

// StreamPreviewModes are the values `anet preview` accepts.
func StreamPreviewModes() []string {
	return []string{streamPreviewOff, streamPreviewOn, streamPreviewWR}
}

// The setting is checked when the daemon starts (CUX §2.10): a value that
// is not off, on or with_reasoning does not start, and says so
// (config_invalid). An absent key, or null, is off.
func init() { configChecks = append(configChecks, checkStreamPreview) }

func checkStreamPreview(_ Config, raw map[string]any) *a2ashape.Refusal {
	v, ok := raw["stream_preview"]
	if !ok || v == nil {
		return nil
	}
	if s, isText := v.(string); isText && validStreamPreview(s) {
		return nil
	}
	return a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "stream_preview", "rule": "off, on or with_reasoning"})
}

// withoutTypeRefusal is config_invalid for a "without" that is not a list of
// module names, in the reason table's words rather than the decoder's ("json:
// cannot unmarshal string into Go struct field Config.without of type
// []string"). nil when the file has no such mistake or is not an object.
func withoutTypeRefusal(raw []byte) *a2ashape.Refusal {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	if _, err := withoutOf(m); err != nil {
		var ref *a2ashape.Refusal
		if errors.As(err, &ref) {
			return ref
		}
	}
	return nil
}

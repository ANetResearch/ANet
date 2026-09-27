package main

// doctor.go holds `anet doctor [--json]` (A2A-DESIGN §13.1): what this
// identity is configured to do, read from its data directory, with no
// daemon required and nothing written.
//
// --json is a contract: the keys below are stable, and "si5" carries every
// key of SI-5 under the name the invariant uses, so a test (and an
// installer) can assert the fresh-install defaults key by key. The report
// exits non-zero only when a check fails — a finding that breaks the node or
// exposes a credential; a setting that differs from a fresh install is the
// operator's choice and is reported, not failed.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/module"
)

// Check statuses.
const (
	stOK      = "ok"
	stInfo    = "info"
	stWarn    = "warn"
	stFail    = "fail"
	stUnknown = "unknown"
)

type doctorCheck struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

type fileState struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Mode    string `json:"mode,omitempty"`
	// Private is true when no one but the owner can read the file.
	Private bool `json:"private"`
}

type agentWire struct {
	Tool    string `json:"tool"`
	Config  string `json:"config"`
	Present bool   `json:"present"` // the tool's config file exists
	Wired   bool   `json:"wired"`   // it has an anet entry
	Detail  string `json:"detail,omitempty"`
	// Handshake is whether the tool was started and answered through
	// anet's MCP server. doctor does not start tools: "not_checked".
	Handshake string `json:"handshake"`
}

type a2aAgentEntry struct {
	URL     string `json:"url"`
	Port    string `json:"port"`
	Matches bool   `json:"matches_a2a_addr"`
}

type doctorReport struct {
	Schema  string `json:"schema"`
	DataDir string `json:"data_dir"`
	Version struct {
		Version         string `json:"version"`
		Commit          string `json:"commit"`
		Built           string `json:"built"`
		Signature       string `json:"signature"`
		SignatureDetail string `json:"signature_detail"`
	} `json:"version"`
	Modules  []string `json:"modules"`
	Identity struct {
		Present bool   `json:"present"`
		AID     string `json:"aid"`
	} `json:"identity"`
	Config struct {
		Path    string `json:"path"`
		Present bool   `json:"present"`
		Error   string `json:"error,omitempty"`
		Mode    string `json:"mode,omitempty"`
	} `json:"config"`
	Control struct {
		Addr    string    `json:"addr"`
		Running bool      `json:"running"`
		Token   fileState `json:"token_file"`
	} `json:"control"`
	A2A struct {
		Compiled bool      `json:"compiled"`
		AddrFile fileState `json:"addr_file"`
		Addr     string    `json:"addr"`
		Token    fileState `json:"token_file"`
	} `json:"a2a"`
	Hub struct {
		URL        string `json:"url"`
		Registered bool   `json:"registered"`
	} `json:"hub"`
	Inbound struct {
		Policy             string   `json:"policy"`
		Allow              []string `json:"allow"`
		Trust              []string `json:"trust"`
		Deny               []string `json:"deny"`
		PublicCapabilities []string `json:"public_capabilities"`
	} `json:"inbound"`
	Payments  daemon.PaymentsConfig `json:"payments"`
	Payees    []string              `json:"payees"`
	AutoReply struct {
		Configured       bool   `json:"configured"`
		Backend          string `json:"backend"`
		Untrusted        string `json:"untrusted"`
		APIKeyConfigured bool   `json:"api_key_configured"`
	} `json:"auto_reply"`
	// SI5 is every key of SI-5 with its current value; SI5Changed names the
	// keys whose value is not the fresh-install default.
	SI5        map[string]any `json:"si5"`
	SI5Changed []string       `json:"si5_changed"`
	Agents     []agentWire    `json:"agents"`
	Hermes     struct {
		Config    fileState       `json:"config"`
		A2AAgents []a2aAgentEntry `json:"a2a_agents"`
		// A2AToken is whether the entries on this node's A2A port carry the
		// current local A2A token: "current", "stale" (a2a_token.txt was
		// replaced since they were written), "unknown" (no token file, or no
		// entry on this node's port, to compare with) or "no_entries".
		A2AToken string `json:"a2a_token"`
	} `json:"hermes"`
	Checks []doctorCheck `json:"checks"`
	OK     bool          `json:"ok"`
}

// doctorEnv is what doctor reads outside the data directory. Tests replace
// it so no real home directory is read.
type doctorEnv struct {
	home       string // the user's home directory
	hermesHome string // HERMES_HOME, or ~/.hermes
	// running reports whether a daemon answers at the control address with
	// the token in tokenPath.
	running func(addr, tokenPath string) bool
}

func defaultDoctorEnv() doctorEnv {
	home, _ := os.UserHomeDir()
	hh := os.Getenv("HERMES_HOME")
	if hh == "" && home != "" {
		hh = filepath.Join(home, ".hermes")
	}
	return doctorEnv{home: home, hermesHome: hh, running: daemonAnswersAt}
}

// daemonAnswersAt is localDaemonUp for the address doctor has already read.
// localDaemonUp finds the address through LoadConfig, which writes a
// config.json when there is none, and doctor writes nothing.
func daemonAnswersAt(addr, tokenPath string) bool {
	tb, err := os.ReadFile(tokenPath)
	if err != nil {
		return false
	}
	c := &client{base: "http://" + addr, token: strings.TrimSpace(string(tb)), timeout: 1500 * time.Millisecond}
	_, code, e := c.fetch("/status", nil)
	return e == nil && code == 200
}

// releaseSignature reports whether this binary was checked against a signed
// release manifest (§13.2). The release signing work has not landed in this
// build, so the answer is "unknown" rather than a guess.
var releaseSignature = func() (status, detail string) {
	return stUnknown, "this build has no release-manifest check (anet update / release.json)"
}

func runDoctor(layout daemon.Layout, rest []string) error {
	_, flags := splitFlags(rest)
	return doctorTo(os.Stdout, layout, defaultDoctorEnv(), flags["json"] == "true")
}

func doctorTo(w io.Writer, layout daemon.Layout, env doctorEnv, asJSON bool) error {
	rep, err := collectDoctor(layout, env)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		renderDoctor(w, rep)
	}
	if !rep.OK {
		return errQuiet
	}
	return nil
}

func statFile(path string) fileState {
	fs := fileState{Path: path}
	fi, err := os.Stat(path)
	if err != nil {
		return fs
	}
	fs.Present = true
	fs.Mode = fmt.Sprintf("%04o", fi.Mode().Perm())
	fs.Private = fi.Mode().Perm()&0o077 == 0
	return fs
}

func collectDoctor(layout daemon.Layout, env doctorEnv) (*doctorReport, error) {
	rep := &doctorReport{Schema: "anet.doctor/1", DataDir: layout.Root}
	add := func(id, status, detail, hint string) {
		rep.Checks = append(rep.Checks, doctorCheck{ID: id, Status: status, Detail: detail, Hint: hint})
	}

	rep.Version.Version, rep.Version.Commit, rep.Version.Built = daemon.Version, daemon.BuildCommit, daemon.BuildAt
	rep.Version.Signature, rep.Version.SignatureDetail = releaseSignature()
	add("version", stInfo, fmt.Sprintf("anet %s (commit %s, built %s)", daemon.Version, daemon.BuildCommit, daemon.BuildAt), "")
	add("version.signature", rep.Version.Signature, rep.Version.SignatureDetail, "")
	rep.Modules = module.Compiled()
	if rep.Modules == nil {
		rep.Modules = []string{}
	}
	add("modules", stInfo, compiledModulesReport(), "")

	// Configuration and policy.
	st, err := daemon.ReadPolicy(layout)
	if err != nil {
		return nil, err
	}
	cfg := st.Config
	rep.Config.Path, rep.Config.Present, rep.Config.Error = st.ConfigPath, st.ConfigPresent, st.ConfigError
	cfgFile := statFile(st.ConfigPath)
	rep.Config.Mode = cfgFile.Mode
	switch {
	case !st.ConfigPresent:
		add("config", stWarn, "no "+st.ConfigPath+"; the defaults below are what a new node would use",
			"anet init")
	case st.ConfigError != "":
		add("config", stFail, st.ConfigError, "the daemon will not start with this config")
	case !cfgFile.Private:
		add("config", stWarn, st.ConfigPath+" is readable by others (mode "+cfgFile.Mode+"); it may hold an API key",
			"chmod 600 "+st.ConfigPath)
	default:
		add("config", stOK, st.ConfigPath, "")
	}

	// Identity.
	rep.Identity.AID = daemon.ReadIdentityAID(layout)
	rep.Identity.Present = rep.Identity.AID != ""
	if rep.Identity.Present {
		add("identity", stOK, rep.Identity.AID, "")
	} else {
		add("identity", stInfo, "no identity yet; the first `anet up` creates one", "anet up")
	}

	// Control plane.
	rep.Control.Addr = cfg.ControlAddr
	if rep.Control.Addr == "" {
		rep.Control.Addr = daemon.DefaultConfig().ControlAddr
	}
	if env.running != nil {
		rep.Control.Running = env.running(rep.Control.Addr, layout.ControlTokenPath())
	}
	rep.Control.Token = statFile(layout.ControlTokenPath())
	if rep.Control.Running {
		add("control", stOK, "daemon running, control plane at "+rep.Control.Addr, "")
	} else {
		add("control", stInfo, "daemon not running (control plane "+rep.Control.Addr+")", "anet up")
	}
	tokenCheck(add, "control.token", rep.Control.Token)

	// Local A2A interface (module a2a, §11).
	rep.A2A.Compiled = slices.Contains(rep.Modules, "a2a")
	rep.A2A.AddrFile = statFile(filepath.Join(layout.Root, "a2a_addr.txt"))
	rep.A2A.Token = statFile(filepath.Join(layout.Root, "a2a_token.txt"))
	if rep.A2A.AddrFile.Present {
		if b, err := os.ReadFile(rep.A2A.AddrFile.Path); err == nil {
			rep.A2A.Addr = strings.TrimSpace(string(b))
		}
	}
	switch {
	case !rep.A2A.Compiled && rep.A2A.Addr == "":
		add("a2a", stInfo, "the local A2A interface is not in this build (no_a2a)", "")
	case rep.A2A.Addr == "":
		add("a2a", stWarn, "no "+rep.A2A.AddrFile.Path+": the local A2A interface has not started in this data directory yet",
			"anet up")
	default:
		add("a2a", stOK, "local A2A interface at http://"+rep.A2A.Addr+"/a2a/v1/agents", "")
	}
	if rep.A2A.Compiled || rep.A2A.Token.Present {
		tokenCheck(add, "a2a.token", rep.A2A.Token)
	}

	// Hub.
	rep.Hub.URL = cfg.HubURL
	rep.Hub.Registered = cfg.HubURL != ""
	if rep.Hub.Registered {
		add("hub", stOK, "registered with "+cfg.HubURL+" (per config)", "")
	} else {
		add("hub", stInfo, "not registered with a hub", "anet hub-register <url>")
	}

	// Inbound.
	rep.Inbound.Policy = st.Inbound.Policy
	rep.Inbound.Allow, rep.Inbound.Trust, rep.Inbound.Deny = st.Allow, st.Trust, st.Deny
	rep.Inbound.PublicCapabilities = []string{}
	for _, c := range st.Inbound.PublicCapabilities {
		rep.Inbound.PublicCapabilities = append(rep.Inbound.PublicCapabilities, c.ID)
	}
	inDetail := fmt.Sprintf("policy %s; allow %d, trust %d, deny %d; public capabilities %d",
		st.Inbound.Policy, len(st.Allow), len(st.Trust), len(st.Deny), len(rep.Inbound.PublicCapabilities))
	if st.Inbound.Policy == daemon.PolicyOpen {
		add("inbound", stWarn, inDetail+" — anyone may send this node natural-language tasks", "")
	} else {
		add("inbound", stOK, inDetail, "")
	}
	for _, e := range st.ListErrors {
		add("inbound.lists", stFail, e, "a list that cannot be read: deny refuses everyone, allow and trust are empty")
	}

	// Payments (§8.6).
	rep.Payments, rep.Payees = st.Payments, st.Payees
	p := st.Payments
	payees := "payee list off (any payee)"
	if p.PayeesFile != "" {
		payees = fmt.Sprintf("payee list on, %d entries (%s)", len(st.Payees), st.PayeesPath)
	}
	payDetail := fmt.Sprintf("auto_max %d; agent_max %d, agent_daily_max %d; explicit_max %d, daily_max %d; %s",
		p.AutoMax, p.AgentMax, p.AgentDailyMax, p.ExplicitMax, p.DailyMax, payees)
	if p.PayeesFile == "" && (p.AutoMax > 0 || p.AgentMax > 0) {
		add("payments", stWarn, payDetail+" — automatic or agent payments to any payee", "")
	} else {
		add("payments", stOK, payDetail, "")
	}

	// Auto-reply and the sandbox (§6).
	if ar := cfg.AutoReply; ar != nil {
		rep.AutoReply.Configured = true
		rep.AutoReply.Backend = ar.Backend
		if rep.AutoReply.Backend == "" {
			rep.AutoReply.Backend = "openai"
		}
		rep.AutoReply.APIKeyConfigured = strings.TrimSpace(ar.APIKey) != ""
	}
	rep.SI5, rep.SI5Changed = st.SI5()
	if rep.SI5Changed == nil {
		rep.SI5Changed = []string{}
	}
	rep.AutoReply.Untrusted, _ = rep.SI5["auto_reply.untrusted"].(string)
	switch {
	case !rep.AutoReply.Configured:
		add("auto_reply", stInfo, "auto-reply off", "")
	case rep.AutoReply.Backend == "exec" && rep.AutoReply.Untrusted == daemon.UntrustedSandbox && !rep.AutoReply.APIKeyConfigured:
		add("auto_reply.sandbox", stFail, "auto_reply.untrusted=sandbox without auto_reply.api_key: the sandboxed agent "+
			"cannot log in, so every untrusted task fails closed (sandbox_unavailable)",
			"set auto_reply.api_key, or auto_reply.untrusted=off")
	case rep.AutoReply.Backend == "exec" && rep.AutoReply.Untrusted == daemon.UntrustedSandbox:
		add("auto_reply.sandbox", stOK, "exec auto-reply runs the local agent for untrusted peers in the sandbox, with auto_reply.api_key", "")
	default:
		add("auto_reply", stOK, fmt.Sprintf("backend %s; untrusted peers: %s", rep.AutoReply.Backend, rep.AutoReply.Untrusted), "")
	}
	if backendAcceptsUntrusted(cfg) {
		add("a2a.backends", stWarn, "an A2A backend accepts untrusted peers (modules.a2a.backends[].accept_untrusted)",
			"only with toolless: true; see A2A-DESIGN §11.6")
	}

	// SI-5.
	if len(rep.SI5Changed) == 0 {
		add("si5", stOK, "all fresh-install safety defaults in place (SI-5)", "")
	} else {
		add("si5", stInfo, "differs from a fresh install: "+strings.Join(rep.SI5Changed, ", "), "")
	}

	// Coding tools and Hermes (§13.1).
	rep.Agents = probeAgentWiring(env)
	for _, a := range rep.Agents {
		switch {
		case a.Wired:
			add("agents."+a.Tool, stOK, "anet wired in "+a.Config+a.detailSuffix(), "")
		case a.Present:
			add("agents."+a.Tool, stInfo, "installed, anet not wired ("+a.Config+")", "anet agents wire "+a.Tool)
		}
	}
	rep.Hermes.A2AAgents = []a2aAgentEntry{}
	rep.Hermes.A2AToken = "no_entries"
	if env.hermesHome != "" {
		rep.Hermes.Config = statFile(filepath.Join(env.hermesHome, "config.yaml"))
	}
	if rep.Hermes.Config.Present {
		b, _ := os.ReadFile(rep.Hermes.Config.Path)
		rep.Hermes.A2AAgents = hermesA2AAgents(string(b), rep.A2A.Addr)
		// Only entries on this node's A2A port are this node's; the token is
		// compared for those (entries of another identity carry its token).
		if slices.ContainsFunc(rep.Hermes.A2AAgents, func(e a2aAgentEntry) bool { return e.Matches }) {
			rep.Hermes.A2AToken = hermesTokenState(string(b), rep.A2A.Token)
		} else if len(rep.Hermes.A2AAgents) > 0 {
			rep.Hermes.A2AToken = "unknown"
		}
		switch rep.Hermes.A2AToken {
		case "stale":
			add("hermes.a2a_token", stWarn, rep.Hermes.Config.Path+": the a2a_agents entries do not carry the current "+
				"local A2A token (a2a_token.txt was replaced), so Hermes' calls are refused", "anet agents wire --refresh")
		case "current":
			add("hermes.a2a_token", stOK, "the a2a_agents entries carry the current local A2A token", "")
		}
		switch {
		case rep.Hermes.Config.Private:
			add("hermes.config", stOK, rep.Hermes.Config.Path+" mode "+rep.Hermes.Config.Mode, "")
		case len(rep.Hermes.A2AAgents) > 0:
			add("hermes.config", stFail, rep.Hermes.Config.Path+" holds the local A2A token and is readable by others (mode "+
				rep.Hermes.Config.Mode+")", "chmod 600 "+rep.Hermes.Config.Path)
		default:
			add("hermes.config", stWarn, rep.Hermes.Config.Path+" is readable by others (mode "+rep.Hermes.Config.Mode+")",
				"chmod 600 "+rep.Hermes.Config.Path)
		}
		for _, e := range rep.Hermes.A2AAgents {
			switch {
			case rep.A2A.Addr == "":
				add("hermes.a2a_agents", stUnknown, e.URL+": no a2a_addr.txt to compare the port with", "")
			case !e.Matches:
				add("hermes.a2a_agents", stWarn, e.URL+": port "+e.Port+" is not the local A2A port ("+rep.A2A.Addr+")",
					"anet agents wire --refresh")
			default:
				add("hermes.a2a_agents", stOK, e.URL, "")
			}
		}
	}

	rep.OK = true
	for _, c := range rep.Checks {
		if c.Status == stFail {
			rep.OK = false
		}
	}
	return rep, nil
}

func tokenCheck(add func(id, status, detail, hint string), id string, fs fileState) {
	switch {
	case !fs.Present:
		add(id, stInfo, "no "+fs.Path+" yet (written when the daemon starts)", "")
	case !fs.Private:
		add(id, stFail, fs.Path+" is readable by others (mode "+fs.Mode+")", "chmod 600 "+fs.Path)
	default:
		add(id, stOK, fs.Path+" mode "+fs.Mode+" (does not expire; replaced by deleting it and restarting)", "")
	}
}

// backendAcceptsUntrusted reads modules.a2a.backends for accept_untrusted,
// without depending on the a2a module's types.
func backendAcceptsUntrusted(cfg daemon.Config) bool {
	raw, ok := cfg.Modules["a2a"]
	if !ok {
		return false
	}
	var m struct {
		Backends []struct {
			AcceptUntrusted bool `json:"accept_untrusted"`
		} `json:"backends"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	for _, b := range m.Backends {
		if b.AcceptUntrusted {
			return true
		}
	}
	return false
}

func (a agentWire) detailSuffix() string {
	if a.Detail == "" {
		return ""
	}
	return "; " + a.Detail
}

// probeAgentWiring looks for an anet entry in each coding tool's own
// configuration file, where `anet agents wire` (internal/agentwire) puts it.
// It reads files only; whether the tool can start `anet mcp` is not
// checked (Handshake "not_checked"). When agentwire lands it should report
// this itself, and this probe is the fallback for a no_mcp build.
func probeAgentWiring(env doctorEnv) []agentWire {
	out := []agentWire{}
	add := func(tool, cfg string, present, wired bool, detail string) {
		out = append(out, agentWire{Tool: tool, Config: cfg, Present: present, Wired: wired, Detail: detail,
			Handshake: "not_checked"})
	}
	if env.home != "" {
		probeHomeTools(env.home, add)
	}

	if env.hermesHome != "" {
		hermes := filepath.Join(env.hermesHome, "config.yaml")
		hp, hw := false, false
		if b, err := os.ReadFile(hermes); err == nil {
			hp, hw = true, yamlHasChild(string(b), "mcp_servers", "anet")
		}
		add("hermes", hermes, hp, hw, "")
	}
	return out
}

// probeHomeTools looks at the tools whose configuration lives under the
// home directory.
func probeHomeTools(h string, add func(tool, cfg string, present, wired bool, detail string)) {
	claude := filepath.Join(h, ".claude.json")
	p, w := jsonHasPath(claude, "mcpServers", "anet")
	detail := ""
	if fileExists(filepath.Join(h, ".claude", "skills", "anet", "SKILL.md")) {
		detail = "skill ~/.claude/skills/anet/SKILL.md present"
	}
	add("claude-code", claude, p || fileExists(filepath.Join(h, ".claude")), w, detail)

	codex := filepath.Join(h, ".codex", "config.toml")
	cp, cw := false, false
	if b, err := os.ReadFile(codex); err == nil {
		cp = true
		for _, ln := range strings.Split(string(b), "\n") {
			t := strings.ReplaceAll(strings.TrimSpace(ln), " ", "")
			if t == "[mcp_servers.anet]" || t == `[mcp_servers."anet"]` {
				cw = true
			}
		}
	}
	add("codex", codex, cp, cw, "")

	cursor := filepath.Join(h, ".cursor", "mcp.json")
	p, w = jsonHasPath(cursor, "mcpServers", "anet")
	add("cursor", cursor, p || fileExists(filepath.Join(h, ".cursor")), w, "")

	opencode := filepath.Join(h, ".config", "opencode", "opencode.json")
	p, w = jsonHasPath(opencode, "mcp", "anet")
	add("opencode", opencode, p, w, "")
}

// jsonHasPath reports whether a JSON file exists and has the nested key
// path keys.
func jsonHasPath(path string, keys ...string) (present, wired bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return true, false
	}
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return true, false
		}
		if cur, ok = mm[k]; !ok {
			return true, false
		}
	}
	return true, true
}

// yamlHasChild reports whether a top-level YAML mapping key has a child
// key: `parent:` at some indent, then `child:` indented deeper before the
// block ends. A line scan, not a parser; the module has no YAML dependency
// and needs none for this.
func yamlHasChild(text, parent, child string) bool {
	in, indent := false, 0
	for _, ln := range strings.Split(text, "\n") {
		body := ln
		if i := strings.Index(body, " #"); i >= 0 {
			body = body[:i]
		}
		t := strings.TrimSpace(body)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		ind := len(body) - len(strings.TrimLeft(body, " \t"))
		if in {
			if ind <= indent {
				in = false
			} else if strings.HasPrefix(t, child+":") || strings.HasPrefix(t, `"`+child+`":`) {
				return true
			}
		}
		if !in && (t == parent+":" || t == `"`+parent+`":`) {
			in, indent = true, ind
		}
	}
	return false
}

var a2aURLRe = regexp.MustCompile(`https?://[^\s"'<>]+/a2a/v1/agents/[^\s"'<>]+`)

// hermesTokenState compares the tokens of the a2a_agents entries with the
// local A2A token, without reporting either: "current" when the token file's
// token is in the Hermes config, "stale" when it is not, "unknown" when there
// is no token file to compare with. The local A2A token does not expire; it
// goes stale for Hermes when a2a_token.txt is replaced.
func hermesTokenState(text string, token fileState) string {
	if !token.Present {
		return "unknown"
	}
	b, err := os.ReadFile(token.Path)
	tok := strings.TrimSpace(string(b))
	if err != nil || tok == "" {
		return "unknown"
	}
	if strings.Contains(text, tok) {
		return "current"
	}
	return "stale"
}

// hermesA2AAgents finds the a2a_agents URLs that point at an anet local A2A
// interface and compares each one's port with a2aAddr.
func hermesA2AAgents(text, a2aAddr string) []a2aAgentEntry {
	_, wantPort, _ := net.SplitHostPort(a2aAddr)
	out := []a2aAgentEntry{}
	for _, raw := range a2aURLRe.FindAllString(text, -1) {
		// A flow mapping ({url: …, auth: …}) leaves its punctuation on the match.
		raw = strings.TrimRight(raw, ",}]")
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		port := u.Port()
		if port == "" {
			port = "80"
			if u.Scheme == "https" {
				port = "443"
			}
		}
		out = append(out, a2aAgentEntry{URL: raw, Port: port, Matches: wantPort != "" && port == wantPort})
	}
	return out
}

// renderDoctor prints the report for a person.
func renderDoctor(w io.Writer, rep *doctorReport) {
	mark := map[string]string{stOK: "✓", stInfo: "·", stWarn: "!", stFail: "✗", stUnknown: "?"}
	fmt.Fprintf(w, "anet doctor — %s\n\n", rep.DataDir)
	for _, c := range rep.Checks {
		fmt.Fprintf(w, "  %s %-20s %s\n", mark[c.Status], c.ID, c.Detail)
		if c.Hint != "" && c.Status != stOK {
			fmt.Fprintf(w, "    %-20s → %s\n", "", c.Hint)
		}
	}
	fmt.Fprintln(w, "\nSI-5 (the safety defaults of a fresh install):")
	for _, k := range daemon.SI5Keys {
		m := "✓"
		if slices.Contains(rep.SI5Changed, k) {
			m = "≠"
		}
		fmt.Fprintf(w, "  %s %-28s %s\n", m, k, compactJSON(rep.SI5[k]))
	}
	if !rep.OK {
		fmt.Fprintln(w, "\nA check failed (✗); see above.")
	}
}

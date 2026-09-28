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
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/anethome"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/localpeer"
	"github.com/ANetResearch/ANet/internal/loopguard"
	"github.com/ANetResearch/ANet/internal/official"
	"github.com/ANetResearch/ANet/internal/release"
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

// agentWire is one coding tool as doctor reports it: from agentwire.Inspect
// (doctor_agents.go), or in a -tags no_mcp build from a read of the tool's
// configuration file (doctor_agents_nomcp.go).
type agentWire struct {
	Tool    string `json:"tool"`
	Config  string `json:"config"`
	Present bool   `json:"present"` // the tool is on this machine
	Wired   bool   `json:"wired"`   // its configuration has anet's entry
	// Current is set when `anet agents wire` would change nothing; Pending
	// says what it would change otherwise.
	Current  bool     `json:"current"`
	Pending  []string `json:"pending,omitempty"`
	Conflict string   `json:"conflict,omitempty"` // a same-named entry anet does not manage
	Error    string   `json:"error,omitempty"`    // the tool could not be checked
	Detail   string   `json:"detail,omitempty"`
	// Handshake is whether the tool was started and answered through
	// anet's MCP server. doctor does not start tools: "not_checked".
	Handshake string `json:"handshake"`
}

// a2aAgentEntry is one Hermes a2a_agents entry pointing at this node's
// local A2A interface. Matches is whether its address is a2a_addr.txt's;
// Token whether it carries a2a_token.txt's token: "current", "stale" or
// "unknown" (Unknown says why it could not be compared). Either one off
// is what `anet agents wire --refresh` repairs (0017 Q13).
type a2aAgentEntry struct {
	URL     string `json:"url"`
	AID     string `json:"aid,omitempty"`
	Port    string `json:"port"`
	Matches bool   `json:"matches_a2a_addr"`
	Token   string `json:"token"`
	Unknown string `json:"unknown,omitempty"`
}

// agentsView is what the build's probe found: the tools, Hermes'
// configuration file, and its a2a_agents entries.
type agentsView struct {
	tools        []agentWire
	hermesConfig string
	a2a          []a2aAgentEntry
	// note is said once about the probe itself (a no_mcp build); err is
	// set when the tools could not be inspected at all.
	note, err string
}

type doctorReport struct {
	Schema  string `json:"schema"`
	DataDir string `json:"data_dir"`
	Version struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Built   string `json:"built"`
		// Signature is the check of the release record the installer left
		// beside this binary (release.CheckInstalled): verified,
		// unverified or unknown.
		Signature       string `json:"signature"`
		SignatureDetail string `json:"signature_detail"`
		ReleaseRecord   string `json:"release_record,omitempty"`
	} `json:"version"`
	Modules []string `json:"modules"`
	// Official is the official-agent manifest built into this binary
	// (A2A-DESIGN §15): "ok", "expired" or "invalid".
	Official struct {
		Status         string `json:"status"`
		Seq            uint64 `json:"seq,omitempty"`
		Agents         int    `json:"agents"`
		ExpiresAt      string `json:"expires_at,omitempty"`
		KeyFingerprint string `json:"key_fingerprint,omitempty"`
		Error          string `json:"error,omitempty"`
	} `json:"official"`
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
		// PortConflict is the record module/a2a leaves while the interface
		// is down because another process held its port (A2A-DESIGN §11.1
		// [redteam:F18]); PortHolderUIDs the uids the socket table shows
		// listening on Addr, when this system has one.
		PortConflict   string `json:"port_conflict,omitempty"`
		PortHolderUIDs []int  `json:"port_holder_uids,omitempty"`
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
	Payments  daemon.SpendLimits `json:"payments"`
	Payees    []string           `json:"payees"`
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
		// A2AToken sums up the entries' tokens: "stale" when one does not
		// carry the current local A2A token (a2a_token.txt was replaced
		// since it was written), "current" when those that can be compared
		// do, "unknown" when none can be, and "no_entries".
		A2AToken string `json:"a2a_token"`
	} `json:"hermes"`
	Checks []doctorCheck `json:"checks"`
	OK     bool          `json:"ok"`
}

// doctorEnv is what doctor reads outside the data directory. Tests replace
// it so no real home directory, environment or PATH is read.
type doctorEnv struct {
	home       string // the user's home directory
	hermesHome string // HERMES_HOME, or ~/.hermes
	// exe is this binary, whose release record is checked and which the
	// coding tools' entries should run ("": the running executable).
	exe string
	// getenv and lookPath are what the tool probe reads the environment
	// and PATH with (nil: os.Getenv, exec.LookPath).
	getenv   func(string) string
	lookPath func(string) (string, error)
	// running reports whether a daemon answers at the control address with
	// the token in tokenPath.
	running func(addr, tokenPath string) bool
	// listenerUIDs returns the uids listening on an address, from the
	// socket table (nil: not checked; see internal/localpeer).
	listenerUIDs func(addr string) ([]int, error)
}

func defaultDoctorEnv() doctorEnv {
	home, _ := os.UserHomeDir()
	hh := os.Getenv("HERMES_HOME")
	if hh == "" && home != "" {
		hh = filepath.Join(home, ".hermes")
	}
	return doctorEnv{home: home, hermesHome: hh, running: daemonAnswersAt, listenerUIDs: localpeer.ListenerUIDs}
}

// env reads an environment variable; HERMES_HOME is the one doctor
// already resolved.
func (e doctorEnv) env(k string) string {
	if k == "HERMES_HOME" && e.hermesHome != "" {
		return e.hermesHome
	}
	if e.getenv != nil {
		return e.getenv(k)
	}
	return os.Getenv(k)
}

// binary is the running anet binary, symlinks resolved.
func (e doctorEnv) binary() string {
	if e.exe != "" {
		return e.exe
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe
}

// daemonAnswersAt is localDaemonUp for the address doctor has already read.
// localDaemonUp finds the address through LoadConfig, which writes a
// config.json when there is none, and doctor writes nothing.
func daemonAnswersAt(addr, tokenPath string) bool {
	// Only a loopback control address is sent the token (§7.1); the daemon
	// does not start on any other.
	if loopguard.CheckLoopbackAddr(addr) != nil {
		return false
	}
	tb, err := os.ReadFile(tokenPath)
	if err != nil {
		return false
	}
	c := &client{base: "http://" + addr, token: strings.TrimSpace(string(tb)), timeout: 1500 * time.Millisecond}
	_, code, e := c.fetch("/status", nil)
	return e == nil && code == 200
}

// officialCheck reports the official-agent manifest this binary carries:
// which agents list_agents and anet find will mark "anet.official", and
// until when. A manifest that does not verify or has expired marks no one;
// that is a warning, not a failure — the node works, it only cannot tell
// the project's agents from their namesakes.
func officialCheck(add func(id, status, detail, hint string), rep *doctorReport, now time.Time) {
	m, err := official.Embedded()
	if err != nil {
		rep.Official.Status, rep.Official.Error = "invalid", err.Error()
		add("official", stWarn, "the official-agent manifest in this binary does not verify; no agent is marked official: "+err.Error(),
			"install a signed release: anet update")
		return
	}
	rep.Official.Seq, rep.Official.Agents = m.Seq, len(m.Agents)
	rep.Official.ExpiresAt, rep.Official.KeyFingerprint = m.ExpiresAt, m.KeyFingerprint
	if err := m.CheckFresh(now); err != nil {
		rep.Official.Status = "expired"
		add("official", stWarn, err.Error()+"; no agent is marked official", "anet update")
		return
	}
	rep.Official.Status = "ok"
	add("official", stInfo, fmt.Sprintf("official-agent manifest seq %d: %d agent(s) marked anet.official by AID, valid until %s (signed by %s)",
		m.Seq, len(m.Agents), m.ExpiresAt, m.KeyFingerprint), "")
}

// releaseCheck checks the record install.sh or `anet update` left beside
// this binary (§13.2): the manifest verified again with the release key
// compiled in, and this binary's sha256 found in it.
func releaseCheck(env doctorEnv) release.InstalledCheck {
	exe := env.binary()
	if exe == "" {
		return release.InstalledCheck{Status: release.InstalledUnknown, Detail: "cannot locate this binary"}
	}
	return release.CheckInstalled(exe, release.DefaultTrust(), time.Now())
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
	rc := releaseCheck(env)
	rep.Version.Signature, rep.Version.SignatureDetail, rep.Version.ReleaseRecord = rc.Status, rc.Detail, rc.Record
	add("version", stInfo, fmt.Sprintf("anet %s (commit %s, built %s)", daemon.Version, daemon.BuildCommit, daemon.BuildAt), "")
	switch rc.Status {
	case release.InstalledVerified:
		add("version.signature", stOK, "verified: "+rc.Detail, "")
	case release.InstalledUnverified:
		add("version.signature", stWarn, "unverified: "+rc.Detail,
			"install the signed release again (install.sh), or `anet update` to a newer one")
	default:
		add("version.signature", stUnknown, rc.Detail, "")
	}
	rep.Modules = module.Compiled()
	if rep.Modules == nil {
		rep.Modules = []string{}
	}
	add("modules", stInfo, compiledModulesReport(), "")
	officialCheck(add, rep, time.Now())

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
	// The module keeps both files in its state directory (anethome.A2ADir),
	// the same path in every build, no_a2a included.
	rep.A2A.AddrFile = statFile(filepath.Join(anethome.A2ADir(layout.Root), anethome.A2AAddrFile))
	rep.A2A.Token = statFile(filepath.Join(anethome.A2ADir(layout.Root), anethome.A2ATokenFile))
	if rep.A2A.AddrFile.Present {
		if b, err := os.ReadFile(rep.A2A.AddrFile.Path); err == nil {
			rep.A2A.Addr = strings.TrimSpace(string(b))
		}
	}
	if b, err := os.ReadFile(filepath.Join(anethome.A2ADir(layout.Root), anethome.A2AConflictFile)); err == nil {
		rep.A2A.PortConflict = strings.TrimSpace(string(b))
	}
	const freePort = "free the port (stop the process holding it), restart the node (anet stop && anet up), then: anet agents wire --refresh"
	switch {
	case !rep.A2A.Compiled && rep.A2A.Addr == "":
		add("a2a", stInfo, "the local A2A interface is not in this build (no_a2a)", "")
	case rep.A2A.Addr == "":
		add("a2a", stWarn, "no "+rep.A2A.AddrFile.Path+": the local A2A interface has not started in this data directory yet",
			"anet up")
	case rep.A2A.PortConflict != "":
		// The interface stays down rather than move away from the port its
		// clients were given (A2A-DESIGN §11.1 [redteam:F18]).
		add("a2a", stFail, "the local A2A interface is not running: "+rep.A2A.PortConflict, freePort)
	default:
		add("a2a", stOK, "local A2A interface at http://"+rep.A2A.Addr+"/a2a/v1/agents", "")
	}
	// Who listens on the recorded port. Clients configured with it (Hermes'
	// a2a_agents entries) send the local A2A token there without asking, so
	// a listener of another user is a credential leak in progress — above
	// all while the daemon is down, when nothing else notices.
	if rep.A2A.Addr != "" && env.listenerUIDs != nil {
		if uids, err := env.listenerUIDs(rep.A2A.Addr); err == nil {
			rep.A2A.PortHolderUIDs = uids
			for _, u := range uids {
				if u != os.Getuid() {
					add("a2a.port", stFail, fmt.Sprintf("%s is held by uid %d, another local user: clients configured with this "+
						"address send the local A2A token to that process", rep.A2A.Addr, u), freePort)
					break
				}
			}
		}
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
	a2aBackendChecks(add, st.Inbound.Policy, cfg, len(st.Trust)) // doctor_backends.go

	// SI-5.
	if len(rep.SI5Changed) == 0 {
		add("si5", stOK, "all fresh-install safety defaults in place (SI-5)", "")
	} else {
		add("si5", stInfo, "differs from a fresh install: "+strings.Join(rep.SI5Changed, ", "), "")
	}

	// Coding tools and Hermes (§13.1).
	av := inspectAgents(layout, env) // doctor_agents.go; doctor_agents_nomcp.go in a no_mcp build
	rep.Agents = av.tools
	if rep.Agents == nil {
		rep.Agents = []agentWire{}
	}
	if av.err != "" {
		add("agents", stUnknown, "the coding tools could not be inspected: "+av.err, "")
	}
	if av.note != "" {
		add("agents", stInfo, av.note, "")
	}
	rep.Hermes.A2AAgents = av.a2a
	if rep.Hermes.A2AAgents == nil {
		rep.Hermes.A2AAgents = []a2aAgentEntry{}
	}
	rep.Hermes.A2AToken = a2aTokenSummary(rep.Hermes.A2AAgents)
	stale := rep.Hermes.A2AToken == "stale" || rep.A2A.Addr != "" &&
		slices.ContainsFunc(rep.Hermes.A2AAgents, func(e a2aAgentEntry) bool { return e.Unknown == "" && !e.Matches })
	for _, a := range rep.Agents {
		fix := "anet agents wire " + a.Tool
		if a.Tool == "hermes" && stale {
			fix += " --refresh"
		}
		switch {
		case a.Conflict != "":
			add("agents."+a.Tool, stWarn, "conflict: "+a.Conflict, "")
		case a.Error != "":
			add("agents."+a.Tool, stWarn, "cannot check "+a.Config+": "+a.Error, "")
		case a.Wired && a.Current:
			add("agents."+a.Tool, stOK, "anet wired in "+a.Config+a.detailSuffix(), "")
		case a.Wired:
			add("agents."+a.Tool, stWarn, "anet wired in "+a.Config+", not current: "+strings.Join(a.Pending, "; "), fix)
		case a.Present:
			add("agents."+a.Tool, stInfo, "installed, anet not wired ("+a.Config+")", fix)
		}
	}
	if av.hermesConfig != "" {
		rep.Hermes.Config = statFile(av.hermesConfig)
	}
	if rep.Hermes.Config.Present {
		// 0017 Q13: an entry "expires" when its token is no longer
		// a2a_token.txt's or its port no longer a2a_addr.txt's; wire
		// --refresh rewrites both.
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
			case e.Unknown != "":
				add("hermes.a2a_agents", stUnknown, e.URL+": cannot compare with the local A2A interface ("+e.Unknown+")", "")
			case rep.A2A.Addr == "" && !e.Matches:
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

func (a agentWire) detailSuffix() string {
	if a.Detail == "" {
		return ""
	}
	return "; " + a.Detail
}

// a2aTokenSummary sums up the tokens of the a2a_agents entries (see
// doctorReport.Hermes.A2AToken).
func a2aTokenSummary(entries []a2aAgentEntry) string {
	if len(entries) == 0 {
		return "no_entries"
	}
	sum := "unknown"
	for _, e := range entries {
		switch e.Token {
		case "stale":
			return "stale"
		case "current":
			sum = "current"
		}
	}
	return sum
}

// urlPort is the port of a URL, the scheme's default when it names none.
func urlPort(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
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

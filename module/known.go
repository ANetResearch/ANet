package module

// known.go is the name table: the one list of the optional modules of this
// release, what each of them owns, and what each of them is granted.
//
// No build tag, on purpose (the same reason as cmd/anet/optin_tags.go): a
// build that does not contain a module still has to know its name, its
// package paths and the sentence that says why it is absent.
//
// Three questions are answered here and nowhere else:
//
//   - which names exist (Known, Lookup), and how a name is built (Tag);
//   - whether a name is available in this process (Availability), which is
//     a value and not process state: the daemon builds one from its own
//     configuration, the CLI builds one from the configuration it read;
//   - what a module may be handed by the kernel (Grants, EvidenceTypes,
//     StatusKeys), which the Host bound to that name enforces.

import (
	"slices"
	"sort"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// Grant is one thing a module's Host hands over: a base Host method that
// would otherwise answer with a refusal, or an optional Host interface that
// would otherwise fail its type assertion.
//
// Adding a grant is a decision made here, in the name table, for a named
// module, with the reason a smaller grant is not enough written beside the
// interface it opens (module.go). There is no way to obtain one by
// convention.
type Grant string

// The grants. The first group are Host methods (they exist on every Host and
// answer a refusal when not granted); the second are optional Host
// interfaces (a Host that does not hold the grant does not implement them,
// so a type assertion fails).
const (
	GrantProviders       Grant = "Providers"
	GrantRecordEvidence  Grant = "RecordEvidence"
	GrantResolveKEL      Grant = "ResolveKEL"
	GrantPaymentSeam     Grant = "PaymentSeam"
	GrantHubSeam         Grant = "HubSeam"
	GrantTaskSeam        Grant = "TaskSeam"
	GrantStateDir        Grant = "StateDir"
	GrantDeclareUntrust  Grant = "DeclareUntrustedBackend"
	GrantInboundTaskHost Grant = "InboundTaskHost"
	GrantProxyCardSigner Grant = "ProxyCardSigner"
	GrantTransportHost   Grant = "TransportHost"
	GrantSensitivePaths  Grant = "SensitivePathsHost"

	// Named here so that the table is complete; the interfaces themselves
	// arrive with the packages that build the seams (MOD-3: the first
	// three; MOD-5: PreviewHost).
	GrantTurnHost         Grant = "TurnHost"
	GrantOwnEvidenceHost  Grant = "OwnEvidenceHost"
	GrantModuleConfigHost Grant = "ModuleConfigHost"
	GrantPreviewHost      Grant = "PreviewHost"
)

// Info is one row of the name table.
type Info struct {
	// Name is the module's one name: its build tag (without the prefix),
	// the word in `anet version`, in `without`, in `--without`.
	Name string
	// Tag is the build tag that removes (`no_<name>`) or adds (`shell`) the
	// module; empty while the module has no tag yet (autoreply and preview
	// until they move out, console until v0.3.x).
	Tag string
	// Additive is true for a module a tag adds (shell); every other module
	// is in the default build and a tag removes it.
	Additive bool
	// Packages are the module's Go packages, relative to this module's
	// path. They are the same list scripts/tagcheck.sh counts symbols of,
	// and the set the kernel may not import.
	Packages []string
	// ConfigKey is where the module's configuration block lives
	// ("modules.<name>", or one of the two historical top-level keys), and
	// empty for a module that has none.
	ConfigKey string
	// Routes are the control-plane routes (method and pattern) the module
	// owns. They exist in every build; the kernel answers module_absent for
	// them while the module is not available.
	Routes []string
	// MCPTools, CLICommands and DoctorIDs name the other entrances that
	// belong to the module.
	MCPTools    []string
	CLICommands []string
	DoctorIDs   []string
	// Zh and En are the module's names in a sentence ("付款" / "payment").
	Zh, En string
	// Grants, EvidenceTypes and StatusKeys are the whole authorization of
	// the module's Host. EvidenceTypes entries ending in ".*" stand for a
	// prefix.
	Grants        []Grant
	EvidenceTypes []string
	StatusKeys    []string
	// AuditFields are the configuration members whose change the kernel
	// audits (anet.policy.changed) when it writes the module's block.
	AuditFields []string
	// AbsenceReady says the module's absent path has landed, so that the
	// name may be written into `without`. A name for which it is false is
	// refused there, so that no configuration says "off" for a module that
	// would keep running.
	AbsenceReady bool
}

// granted reports whether g is among the grants.
func (i Info) granted(g Grant) bool { return slices.Contains(i.Grants, g) }

// Granted reports whether the module is granted g.
func (i Info) Granted(g Grant) bool { return i.granted(g) }

// MayRecord reports whether the module may append evidence of this type.
func (i Info) MayRecord(eventType string) bool {
	if !i.granted(GrantRecordEvidence) || strings.HasPrefix(eventType, family("policy")[:len(family("policy"))-1]) {
		return false
	}
	for _, t := range i.EvidenceTypes {
		if p, ok := strings.CutSuffix(t, "*"); ok {
			if strings.HasPrefix(eventType, p) && len(eventType) > len(p) {
				return true
			}
			continue
		}
		if t == eventType {
			return true
		}
	}
	return false
}

// MayStatus reports whether the module may contribute /status member key.
func (i Info) MayStatus(key string) bool { return slices.Contains(i.StatusKeys, key) }

// The 14 MCP tools. They exist whenever the mcp module is available and do
// not depend on any other module (MOD-P6).
var mcpTools = []string{"audit", "cancel_task", "get_agent_card", "get_balance", "get_task", "inbound_pending",
	"list_agents", "list_tasks", "node_status", "reject_payment", "reply_task", "send_message", "submit_payment",
	"wait_task"}

// known is the table, in alphabetical order of the names.
//
// The grants are what each module's call sites use today (measured, not
// planned): a module that does not call a method is not given it.
var known = []Info{
	{Name: "a2a", Tag: "no_a2a", Packages: []string{"module/a2a"}, ConfigKey: "modules.a2a",
		Zh: "本机 A2A 接口", En: "local A2A interface",
		DoctorIDs: []string{"a2a", "a2a.backends", "a2a.backends.retry", "a2a.port", "a2a.token"},
		Grants: []Grant{GrantTaskSeam, GrantInboundTaskHost, GrantProxyCardSigner, GrantStateDir, GrantRecordEvidence,
			GrantResolveKEL, GrantDeclareUntrust},
		EvidenceTypes: []string{"anet.backend.failed", "anet.backend.forwarded"},
		AbsenceReady:  true},
	{Name: "anetlink", Tag: "no_anetlink", Packages: []string{"module/anetlink", "provider/anetlink"},
		ConfigKey: "modules.anetlink", Zh: "设备能力", En: "device capability",
		DoctorIDs: []string{"anetlink.publish", "anetlink.socket"},
		Grants:    []Grant{GrantProviders}, AbsenceReady: true},
	// autoreply has no tag and no registration yet: it is declared compiled
	// (internal/daemon/declare.go) until MOD-4 moves it out. AbsenceReady is
	// false until MOD-3 lands the seam that makes its absence a real path.
	{Name: "autoreply", ConfigKey: "auto_reply", Zh: "自动回复", En: "auto-reply",
		Routes:      []string{"POST /autoreply", "POST /autoreply-test"},
		CLICommands: []string{"autoreply"},
		DoctorIDs:   []string{"auto_reply", "sandbox", "sandbox.agent", "sandbox.budget", "sandbox.egress", "sandbox.limits", "sandbox.upstream"},
		Grants: []Grant{GrantTurnHost, GrantOwnEvidenceHost, GrantSensitivePaths, GrantModuleConfigHost, GrantStateDir,
			GrantRecordEvidence},
		EvidenceTypes: []string{"anet.autoreply.invoked"},
		StatusKeys:    []string{"auto_reply"},
		AuditFields: []string{"trusted", "sandbox.egress_allow", "sandbox.runs_per_day", "sandbox.runs_per_peer_per_day",
			"api_key_set"}},
	{Name: "blackboard", Tag: "no_blackboard", Packages: []string{"module/blackboard"}, ConfigKey: "modules.blackboard",
		Zh: "共享黑板", En: "shared blackboard",
		Grants: []Grant{GrantProviders, GrantResolveKEL}, AbsenceReady: true},
	{Name: "cas", Tag: "no_cas", Packages: []string{"module/cas"}, ConfigKey: "modules.cas",
		Zh: "内容存储", En: "content store",
		Grants: []Grant{GrantProviders}, AbsenceReady: true},
	// console is not a module.Module and has no configuration block; the
	// names of its routes are listed so that the table owns them.
	{Name: "console", Zh: "控制台", En: "console",
		Routes: []string{"GET /console", "GET /{$}", "POST /console/session", "POST /console/switch",
			"POST /console/ticket"},
		CLICommands: []string{"console"}, AbsenceReady: true},
	{Name: "mcp", Tag: "no_mcp", Packages: []string{"internal/mcpserv", "internal/agentwire"},
		Zh: "MCP", En: "MCP", MCPTools: mcpTools, CLICommands: []string{"agents", "mcp"},
		DoctorIDs: []string{"agents", "agents.*", "hermes.*"}, AbsenceReady: true},
	{Name: "org", Tag: "no_org", Packages: []string{"module/org"}, ConfigKey: "modules.org",
		Zh: "组织", En: "organisation",
		Grants: []Grant{GrantProviders, GrantResolveKEL}, AbsenceReady: true},
	{Name: "p2p", Tag: "no_p2p", Packages: []string{"module/p2p"}, ConfigKey: "modules.p2p",
		Zh: "直连", En: "direct delivery",
		Grants: []Grant{GrantTransportHost}, AbsenceReady: true},
	{Name: "preview", ConfigKey: "stream_preview", Zh: "预览", En: "preview",
		Routes:      []string{"POST /preview", "POST /tasks/preview"},
		CLICommands: []string{"preview"},
		DoctorIDs:   []string{"preview"},
		Grants:      []Grant{GrantPreviewHost}, StatusKeys: []string{"preview"}, AbsenceReady: true},
	{Name: "service", Tag: "no_service", Packages: []string{"module/service"}, ConfigKey: "modules.service",
		Zh: "服务", En: "service",
		DoctorIDs: []string{"capabilities.files"},
		Grants:    []Grant{GrantProviders}, AbsenceReady: true},
	{Name: "shell", Tag: "shell", Additive: true, Packages: []string{"module/shell"}, ConfigKey: "modules.shell",
		Zh: "远程命令", En: "remote command",
		Grants: []Grant{GrantProviders, GrantRecordEvidence}, EvidenceTypes: []string{family("shell")}, AbsenceReady: true},
	{Name: "x402", Tag: "no_x402", Packages: []string{"module/x402"}, ConfigKey: "modules.x402",
		Zh: "付款", En: "payment",
		Routes:        []string{"POST /audit-hub", "POST /reconcile", "POST /redeem", "POST /tasks/pay", "POST /tasks/pay-manual"},
		CLICommands:   []string{"audit-hub", "pay", "reconcile", "redeem"},
		DoctorIDs:     []string{"payments", "payments.audit", "payments.card", "payments.reviews", "payments.unchargeable"},
		Grants:        []Grant{GrantProviders, GrantPaymentSeam, GrantRecordEvidence, GrantStateDir},
		EvidenceTypes: []string{family("payment"), family("credit"), family("issuance"), "anet.supply.proof"},
		AbsenceReady:  true},
}

// Known is the name table, a copy, in the alphabetical order of the names.
func Known() []Info {
	out := make([]Info, len(known))
	for i, k := range known {
		k.Packages = slices.Clone(k.Packages)
		k.Routes = slices.Clone(k.Routes)
		k.MCPTools = slices.Clone(k.MCPTools)
		k.CLICommands = slices.Clone(k.CLICommands)
		k.DoctorIDs = slices.Clone(k.DoctorIDs)
		k.Grants = slices.Clone(k.Grants)
		k.EvidenceTypes = slices.Clone(k.EvidenceTypes)
		k.StatusKeys = slices.Clone(k.StatusKeys)
		k.AuditFields = slices.Clone(k.AuditFields)
		out[i] = k
	}
	return out
}

// Lookup is the row of a name.
func Lookup(name string) (Info, bool) {
	for _, k := range Known() {
		if k.Name == name {
			return k, true
		}
	}
	return Info{}, false
}

// RouteOwner is the module that owns a control-plane route pattern
// ("POST /tasks/pay"), and false for a kernel route.
func RouteOwner(pattern string) (string, bool) {
	for _, k := range known {
		if slices.Contains(k.Routes, pattern) {
			return k.Name, true
		}
	}
	return "", false
}

// Availability says which modules are available in this process: compiled
// in, and not named in `without`. It is a value. There is no process-wide
// instance: a daemon builds one from its configuration when it is created
// and hands it to what it starts, the CLI builds one from the configuration
// it read, and two daemons in one process (the tests start several) each
// hold their own.
//
// Every question of the form "is module m there" goes through Available
// (TestAvailabilityThroughOneFunction). The zero value has an empty
// `without`.
type Availability struct{ without []string }

// NewAvailability validates a `without` list: every name in the name table,
// none twice, and none whose absent path has not landed (AbsenceReady). The
// error is a *a2ashape.Refusal, config_invalid naming `without`.
func NewAvailability(without []string) (Availability, error) {
	seen := map[string]bool{}
	for _, n := range without {
		info, ok := Lookup(n)
		switch {
		case !ok:
			return Availability{}, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "without",
				"rule": ruleText("没有叫 "+clip(n)+" 的模块", "there is no module named "+clip(n))})
		case seen[n]:
			return Availability{}, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "without",
				"rule": ruleText("模块 "+n+" 写了不止一次", "the module "+n+" is listed more than once")})
		case !info.AbsenceReady:
			return Availability{}, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "without",
				"rule": "without_not_ready", "m": n})
		}
		seen[n] = true
	}
	w := slices.Clone(without)
	sort.Strings(w)
	return Availability{without: w}, nil
}

// Available is true when the module is compiled into this binary and not
// written in `without`.
func (a Availability) Available(name string) bool {
	return compiledIn(name) && !slices.Contains(a.without, name)
}

// Without is the names the configuration turned off, sorted.
func (a Availability) Without() []string { return append([]string{}, a.without...) }

// The reasons a module is absent (Absence).
const (
	// AbsentNotCompiled: the binary does not contain a subtractive module.
	AbsentNotCompiled = "not_compiled"
	// AbsentNotCompiledAdd: the binary does not contain an additive module.
	AbsentNotCompiledAdd = "not_compiled_add"
	// AbsentWithout: the configuration turned the module off.
	AbsentWithout = "without"
)

// Absence says why a module is not available: one of the Absent constants,
// and "" when it is available. A name that is not in the table is not
// compiled.
func (a Availability) Absence(name string) string {
	if !compiledIn(name) {
		if info, ok := Lookup(name); ok && info.Additive {
			return AbsentNotCompiledAdd
		}
		return AbsentNotCompiled
	}
	if slices.Contains(a.without, name) {
		return AbsentWithout
	}
	return ""
}

// AbsentArgs are the placeholder values of module_absent and its relatives
// for module m: the reason (which also chooses {why} and {fix}) and the
// module's name, which the sentence renders in its own language.
func AbsentArgs(m, reason string) a2ashape.Args {
	return a2ashape.Args{"m": m, "reason": reason}
}

// Absent is the refusal module_absent (or a variant: "configured",
// "payment") for module m, and nil when the module is available.
func (a Availability) Absent(m, variant string) *a2ashape.Refusal {
	reason := a.Absence(m)
	if reason == "" {
		return nil
	}
	key := "module_absent"
	if variant != "" {
		key += "." + variant
	}
	return a2ashape.Refuse(key, AbsentArgs(m, reason))
}

// compiledIn reports whether name is registered or declared compiled.
func compiledIn(name string) bool { return slices.Contains(Compiled(), name) }

// ruleText is config_invalid's {rule} in this process's language.
func ruleText(zh, en string) string {
	if a2ashape.Lang() == a2ashape.Chinese {
		return zh
	}
	return en
}

// clip shortens a user-written name for a sentence.
func clip(s string) string {
	if r := []rune(s); len(r) > 32 {
		return string(r[:32]) + "…"
	}
	return s
}

// Granted reports whether the name table grants g to the module name; false
// for a name that is not in the table.
func Granted(name string, g Grant) bool {
	i, ok := Lookup(name)
	return ok && i.granted(g)
}

// MayRecord reports whether the name table lets the module name append
// evidence of this type (Info.MayRecord); false for an unknown name.
func MayRecord(name, eventType string) bool {
	i, ok := Lookup(name)
	return ok && i.MayRecord(eventType)
}

// OwnsRoute reports whether the name table gives the module this route.
func OwnsRoute(name, route string) bool {
	i, ok := Lookup(name)
	return ok && slices.Contains(i.Routes, route)
}

// Compiled lists the modules compiled into this binary, sorted: the modules
// the build contains, whether or not the configuration turned them off. It
// is what `anet version`, /status.build.modules and doctor's modules row
// show. It is a method of the value so that nothing outside this package
// reads the registry directly (TestAvailabilityThroughOneFunction).
func (a Availability) Compiled() []string { return Compiled() }

// family is an evidence-type family, "anet.<f>.*": every type under it. It
// is built from parts because the wire registry's scan, which knows complete
// event types only, would take a literal prefix for one it has not registered.
func family(f string) string { return "anet" + "." + f + "." + "*" }

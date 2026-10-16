package autoreply

// autoreply_config.go — the auto_reply configuration: its members, the checks that refuse a block the
// module does not take (LEC §3.2), the merge a POST /autoreply asks for (§2.3.4), and the ConfigSpec
// through which the kernel checks and audits the raw block without parsing it (K-6, MOD-3). Part of the
// execution face: it touches the kernel through package module only.

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// arConfigInvalid is config_invalid for key, with rule in this process's language.
func arConfigInvalid(key, rule string) *a2ashape.Refusal {
	return a2ashape.Refuse("config_invalid", a2ashape.Args{"key": key, "rule": rule})
}

// arRule is config_invalid's {rule} in this process's language.
func arRule(zh, en string) string {
	if a2ashape.Lang() == a2ashape.Chinese {
		return zh
	}
	return en
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
	// Trusted says how the exec backend runs for a peer on the trust list
	// (LEC GT-1): "direct" (the default; as the operator's user) or
	// "sandbox" (in the sandbox, outside the run budget).
	Trusted string `json:"trusted,omitempty"`
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
	// --- tuning (0 ⇒ default) ---
	MaxHistory        int `json:"max_history,omitempty"`         // max conversation turns sent to the backend (default 20)
	APITimeoutSeconds int `json:"api_timeout_seconds,omitempty"` // one backend call (default 180)
	// Sandbox configures the sandbox's network and its run budget (LEC §3.2, ux §2.2); absent, the
	// defaults apply: no network beyond the model route, and the default budget.
	Sandbox *AutoReplySandboxConfig `json:"sandbox,omitempty"`
}

// AutoReplySandboxConfig is auto_reply.sandbox.
type AutoReplySandboxConfig struct {
	// EgressAllow is the CONNECT allowlist (LEC CN-1): the hosts, each by its exact name, that a
	// sandboxed agent may reach on port 443. Empty, the default: none. Each one is a way for the
	// untrusted peer's task to send data out, so adding one is confirmed on a terminal (UX-C12);
	// removing one is not.
	EgressAllow []string `json:"egress_allow,omitempty"`
	// RunsPerDay and RunsPerPeerPerDay are the run budget for untrusted
	// peers per local calendar day (LEC BU-3): 20 and 5 when unset, 0 for
	// no runs (sandbox_budget.go).
	RunsPerDay        *BudgetValue `json:"runs_per_day,omitempty"`
	RunsPerPeerPerDay *BudgetValue `json:"runs_per_peer_per_day,omitempty"`
}

// empty reports a sandbox block that sets nothing.
func (s *AutoReplySandboxConfig) empty() bool {
	return s == nil || (len(s.EgressAllow) == 0 && s.RunsPerDay == nil && s.RunsPerPeerPerDay == nil)
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

// Values of AutoReplyConfig.Trusted.
const (
	TrustedDirect  = "direct"
	TrustedSandbox = "sandbox"
)

// TrustedMode is the effective auto_reply.trusted value: "direct" unless
// the operator set "sandbox".
func (c AutoReplyConfig) TrustedMode() string {
	if c.Trusted == TrustedSandbox {
		return TrustedSandbox
	}
	return TrustedDirect
}

// mergeAutoReply is the configuration a POST /autoreply asks for (LEC §2.3.4): cur with the request's
// members applied — an object member merged member by member into the current object, null resetting a
// member, any other value replacing it — then checked (checkAutoReply, and checkEgressAllow for the
// sandbox's allowlist). A member that is not an auto-reply setting, or has the wrong type, is
// config_invalid.
func mergeAutoReply(cur *AutoReplyConfig, members map[string]json.RawMessage) (*AutoReplyConfig, *a2ashape.Refusal) {
	// A member is named exactly, at every depth: JSON decoding would match "Model" to model, and a merge
	// with both would keep whichever came last.
	settings := make(map[string]json.RawMessage, len(members))
	for k, v := range members {
		if k != "off" {
			settings[k] = v
		}
	}
	if bad := inexactMember(reflect.TypeFor[AutoReplyConfig](), settings); bad != "" {
		return nil, arConfigInvalid("auto_reply."+bad, arRule("不是自动回复的配置项", "it is not an auto-reply setting"))
	}
	if raw, ok := members["api_key"]; ok && string(raw) != "null" {
		var key string
		if json.Unmarshal(raw, &key) != nil {
			return nil, arConfigInvalid("api_key", arRule("必须是字符串", "it must be a string"))
		}
		if strings.TrimSpace(key) == "" {
			return nil, a2ashape.Refuse("api_key_empty", nil)
		}
		members["api_key"], _ = json.Marshal(strings.TrimSpace(key))
	}
	base := map[string]json.RawMessage{}
	if cur != nil {
		raw, _ := json.Marshal(cur)
		_ = json.Unmarshal(raw, &base)
	}
	for k, v := range members {
		if k == "off" {
			continue
		}
		base[k] = mergeJSONMember(base[k], v)
		if base[k] == nil {
			delete(base, k)
		}
	}
	raw, _ := json.Marshal(base)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var next AutoReplyConfig
	if err := dec.Decode(&next); err != nil {
		return nil, autoReplyDecodeRefusal(err)
	}
	if ref := checkAutoReply(next); ref != nil {
		return nil, ref
	}
	// The sandbox block, merged like any other, holds the allowlist: checked against the merged api_base
	// and kept in its normal form (CN-1); a block that sets nothing is dropped.
	if next.Sandbox != nil {
		norm, ref := checkEgressAllow(next.Sandbox.EgressAllow, next.APIBase)
		if ref != nil {
			return nil, ref
		}
		next.Sandbox.EgressAllow = norm
		if next.Sandbox.empty() {
			next.Sandbox = nil
		}
	}
	return &next, nil
}

// inexactMember is the path of the first member of members (in name order) that is not exactly the JSON
// name of a field of the struct t, looking into a member that is an object where its field is a struct
// (a block of settings, merged member by member); "" when every name is exact. A struct that decodes
// itself (BudgetValue) is a value, not a block: any JSON is read into it and its own check says what it
// must be (budget_invalid), so its members are not names to check.
func inexactMember(t reflect.Type, members map[string]json.RawMessage) string {
	fields := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			fields[name] = t.Field(i).Type
		}
	}
	names := make([]string, 0, len(members))
	for k := range members {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		ft, ok := fields[k]
		if !ok {
			return k
		}
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		var sub map[string]json.RawMessage
		if ft.Kind() == reflect.Struct && !reflect.PointerTo(ft).Implements(jsonUnmarshalerType) &&
			json.Unmarshal(members[k], &sub) == nil && sub != nil {
			if bad := inexactMember(ft, sub); bad != "" {
				return k + "." + bad
			}
		}
	}
	return ""
}

var jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// mergeJSONMember is the value a member takes: v merged into cur when both are objects, nil (removed)
// for null, else v.
func mergeJSONMember(cur, v json.RawMessage) json.RawMessage {
	if string(bytes.TrimSpace(v)) == "null" {
		return nil
	}
	var co, vo map[string]json.RawMessage
	if json.Unmarshal(cur, &co) != nil || co == nil || json.Unmarshal(v, &vo) != nil || vo == nil {
		return v
	}
	for k, m := range vo {
		if co[k] = mergeJSONMember(co[k], m); co[k] == nil {
			delete(co, k)
		}
	}
	out, _ := json.Marshal(co)
	return out
}

// autoReplyDecodeRefusal is config_invalid for a merged configuration that does not decode: the member
// that is not a setting, or has the wrong type.
func autoReplyDecodeRefusal(err error) *a2ashape.Refusal {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) && te.Field != "" {
		return arConfigInvalid("auto_reply."+te.Field, arRule("类型不对,应为 "+te.Type.String(), "it must be of type "+te.Type.String()))
	}
	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return arConfigInvalid("auto_reply."+strings.Trim(name, `"`), arRule("不是自动回复的配置项", "it is not an auto-reply setting"))
	}
	return arConfigInvalid("auto_reply", arRule("不是合法的 JSON 对象", "it is not a valid JSON object"))
}

// checkAutoReply checks an auto-reply configuration before it is saved (LEC §2.3.4, §3.2): the backend,
// the agent the exec backend runs, the untrusted mode, what the openai backend needs, and an api_base
// that is https or http on this machine's loopback. Each failure is config_invalid with the key and the
// rule, or api_base_insecure.
func checkAutoReply(c AutoReplyConfig) *a2ashape.Refusal {
	switch c.Backend {
	case "", "openai":
		if strings.TrimSpace(c.APIBase) == "" {
			return arConfigInvalid("auto_reply.api_base", arRule("openai 后端必填", "the openai backend needs it"))
		}
		if strings.TrimSpace(c.Model) == "" {
			return arConfigInvalid("auto_reply.model", arRule("openai 后端必填", "the openai backend needs it"))
		}
	case "exec":
		if _, err := lookupAgent(c.Agent); err != nil {
			agents := strings.Join(SupportedExecAgents(), ", ")
			return arConfigInvalid("auto_reply.agent", arRule("exec 后端必填,只能是 "+agents, "the exec backend needs one of "+agents))
		}
	default:
		return arConfigInvalid("auto_reply.backend", arRule("只能是 openai 或 exec", "it must be openai or exec"))
	}
	if ref := checkAutoReplyStart(c); ref != nil {
		return ref
	}
	return nil
}

// checkAutoReplyStart is the part of checkAutoReply that a daemon also makes before it starts (LEC §3.2,
// CheckConfigForStart): the untrusted mode and the api_base.
func checkAutoReplyStart(c AutoReplyConfig) *a2ashape.Refusal {
	switch c.Untrusted {
	case "", UntrustedOff, UntrustedSandbox:
	default:
		return arConfigInvalid("auto_reply.untrusted", arRule("只能是 off 或 sandbox", "it must be off or sandbox"))
	}
	if base := strings.TrimSpace(c.APIBase); base != "" {
		if _, err := parseEgressAPIBase(base); err != nil {
			return a2ashape.Refuse("api_base_insecure", nil)
		}
	}
	return nil
}

// autoReplySummary is the part of an auto-reply configuration a policy
// change event records: the backend, the agent, how untrusted and trusted
// peers' turns run, the run budgets and whether an API key is set (LEC
// §2.5: never the key). Prompts and paths are left out.
func autoReplySummary(c *AutoReplyConfig) map[string]any {
	if c == nil {
		return nil
	}
	backend := c.Backend
	if backend == "" {
		backend = "openai"
	}
	daily, perPeer := c.runBudgets()
	return map[string]any{"backend": backend, "agent": c.Agent, "untrusted": c.UntrustedMode(),
		"trusted": c.TrustedMode(), "runs_per_day": daily, "runs_per_peer_per_day": perPeer,
		"api_key_set": c.APIKey != ""}
}

// checkTrustedMode is config_invalid for an auto_reply.trusted that is neither direct nor sandbox (LEC
// §3.2).
func checkTrustedMode(c *AutoReplyConfig) *a2ashape.Refusal {
	if c == nil {
		return nil
	}
	switch c.Trusted {
	case "", TrustedDirect, TrustedSandbox:
		return nil
	}
	return a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "auto_reply.trusted",
		"rule": arRule("只能是 direct 或 sandbox", "it must be direct or sandbox")})
}

// checkAutoReplyConfig is the refusal for an auto-reply configuration this area does not take (LEC
// §3.2): auto_reply.trusted neither direct nor sandbox, a run budget out of range, or a work_dir in the
// data dir or the sandbox work root. dataDir empty skips the data dir.
func checkAutoReplyConfig(c *AutoReplyConfig, dataDir string) *a2ashape.Refusal {
	if r := checkTrustedMode(c); r != nil {
		return r
	}
	if r := checkRunBudgets(c); r != nil {
		return r
	}
	return checkWorkDir(c, dataDir)
}

// --- the raw block ---

// parseAutoReply reads an auto_reply block: a member of the wrong type, or text that is not a JSON
// object, is config_invalid. A member that is not a setting is left alone here, as it always was for a
// file read at start (a removed setting is the removed-settings check's refusal, config_key_removed);
// a write refuses it (mergeAutoReply). nil for no block.
func parseAutoReply(raw json.RawMessage) (*AutoReplyConfig, error) {
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		return nil, nil
	}
	var c AutoReplyConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, autoReplyDecodeRefusal(err)
	}
	return &c, nil
}

// lenientAutoReply reads a block the way a reader that must not fail does: what decodes, nil otherwise.
func lenientAutoReply(raw json.RawMessage) *AutoReplyConfig {
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		return nil
	}
	var c AutoReplyConfig
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	return &c
}

// ParseAutoReply is the auto-reply configuration a block holds, nil when there is none or it does not
// decode (doctor reads it; the daemon refuses to start with the second).
func ParseAutoReply(raw json.RawMessage) *AutoReplyConfig { return lenientAutoReply(raw) }

// marshalAutoReply is the block as it is written to config.json; nil for none.
func marshalAutoReply(c *AutoReplyConfig) json.RawMessage {
	if c == nil {
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	return b
}

// refOrNil makes a refusal an error without the typed-nil trap.
func refOrNil(r *a2ashape.Refusal) error {
	if r == nil {
		return nil
	}
	return r
}

// --- the ConfigSpec (K-6) ---

func init() {
	module.RegisterConfigSpec("autoreply", module.ConfigSpec{
		Check:   checkAutoReplyBlock,
		Summary: summaryOfBlock,
		Audit:   auditOfBlock,
		SI5:     si5OfBlock,
	})
}

// checkAutoReplyBlock is the check of a block, at start (dataDir empty or the data directory) and on every
// write: it decodes, then refuses what LEC §3.2 refuses. What a backend needs to run (checkAutoReply: the
// openai backend's address and model, the exec backend's agent) is checked where the block is written
// (mergeAutoReply) and when the module starts (Start).
func checkAutoReplyBlock(raw json.RawMessage, dataDir string) error {
	c, err := parseAutoReply(raw)
	if err != nil || c == nil {
		return err
	}
	if r := checkAutoReplyStart(*c); r != nil {
		return r
	}
	if r := checkAutoReplyConfig(c, dataDir); r != nil {
		return r
	}
	if c.Sandbox != nil {
		if _, r := checkEgressAllow(c.Sandbox.EgressAllow, c.APIBase); r != nil {
			return r
		}
	}
	return nil
}

func summaryOfBlock(raw json.RawMessage) map[string]any {
	c := lenientAutoReply(raw)
	if c == nil {
		return nil
	}
	return autoReplySummary(c)
}

// auditOfBlock gives the value of each audited field (LEC §2.5): the name table's AuditFields.
func auditOfBlock(raw json.RawMessage) map[string]any {
	c := lenientAutoReply(raw)
	if c == nil {
		c = &AutoReplyConfig{} // no block is the defaults: turning auto-reply on or off changes what it changes
	}
	daily, perPeer := c.runBudgets()
	return map[string]any{
		"trusted":                       c.TrustedMode(),
		"sandbox.egress_allow":          egressAllowOf(c),
		"sandbox.runs_per_day":          daily,
		"sandbox.runs_per_peer_per_day": perPeer,
		"api_key_set":                   c.APIKey != "",
	}
}

// si5OfBlock is the SI-5 value the block contributes: the untrusted mode, "off" without a block, an
// invalid value shown as written.
func si5OfBlock(raw json.RawMessage) map[string]any {
	untrusted := UntrustedOff
	if c := lenientAutoReply(raw); c != nil {
		untrusted = c.UntrustedMode()
		if c.Untrusted != "" && c.Untrusted != UntrustedOff && c.Untrusted != UntrustedSandbox {
			untrusted = c.Untrusted
		}
	}
	return map[string]any{"auto_reply.untrusted": untrusted}
}

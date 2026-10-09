package daemon

// egress_upstreams.go is the model service a sandboxed run reaches through its egress endpoint (LEC
// §4.3.4, UP-1 to UP-4).
//
// The agent in the sandbox never holds the operator's key. It is given the endpoint's injection route as
// its model service address and a placeholder key; the endpoint (egress.go) drops whatever credentials
// the agent sends, applies the request body policy (egress_policy.go) and forwards the request to the one
// upstream this run uses, adding the operator's key.
//
// A run has exactly one upstream, chosen from the agent and auto_reply.api_base (UP-2):
//
//	claude                        anthropic
//	codex                         custom when api_base is set, else openai
//	opencode, hermes, openclaw    custom (api_base required: no_api_base)
//	cursor                        none: it cannot send its model requests through this node
//
// An api_base whose host is api.anthropic.com or api.openai.com is that built-in upstream, not custom.
// The model is pinned (UP-3): every request is rewritten to it, and the agent is started with it.

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// The upstream names, the second segment of the injection route.
const (
	egressUpstreamAnthropic = "anthropic"
	egressUpstreamOpenAI    = "openai"
	egressUpstreamCustom    = "custom"
)

// egressRoutePrefix starts every injection route: /anet-upstream/<name>/<upstream path>.
const egressRoutePrefix = "/anet-upstream/"

// egressPlaceholderKey is the only key the agent in the sandbox holds (UP-4). It is a local value that
// never leaves this machine: the endpoint drops it with every other credential the agent sends.
const egressPlaceholderKey = "anet-" + "sandbox"

// The built-in upstreams (UP-1). The base addresses are variables only so that tests can point them at
// a fake service; nothing else assigns them.
var egressBuiltinBase = map[string]string{
	egressUpstreamAnthropic: "https://api.anthropic.com",
	egressUpstreamOpenAI:    "https://api.openai.com",
}

// egressBuiltinHosts are the hosts of the built-in upstreams: an api_base on one of them is that upstream.
var egressBuiltinHosts = map[string]string{
	"api.anthropic.com": egressUpstreamAnthropic,
	"api.openai.com":    egressUpstreamOpenAI,
}

// egressCheapestModel is the model of each built-in upstream when auto_reply.model is not set (UP-3,
// item 3): the lowest-priced tier, which an unattended run for an untrusted peer should use. Updated with
// releases; as of 2026-10 (EXE-6): for Anthropic, Haiku 5.5, the lowest price of the current models in
// the claude CLI's catalog (2.1.295: 0.10/0.50 per million input/output tokens, against 1/5 for Haiku
// 4.5); for OpenAI, the model the codex CLI's catalog (0.162) offers as its fast and affordable one —
// codex is the only agent that uses the openai upstream without an api_base.
var egressCheapestModel = map[string]string{
	egressUpstreamAnthropic: "claude-haiku-5-5",
	egressUpstreamOpenAI:    "gpt-6-luna",
}

// egressClaudeAliases are the claude CLI's model aliases and the API model each stands for (UP-3, item 2):
// the first-party defaults of the CLI's catalog (claude 2.1.295), so that a model set the way the CLI
// takes it is the model the CLI would have used. fable and best are aliases the CLI added after haiku,
// sonnet and opus; left untranslated they would reach the API as unknown models.
var egressClaudeAliases = map[string]string{
	"haiku":  "claude-haiku-5-5",
	"sonnet": "claude-sonnet-5-5",
	"opus":   "claude-opus-5-5",
	"fable":  "claude-fable-5-1",
	"best":   "claude-fable-5-1",
}

// egressRouteKind is what one allowed upstream path is, which decides the body policy it gets.
type egressRouteKind uint8

const (
	routeAnthropicMessages egressRouteKind = iota + 1
	routeAnthropicCountTokens
	routeResponses // OpenAI Responses API
	routeChat      // chat completions (openai and custom)
	routeModels    // GET the model list
	routeModel     // GET one model
)

// egressRoute is one method and path an upstream allows. A Path ending in "/*" matches one more path
// segment, a model id.
type egressRoute struct {
	Method string
	Path   string
	Kind   egressRouteKind
}

// The allowed methods and paths of each upstream (UP-1). Custom paths are relative to api_base.
var (
	egressAnthropicRoutes = []egressRoute{
		{"POST", "/v1/messages", routeAnthropicMessages},
		{"POST", "/v1/messages/count_tokens", routeAnthropicCountTokens},
		{"GET", "/v1/models", routeModels},
		{"GET", "/v1/models/*", routeModel},
	}
	egressOpenAIRoutes = []egressRoute{
		{"POST", "/v1/responses", routeResponses},
		{"POST", "/v1/chat/completions", routeChat},
		{"GET", "/v1/models", routeModels},
		{"GET", "/v1/models/*", routeModel},
	}
	egressCustomRoutes = []egressRoute{
		{"POST", "/chat/completions", routeChat},
		{"GET", "/models", routeModels},
	}
)

// egressModelIDRe is the model id segment of GET /v1/models/{id}.
var egressModelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)

// egressUpstream is the upstream of one sandboxed run.
type egressUpstream struct {
	// Name is anthropic, openai or custom: the injection route's second segment.
	Name string
	// Base is the upstream's address: scheme, host and, for custom, the path the routes are relative to
	// (without a trailing slash).
	Base *url.URL
	// Key is the operator's key, added to every forwarded request; "" only for a custom service that
	// needs none.
	Key string
	// Model is the pinned model P every request is rewritten to.
	Model string
	// Routes are the allowed methods and paths.
	Routes []egressRoute
	// OpenRouter: the custom host is openrouter.ai, whose plugins and model fallback list are refused.
	OpenRouter bool
	// LoopbackOnly: the custom service is plain http, allowed only on this host's loopback; the endpoint
	// dials nothing else for it.
	LoopbackOnly bool
	// ConnectAllow are the hosts the run's CONNECTs may reach (auto_reply.sandbox.egress_allow, CN-2):
	// none by default. The model service is reached by the injection route alone, never by CONNECT.
	ConnectAllow map[string]bool
}

// resolveEgressUpstream chooses the upstream, key and pinned model of a sandboxed run from the auto-reply
// configuration (UP-1 to UP-3). An error is a *sandboxProbeError with the local reason (UX §1.4 H):
// agent_unsupported, no_api_base, no_api_key, no_model, or api_base_insecure for an address the
// configuration check should already have refused.
func resolveEgressUpstream(cfg AutoReplyConfig) (*egressUpstream, error) {
	apiBase := strings.TrimSpace(cfg.APIBase)
	name := ""
	switch cfg.Agent {
	case agentClaude:
		name = egressUpstreamAnthropic
	case agentCodex:
		name = egressUpstreamOpenAI
		if apiBase != "" {
			name = egressUpstreamCustom
		}
	case agentOpenCode, agentHermes, agentOpenClaw:
		if apiBase == "" {
			return nil, &sandboxProbeError{Code: "no_api_base", Detail: cfg.Agent + " needs auto_reply.api_base in the sandbox"}
		}
		name = egressUpstreamCustom
	default:
		return nil, &sandboxProbeError{Code: "agent_unsupported", Detail: cfg.Agent + " has no model service address to point at this node"}
	}
	up := &egressUpstream{Name: name, Key: strings.TrimSpace(cfg.APIKey)}
	if name == egressUpstreamCustom {
		base, err := parseEgressAPIBase(apiBase)
		if err != nil {
			return nil, err
		}
		// A fully qualified name (trailing dot) is the same host: it must not turn a built-in service
		// into custom, or let openrouter.ai's plugins and model list through.
		host := strings.TrimSuffix(strings.ToLower(base.Hostname()), ".")
		if builtin, ok := egressBuiltinHosts[host]; ok {
			up.Name = builtin
		} else {
			up.Base = base
			up.Routes = egressCustomRoutes
			up.OpenRouter = host == "openrouter.ai"
			up.LoopbackOnly = base.Scheme == "http"
		}
	}
	switch up.Name {
	case egressUpstreamAnthropic, egressUpstreamOpenAI:
		base, err := url.Parse(egressBuiltinBase[up.Name])
		if err != nil {
			return nil, &sandboxProbeError{Code: "no_api_base", Detail: err.Error()}
		}
		up.Base = base
		up.Routes = egressAnthropicRoutes
		if up.Name == egressUpstreamOpenAI {
			up.Routes = egressOpenAIRoutes
		}
		if up.Key == "" {
			return nil, &sandboxProbeError{Code: "no_api_key", Detail: "the sandbox needs auto_reply.api_key for " + up.Name}
		}
	}
	model := strings.TrimSpace(cfg.Model)
	switch {
	case model == "" && up.Name == egressUpstreamCustom:
		return nil, &sandboxProbeError{Code: "no_model", Detail: "a custom model service needs auto_reply.model in the sandbox"}
	case model == "":
		model = egressCheapestModel[up.Name]
	case up.Name == egressUpstreamAnthropic && egressClaudeAliases[model] != "":
		model = egressClaudeAliases[model]
	}
	up.Model = model
	up.ConnectAllow = egressAllowSet(cfg)
	return up, nil
}

// parseEgressAPIBase checks a custom api_base (LEC §3.2): https, or http on a loopback literal or
// localhost; no user, query or fragment. It returns the address without a trailing slash.
func parseEgressAPIBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	insecure := func(why string) error {
		return &sandboxProbeError{Code: "api_base_insecure", Detail: "auto_reply.api_base " + why}
	}
	if err != nil || u.Host == "" || u.Opaque != "" {
		return nil, insecure("is not an http(s) address")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return nil, insecure("carries a user, query or fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopbackHost(u.Hostname()) {
			return nil, insecure("uses http on a host other than this machine's loopback")
		}
	default:
		return nil, insecure("is not an http(s) address")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

// loopbackHost reports a loopback IP literal (127.0.0.0/8, ::1) or localhost.
func loopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// match finds the route of a method and an upstream path (the part of the injection route after
// /anet-upstream/<name>).
func (up *egressUpstream) match(method, p string) (egressRouteKind, bool) {
	for _, r := range up.Routes {
		if r.Method != method {
			continue
		}
		if prefix, ok := strings.CutSuffix(r.Path, "/*"); ok {
			if id, ok := strings.CutPrefix(p, prefix+"/"); ok && egressModelIDRe.MatchString(id) {
				return r.Kind, true
			}
			continue
		}
		if p == r.Path {
			return r.Kind, true
		}
	}
	return 0, false
}

// pathsText lists the allowed calls for policy_path's {paths}: method and full local path.
func (up *egressUpstream) pathsText() string {
	parts := make([]string, 0, len(up.Routes))
	for _, r := range up.Routes {
		p := strings.Replace(r.Path, "/*", "/<id>", 1)
		parts = append(parts, r.Method+" "+egressRoutePrefix+up.Name+p)
	}
	return strings.Join(parts, ", ")
}

// target is the upstream URL of an allowed upstream path, with the request's query.
func (up *egressUpstream) target(p, rawQuery string) *url.URL {
	u := *up.Base
	u.Path = strings.TrimRight(u.Path, "/") + p
	u.RawPath = ""
	u.RawQuery = rawQuery
	u.Fragment = ""
	return &u
}

// egressRouteBase is the address the agent in the sandbox uses for its model service: the injection
// route of this run's upstream on the sandbox's own 127.0.0.1:3128 (LEC §2.6.2).
func egressRouteBase(up *egressUpstream) string {
	return "http://" + sandboxBridgeListen + egressRoutePrefix + up.Name
}

// egressProxyVars are the proxy variables pointed at the endpoint in the sandbox (LEC §2.6.3), and
// egressNoProxyVars the exemption for the endpoint itself.
var (
	egressProxyVars   = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"}
	egressNoProxyVars = []string{"NO_PROXY", "no_proxy"}
)

// egressSecretVars are the variables an agent CLI reads its model credentials or service address from.
// The sandbox's environment never carries the daemon's values of them (UP-4): sandboxEgressEnv sets the
// ones the run's upstream needs to the route and the placeholder.
var egressSecretVars = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_API_BASE", "CODEX_API_KEY",
	"CURSOR_API_KEY", "ANET_EXEC_API_KEY",
}

// sandboxEgressEnv is what the agent in the sandbox is told about its egress (LEC §2.6.3, AV-4, UP-4):
// every proxy variable names the endpoint, and the model service address of the run's upstream is the
// injection route, with the placeholder key. Nil without an upstream.
func sandboxEgressEnv(up *egressUpstream) []string {
	if up == nil {
		return nil
	}
	proxy := "http://" + sandboxBridgeListen
	var env []string
	for _, k := range egressProxyVars {
		env = append(env, k+"="+proxy)
	}
	for _, k := range egressNoProxyVars {
		env = append(env, k+"=127.0.0.1,localhost")
	}
	base := egressRouteBase(up)
	switch up.Name {
	case egressUpstreamAnthropic:
		env = append(env, "ANTHROPIC_BASE_URL="+base, "ANTHROPIC_API_KEY="+egressPlaceholderKey,
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1")
	case egressUpstreamOpenAI:
		env = append(env, "OPENAI_BASE_URL="+base+"/v1", "OPENAI_API_KEY="+egressPlaceholderKey,
			"CODEX_API_KEY="+egressPlaceholderKey)
	default:
		env = append(env, "OPENAI_BASE_URL="+base, "OPENAI_API_KEY="+egressPlaceholderKey,
			"CODEX_API_KEY="+egressPlaceholderKey)
	}
	return env
}

// sandboxEnvOverlay is env without any proxy, credential or model service variable the daemon passed
// on, followed by extra. It is applied to every sandboxed agent's environment: the operator's key stays
// with the endpoint, and the daemon's own proxy settings (which may carry a password) stay out.
func sandboxEnvOverlay(env, extra []string) []string {
	drop := map[string]bool{}
	for _, set := range [][]string{egressProxyVars, egressNoProxyVars, egressSecretVars} {
		for _, k := range set {
			drop[k] = true
		}
	}
	out := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// egressArg is a value from the sandbox's request ({host}, {field}, {type}) as a refusal sentence shows
// it: control and format characters removed, at most 64 characters (ux §1.4 I).
func egressArg(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0xfffd ||
			(r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0xfeff {
			return -1
		}
		return r
	}, s)
	if rs := []rune(s); len(rs) > 64 {
		s = string(rs[:64])
	}
	return s
}

// egressSentence is the refusal sentence of code in this process's language (core G13).
func egressSentence(lang a2ashape.Language, code string, args a2ashape.Args) string {
	return a2ashape.Sentence(code, lang, args)
}

func (up *egressUpstream) String() string {
	if up == nil {
		return "<none>"
	}
	return fmt.Sprintf("%s %s model %s", up.Name, up.Base, up.Model)
}

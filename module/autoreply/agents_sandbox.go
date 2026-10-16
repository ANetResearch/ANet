package autoreply

// agents_sandbox.go is how each agent runs in the sandbox (LEC AV-4): the model services it can use there
// and how it is pointed at the run's injection route.
//
// The table below is what running each agent's own CLI in the sandbox, against the injection route,
// established (EXE-6, 2026-10-09; claude 2.1.295, codex 0.162.0, opencode 1.18.35, hermes-agent 0.19.0,
// openclaw 2026.9.9). An agent reaches its model service in one of three ways:
//
//	claude     the Anthropic Messages API, through ANTHROPIC_BASE_URL (sandboxEgressEnv)
//	codex      the OpenAI Responses API, through a provider in $HOME/.codex/config.toml; codex no longer
//	           speaks chat completions (wire_api "chat" was removed), which is all the custom route
//	           carries, so codex runs in the sandbox only against api.openai.com
//	opencode,  OpenAI chat completions, through a provider in their own configuration file under $HOME;
//	hermes,    a custom service (auto_reply.api_base) or api.openai.com
//	openclaw
//
// cursor has no model service address of its own to point anywhere, and is not run in the sandbox.
//
// An api_base that names a model service whose route does not carry the agent's API is refused when the
// configuration is resolved (agent_unsupported.combo, LEC UP-2), the same on Linux and macOS: claude
// with a service other than api.anthropic.com (a key meant for a third-party service must not be sent
// to Anthropic), codex with one other than api.openai.com, and the chat agents with api.anthropic.com.
//
// Every file written here is the agent's ordinary configuration in the run's home directory, which is
// empty and private to the run: it names the injection route and the placeholder key, never the
// operator's key, and it is gone when the run ends. On macOS the route the files name carries the run's
// key, as the agent's environment does (EG-3): codex, opencode, hermes and openclaw read their model
// service's address only from these files, and the endpoint there takes nothing without the key. The
// files are 0600 in the run's home, readable only by the user who can read the agent's environment
// anyway, and the key is void once the run's endpoint closes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// agentSandbox is one agent's configuration in the sandbox.
type agentSandbox struct {
	// upstream is the model service the agent uses when auto_reply.api_base is not set; "" when the agent
	// needs one (no_api_base, LEC UP-2).
	upstream string
	// speaks are the upstreams whose injection route carries the API the agent uses (UP-1). An api_base
	// that makes the run's upstream another one fails closed (agent_unsupported.combo), before anything
	// runs.
	speaks []string
	// combo is that refusal's {why} (one of the agentCombo constants): what the agent can use instead.
	combo string
	// api names that API, for the refusal's local detail.
	api string
	// prepare points the agent at the run's upstream: it writes the agent's configuration under the run's
	// home directory (hostHome on this host, which the agent sees as home) and returns what the agent's
	// environment needs besides the egress variables. route is the run's injection route as the agent
	// reaches it (egressRouteBaseAt: on Linux the sandbox's own 127.0.0.1:3128, on macOS the keyed
	// endpoint on the host's loopback); a configuration file names it, never another address. Nil:
	// nothing.
	prepare func(up *egressUpstream, hostHome, home, route string) ([]string, error)
}

// The {why} of agent_unsupported.combo (ux §1.4 H, LEC UP-2): why an agent the sandbox runs cannot use
// the model service auto_reply.api_base names. They are local values of the operator's sentence.
const (
	// agentComboAnthropicOnly: claude speaks only the Anthropic Messages API, which only the anthropic
	// upstream carries; it would otherwise send a third-party service's key to api.anthropic.com.
	agentComboAnthropicOnly = "anthropic_only"
	// agentComboOpenAIOnly: codex speaks only the Responses API, which no custom route carries.
	agentComboOpenAIOnly = "openai_only"
	// agentComboNoChat: api.anthropic.com does not offer the chat completions API the chat agents speak.
	agentComboNoChat = "no_chat"
)

// homeFor is the plan's AgentHome for this agent and the run's upstream: nil when the agent needs no
// configuration of its own.
func (sb *agentSandbox) homeFor(up *egressUpstream) func(hostHome, home, route string) ([]string, error) {
	if sb == nil || sb.prepare == nil {
		return nil
	}
	return func(hostHome, home, route string) ([]string, error) { return sb.prepare(up, hostHome, home, route) }
}

// sandboxProviderID names the model provider in the configuration files the sandbox writes for an agent
// (codex, opencode, openclaw): it is the run's injection route. It is a local name, in no message.
const sandboxProviderID = "anet"

// sandboxProviderModel is the pinned model as an agent that writes "provider/model" names it.
func sandboxProviderModel(model string) string { return sandboxProviderID + "/" + model }

// chatAPIBase is the address an OpenAI-compatible client is given for the run's upstream on the injection
// route: the route of a custom service is relative to api_base, which already holds its version path
// (/v1 or other); the route of api.openai.com takes the /v1 an SDK's base address carries.
func chatAPIBase(up *egressUpstream, route string) string {
	if up.Name == egressUpstreamOpenAI {
		return route + "/v1"
	}
	return route
}

var (
	claudeSandbox = &agentSandbox{
		upstream: egressUpstreamAnthropic,
		speaks:   []string{egressUpstreamAnthropic}, combo: agentComboAnthropicOnly, api: "the Anthropic Messages API",
		// IS_SANDBOX tells claude it runs confined, which it requires before it lets a root user's agent
		// use its tools without asking (a daemon run as root).
		prepare: func(*egressUpstream, string, string, string) ([]string, error) { return []string{"IS_SANDBOX=1"}, nil },
	}
	codexSandbox = &agentSandbox{
		upstream: egressUpstreamOpenAI,
		speaks:   []string{egressUpstreamOpenAI}, combo: agentComboOpenAIOnly, api: "the OpenAI Responses API",
		prepare: prepareCodex,
	}
	openCodeSandbox = &agentSandbox{
		speaks: []string{egressUpstreamCustom, egressUpstreamOpenAI}, combo: agentComboNoChat, api: "the OpenAI chat completions API",
		prepare: prepareOpenCode,
	}
	hermesSandbox = &agentSandbox{
		speaks: []string{egressUpstreamCustom, egressUpstreamOpenAI}, combo: agentComboNoChat, api: "the OpenAI chat completions API",
		prepare: prepareHermes,
	}
	openClawSandbox = &agentSandbox{
		speaks: []string{egressUpstreamCustom, egressUpstreamOpenAI}, combo: agentComboNoChat, api: "the OpenAI chat completions API",
		prepare: prepareOpenClaw,
	}
)

// writeHomeFile writes data to rel under the run's home directory (0600; the directories it makes 0700).
// The home directory is fresh and private to the run, so nothing is there to follow; O_EXCL still makes
// sure the file is the one this call created.
func writeHomeFile(hostHome, rel string, data []byte) error {
	if !filepath.IsAbs(hostHome) || filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return fmt.Errorf("agent configuration %q under %q", rel, hostHome)
	}
	p := filepath.Join(hostHome, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

// prepareCodex writes $HOME/.codex/config.toml: the pinned model on a provider that is the run's
// injection route, over HTTP (codex prefers a WebSocket to api.openai.com for its newest models, which the
// route does not carry), with codex's web search off — it is a tool the model service runs, which the
// route refuses (BP-4), and a request that lists it would be refused whole.
func prepareCodex(up *egressUpstream, hostHome, _, route string) ([]string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "model = %s\n", tomlString(up.Model))
	fmt.Fprintf(&b, "model_provider = %s\n", tomlString(sandboxProviderID))
	b.WriteString("web_search = \"disabled\"\ncheck_for_update_on_startup = false\n\n")
	fmt.Fprintf(&b, "[model_providers.%s]\n", sandboxProviderID)
	fmt.Fprintf(&b, "name = %s\n", tomlString(sandboxProviderID))
	fmt.Fprintf(&b, "base_url = %s\n", tomlString(chatAPIBase(up, route)))
	b.WriteString("wire_api = \"responses\"\nenv_key = \"OPENAI_API_KEY\"\nsupports_websockets = false\n")
	return nil, writeHomeFile(hostHome, filepath.Join(".codex", "config.toml"), []byte(b.String()))
}

// openCodeFlags turn off what opencode would fetch from the network at start-up (its model catalog,
// plugins, language servers, updates) and the project configuration a peer's earlier turn could have left
// in the work dir; the sandbox would refuse the fetches anyway, each costing a refused CONNECT.
var openCodeFlags = []string{
	"OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_MODELS_FETCH=1", "OPENCODE_DISABLE_LSP_DOWNLOAD=1",
	"OPENCODE_DISABLE_SHARE=1", "OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_DISABLE_CLAUDE_CODE=1", "OPENCODE_PURE=1",
}

// prepareOpenCode writes $HOME/.config/opencode/opencode.json: an OpenAI-compatible provider that is the
// run's injection route, holding the pinned model, and the permission to edit and run commands (the sandbox
// is the boundary; opencode run cannot ask).
func prepareOpenCode(up *egressUpstream, hostHome, _, route string) ([]string, error) {
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{sandboxProviderID: map[string]any{
			"npm":     "@ai-sdk/openai-compatible",
			"name":    sandboxProviderID,
			"options": map[string]any{"baseURL": chatAPIBase(up, route), "apiKey": egressPlaceholderKey},
			"models":  map[string]any{up.Model: map[string]any{"name": up.Model}},
		}},
		"model":      sandboxProviderModel(up.Model),
		"autoupdate": false,
		"share":      "disabled",
		"permission": map[string]any{"edit": "allow", "bash": "allow", "webfetch": "allow"},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return openCodeFlags, writeHomeFile(hostHome, filepath.Join(".config", "opencode", "opencode.json"), b)
}

// prepareHermes writes $HOME/.hermes/config.yaml: the custom provider at the run's injection route, with
// the pinned model. The values are JSON strings, which YAML reads as the same strings.
func prepareHermes(up *egressUpstream, hostHome, _, route string) ([]string, error) {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	y := "model:\n" +
		"  provider: custom\n" +
		"  base_url: " + q(chatAPIBase(up, route)) + "\n" +
		"  default: " + q(up.Model) + "\n" +
		"  api_key: " + q(egressPlaceholderKey) + "\n"
	return nil, writeHomeFile(hostHome, filepath.Join(".hermes", "config.yaml"), []byte(y))
}

// prepareOpenClaw writes $HOME/.openclaw/openclaw.json: an OpenAI-completions provider that is the run's
// injection route, holding the pinned model, as the default agent model.
func prepareOpenClaw(up *egressUpstream, hostHome, _, route string) ([]string, error) {
	cfg := map[string]any{
		"models": map[string]any{"mode": "merge", "providers": map[string]any{sandboxProviderID: map[string]any{
			"baseUrl": chatAPIBase(up, route),
			"apiKey":  egressPlaceholderKey,
			"api":     "openai-completions",
			"models":  []any{map[string]any{"id": up.Model, "name": up.Model}},
		}}},
		"agents": map[string]any{"defaults": map[string]any{"model": map[string]any{"primary": sandboxProviderModel(up.Model)}}},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return nil, writeHomeFile(hostHome, filepath.Join(".openclaw", "openclaw.json"), b)
}

// tomlString renders s as a TOML basic string, with only the escapes TOML 1.0 defines (internal/agentwire
// has the same for the configuration `anet agents wire` edits).
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\u00`)
			b.WriteByte("0123456789ABCDEF"[r>>4])
			b.WriteByte("0123456789ABCDEF"[r&0xF])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// hermesOneShot is hermes's one-shot option with its message attached ("--oneshot=" + message): hermes
// parses its arguments with Python's argparse, which takes everything after the "=" as the value, while
// a separate value starting with "-" would be read as an option (the operator's system_prompt may start
// with one). hermes's own launcher recognises the same spelling.
const hermesOneShot = "--oneshot="

package daemon

import (
	"errors"
	"strings"
	"testing"
)

// UP-2, UP-1, AV-4: the upstream follows from the agent and api_base; an api_base on a built-in host is
// that built-in; the upstream's route must carry the API the agent speaks (codex: Responses, only to
// api.openai.com; opencode, hermes, openclaw: chat completions, not to api.anthropic.com); a run that
// cannot be served fails closed with the local reason of UX §1.4 H.
func TestEgressUpstreamSelection(t *testing.T) {
	const key = testOperatorKey
	for _, tc := range []struct {
		cfg      AutoReplyConfig
		name     string // upstream, or "" when refused
		base     string
		code     string
		loopback bool
		openr    bool
	}{
		{AutoReplyConfig{Agent: agentClaude, APIKey: key}, "anthropic", "https://api.anthropic.com", "", false, false},
		// claude speaks only the Anthropic API: another api_base is refused (UP-2), never ignored.
		{AutoReplyConfig{Agent: agentClaude, APIKey: key, APIBase: "http://127.0.0.1:11434/v1"}, "", "", "agent_unsupported", false, false},
		{AutoReplyConfig{Agent: agentClaude, APIKey: key, APIBase: "https://api.anthropic.com/v1"}, "anthropic", "https://api.anthropic.com", "", false, false},
		{AutoReplyConfig{Agent: agentClaude}, "", "", "no_api_key", false, false},
		{AutoReplyConfig{Agent: agentClaude, APIKey: "  "}, "", "", "no_api_key", false, false},
		{AutoReplyConfig{Agent: agentCodex, APIKey: key}, "openai", "https://api.openai.com", "", false, false},
		{AutoReplyConfig{Agent: agentCodex}, "", "", "no_api_key", false, false},
		{AutoReplyConfig{Agent: agentCodex, APIKey: key, APIBase: "https://API.OpenAI.com/v1"}, "openai", "https://api.openai.com", "", false, false},
		// codex 0.162 no longer speaks chat completions, the one API the custom route carries.
		{AutoReplyConfig{Agent: agentCodex, APIBase: "http://127.0.0.1:11434/v1/", Model: "qwen3"}, "", "", "agent_unsupported", false, false},
		{AutoReplyConfig{Agent: agentCodex, APIBase: "http://127.0.0.1:11434/v1"}, "", "", "agent_unsupported", false, false},
		{AutoReplyConfig{Agent: agentCodex, APIBase: "https://openrouter.ai/api/v1", Model: "x/y", APIKey: key}, "", "", "agent_unsupported", false, false},
		{AutoReplyConfig{Agent: agentOpenCode, APIBase: "http://127.0.0.1:11434/v1", Model: "qwen3"}, "custom", "http://127.0.0.1:11434/v1", "", true, false},
		{AutoReplyConfig{Agent: agentHermes, APIKey: key, APIBase: "https://api.openai.com/v1", Model: "gpt-6-luna"}, "openai", "https://api.openai.com", "", false, false},
		{AutoReplyConfig{Agent: agentOpenClaw}, "", "", "no_api_base", false, false},
		{AutoReplyConfig{Agent: agentOpenCode, APIKey: key}, "", "", "no_api_base", false, false},
		{AutoReplyConfig{Agent: agentOpenCode, APIKey: key, APIBase: "https://openrouter.ai/api/v1", Model: "x/y"}, "custom", "https://openrouter.ai/api/v1", "", false, true},
		{AutoReplyConfig{Agent: agentOpenCode, APIKey: key, APIBase: "https://OpenRouter.ai.:443/api/v1", Model: "x/y"}, "custom", "https://OpenRouter.ai.:443/api/v1", "", false, true},
		{AutoReplyConfig{Agent: agentCodex, APIKey: key, APIBase: "https://api.openai.com./v1"}, "openai", "https://api.openai.com", "", false, false},
		// The chat agents cannot use the Anthropic route, which carries only the Messages API.
		{AutoReplyConfig{Agent: agentOpenCode, APIKey: key, APIBase: "https://api.anthropic.com"}, "", "", "agent_unsupported", false, false},
		{AutoReplyConfig{Agent: agentHermes, APIBase: "https://api.anthropic.com"}, "", "", "agent_unsupported", false, false},
		{AutoReplyConfig{Agent: agentHermes, APIBase: "http://198.51.100.5:8000/v1", Model: "m"}, "", "", "api_base_insecure", false, false},
		{AutoReplyConfig{Agent: agentHermes, APIBase: "http://models.example/v1", Model: "m"}, "", "", "api_base_insecure", false, false},
		{AutoReplyConfig{Agent: agentOpenClaw, APIBase: "http://localhost:8000/v1", Model: "m"}, "custom", "http://localhost:8000/v1", "", true, false},
		{AutoReplyConfig{Agent: agentOpenClaw, APIBase: "http://[::1]:8000", Model: "m"}, "custom", "http://[::1]:8000", "", true, false},
		{AutoReplyConfig{Agent: agentOpenClaw, APIBase: "https://u:p@models.example/v1", Model: "m"}, "", "", "api_base_insecure", false, false},
		{AutoReplyConfig{Agent: agentOpenClaw, APIBase: "https://models.example/v1?x=1", Model: "m"}, "", "", "api_base_insecure", false, false},
		{AutoReplyConfig{Agent: agentOpenClaw, APIBase: "ftp://models.example", Model: "m"}, "", "", "api_base_insecure", false, false},
		{AutoReplyConfig{Agent: agentOpenClaw, APIBase: "models.example", Model: "m"}, "", "", "api_base_insecure", false, false},
		{AutoReplyConfig{Agent: agentCursor, APIKey: key}, "", "", "agent_unsupported", false, false},
		{AutoReplyConfig{Agent: "nobody", APIKey: key}, "", "", "agent_unsupported", false, false},
	} {
		up, err := resolveEgressUpstream(tc.cfg)
		if tc.code != "" {
			var pe *sandboxProbeError
			if !errors.As(err, &pe) || pe.Code != tc.code || !errors.Is(err, errSandboxUnavailable) {
				t.Errorf("%+v: want %s, got %v", tc.cfg, tc.code, err)
			}
			if err != nil && strings.Contains(err.Error(), key) {
				t.Errorf("the refusal names the key: %v", err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%+v: %v", tc.cfg, err)
			continue
		}
		if up.Name != tc.name || up.Base.String() != tc.base || up.LoopbackOnly != tc.loopback || up.OpenRouter != tc.openr {
			t.Errorf("%+v: got %s base %s loopback %v openrouter %v", tc.cfg, up.Name, up.Base, up.LoopbackOnly, up.OpenRouter)
		}
		if up.Key != strings.TrimSpace(tc.cfg.APIKey) {
			t.Errorf("%+v: key not carried", tc.cfg)
		}
		if strings.Contains(up.String(), key) {
			t.Errorf("the upstream's description names the key: %s", up)
		}
	}
}

// UP-3: the pinned model is auto_reply.model as an API id, the claude alias's API id, or the upstream's
// cheapest model; a custom service without a model is refused (above). The tables are the constants of
// 2026-10 (EXE-6): the claude CLI's aliases and its lowest-priced current model, and the codex CLI's
// affordable model.
func TestEgressPinnedModel(t *testing.T) {
	for _, tc := range []struct {
		cfg  AutoReplyConfig
		want string
	}{
		{AutoReplyConfig{Agent: agentClaude, APIKey: "k"}, "claude-haiku-5-5"},
		{AutoReplyConfig{Agent: agentClaude, APIKey: "k", Model: "haiku"}, "claude-haiku-5-5"},
		{AutoReplyConfig{Agent: agentClaude, APIKey: "k", Model: "sonnet"}, "claude-sonnet-5-5"},
		{AutoReplyConfig{Agent: agentClaude, APIKey: "k", Model: "opus"}, "claude-opus-5-5"},
		{AutoReplyConfig{Agent: agentClaude, APIKey: "k", Model: "fable"}, "claude-fable-5-1"},
		{AutoReplyConfig{Agent: agentClaude, APIKey: "k", Model: "best"}, "claude-fable-5-1"},
		{AutoReplyConfig{Agent: agentClaude, APIKey: "k", Model: " claude-opus-4-8 "}, "claude-opus-4-8"},
		{AutoReplyConfig{Agent: agentClaude, APIKey: "k", Model: "Haiku"}, "Haiku"}, // aliases are the CLI's, lower case
		{AutoReplyConfig{Agent: agentCodex, APIKey: "k"}, "gpt-6-luna"},
		{AutoReplyConfig{Agent: agentCodex, APIKey: "k", Model: "haiku"}, "haiku"},
		{AutoReplyConfig{Agent: agentOpenCode, APIBase: "http://127.0.0.1:1/v1", Model: "llama3"}, "llama3"},
		{AutoReplyConfig{Agent: agentHermes, APIKey: "k", APIBase: "https://api.openai.com/v1"}, "gpt-6-luna"},
		{AutoReplyConfig{Agent: agentOpenClaw, APIKey: "k", APIBase: "https://api.openai.com/v1", Model: "sonnet"}, "sonnet"},
	} {
		up, err := resolveEgressUpstream(tc.cfg)
		if err != nil || up.Model != tc.want {
			t.Errorf("%+v: model %v, want %s (%v)", tc.cfg, up, tc.want, err)
		}
	}
}

// UP-4, LEC §2.6.3: the agent in the sandbox is told the injection route and the placeholder key, and
// every proxy variable names the endpoint; the daemon's own keys, service addresses and proxy settings
// (here with a password) never reach the agent's environment.
func TestSandboxEgressEnv(t *testing.T) {
	daemonEnv := []string{"PATH=/usr/bin", "LANG=C.UTF-8", "ANTHROPIC_API_KEY=" + testOperatorKey, "ANTHROPIC_AUTH_TOKEN=tok",
		"OPENAI_API_KEY=" + testOperatorKey, "CODEX_API_KEY=" + testOperatorKey, "ANET_EXEC_API_KEY=" + testOperatorKey,
		"OPENAI_BASE_URL=https://proxy.example/v1", "HTTPS_PROXY=http://user:secretpw@proxy.example:3128",
		"https_proxy=http://user:secretpw@proxy.example:3128", "ALL_PROXY=socks5://proxy.example", "NO_PROXY=*", "HOME=/home/u"}
	for _, tc := range []struct {
		up   *egressUpstream
		want []string
	}{
		{&egressUpstream{Name: egressUpstreamAnthropic}, []string{"ANTHROPIC_BASE_URL=http://127.0.0.1:3128/anet-upstream/anthropic",
			"ANTHROPIC_API_KEY=anet-sandbox", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1"}},
		{&egressUpstream{Name: egressUpstreamOpenAI}, []string{"OPENAI_BASE_URL=http://127.0.0.1:3128/anet-upstream/openai/v1",
			"OPENAI_API_KEY=anet-sandbox", "CODEX_API_KEY=anet-sandbox"}},
		{&egressUpstream{Name: egressUpstreamCustom}, []string{"OPENAI_BASE_URL=http://127.0.0.1:3128/anet-upstream/custom",
			"OPENAI_API_KEY=anet-sandbox"}},
	} {
		env := sandboxEnvOverlay(daemonEnv, sandboxEgressEnv(tc.up))
		joined := "\n" + strings.Join(env, "\n") + "\n"
		for _, w := range append(tc.want, "PATH=/usr/bin", "LANG=C.UTF-8", "HOME=/home/u",
			"HTTPS_PROXY=http://127.0.0.1:3128", "https_proxy=http://127.0.0.1:3128", "HTTP_PROXY=http://127.0.0.1:3128",
			"http_proxy=http://127.0.0.1:3128", "ALL_PROXY=http://127.0.0.1:3128", "all_proxy=http://127.0.0.1:3128",
			"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost") {
			if !strings.Contains(joined, "\n"+w+"\n") {
				t.Errorf("%s: env lacks %s: %v", tc.up.Name, w, env)
			}
		}
		for _, bad := range []string{testOperatorKey, "secretpw", "proxy.example", "ANTHROPIC_AUTH_TOKEN", "NO_PROXY=*"} {
			if strings.Contains(joined, bad) {
				t.Errorf("%s: env carries %s: %v", tc.up.Name, bad, env)
			}
		}
		seen := map[string]bool{}
		for _, kv := range env {
			k, _, _ := strings.Cut(kv, "=")
			if seen[k] {
				t.Errorf("%s: %s set twice", tc.up.Name, k)
			}
			seen[k] = true
		}
	}
	// Without an endpoint the overlay still takes the keys and proxies out.
	env := strings.Join(sandboxEnvOverlay(daemonEnv, sandboxEgressEnv(nil)), "\n")
	if strings.Contains(env, testOperatorKey) || strings.Contains(env, "PROXY") || !strings.Contains(env, "PATH=/usr/bin") {
		t.Errorf("overlay without an endpoint: %s", env)
	}
}

// The model id of GET /v1/models/{id} is one plain segment.
func TestEgressRouteMatching(t *testing.T) {
	up := &egressUpstream{Name: egressUpstreamAnthropic, Routes: egressAnthropicRoutes}
	for p, ok := range map[string]bool{
		"/v1/models/claude-haiku-4-5": true, "/v1/models/ft:gpt-4o:org::abc": true, "/v1/models/a@b.c": true,
		"/v1/models/": false, "/v1/models/a/b": false, "/v1/models/.x": false, "/v1/models/a b": false,
		"/v1/models/" + strings.Repeat("a", 129): false, "/v1/modelsx": false,
	} {
		if _, got := up.match("GET", p); got != ok {
			t.Errorf("GET %s: %v", p, got)
		}
	}
	if k, ok := up.match("POST", "/v1/messages"); !ok || k != routeAnthropicMessages {
		t.Error("POST /v1/messages")
	}
	if _, ok := up.match("GET", "/v1/messages"); ok {
		t.Error("GET /v1/messages")
	}
}

// egressArg keeps a value from the sandbox to 64 characters with no control or format characters.
func TestEgressArg(t *testing.T) {
	if got := egressArg("a\nb‮c\x00d​" + strings.Repeat("é", 100)); got != "abcd"+strings.Repeat("é", 60) {
		t.Errorf("%q", got)
	}
}

// UP-3's constant tables are what they claim: every alias names a model of the anthropic upstream that is
// not itself an alias, the cheapest model of each built-in upstream is set and is no alias, and the
// cheapest Anthropic model is the haiku alias's model (the lowest-priced family).
func TestEgressModelTables(t *testing.T) {
	for alias, model := range egressClaudeAliases {
		if alias != strings.ToLower(alias) || !strings.HasPrefix(model, "claude-") || egressClaudeAliases[model] != "" {
			t.Errorf("alias %q -> %q", alias, model)
		}
		if !egressModelIDRe.MatchString(model) {
			t.Errorf("alias %q names %q, which GET /v1/models/{id} would refuse", alias, model)
		}
	}
	for _, name := range []string{egressUpstreamAnthropic, egressUpstreamOpenAI} {
		m := egressCheapestModel[name]
		if m == "" || egressClaudeAliases[m] != "" || !egressModelIDRe.MatchString(m) {
			t.Errorf("%s: cheapest model %q", name, m)
		}
	}
	if egressCheapestModel[egressUpstreamAnthropic] != egressClaudeAliases["haiku"] {
		t.Errorf("the cheapest Anthropic model %q is not the haiku alias's %q", egressCheapestModel[egressUpstreamAnthropic], egressClaudeAliases["haiku"])
	}
	if len(egressCheapestModel) != 2 {
		t.Errorf("a cheapest model for a custom service: %v (custom requires auto_reply.model)", egressCheapestModel)
	}
}

// UP-2 (2026-10-10), ux §1.4 H: an agent the sandbox runs, with an api_base whose model service does not
// carry the API it speaks, is refused when the configuration is resolved — on every platform, before any
// sandbox, key or model is looked at — as agent_unsupported.combo with the agent's {why}: claude with any
// service but api.anthropic.com (anthropic_only: a third-party service's key must not go to Anthropic),
// codex with any but api.openai.com (openai_only), opencode, hermes and openclaw with api.anthropic.com
// (no_chat). Spellings of the same host are the same host. The refusal is an ordinary sandbox refusal
// (code agent_unsupported, errSandboxUnavailable) and names neither the key nor the address's user part.
func TestUpstreamCombinationsFailClosed(t *testing.T) {
	const key = testOperatorKey
	anthropicBases := []string{"https://api.anthropic.com", "https://api.anthropic.com/v1", "https://API.Anthropic.com./"}
	openaiBases := []string{"https://api.openai.com/v1", "https://Api.OpenAI.com.:443/v1"}
	customBases := []string{"http://127.0.0.1:11434/v1", "http://localhost:8000", "https://openrouter.ai/api/v1",
		"https://models.example/v1", "https://api.anthropic.com.evil.example/v1", "https://anthropic.com/v1"}
	type want struct{ upstream, why string }
	cases := map[string]map[string]want{}
	add := func(agent, base string, w want) {
		if cases[agent] == nil {
			cases[agent] = map[string]want{}
		}
		cases[agent][base] = w
	}
	for _, b := range anthropicBases {
		add(agentClaude, b, want{upstream: egressUpstreamAnthropic})
		add(agentCodex, b, want{why: agentComboOpenAIOnly})
		for _, a := range []string{agentOpenCode, agentHermes, agentOpenClaw} {
			add(a, b, want{why: agentComboNoChat})
		}
	}
	for _, b := range openaiBases {
		add(agentClaude, b, want{why: agentComboAnthropicOnly})
		add(agentCodex, b, want{upstream: egressUpstreamOpenAI})
		for _, a := range []string{agentOpenCode, agentHermes, agentOpenClaw} {
			add(a, b, want{upstream: egressUpstreamOpenAI})
		}
	}
	for _, b := range customBases {
		add(agentClaude, b, want{why: agentComboAnthropicOnly})
		add(agentCodex, b, want{why: agentComboOpenAIOnly})
		for _, a := range []string{agentOpenCode, agentHermes, agentOpenClaw} {
			add(a, b, want{upstream: egressUpstreamCustom})
		}
	}
	for agent, bases := range cases {
		for base, w := range bases {
			for _, k := range []string{key, ""} {
				cfg := AutoReplyConfig{Backend: "exec", Agent: agent, APIBase: base, APIKey: k, Model: "m"}
				up, err := resolveEgressUpstream(cfg)
				if w.why == "" {
					// Allowed: the only refusal left is the missing key of a built-in service.
					if k == "" && w.upstream != egressUpstreamCustom {
						if refusalCode(err) != "no_api_key" {
							t.Errorf("%s %s without a key: %v", agent, base, err)
						}
						continue
					}
					if err != nil || up.Name != w.upstream {
						t.Errorf("%s %s: %v %v, want %s", agent, base, up, err, w.upstream)
					}
					continue
				}
				var combo *egressComboError
				if !errors.As(err, &combo) || combo.Why != w.why || combo.Code != "agent_unsupported" ||
					combo.reasonKey() != "agent_unsupported.combo" || refusalCode(err) != "agent_unsupported" ||
					!errors.Is(err, errSandboxUnavailable) || up != nil {
					t.Errorf("%s %s (key %v): %v, want agent_unsupported.combo {why %s}", agent, base, k != "", err, w.why)
					continue
				}
				if strings.Contains(err.Error(), key) {
					t.Errorf("the refusal names the key: %v", err)
				}
			}
		}
	}
	// What is not a combination stays what it was: no api_base where one is needed, an agent the sandbox
	// does not run, an address that is not allowed at all.
	for _, tc := range []struct {
		cfg  AutoReplyConfig
		code string
	}{
		{AutoReplyConfig{Agent: agentHermes, APIKey: key, Model: "m"}, "no_api_base"},
		{AutoReplyConfig{Agent: agentCursor, APIKey: key, APIBase: "https://api.openai.com/v1"}, "agent_unsupported"},
		{AutoReplyConfig{Agent: agentClaude, APIKey: key, APIBase: "http://api.anthropic.com"}, "api_base_insecure"},
		{AutoReplyConfig{Agent: agentCodex, APIKey: key, APIBase: "http://198.51.100.5/v1"}, "api_base_insecure"},
	} {
		_, err := resolveEgressUpstream(tc.cfg)
		var combo *egressComboError
		if refusalCode(err) != tc.code || errors.As(err, &combo) {
			t.Errorf("%+v: %v, want %s", tc.cfg, err, tc.code)
		}
	}
}

// refusalCode is the local reason of a sandbox refusal, "" for none and "?" for another error.
func refusalCode(err error) string {
	var pe *sandboxProbeError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &pe):
		return pe.Code
	}
	return "?"
}

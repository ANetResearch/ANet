package daemon

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

var updateInjectGolden = flag.Bool("update-inject", false, "rewrite the injection route golden files in testdata/inject")

// injectCase is one request from the sandbox and what must become of it: forwarded (refused "") or
// refused with a code.
type injectCase struct {
	name     string
	upstream string // anthropic, openai, custom or openrouter
	method   string
	path     string // the upstream path
	header   map[string]string
	body     string
	refused  string
}

// injectCorpus is the SI-57 corpus: ordinary requests of each dialect and every way around the policy
// this package knows of.
var injectCorpus = []injectCase{
	// anthropic: model and limits (BP-2, BP-3)
	{"opus-and-huge-limit", "anthropic", "POST", "/v1/messages", nil, `{"model":"claude-opus-5","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`, ""},
	{"no-limit", "anthropic", "POST", "/v1/messages", nil, `{"messages":[]}`, ""},
	{"limit-as-string", "anthropic", "POST", "/v1/messages", nil, `{"max_tokens":"100000","messages":[]}`, ""},
	{"limit-as-float", "anthropic", "POST", "/v1/messages", nil, `{"max_tokens":1e9,"messages":[]}`, ""},
	{"limit-fraction", "anthropic", "POST", "/v1/messages", nil, `{"max_tokens":8191.5,"messages":[]}`, ""},
	{"limit-null", "anthropic", "POST", "/v1/messages", nil, `{"max_tokens":null,"messages":[]}`, ""},
	{"limit-negative", "anthropic", "POST", "/v1/messages", nil, `{"max_tokens":-1,"messages":[]}`, ""},
	{"limit-zero", "anthropic", "POST", "/v1/messages", nil, `{"max_tokens":0,"messages":[]}`, ""},
	{"thinking-over-limit", "anthropic", "POST", "/v1/messages", nil, `{"max_tokens":64000,"thinking":{"type":"enabled","budget_tokens":60000},"messages":[]}`, ""},
	{"thinking-budget-string", "anthropic", "POST", "/v1/messages", nil, `{"max_tokens":4000,"thinking":{"type":"enabled","budget_tokens":"9999"},"messages":[]}`, ""},
	{"duplicate-model", "anthropic", "POST", "/v1/messages", nil, `{"model":"a","model":"claude-opus-5","messages":[]}`, ""},
	{"count-tokens", "anthropic", "POST", "/v1/messages/count_tokens", nil, `{"model":"claude-opus-5","messages":[]}`, ""},
	{"count-tokens-limit", "anthropic", "POST", "/v1/messages/count_tokens", nil, `{"max_tokens":99999,"messages":[]}`, ""},
	// anthropic: tools (BP-4)
	{"client-tools", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"tools":[{"type":"bash_20250124","name":"bash"},` +
		`{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"},{"type":"computer_20251124","name":"computer"},` +
		`{"type":"memory_20250818","name":"memory"},{"type":"custom","name":"c","input_schema":{}},{"name":"plain","input_schema":{}}]}`, ""},
	{"web-search", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"tools":[{"type":"web_search_20260209","name":"web_search"}]}`, "policy_server_tool"},
	{"web-fetch", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"tools":[{"name":"x","input_schema":{}},{"type":"web_fetch_20250910","name":"web_fetch"}]}`, "policy_server_tool"},
	{"code-execution", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"tools":[{"type":"code_execution_20260521","name":"code_execution"}]}`, "policy_server_tool"},
	{"mcp-toolset", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"tools":[{"type":"mcp_toolset","mcp_server_name":"x"}]}`, "policy_server_tool"},
	{"tool-search", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"tools":[{"type":"tool_search_tool_regex_20251119","name":"s"}]}`, "policy_server_tool"},
	{"unknown-tool-type", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"tools":[{"type":"advisor_20991231","name":"a"}]}`, "policy_server_tool"},
	{"tool-type-number", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"tools":[{"type":5,"name":"a"}]}`, "policy_server_tool"},
	{"tool-type-empty", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"tools":[{"type":"","name":"a"}]}`, "policy_server_tool"},
	{"tools-not-a-list", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"tools":{"type":"custom"}}`, "policy_field"},
	{"tool-not-an-object", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"tools":["web_search"]}`, "policy_field"},
	{"duplicate-tool-type", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"tools":[{"type":"web_search_20260209","type":"custom","name":"x"}]}`, ""},
	{"count-tokens-server-tool", "anthropic", "POST", "/v1/messages/count_tokens", nil, `{"messages":[],"tools":[{"type":"web_search_20250305","name":"w"}]}`, "policy_server_tool"},
	// anthropic: fields (BP-5)
	{"mcp-servers", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"mcp_servers":[{"type":"url","url":"https://mcp.example","name":"m"}]}`, "policy_mcp_servers"},
	{"mcp-servers-empty", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"mcp_servers":[]}`, "policy_mcp_servers"},
	{"mcp-servers-null", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"mcp_servers":null}`, ""},
	{"mcp-servers-escaped", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"mcp\u005fservers":[{"type":"url","url":"https://mcp.example","name":"m"}]}`, "policy_mcp_servers"},
	{"container", "anthropic", "POST", "/v1/messages", nil, `{"messages":[],"container":{"skills":[{"type":"anthropic","skill_id":"pptx"}]}}`, "policy_field"},
	// anthropic: headers (UP-4, BP-6)
	{"agent-key", "anthropic", "POST", "/v1/messages", map[string]string{"x-api-key": testAgentKey, "Authorization": "Bearer " + testAgentKey}, `{"messages":[]}`, ""},
	{"betas", "anthropic", "POST", "/v1/messages", map[string]string{"anthropic-beta": "mcp-client-2025-11-20,claude-code-20250219, web-fetch-2025-09-10,code-execution-2025-08-25,files-api-2025-04-14,skills-2025-10-02,container-x,web-search-x,compact-2026-01-12"}, `{"messages":[]}`, ""},
	// anthropic: encoding (BP-1)
	{"gzip", "anthropic", "POST", "/v1/messages", map[string]string{"Content-Encoding": "gzip"}, `{"messages":[]}`, "policy_encoding"},
	{"identity", "anthropic", "POST", "/v1/messages", map[string]string{"Content-Encoding": "identity"}, `{"messages":[]}`, ""},
	{"not-an-object", "anthropic", "POST", "/v1/messages", nil, `[{"messages":[]}]`, "policy_encoding"},
	{"two-objects", "anthropic", "POST", "/v1/messages", nil, `{"messages":[]} {"mcp_servers":[{}]}`, "policy_encoding"},
	{"null-body", "anthropic", "POST", "/v1/messages", nil, `null`, "policy_encoding"},
	{"empty-body", "anthropic", "POST", "/v1/messages", nil, ``, "policy_encoding"},
	// openai responses
	{"codex-turn", "openai", "POST", "/v1/responses", map[string]string{"OpenAI-Beta": "responses=experimental"},
		`{"model":"gpt-5-codex","instructions":"x","input":[],"tools":[{"type":"function","name":"shell","parameters":{}},` +
			`{"type":"local_shell"},{"type":"custom","name":"apply_patch"}],"tool_choice":"auto","store":true,"stream":true,` +
			`"service_tier":"priority","max_output_tokens":50000,"include":["reasoning.encrypted_content"]}`, ""},
	{"background", "openai", "POST", "/v1/responses", nil, `{"input":"x","background":true}`, "policy_field"},
	{"background-string", "openai", "POST", "/v1/responses", nil, `{"input":"x","background":"true"}`, "policy_field"},
	{"background-false", "openai", "POST", "/v1/responses", nil, `{"input":"x","background":false}`, ""},
	{"conversation", "openai", "POST", "/v1/responses", nil, `{"input":"x","conversation":"conv_1"}`, "policy_field"},
	{"stored-prompt", "openai", "POST", "/v1/responses", nil, `{"input":"x","prompt":{"id":"pmpt_1"}}`, "policy_field"},
	{"previous-response", "openai", "POST", "/v1/responses", nil, `{"input":"x","previous_response_id":"resp_1"}`, "policy_field"},
	{"previous-response-null", "openai", "POST", "/v1/responses", nil, `{"input":"x","previous_response_id":null}`, ""},
	{"hosted-web-search", "openai", "POST", "/v1/responses", nil, `{"input":"x","tools":[{"type":"web_search"}]}`, "policy_server_tool"},
	{"hosted-file-search", "openai", "POST", "/v1/responses", nil, `{"input":"x","tools":[{"type":"file_search","vector_store_ids":["v"]}]}`, "policy_server_tool"},
	{"hosted-code", "openai", "POST", "/v1/responses", nil, `{"input":"x","tools":[{"type":"code_interpreter","container":{"type":"auto"}}]}`, "policy_server_tool"},
	{"hosted-mcp", "openai", "POST", "/v1/responses", nil, `{"input":"x","tools":[{"type":"mcp","server_url":"https://mcp.example"}]}`, "policy_server_tool"},
	{"hosted-image", "openai", "POST", "/v1/responses", nil, `{"input":"x","tools":[{"type":"image_generation"}]}`, "policy_server_tool"},
	{"tool-without-type", "openai", "POST", "/v1/responses", nil, `{"input":"x","tools":[{"name":"f"}]}`, "policy_server_tool"},
	{"hosted-tool-choice", "openai", "POST", "/v1/responses", nil, `{"input":"x","tool_choice":{"type":"web_search_preview"}}`, "policy_server_tool"},
	{"allowed-tools-choice", "openai", "POST", "/v1/responses", nil, `{"input":"x","tools":[{"type":"function","name":"f"}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"web_search"}]}}`, "policy_server_tool"},
	{"function-choice", "openai", "POST", "/v1/responses", nil, `{"input":"x","tools":[{"type":"function","name":"f"}],"tool_choice":{"type":"function","name":"f"}}`, ""},
	{"openai-agent-key", "openai", "POST", "/v1/responses", map[string]string{"x-api-key": testAgentKey, "api-key": testAgentKey, "OpenAI-Organization": "org-x", "OpenAI-Beta": "assistants=v2"}, `{"input":"x"}`, ""},
	// openai chat
	{"chat", "openai", "POST", "/v1/chat/completions", nil, `{"model":"gpt-5","messages":[],"n":4,"max_tokens":1000000,"store":true,"service_tier":"flex"}`, ""},
	{"chat-completion-limit", "openai", "POST", "/v1/chat/completions", nil, `{"messages":[],"max_completion_tokens":99999}`, ""},
	{"chat-no-limit", "openai", "POST", "/v1/chat/completions", nil, `{"messages":[]}`, ""},
	{"chat-web-search", "openai", "POST", "/v1/chat/completions", nil, `{"messages":[],"web_search_options":{}}`, "policy_field"},
	{"chat-custom-tool", "openai", "POST", "/v1/chat/completions", nil, `{"messages":[],"tools":[{"type":"custom","name":"c"}]}`, "policy_server_tool"},
	{"chat-function", "openai", "POST", "/v1/chat/completions", nil, `{"messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"function","function":{"name":"f"}}}`, ""},
	// custom
	{"custom-chat", "custom", "POST", "/chat/completions", nil, `{"model":"other","messages":[],"plugins":[{"id":"web"}],"n":2}`, ""},
	{"custom-models", "custom", "GET", "/models", nil, ``, ""},
	{"openrouter-plugins", "openrouter", "POST", "/chat/completions", nil, `{"messages":[],"plugins":[{"id":"web"}]}`, "policy_field"},
	{"openrouter-models", "openrouter", "POST", "/chat/completions", nil, `{"messages":[],"models":["openai/gpt-5","x"]}`, "policy_field"},
	{"openrouter-limit", "openrouter", "POST", "/chat/completions", nil, `{"messages":[],"max_tokens":20000}`, ""},
	// -1 is "generate until the context is full" to a local Ollama or llama.cpp server (BP-3)
	{"custom-unlimited", "custom", "POST", "/chat/completions", nil, `{"messages":[],"max_tokens":-1}`, ""},
	{"custom-unlimited-completion", "custom", "POST", "/chat/completions", nil, `{"messages":[],"max_completion_tokens":-2,"max_tokens":100}`, ""},
	{"responses-zero-limit", "openai", "POST", "/v1/responses", nil, `{"input":"x","max_output_tokens":0}`, ""},
	{"chat-allowed-tools-choice", "openai", "POST", "/v1/chat/completions", nil, `{"messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[{"type":"function","function":{"name":"f"}}]}}}`, ""},
	{"chat-allowed-tools-custom", "openai", "POST", "/v1/chat/completions", nil, `{"messages":[],"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[{"type":"custom","custom":{"name":"c"}}]}}}`, "policy_server_tool"},
	// BP-3: no thinking budget is valid with max_tokens of 1024 or less, so the request goes without thinking
	{"thinking-small-limit", "anthropic", "POST", "/v1/messages", nil, `{"max_tokens":1000,"thinking":{"type":"enabled","budget_tokens":2000},"messages":[]}`, ""},
	{"thinking-1024", "anthropic", "POST", "/v1/messages", nil, `{"max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[]}`, ""},
	{"thinking-small-adaptive", "anthropic", "POST", "/v1/messages", nil, `{"max_tokens":200,"thinking":{"type":"adaptive"},"messages":[]}`, ""},
	{"thinking-small-count", "anthropic", "POST", "/v1/messages/count_tokens", nil, `{"max_tokens":100,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[]}`, ""},
	// BP-5: media only inline; a URL is fetched by the model service, the way web_fetch would
	{"image-url", "anthropic", "POST", "/v1/messages", nil, `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},` +
		`{"type":"image","source":{"type":"url","url":"https://collect.example/x.png?d=secret"}}]}]}`, "policy_field"},
	{"document-url", "anthropic", "POST", "/v1/messages", nil, `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"https://collect.example/a.pdf"}}]}]}`, "policy_field"},
	{"tool-result-image-url", "anthropic", "POST", "/v1/messages", nil, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t",` +
		`"content":[{"type":"image","source":{"type":"url","url":"https://collect.example/y.png"}}]}]}]}`, "policy_field"},
	{"document-content-image-url", "anthropic", "POST", "/v1/messages", nil, `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"content",` +
		`"content":[{"type":"image","source":{"type":"url","url":"https://collect.example/z.png"}}]}}]}]}`, "policy_field"},
	{"system-image-url", "anthropic", "POST", "/v1/messages", nil, `{"system":[{"type":"image","source":{"type":"url","url":"https://collect.example/s.png"}}],"messages":[]}`, "policy_field"},
	{"count-tokens-image-url", "anthropic", "POST", "/v1/messages/count_tokens", nil, `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://collect.example/c.png"}}]}]}`, "policy_field"},
	{"image-base64", "anthropic", "POST", "/v1/messages", nil, `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}`, ""},
	{"tool-use-input-source", "anthropic", "POST", "/v1/messages", nil, `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"get",` +
		`"input":{"source":{"type":"url","url":"https://example.org"}}}]}]}`, ""},
	{"responses-image-url", "openai", "POST", "/v1/responses", nil, `{"input":[{"role":"user","content":[{"type":"input_text","text":"x"},{"type":"input_image","image_url":"https://collect.example/i.png"}]}]}`, "policy_field"},
	{"responses-file-url", "openai", "POST", "/v1/responses", nil, `{"input":[{"role":"user","content":[{"type":"input_file","file_url":"https://collect.example/f.pdf"}]}]}`, "policy_field"},
	{"responses-tool-output-image-url", "openai", "POST", "/v1/responses", nil, `{"input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"http://collect.example/o.png"}]}]}`, "policy_field"},
	{"responses-image-data", "openai", "POST", "/v1/responses", nil, `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]}]}`, ""},
	{"chat-image-url", "openai", "POST", "/v1/chat/completions", nil, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://collect.example/c.png"}}]}]}`, "policy_field"},
	{"chat-image-data", "openai", "POST", "/v1/chat/completions", nil, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"DATA:image/png;base64,iVBORw0KGgo="}}]}]}`, ""},
	{"custom-image-url-string", "custom", "POST", "/chat/completions", nil, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://collect.example/s.png"}]}]}`, "policy_field"},
	// the same for the media other compatible services fetch: video_url and audio_url (with or without a
	// type naming them), and a file whose file_data is a URL
	{"custom-video-url", "custom", "POST", "/chat/completions", nil, `{"messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"https://collect.example/v.mp4?d=secret"}}]}]}`, "policy_field"},
	{"custom-audio-url-untyped", "custom", "POST", "/chat/completions", nil, `{"messages":[{"role":"user","content":[{"audio_url":{"url":"http://10.0.0.8/a.wav"}}]}]}`, "policy_field"},
	{"custom-image-url-untyped", "custom", "POST", "/chat/completions", nil, `{"messages":[{"role":"user","content":[{"type":"text","text":"x","image_url":"https://collect.example/t.png"}]}]}`, "policy_field"},
	{"openrouter-file-data-url", "openrouter", "POST", "/chat/completions", nil, `{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"p.pdf","file_data":"https://collect.example/p.pdf"}}]}]}`, "policy_field"},
	{"openrouter-file-data-inline", "openrouter", "POST", "/chat/completions", nil, `{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"p.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}}]}]}`, ""},
	{"chat-file-data-base64", "openai", "POST", "/v1/chat/completions", nil, `{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"p.pdf","file_data":"JVBERi0xLjQK"}}]}]}`, ""},
	{"custom-video-data", "custom", "POST", "/chat/completions", nil, `{"messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"data:video/mp4;base64,AAAA"}}]}]}`, ""},
}

// upstreamForCase is the upstream a case runs against, at base (the custom ones at base/v1, openrouter
// with its flag set as for an openrouter.ai address).
func upstreamForCase(t testing.TB, name, base string) *egressUpstream {
	if name == "openrouter" {
		up := testUpstreamAt(t, egressUpstreamCustom, base)
		up.OpenRouter = true
		return up
	}
	return testUpstreamAt(t, name, base)
}

// TestInjectPolicyAsserts (SI-57): every request of the corpus goes through the endpoint to a fake model
// service. Each one is forwarded or refused as the case says, a refusal is the code's sentence, and every
// request the service receives passes the independent reading of SI-57 (si57Check): model pinned, every
// output limit at most 8192, no server-side tool, MCP connector or hosted state, OpenAI requests not
// stored, only the operator's key, an allowed path.
func TestInjectPolicyAsserts(t *testing.T) {
	f := newFakeModelService(t)
	for _, tc := range injectCorpus {
		t.Run(tc.name, func(t *testing.T) {
			f.reset()
			up := upstreamForCase(t, tc.upstream, f.srv.URL)
			_, c, _ := startTestEgress(t, up)
			st, body := doJSON(t, c, tc.method, routeURL(up.Name, tc.path), tc.header, tc.body)
			got := f.requests()
			for _, r := range got {
				if err := si57Check(up, r); err != nil {
					t.Errorf("the service received a request against SI-57: %v\n%s", err, r.Body)
				}
			}
			if tc.refused == "" {
				if st != 200 || len(got) != 1 {
					t.Fatalf("want it forwarded: %d %s (%d received)", st, body, len(got))
				}
				return
			}
			msg, code, ok := refusalMessage(body)
			if len(got) != 0 || !ok || !strings.HasPrefix(msg, "anet: ") || (up.Name != egressUpstreamAnthropic && code != tc.refused) {
				t.Fatalf("want %s: %d %s (%d received)", tc.refused, st, body, len(got))
			}
			if row, _ := a2ashape.Lookup(tc.refused); row.Class&a2ashape.ClassModel == 0 {
				t.Fatalf("%s is not a sandbox egress code", tc.refused)
			}
			want := map[string]int{"policy_encoding": 400, "policy_field": 403, "policy_server_tool": 403, "policy_mcp_servers": 403}[tc.refused]
			if tc.refused == "policy_encoding" && tc.header["Content-Encoding"] == "gzip" {
				want = 415
			}
			if st != want {
				t.Errorf("status %d, want %d", st, want)
			}
		})
	}
}

// The rules one by one, with the values the forwarded body must carry (BP-2 to BP-5).
func TestInjectBodyPolicy(t *testing.T) {
	ant := testUpstreamAt(t, egressUpstreamAnthropic, "https://model.invalid")
	oai := testUpstreamAt(t, egressUpstreamOpenAI, "https://model.invalid")
	cus := testUpstreamAt(t, egressUpstreamCustom, "http://127.0.0.1:9")
	for _, tc := range []struct {
		up   *egressUpstream
		kind egressRouteKind
		in   string
		want string // the forwarded body, or "refused:<code>"
	}{
		{ant, routeAnthropicMessages, `{"model":"claude-opus-5","max_tokens":100000}`, `{"max_tokens":8192,"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicMessages, `{"max_tokens":512}`, `{"max_tokens":512,"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicMessages, `{}`, `{"max_tokens":8192,"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicMessages, `{"max_tokens":"512"}`, `{"max_tokens":8192,"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicMessages, `{"max_tokens":512.0}`, `{"max_tokens":8192,"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicMessages, `{"max_tokens":-1}`, `{"max_tokens":8192,"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicMessages, `{"max_tokens":0}`, `{"max_tokens":8192,"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicMessages, `{"max_tokens":-5,"thinking":{"type":"enabled","budget_tokens":-1}}`,
			`{"max_tokens":8192,"model":"claude-haiku-4-5","thinking":{"budget_tokens":4096,"type":"enabled"}}`},
		{ant, routeAnthropicMessages, `{"max_tokens":64000,"thinking":{"type":"enabled","budget_tokens":60000}}`,
			`{"max_tokens":8192,"model":"claude-haiku-4-5","thinking":{"budget_tokens":4096,"type":"enabled"}}`},
		{ant, routeAnthropicMessages, `{"max_tokens":1500,"thinking":{"type":"enabled","budget_tokens":1500}}`,
			`{"max_tokens":1500,"model":"claude-haiku-4-5","thinking":{"budget_tokens":1024,"type":"enabled"}}`},
		// max_tokens of 1024 or less leaves no valid budget (the service's smallest is 1024): no thinking
		{ant, routeAnthropicMessages, `{"max_tokens":1,"thinking":{"type":"enabled","budget_tokens":""}}`,
			`{"max_tokens":1,"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicMessages, `{"max_tokens":1000,"thinking":{"type":"enabled","budget_tokens":500}}`,
			`{"max_tokens":1000,"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicMessages, `{"max_tokens":1024,"thinking":{"type":"disabled"}}`, `{"max_tokens":1024,"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicMessages, `{"max_tokens":1025,"thinking":{"type":"enabled","budget_tokens":1025}}`,
			`{"max_tokens":1025,"model":"claude-haiku-4-5","thinking":{"budget_tokens":1024,"type":"enabled"}}`},
		{ant, routeAnthropicCountTokens, `{"max_tokens":10,"thinking":{"type":"enabled","budget_tokens":2048}}`, `{"max_tokens":10,"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicCountTokens, `{"thinking":{"type":"enabled","budget_tokens":2048}}`,
			`{"model":"claude-haiku-4-5","thinking":{"budget_tokens":2048,"type":"enabled"}}`},
		{ant, routeAnthropicMessages, `{"max_tokens":512,"thinking":null}`, `{"max_tokens":512,"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicMessages, `{"max_tokens":8000,"thinking":{"type":"enabled","budget_tokens":2000}}`,
			`{"max_tokens":8000,"model":"claude-haiku-4-5","thinking":{"budget_tokens":2000,"type":"enabled"}}`},
		{ant, routeAnthropicMessages, `{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`,
			`{"max_tokens":8192,"model":"claude-haiku-4-5","output_config":{"effort":"low"},"thinking":{"type":"adaptive"}}`},
		{ant, routeAnthropicCountTokens, `{"model":"x"}`, `{"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicCountTokens, `{"max_tokens":9000}`, `{"max_tokens":8192,"model":"claude-haiku-4-5"}`},
		{ant, routeAnthropicMessages, `{"system":"<b>&</b>","temperature":0.70}`, `{"max_tokens":8192,"model":"claude-haiku-4-5","system":"<b>&</b>","temperature":0.70}`},
		{ant, routeAnthropicMessages, `{"mcp_servers":[]}`, "refused:policy_mcp_servers"},
		{ant, routeAnthropicMessages, `{"container":"c"}`, "refused:policy_field"},
		{ant, routeAnthropicMessages, `{"tools":[{"type":"web_fetch_20260209","name":"f"}]}`, "refused:policy_server_tool"},
		{ant, routeAnthropicMessages, `{"tools":[{"type":"code_execution_20260120","name":"c"}]}`, "refused:policy_server_tool"},
		{ant, routeAnthropicMessages, `{"tools":[{"type":"zzz","name":"c"}]}`, "refused:policy_server_tool"},
		{oai, routeResponses, `{"model":"gpt-5","store":true,"service_tier":"priority","input":"x"}`,
			`{"input":"x","max_output_tokens":8192,"model":"gpt-5-nano","store":false}`},
		{oai, routeResponses, `{"max_output_tokens":100,"background":false}`, `{"background":false,"max_output_tokens":100,"model":"gpt-5-nano","store":false}`},
		{oai, routeResponses, `{"previous_response_id":"resp_1"}`, "refused:policy_field"},
		{oai, routeResponses, `{"tools":[{"type":"web_search_preview"}]}`, "refused:policy_server_tool"},
		{oai, routeResponses, `{"tools":[{"type":"code_interpreter"}]}`, "refused:policy_server_tool"},
		{oai, routeChat, `{"n":3,"max_tokens":100000}`, `{"max_tokens":8192,"model":"gpt-5-nano","n":1,"store":false}`},
		{oai, routeChat, `{}`, `{"max_completion_tokens":8192,"model":"gpt-5-nano","store":false}`},
		{oai, routeChat, `{"max_completion_tokens":10,"max_tokens":20000}`, `{"max_completion_tokens":10,"max_tokens":8192,"model":"gpt-5-nano","store":false}`},
		{cus, routeChat, `{}`, `{"max_tokens":8192,"model":"local-model","store":false}`},
		{cus, routeChat, `{"max_tokens":-1}`, `{"max_tokens":8192,"model":"local-model","store":false}`},
		{cus, routeChat, `{"max_completion_tokens":0}`, `{"max_completion_tokens":8192,"model":"local-model","store":false}`},
		{oai, routeResponses, `{"max_output_tokens":-1}`, `{"max_output_tokens":8192,"model":"gpt-5-nano","store":false}`},
		{cus, routeChat, `{"plugins":[1],"models":[2]}`, `{"max_tokens":8192,"model":"local-model","models":[2],"plugins":[1],"store":false}`},
		// BP-5: media by URL, refused with the field's path; inline data passes
		{ant, routeAnthropicMessages, `{"messages":[{"content":"x"},{"content":[{"type":"text"},{"type":"image","source":{"type":"url","url":"u"}}]}]}`,
			"refused:policy_field:messages[1].content[1].source"},
		{ant, routeAnthropicMessages, `{"messages":[{"content":[{"type":"tool_result","content":[{"type":"document","source":{"type":"url"}}]}]}]}`,
			"refused:policy_field:messages[0].content[0].content[0].source"},
		{ant, routeAnthropicMessages, `{"messages":[{"content":[{"type":"image","source":{"type":"base64","data":"AA=="}}]}]}`,
			`{"max_tokens":8192,"messages":[{"content":[{"source":{"data":"AA==","type":"base64"},"type":"image"}]}],"model":"claude-haiku-4-5"}`},
		{oai, routeResponses, `{"input":[{"content":[{"type":"input_image","image_url":"https://u"}]}]}`, "refused:policy_field:input[0].content[0].image_url"},
		{oai, routeResponses, `{"input":[{"content":[{"type":"input_image","image_url":7}]}]}`, "refused:policy_field:input[0].content[0].image_url"},
		{oai, routeResponses, `{"input":[{"content":[{"type":"input_file","file_url":"https://u/f.pdf"}]}]}`, "refused:policy_field:input[0].content[0].file_url"},
		{oai, routeResponses, `{"input":[{"content":[{"type":"input_image","image_url":null,"file_id":"file_1"}]}]}`,
			`{"input":[{"content":[{"file_id":"file_1","type":"input_image"}]}],"max_output_tokens":8192,"model":"gpt-5-nano","store":false}`},
		{oai, routeChat, `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"http://u"}}]}]}`, "refused:policy_field:messages[0].content[0].image_url"},
		{oai, routeChat, `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`,
			`{"max_completion_tokens":8192,"messages":[{"content":[{"image_url":{"url":"data:image/png;base64,AA=="},"type":"image_url"}]}],"model":"gpt-5-nano","store":false}`},
		{cus, routeChat, `{"messages":[{"content":[{"type":"image_url","image_url":"https://u"}]}]}`, "refused:policy_field:messages[0].content[0].image_url"},
		{cus, routeChat, `{"messages":[{"content":[{"type":"video_url","video_url":{"url":"https://u/v.mp4"}}]}]}`, "refused:policy_field:messages[0].content[0].video_url"},
		{cus, routeChat, `{"messages":[{"content":[{"audio_url":{"url":"http://u/a.wav"}}]}]}`, "refused:policy_field:messages[0].content[0].audio_url"},
		{cus, routeChat, `{"messages":[{"content":[{"type":"text","text":"x","image_url":{"url":"https://u"}}]}]}`, "refused:policy_field:messages[0].content[0].image_url"},
		{oai, routeChat, `{"messages":[{"content":[{"type":"file","file":{"file_data":"https://u/f.pdf"}}]}]}`, "refused:policy_field:messages[0].content[0].file.file_data"},
		{oai, routeChat, `{"messages":[{"content":[{"type":"file","file":{"file_data":"JVBERi0xLjQK"}}]}]}`,
			`{"max_completion_tokens":8192,"messages":[{"content":[{"file":{"file_data":"JVBERi0xLjQK"},"type":"file"}]}],"model":"gpt-5-nano","store":false}`},
	} {
		out, ref := rewriteInjectBody(tc.up, tc.kind, []byte(tc.in))
		got := string(out)
		if ref != nil {
			got = "refused:" + ref.Code
			if strings.Count(tc.want, ":") == 2 {
				got += ":" + fmt.Sprint(ref.Args["field"])
			}
		}
		if got != tc.want {
			t.Errorf("%s %s:\n got %s\nwant %s", tc.up.Name, tc.in, got, tc.want)
		}
	}
}

// BP-6 (M-BP-7): no forwarded beta value gives the model service reach or hosted state of its own, and
// the filter keeps only listed values.
func TestEgressBetaAllowlists(t *testing.T) {
	for _, allow := range []map[string]bool{egressAnthropicBetas, egressOpenAIBetas} {
		for v := range allow {
			for _, p := range egressForbiddenBetaPrefixes {
				if strings.HasPrefix(v, p) {
					t.Errorf("the beta allowlist holds %s, which begins %s", v, p)
				}
			}
		}
	}
	for _, p := range []string{"mcp-client", "code-execution", "files-api", "web-fetch", "web-search", "skills", "container"} {
		found := false
		for _, q := range egressForbiddenBetaPrefixes {
			found = found || q == p
		}
		if !found {
			t.Errorf("forbidden prefix %s is missing", p)
		}
	}
	h := http.Header{}
	h.Add("Anthropic-Beta", "mcp-client-2025-11-20, claude-code-20250219")
	h.Add("Anthropic-Beta", "web-search-2025-03-05,interleaved-thinking-2025-05-14")
	h.Set("Openai-Beta", "assistants=v2")
	filterBetaHeader(h, "Anthropic-Beta", egressAnthropicBetas)
	filterBetaHeader(h, "Openai-Beta", egressOpenAIBetas)
	if got := h.Values("Anthropic-Beta"); len(got) != 1 || got[0] != "claude-code-20250219,interleaved-thinking-2025-05-14" {
		t.Errorf("anthropic-beta %q", got)
	}
	if _, ok := h["Openai-Beta"]; ok {
		t.Errorf("openai-beta %q survived", h.Values("Openai-Beta"))
	}
}

// --- G-BP ---

// goldenInjectCase is one file of testdata/inject: a request from the sandbox and what the endpoint
// forwarded or answered.
type goldenInjectCase struct {
	Upstream string            `json:"upstream"`
	Model    string            `json:"model"`
	Request  goldenInjectReq   `json:"request"`
	Forward  *goldenInjectFwd  `json:"forwarded,omitempty"`
	Refused  *goldenInjectResp `json:"refused,omitempty"`
}

type goldenInjectReq struct {
	Method string            `json:"method"`
	Target string            `json:"target"`
	Header map[string]string `json:"header,omitempty"`
	Body   string            `json:"body,omitempty"`
}

type goldenInjectFwd struct {
	Method string          `json:"method"`
	URL    string          `json:"url"`
	Header http.Header     `json:"header"`
	Body   json.RawMessage `json:"body,omitempty"`
}

type goldenInjectResp struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// goldenInjectCases are the G-BP request pairs (LEC §7.1): anthropic with thinking, client tools and a
// refused server tool, OpenAI Responses, chat completions and OpenRouter.
var goldenInjectCases = []injectCase{
	{"anthropic-thinking-client-tools", "anthropic", "POST", "/v1/messages?beta=true",
		map[string]string{"x-api-key": egressPlaceholderKey, "anthropic-version": "2023-06-01",
			"anthropic-beta": "claude-code-20250219,interleaved-thinking-2025-05-14,mcp-client-2025-11-20,web-fetch-2025-09-10",
			"User-Agent":     "claude-cli/2", "X-Forwarded-For": "203.0.113.7"},
		`{"model":"opus","max_tokens":64000,"stream":true,"thinking":{"type":"enabled","budget_tokens":60000},` +
			`"system":[{"type":"text","text":"You are a coding agent."}],"messages":[{"role":"user","content":"fix the test"}],` +
			`"tools":[{"type":"bash_20250124","name":"bash"},{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"},` +
			`{"name":"Read","description":"read a file","input_schema":{"type":"object"}}]}`, ""},
	{"anthropic-server-tool-refused", "anthropic", "POST", "/v1/messages",
		map[string]string{"x-api-key": egressPlaceholderKey},
		`{"model":"claude-haiku-4-5","max_tokens":1024,"messages":[],"tools":[{"type":"web_search_20260209","name":"web_search"}]}`, "policy_server_tool"},
	{"anthropic-mcp-refused", "anthropic", "POST", "/v1/messages", nil,
		`{"max_tokens":1024,"messages":[],"mcp_servers":[{"type":"url","url":"https://mcp.example","name":"m"}]}`, "policy_mcp_servers"},
	{"anthropic-count-tokens", "anthropic", "POST", "/v1/messages/count_tokens",
		map[string]string{"x-api-key": egressPlaceholderKey, "anthropic-beta": "token-counting-2024-11-01"},
		`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`, ""},
	{"openai-responses", "openai", "POST", "/v1/responses",
		map[string]string{"Authorization": "Bearer " + egressPlaceholderKey, "OpenAI-Beta": "responses=experimental", "originator": "codex_cli_rs"},
		`{"model":"gpt-5-codex","instructions":"You are Codex.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
			`"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}},{"type":"local_shell"},{"type":"custom","name":"apply_patch"}],` +
			`"tool_choice":"auto","parallel_tool_calls":false,"store":true,"stream":true,"service_tier":"priority","include":["reasoning.encrypted_content"]}`, ""},
	{"openai-responses-hosted-refused", "openai", "POST", "/v1/responses", nil,
		`{"input":"x","tools":[{"type":"web_search"}]}`, "policy_server_tool"},
	{"openai-previous-response-refused", "openai", "POST", "/v1/responses", nil,
		`{"input":"x","previous_response_id":"resp_1"}`, "policy_field"},
	{"openai-chat", "openai", "POST", "/v1/chat/completions",
		map[string]string{"Authorization": "Bearer " + egressPlaceholderKey},
		`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"n":3,"max_tokens":100000,"store":true,` +
			`"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`, ""},
	{"openrouter-chat", "openrouter", "POST", "/chat/completions",
		map[string]string{"Authorization": "Bearer " + egressPlaceholderKey, "HTTP-Referer": "https://example.org"},
		`{"model":"anthropic/claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":true}`, ""},
	{"openrouter-plugins-refused", "openrouter", "POST", "/chat/completions", nil,
		`{"messages":[],"plugins":[{"id":"web"}]}`, "policy_field"},
	{"anthropic-small-limit-no-thinking", "anthropic", "POST", "/v1/messages",
		map[string]string{"x-api-key": egressPlaceholderKey, "anthropic-version": "2023-06-01"},
		`{"model":"haiku","max_tokens":512,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"title this"}]}`, ""},
	{"anthropic-image-url-refused", "anthropic", "POST", "/v1/messages",
		map[string]string{"x-api-key": egressPlaceholderKey},
		`{"max_tokens":1024,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://collect.example/p.png?d=x"}},` +
			`{"type":"text","text":"describe"}]}]}`, "policy_field"},
	{"openai-chat-image-url-refused", "openai", "POST", "/v1/chat/completions", nil,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://collect.example/q.png"}}]}]}`, "policy_field"},
	{"openai-responses-file-url-refused", "openai", "POST", "/v1/responses", nil,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_url":"https://collect.example/r.pdf"}]}]}`, "policy_field"},
}

// TestInjectPolicyGolden (G-BP): each request pair of goldenInjectCases, as the endpoint forwards it
// (method, address, headers, body) or answers it, equals testdata/inject/<case>.json; refusals.json holds
// every refusal body of ux §1.4 I in both formats. Rewrite with -update-inject and review the diff.
func TestInjectPolicyGolden(t *testing.T) {
	t.Setenv("LC_ALL", "C")
	for _, tc := range goldenInjectCases {
		t.Run(tc.name, func(t *testing.T) {
			g := goldenInjectCase{Upstream: tc.upstream, Request: goldenInjectReq{Method: tc.method, Target: tc.path, Header: tc.header, Body: tc.body}}
			up := goldenUpstream(t, tc.upstream)
			g.Model = up.Model
			rt := &recordingTransport{}
			ep := newEgressEndpoint(up)
			ep.transport = rt
			defer ep.Close()
			req := httptest.NewRequest(tc.method, egressRoutePrefix+up.Name+tc.path, strings.NewReader(tc.body))
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			ep.ServeHTTP(rec, req)
			if len(rt.got) == 1 {
				r := rt.got[0]
				// The file records what the endpoint does; what it records is held to the independent
				// reading of SI-57, so a golden file cannot capture a request against it.
				if err := si57Check(up, r); err != nil {
					t.Errorf("the golden request is against SI-57: %v", err)
				}
				g.Forward = &goldenInjectFwd{Method: r.Method, URL: r.URL, Header: r.Header, Body: r.Body}
			} else {
				g.Refused = &goldenInjectResp{Status: rec.Code, Body: rec.Body.Bytes()}
			}
			if (tc.refused == "") != (g.Forward != nil) {
				t.Fatalf("want refused %q, got %d %s", tc.refused, rec.Code, rec.Body)
			}
			compareGolden(t, filepath.Join("testdata", "inject", tc.name+".json"), g)
		})
	}
	// Every refusal of ux §1.4 I in both formats, with representative values.
	type pair struct {
		Status    int             `json:"status"`
		Anthropic json.RawMessage `json:"anthropic"`
		OpenAI    json.RawMessage `json:"openai"`
	}
	ant, oai := goldenUpstream(t, "anthropic"), goldenUpstream(t, "openai")
	all := map[string]pair{}
	for _, r := range egressRefusalSamples(ant) {
		all[r.Code] = pair{Status: r.Status,
			Anthropic: egressRefusalBody(ant, r.Status, r.Code, a2ashape.Sentence(r.Code, a2ashape.English, r.Args)),
			OpenAI:    egressRefusalBody(oai, r.Status, r.Code, a2ashape.Sentence(r.Code, a2ashape.English, r.Args))}
	}
	compareGolden(t, filepath.Join("testdata", "inject", "refusals.json"), all)
	for _, row := range a2ashape.Rows() {
		if row.Class&a2ashape.ClassModel != 0 && !row.NoSentence {
			if _, ok := all[row.Code]; !ok {
				t.Errorf("refusals.json lacks %s", row.Code)
			}
		}
	}
}

// goldenUpstream is an upstream of the golden files: the built-in services at their real addresses, and
// OpenRouter as a custom service.
func goldenUpstream(t *testing.T, name string) *egressUpstream {
	t.Helper()
	cfg := AutoReplyConfig{Backend: "exec", APIKey: "operator-key-golden"}
	switch name {
	case "anthropic":
		cfg.Agent = agentClaude
	case "openai":
		cfg.Agent = agentCodex
	default:
		cfg.Agent, cfg.APIBase, cfg.Model = agentOpenCode, "https://openrouter.ai/api/v1", "anthropic/claude-haiku-4.5"
	}
	saved := map[string]string{}
	for k, v := range egressBuiltinBase {
		saved[k] = v
	}
	egressBuiltinBase[egressUpstreamAnthropic], egressBuiltinBase[egressUpstreamOpenAI] = "https://api.anthropic.com", "https://api.openai.com"
	defer func() {
		for k, v := range saved {
			egressBuiltinBase[k] = v
		}
	}()
	up, err := resolveEgressUpstream(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return up
}

// egressRefusalSamples is one refusal of each code of ux §1.4 I that has a sentence, with its status.
func egressRefusalSamples(up *egressUpstream) []egressRefusal {
	return []egressRefusal{
		{403, "egress_host_not_allowed", a2ashape.Args{"host": "example.com"}},
		{403, "egress_port", nil},
		{403, "egress_private_address", a2ashape.Args{"host": "metadata.internal.example"}},
		{403, "egress_plain_http", nil},
		{407, "egress_unauthorized", nil},
		{429, "egress_tunnels", nil},
		{413, "egress_too_large", a2ashape.Args{"n": egressMaxBody >> 20}},
		{502, "egress_upstream_unreachable", nil},
		{403, "policy_server_tool", a2ashape.Args{"type": "web_search_20260209"}},
		{403, "policy_mcp_servers", nil},
		{403, "policy_field", a2ashape.Args{"field": "previous_response_id"}},
		{403, "policy_path", a2ashape.Args{"paths": up.pathsText()}},
		{400, "policy_encoding", nil},
		{429, "run_budget_requests", a2ashape.Args{"n": egressMaxRequests}},
		{429, "run_budget_upload", a2ashape.Args{"n": egressMaxInjectUpload >> 20}},
	}
}

// compareGolden compares v, encoded as indented JSON, with the file at p (or writes it under
// -update-inject).
func compareGolden(t *testing.T, p string, v any) {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	if *updateInjectGolden {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run go test -run %s -update-inject to create it)", err, t.Name())
	}
	if !bytes.Equal(want, b.Bytes()) {
		t.Errorf("%s differs from what the endpoint does; if the change is intended, rerun with -update-inject and review the diff\ngot:\n%s", p, b.Bytes())
	}
}

// --- F-BP ---

// fuzzInjectTargets are the routes FuzzInjectPolicy drives, with the path si57Body reads them by.
func fuzzInjectTargets(t testing.TB) []struct {
	up   *egressUpstream
	kind egressRouteKind
	rel  string
} {
	ant := testUpstreamAt(t, egressUpstreamAnthropic, "https://model.invalid")
	oai := testUpstreamAt(t, egressUpstreamOpenAI, "https://model.invalid")
	cus := testUpstreamAt(t, egressUpstreamCustom, "http://127.0.0.1:9")
	or := testUpstreamAt(t, egressUpstreamCustom, "http://127.0.0.1:9")
	or.OpenRouter = true
	return []struct {
		up   *egressUpstream
		kind egressRouteKind
		rel  string
	}{
		{ant, routeAnthropicMessages, "/v1/messages"},
		{ant, routeAnthropicCountTokens, "/v1/messages/count_tokens"},
		{oai, routeResponses, "/v1/responses"},
		{oai, routeChat, "/v1/chat/completions"},
		{cus, routeChat, "/chat/completions"},
		{or, routeChat, "/chat/completions"},
	}
}

// FuzzInjectPolicy (F-BP): for any body on any route the policy either refuses it, with a code of ux
// §1.4 I and its status, or forwards a body that passes the independent reading of SI-57 and that the
// policy leaves unchanged when applied again. Under plain go test it runs its seeds only.
func FuzzInjectPolicy(f *testing.F) {
	for i, c := range injectCorpus {
		if c.method == "POST" {
			f.Add(uint8(i), []byte(c.body))
		}
	}
	for _, c := range goldenInjectCases {
		f.Add(uint8(len(c.name)), []byte(c.body))
	}
	targets := fuzzInjectTargets(f)
	statusOf := map[string]int{"policy_encoding": 400, "policy_field": 403, "policy_server_tool": 403, "policy_mcp_servers": 403}
	f.Fuzz(func(t *testing.T, which uint8, body []byte) {
		tg := targets[int(which)%len(targets)]
		out, ref := rewriteInjectBody(tg.up, tg.kind, body)
		if ref != nil {
			if want, ok := statusOf[ref.Code]; !ok || want != ref.Status {
				t.Fatalf("refusal %d %s", ref.Status, ref.Code)
			}
			if msg := a2ashape.Sentence(ref.Code, a2ashape.English, ref.Args); !strings.HasPrefix(msg, "anet: ") {
				t.Fatalf("refusal sentence %q", msg)
			}
			return
		}
		if err := si57Body(tg.up, tg.rel, out); err != nil {
			t.Fatalf("%v\nin:  %q\nout: %q", err, body, out)
		}
		again, ref2 := rewriteInjectBody(tg.up, tg.kind, out)
		if ref2 != nil || !bytes.Equal(again, out) {
			t.Fatalf("not stable: %v\nout:   %q\nagain: %q", ref2, out, again)
		}
	})
}

// The corpus names are unique (they name subtests and golden files).
func TestInjectCorpusNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range append(append([]injectCase(nil), injectCorpus...), goldenInjectCases...) {
		if seen[c.name] {
			t.Errorf("duplicate case %s", c.name)
		}
		seen[c.name] = true
	}
}

package daemon

// egress_policy.go is what the injection route changes in, or refuses of, a request from the sandbox
// before it reaches the model service (LEC §4.3.4, BP-1 to BP-7; SI-57).
//
// The point of the route is that the agent in the sandbox can use the model and nothing more: not the
// model service's own network reach (server-side web search, fetch and code execution, MCP connectors,
// hosted conversations), not a more expensive model, not unbounded output, not the operator's stored
// conversations. So:
//
//   - The body must be one uncompressed JSON object (BP-1). It is decoded, changed and encoded again:
//     what the model service receives is exactly what was checked, so a member written twice, or with
//     escapes in its name, cannot be read one way here and another way there.
//   - model is always the pinned model (BP-2); output limits are at most 8192 tokens, and set when
//     missing (BP-3).
//   - Only tools the client executes pass (BP-4); the fields that reach out from the model service are
//     refused (BP-5); OpenAI requests are not stored and have no service tier and one choice.
//   - Credentials the agent sends are dropped and the operator's key is added (UP-4); beta headers keep
//     only listed values (BP-6).
//
// Members the policy does not know pass unchanged (core Q-LEC-8): a model service that adds a new way
// to reach out through a new top-level member is missed until anet learns it; the beta allowlist covers
// part of that (LEC §6.2, item 10).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// egressMaxOutputTokens bounds every output limit of a forwarded request (BP-3).
const egressMaxOutputTokens = 8192

// egressRefusal is a request the endpoint answers itself: the HTTP status, the code (ux §1.4 I) and the
// sentence's values.
type egressRefusal struct {
	Status int
	Code   string
	Args   a2ashape.Args
}

func refuseEgress(status int, code string, args a2ashape.Args) *egressRefusal {
	return &egressRefusal{Status: status, Code: code, Args: args}
}

// --- headers (UP-4, BP-6) ---

// egressDropRequestHeaders are never forwarded: every credential an agent can send (the operator's key
// is added instead), cookies, proxy and forwarding headers, and the framing the endpoint redoes itself.
var egressDropRequestHeaders = map[string]bool{
	"Authorization": true, "X-Api-Key": true, "Api-Key": true,
	"Proxy-Authorization": true, "Cookie": true, "Openai-Organization": true, "Openai-Project": true,
	"X-Goog-Api-Key": true, "Forwarded": true,
	"Host": true, "Content-Length": true, "Content-Encoding": true, "Accept-Encoding": true, "Expect": true,
}

// egressHopHeaders are the hop-by-hop headers (RFC 9110 §7.6.1), dropped both ways along with every
// header the Connection header names.
var egressHopHeaders = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

// egressDropResponseHeaders are dropped from the model service's answer (EG-6): cookies, and the
// operator's account identifiers, which the agent has no use for and could pass on to the peer.
var egressDropResponseHeaders = map[string]bool{
	"Set-Cookie": true, "Set-Cookie2": true,
	"Anthropic-Organization-Id": true, "Openai-Organization": true, "Openai-Project": true,
}

// egressAnthropicBetas are the anthropic-beta values forwarded (BP-6); the rest are dropped. None of
// them may begin with a prefix in egressForbiddenBetaPrefixes (the test asserts it).
var egressAnthropicBetas = map[string]bool{
	"claude-code-20250219":                   true,
	"interleaved-thinking-2025-05-14":        true,
	"fine-grained-tool-streaming-2025-05-14": true,
	"token-efficient-tools-2025-02-19":       true,
	"context-management-2025-06-27":          true,
	"prompt-caching-2024-07-31":              true,
	"extended-cache-ttl-2025-04-11":          true,
	"token-counting-2024-11-01":              true,
	"output-128k-2025-02-19":                 true,
	"computer-use-2024-10-22":                true,
	"computer-use-2025-01-24":                true,
}

// egressOpenAIBetas are the OpenAI-Beta values forwarded (BP-6).
var egressOpenAIBetas = map[string]bool{
	"responses=experimental": true,
}

// egressForbiddenBetaPrefixes start the beta values that give the model service network reach or
// hosted state of its own; no allowlist may hold one (BP-6).
var egressForbiddenBetaPrefixes = []string{"mcp-client", "code-execution", "files-api", "web-fetch", "web-search", "skills", "container"}

// connectionTokens are the header names a Connection header lists (each also hop-by-hop).
func connectionTokens(h http.Header) map[string]bool {
	out := map[string]bool{}
	for _, v := range h.Values("Connection") {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out[http.CanonicalHeaderKey(t)] = true
			}
		}
	}
	return out
}

// injectHeaders are the headers forwarded for a request from the sandbox: the agent's, without
// credentials, cookies, proxy, forwarding and hop-by-hop headers, with the beta headers filtered, and
// with the operator's key (UP-1, UP-4).
func injectHeaders(up *egressUpstream, in http.Header, hasBody bool) http.Header {
	out := http.Header{}
	named := connectionTokens(in)
	for k, vs := range in {
		ck := http.CanonicalHeaderKey(k)
		if egressDropRequestHeaders[ck] || egressHopHeaders[ck] || named[ck] || strings.HasPrefix(ck, "X-Forwarded-") {
			continue
		}
		out[ck] = append([]string(nil), vs...)
	}
	filterBetaHeader(out, "Anthropic-Beta", egressAnthropicBetas)
	filterBetaHeader(out, "Openai-Beta", egressOpenAIBetas)
	if hasBody {
		out.Set("Content-Type", "application/json")
	}
	// Uncompressed answers: the bytes counted are the bytes the agent reads, and a stream is passed on
	// as it arrives.
	out.Set("Accept-Encoding", "identity")
	switch {
	case up.Name == egressUpstreamAnthropic:
		out.Set("X-Api-Key", up.Key)
	case up.Key != "":
		out.Set("Authorization", "Bearer "+up.Key)
	}
	return out
}

// filterBetaHeader keeps the comma-separated values of header name that allow lists, and drops the
// header when none is left.
func filterBetaHeader(h http.Header, name string, allow map[string]bool) {
	vs := h.Values(name)
	if len(vs) == 0 {
		return
	}
	var keep []string
	for _, v := range vs {
		for _, b := range strings.Split(v, ",") {
			if b = strings.TrimSpace(b); allow[b] {
				keep = append(keep, b)
			}
		}
	}
	h.Del(name)
	if len(keep) > 0 {
		h.Set(name, strings.Join(keep, ","))
	}
}

// filterResponseHeaders copies the model service's answer headers that reach the agent (EG-6).
func filterResponseHeaders(dst, src http.Header) {
	named := connectionTokens(src)
	for k, vs := range src {
		ck := http.CanonicalHeaderKey(k)
		if egressHopHeaders[ck] || named[ck] || egressDropResponseHeaders[ck] {
			continue
		}
		dst[ck] = append([]string(nil), vs...)
	}
}

// --- body (BP-1 to BP-5) ---

// contentEncodingOK reports a request body that is not compressed (BP-1): no Content-Encoding, or
// identity.
func contentEncodingOK(h http.Header) bool {
	for _, v := range h.Values("Content-Encoding") {
		for _, e := range strings.Split(v, ",") {
			if e = strings.TrimSpace(e); e != "" && !strings.EqualFold(e, "identity") {
				return false
			}
		}
	}
	return true
}

// rewriteInjectBody applies the body policy of a route to a POST body and returns the body to forward,
// or the refusal.
func rewriteInjectBody(up *egressUpstream, kind egressRouteKind, body []byte) ([]byte, *egressRefusal) {
	obj, ok := decodeJSONObject(body)
	if !ok {
		return nil, refuseEgress(http.StatusBadRequest, "policy_encoding", nil)
	}
	var r *egressRefusal
	switch kind {
	case routeAnthropicMessages, routeAnthropicCountTokens:
		r = anthropicBodyPolicy(obj, kind == routeAnthropicMessages)
	case routeResponses:
		r = responsesBodyPolicy(obj)
	case routeChat:
		r = chatBodyPolicy(obj, up)
	default:
		return nil, refuseEgress(http.StatusForbidden, "policy_path", a2ashape.Args{"paths": up.pathsText()})
	}
	if r != nil {
		return nil, r
	}
	obj["model"] = up.Model // BP-2: always the pinned model, never refused
	out, err := encodeJSONObject(obj)
	if err != nil {
		return nil, refuseEgress(http.StatusBadRequest, "policy_encoding", nil)
	}
	return out, nil
}

// decodeJSONObject decodes one JSON object and nothing after it. Numbers stay as written.
func decodeJSONObject(body []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return obj, true
}

func encodeJSONObject(obj map[string]any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// present reports a member that is there and not null. A null member is removed: it means the same as
// an absent one, and nothing reaches the model service that was not checked.
func presentMember(obj map[string]any, k string) bool {
	v, ok := obj[k]
	if ok && v == nil {
		delete(obj, k)
		return false
	}
	return ok
}

// refuseFields refuses the first of fields present (BP-5).
func refuseFields(obj map[string]any, fields ...string) *egressRefusal {
	for _, f := range fields {
		if presentMember(obj, f) {
			return refuseEgress(http.StatusForbidden, "policy_field", a2ashape.Args{"field": f})
		}
	}
	return nil
}

func anthropicBodyPolicy(obj map[string]any, messages bool) *egressRefusal {
	if presentMember(obj, "mcp_servers") {
		return refuseEgress(http.StatusForbidden, "policy_mcp_servers", nil)
	}
	if r := refuseFields(obj, "container"); r != nil {
		return r
	}
	if r := checkTools(obj, "tools", anthropicClientTool); r != nil {
		return r
	}
	// count_tokens generates nothing and takes no max_tokens: one that is there is bounded, none added.
	maxTokens := clampOutputLimit(obj, "max_tokens", messages)
	if th, ok := obj["thinking"].(map[string]any); ok && maxTokens > 0 {
		if _, has := th["budget_tokens"]; has {
			// The thinking budget must stay below max_tokens for the request to be valid.
			if b, ok := jsonInteger(th["budget_tokens"]); !ok || b >= maxTokens {
				th["budget_tokens"] = json.Number(strconv.FormatInt(max(1024, maxTokens/2), 10))
			}
		}
	}
	return nil
}

func responsesBodyPolicy(obj map[string]any) *egressRefusal {
	if v, ok := obj["background"]; ok && v != false {
		if v == nil {
			delete(obj, "background")
		} else {
			return refuseEgress(http.StatusForbidden, "policy_field", a2ashape.Args{"field": "background"})
		}
	}
	if r := refuseFields(obj, "conversation", "prompt", "previous_response_id"); r != nil {
		return r
	}
	if r := checkTools(obj, "tools", responsesClientTool); r != nil {
		return r
	}
	if r := checkToolChoice(obj, responsesClientTool); r != nil {
		return r
	}
	openAIStorageRules(obj)
	clampOutputLimit(obj, "max_output_tokens", true)
	return nil
}

func chatBodyPolicy(obj map[string]any, up *egressUpstream) *egressRefusal {
	if r := refuseFields(obj, "web_search_options"); r != nil {
		return r
	}
	if up.OpenRouter {
		if r := refuseFields(obj, "plugins", "models"); r != nil {
			return r
		}
	}
	if r := checkTools(obj, "tools", chatClientTool); r != nil {
		return r
	}
	if r := checkToolChoice(obj, chatClientTool); r != nil {
		return r
	}
	openAIStorageRules(obj)
	if presentMember(obj, "n") {
		obj["n"] = json.Number("1")
	}
	a := clampOutputLimit(obj, "max_tokens", false)
	b := clampOutputLimit(obj, "max_completion_tokens", false)
	if a == 0 && b == 0 {
		// Neither is set: add the one the service reads (OpenAI's newer models take only
		// max_completion_tokens; OpenAI-compatible services take max_tokens).
		if up.Name == egressUpstreamOpenAI {
			obj["max_completion_tokens"] = json.Number(strconv.Itoa(egressMaxOutputTokens))
		} else {
			obj["max_tokens"] = json.Number(strconv.Itoa(egressMaxOutputTokens))
		}
	}
	return nil
}

// openAIStorageRules: an OpenAI-dialect request is never stored by the service and has no service tier
// (BP-5).
func openAIStorageRules(obj map[string]any) {
	obj["store"] = false
	delete(obj, "service_tier")
}

// clampOutputLimit bounds output limit k to egressMaxOutputTokens (BP-3): a decimal integer from 1 to the
// bound stays; anything else present (larger, zero or negative, which a lenient OpenAI-compatible
// service such as a local Ollama or llama.cpp reads as "no limit", a fraction, a string a lenient
// service would read as a number) becomes the bound; absent, it is added when add is set. It returns the
// value the request now carries, 0 when it carries none.
func clampOutputLimit(obj map[string]any, k string, add bool) int64 {
	if !presentMember(obj, k) {
		if !add {
			return 0
		}
		obj[k] = json.Number(strconv.Itoa(egressMaxOutputTokens))
		return egressMaxOutputTokens
	}
	if n, ok := jsonInteger(obj[k]); ok && n >= 1 && n <= egressMaxOutputTokens {
		return n
	}
	obj[k] = json.Number(strconv.Itoa(egressMaxOutputTokens))
	return egressMaxOutputTokens
}

// jsonInteger is a JSON number written as an unsigned decimal integer of at most 9 digits.
func jsonInteger(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	s := string(n)
	if s == "" || len(s) > 9 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	i, err := strconv.ParseInt(s, 10, 64)
	return i, err == nil
}

// toolTypeOK says whether a tool definition is one the client executes, and names its type for the
// refusal when it is not.
type toolTypeOK func(tool map[string]any) (typ string, ok bool)

// checkTools refuses a tools member that is not a list of objects (policy_field) or that holds a tool
// the model service would run itself (policy_server_tool, BP-4).
func checkTools(obj map[string]any, k string, ok toolTypeOK) *egressRefusal {
	if !presentMember(obj, k) {
		return nil
	}
	list, isList := obj[k].([]any)
	if !isList {
		return refuseEgress(http.StatusForbidden, "policy_field", a2ashape.Args{"field": k})
	}
	for _, t := range list {
		m, isObj := t.(map[string]any)
		if !isObj {
			return refuseEgress(http.StatusForbidden, "policy_field", a2ashape.Args{"field": k})
		}
		if typ, allowed := ok(m); !allowed {
			return refuseEgress(http.StatusForbidden, "policy_server_tool", a2ashape.Args{"type": egressArg(typ)})
		}
	}
	return nil
}

// checkToolChoice refuses a tool_choice that names a tool the model service runs itself (an OpenAI
// hosted tool can be chosen without being listed in tools). The string modes pass.
func checkToolChoice(obj map[string]any, ok toolTypeOK) *egressRefusal {
	if !presentMember(obj, "tool_choice") {
		return nil
	}
	m, isObj := obj["tool_choice"].(map[string]any)
	if !isObj {
		if _, isStr := obj["tool_choice"].(string); isStr {
			return nil
		}
		return refuseEgress(http.StatusForbidden, "policy_field", a2ashape.Args{"field": "tool_choice"})
	}
	if typ, _ := m["type"].(string); typ == "allowed_tools" {
		// Responses lists them in tools; chat completions in allowed_tools.tools.
		if r := checkTools(m, "tools", ok); r != nil {
			return r
		}
		if inner, isObj := m["allowed_tools"].(map[string]any); isObj {
			return checkTools(inner, "tools", ok)
		}
		return nil
	}
	if typ, allowed := ok(m); !allowed {
		return refuseEgress(http.StatusForbidden, "policy_server_tool", a2ashape.Args{"type": egressArg(typ)})
	}
	return nil
}

// toolType is the type member of a tool, "" when absent, its JSON text when not a string.
func toolType(m map[string]any) (string, bool) {
	v, has := m["type"]
	if !has || v == nil {
		return "", false
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	b, _ := json.Marshal(v)
	return string(b), true
}

// anthropicClientTool: a tool with no type or type custom (defined by the caller), or one of the
// Anthropic-defined tools the client executes (bash_, text_editor_, computer_, memory_). Everything else,
// web search, web fetch, code execution, MCP toolsets and any type not known here, runs on the model
// service.
func anthropicClientTool(m map[string]any) (string, bool) {
	typ, has := toolType(m)
	if !has || typ == "custom" {
		return typ, true
	}
	for _, p := range []string{"bash_", "text_editor_", "computer_", "memory_"} {
		if strings.HasPrefix(typ, p) {
			return typ, true
		}
	}
	return typ, false
}

// egressResponsesClientTools are the OpenAI Responses tool types the client executes (BP-4); the
// per-agent trials add what each agent needs.
var egressResponsesClientTools = map[string]bool{"function": true, "custom": true, "local_shell": true}

func responsesClientTool(m map[string]any) (string, bool) {
	typ, _ := toolType(m)
	return typ, egressResponsesClientTools[typ]
}

// chatClientTool: chat completions pass function tools only.
func chatClientTool(m map[string]any) (string, bool) {
	typ, _ := toolType(m)
	return typ, typ == "function"
}

// --- refusal bodies (LEC §2.6.4) ---

// egressRefusalBody is a refusal in the dialect of the run's upstream, so that the agent's client reads
// it as the model service's own error: the message is the sentence, which begins "anet: ". The error
// type is the dialect's for a request it refuses (permission_error, invalid_request_error), or, for a
// status of 500 and over (the model service could not be reached), its type for a failure on the
// service's side (api_error, server_error), which a client retries.
func egressRefusalBody(up *egressUpstream, status int, code, msg string) []byte {
	var v any
	if up != nil && up.Name == egressUpstreamAnthropic {
		typ := "permission_error"
		if status >= 500 {
			typ = "api_error"
		}
		v = map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}}
	} else {
		typ := "invalid_request_error"
		if status >= 500 {
			typ = "server_error"
		}
		v = map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}}
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("egress refusal body: %v", err))
	}
	return b
}

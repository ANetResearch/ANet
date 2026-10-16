package autoreply

// Test helpers for the sandbox egress endpoint: a fake model service that records what it receives, an
// endpoint on a Unix socket with a client for it, and an independent reading of SI-57 that every request
// reaching the fake service is checked against.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The operator's key in these tests. Never sent by the agent side, so finding it anywhere the agent can
// see is a failure.
const testOperatorKey = "operator-key-7d1e"

// testAgentKey is what an agent in the sandbox might send as its own key.
const testAgentKey = "agent-key-attacker-55aa"

// receivedRequest is one request as the fake model service received it.
type receivedRequest struct {
	Method, Path, RawQuery string
	URL                    string // the whole address (recordingTransport only)
	Header                 http.Header
	Body                   []byte
}

// fakeModelService records every request and answers with handle (default: 200 {"ok":true}).
type fakeModelService struct {
	srv    *httptest.Server
	mu     sync.Mutex
	got    []receivedRequest
	handle func(w http.ResponseWriter, r *http.Request, body []byte)
}

func newFakeModelService(t testing.TB) *fakeModelService {
	f := &fakeModelService{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.got = append(f.got, receivedRequest{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
		h := f.handle
		f.mu.Unlock()
		if h != nil {
			h(w, r, body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeModelService) requests() []receivedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]receivedRequest(nil), f.got...)
}

func (f *fakeModelService) reset() {
	f.mu.Lock()
	f.got = nil
	f.mu.Unlock()
}

// testUpstream is an upstream named name whose address is the fake service (for custom: its /v1).
func testUpstream(t testing.TB, name string, f *fakeModelService) *egressUpstream {
	t.Helper()
	return testUpstreamAt(t, name, f.srv.URL)
}

// testUpstreamAt is an upstream named name at base (for custom: base/v1).
func testUpstreamAt(t testing.TB, name, base string) *egressUpstream {
	t.Helper()
	switch name {
	case egressUpstreamAnthropic, egressUpstreamOpenAI:
		old := egressBuiltinBase[name]
		egressBuiltinBase[name] = base
		t.Cleanup(func() { egressBuiltinBase[name] = old })
	}
	cfg := AutoReplyConfig{Backend: "exec", APIKey: testOperatorKey}
	switch name {
	case egressUpstreamAnthropic:
		cfg.Agent = agentClaude
	case egressUpstreamOpenAI:
		cfg.Agent = agentCodex
	default:
		cfg.Agent, cfg.APIBase, cfg.Model = agentOpenCode, base+"/v1", "local-model"
	}
	up, err := resolveEgressUpstream(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return up
}

// startTestEgress serves an endpoint for up on a Unix socket and returns it with a client that reaches
// it as the agent in the sandbox does (requests to http://127.0.0.1:3128/...).
func startTestEgress(t testing.TB, up *egressUpstream) (*egressEndpoint, *http.Client, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "anet-"+"egt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "e.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ep := newEgressEndpoint(up)
	ep.Serve(ln)
	t.Cleanup(ep.Close)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		DisableCompression: true,
		MaxConnsPerHost:    64,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return ep, &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, sock
}

// routeURL is the injection route of upstream name for path p, as the agent writes it.
func routeURL(name, p string) string {
	return "http://" + sandboxBridgeListen + egressRoutePrefix + name + p
}

// doJSON sends one request with body and headers h and returns the status and body.
func doJSON(t testing.TB, c *http.Client, method, url string, h map[string]string, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// refusalMessage reads a refusal body in either dialect and returns its message and (openai) code.
func refusalMessage(body []byte) (msg, code string, ok bool) {
	var a struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &a) != nil || a.Error.Message == "" {
		return "", "", false
	}
	return a.Error.Message, a.Error.Code, true
}

// --- SI-57, read independently of egress_policy.go ---

var si57Integer = regexp.MustCompile(`^[0-9]{1,9}$`)

// si57Paths are the paths each upstream may be called on, relative to its address.
var si57Paths = map[string][]string{
	egressUpstreamAnthropic: {"POST /v1/messages", "POST /v1/messages/count_tokens", "GET /v1/models", "GET /v1/models/"},
	egressUpstreamOpenAI:    {"POST /v1/responses", "POST /v1/chat/completions", "GET /v1/models", "GET /v1/models/"},
	egressUpstreamCustom:    {"POST /chat/completions", "GET /models"},
}

// si57Check reads one request as the model service received it against SI-57: the path is allowed;
// only the operator's key, once, and no other credential, cookie or forwarding header; no beta value
// that reaches out; the model is P; every output limit from 1 to 8192 (zero or negative is "no limit"
// to some OpenAI-compatible services) and the main one present; no tool the service runs itself, no
// MCP connector, no hosted state, no media the service would fetch from a URL; OpenAI requests not
// stored.
func si57Check(up *egressUpstream, r receivedRequest) error {
	rel := strings.TrimPrefix(r.Path, strings.TrimRight(up.Base.Path, "/"))
	allowed := false
	for _, mp := range si57Paths[up.Name] {
		m, p, _ := strings.Cut(mp, " ")
		if m != r.Method {
			continue
		}
		if strings.HasSuffix(p, "/") {
			if id, ok := strings.CutPrefix(rel, p); ok && id != "" && !strings.ContainsAny(id, "/") {
				allowed = true
			}
		} else if rel == p {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("path %s %s is not allowed for %s", r.Method, r.Path, up.Name)
	}
	h := r.Header
	if up.Name == egressUpstreamAnthropic {
		if v := h.Values("X-Api-Key"); len(v) != 1 || v[0] != up.Key {
			return fmt.Errorf("x-api-key %q, want only the operator's", v)
		}
		if v := h.Values("Authorization"); len(v) != 0 {
			return fmt.Errorf("authorization %q reached the service", v)
		}
	} else {
		want := []string(nil)
		if up.Key != "" {
			want = []string{"Bearer " + up.Key}
		}
		if v := h.Values("Authorization"); fmt.Sprint(v) != fmt.Sprint(want) {
			return fmt.Errorf("authorization %q, want %q", v, want)
		}
		if v := h.Values("X-Api-Key"); len(v) != 0 {
			return fmt.Errorf("x-api-key %q reached the service", v)
		}
	}
	for k := range h {
		ck := http.CanonicalHeaderKey(k)
		switch {
		case ck == "Api-Key", ck == "X-Goog-Api-Key", ck == "Cookie", ck == "Proxy-Authorization", ck == "Forwarded",
			ck == "Openai-Organization", ck == "Openai-Project", strings.HasPrefix(ck, "X-Forwarded-"):
			return fmt.Errorf("header %s reached the service", ck)
		}
	}
	for _, name := range []string{"Anthropic-Beta", "Openai-Beta"} {
		for _, v := range h.Values(name) {
			for _, b := range strings.Split(v, ",") {
				for _, p := range []string{"mcp-client", "code-execution", "files-api", "web-fetch", "web-search", "skills", "container"} {
					if strings.HasPrefix(strings.TrimSpace(b), p) {
						return fmt.Errorf("%s %q reached the service", name, b)
					}
				}
			}
		}
	}
	if r.Method != http.MethodPost {
		return nil
	}
	return si57Body(up, rel, r.Body)
}

// si57Body checks a forwarded body; rel is the path relative to the upstream's address.
func si57Body(up *egressUpstream, rel string, body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil || dec.More() {
		return fmt.Errorf("body is not one JSON object: %q", body)
	}
	if obj["model"] != up.Model {
		return fmt.Errorf("model %v, want %s", obj["model"], up.Model)
	}
	limit := func(k string, required bool) (int64, error) {
		v, ok := obj[k]
		if !ok {
			if required {
				return 0, fmt.Errorf("%s missing", k)
			}
			return 0, nil
		}
		n, isNum := v.(json.Number)
		if !isNum || !si57Integer.MatchString(string(n)) {
			return 0, fmt.Errorf("%s = %v is not an integer", k, v)
		}
		i, _ := strconv.ParseInt(string(n), 10, 64)
		if i < 1 || i > 8192 {
			return 0, fmt.Errorf("%s = %d is not from 1 to 8192", k, i)
		}
		return i, nil
	}
	absent := func(keys ...string) error {
		for _, k := range keys {
			if _, ok := obj[k]; ok {
				return fmt.Errorf("member %s reached the service", k)
			}
		}
		return nil
	}
	toolTypes := func(list any) ([]string, error) {
		if list == nil {
			return nil, nil
		}
		l, ok := list.([]any)
		if !ok {
			return nil, fmt.Errorf("tools is not a list")
		}
		var out []string
		for _, t := range l {
			m, ok := t.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("a tool is not an object")
			}
			typ, has := m["type"]
			s, isStr := typ.(string)
			switch {
			case !has:
				out = append(out, "")
			case !isStr:
				return nil, fmt.Errorf("tool type %v", typ)
			default:
				out = append(out, s)
			}
		}
		return out, nil
	}
	openAIRules := func() error {
		if obj["store"] != false {
			return fmt.Errorf("store = %v", obj["store"])
		}
		if err := absent("service_tier"); err != nil {
			return err
		}
		return nil
	}
	switch {
	case up.Name == egressUpstreamAnthropic:
		mt, err := limit("max_tokens", rel == "/v1/messages")
		if err != nil {
			return err
		}
		if err := absent("mcp_servers", "container"); err != nil {
			return err
		}
		// The thinking budget is an integer below max_tokens. The service's smallest budget is 1024, so
		// with max_tokens of 1024 or less no budget is valid and the request has no thinking at all.
		if th, has := obj["thinking"]; has && mt > 0 && mt <= 1024 {
			return fmt.Errorf("thinking %v with max_tokens %d", th, mt)
		}
		if th, ok := obj["thinking"].(map[string]any); ok && mt > 0 {
			if b, has := th["budget_tokens"]; has {
				n, _ := b.(json.Number)
				i, err := strconv.ParseInt(string(n), 10, 64)
				if err != nil || i >= mt {
					return fmt.Errorf("thinking budget %v not below max_tokens %d", b, mt)
				}
			}
		}
		if u := si57AnthropicURLMedia(obj); u != "" {
			return fmt.Errorf("media by URL reached the service: %s", u)
		}
		types, err := toolTypes(obj["tools"])
		if err != nil {
			return err
		}
		for _, typ := range types {
			// computer use by its versions only (EXE-6); the other client tool families by prefix
			ok := typ == "" || typ == "custom" || typ == "computer_20241022" || typ == "computer_20250124" || typ == "computer_20251124"
			for _, p := range []string{"bash_", "text_editor_", "memory_"} {
				ok = ok || strings.HasPrefix(typ, p)
			}
			if !ok {
				return fmt.Errorf("anthropic tool type %q reached the service", typ)
			}
		}
	case rel == "/v1/responses":
		if _, err := limit("max_output_tokens", true); err != nil {
			return err
		}
		if b, ok := obj["background"]; ok && b != false {
			return fmt.Errorf("background = %v", b)
		}
		if err := absent("conversation", "prompt", "previous_response_id"); err != nil {
			return err
		}
		if u := si57URLMedia(obj["input"], "input_image"); u != "" {
			return fmt.Errorf("media by URL reached the service: %s", u)
		}
		if err := openAIRules(); err != nil {
			return err
		}
		// Every tool the request declares — in tools, in a namespace (codex 0.162), or in a list named tools
		// anywhere in the input (codex's additional_tools item) — is one the client executes.
		var declared []any
		if obj["tools"] != nil {
			l, ok := obj["tools"].([]any)
			if !ok {
				return fmt.Errorf("tools is not a list")
			}
			declared = append(declared, l...)
		}
		var inInput func(v any)
		inInput = func(v any) {
			switch x := v.(type) {
			case []any:
				for _, e := range x {
					inInput(e)
				}
			case map[string]any:
				for k, e := range x {
					if l, ok := e.([]any); ok && k == "tools" {
						declared = append(declared, l...)
						continue
					}
					inInput(e)
				}
			}
		}
		inInput(obj["input"])
		for len(declared) > 0 {
			t := declared[0]
			declared = declared[1:]
			m, ok := t.(map[string]any)
			if !ok {
				return fmt.Errorf("a declared tool is not an object")
			}
			typ, _ := m["type"].(string)
			if typ == "namespace" {
				l, ok := m["tools"].([]any)
				if !ok {
					return fmt.Errorf("a namespace without a list of tools reached the service")
				}
				declared = append(declared, l...)
				continue
			}
			if typ != "function" && typ != "custom" && typ != "local_shell" {
				return fmt.Errorf("responses tool type %q reached the service", typ)
			}
		}
		if tc, ok := obj["tool_choice"].(map[string]any); ok {
			typ, _ := tc["type"].(string)
			if typ != "function" && typ != "custom" && typ != "local_shell" && typ != "allowed_tools" {
				return fmt.Errorf("tool_choice %q reached the service", typ)
			}
			inner, err := toolTypes(tc["tools"])
			if err != nil {
				return err
			}
			for _, it := range inner {
				if it != "function" && it != "custom" && it != "local_shell" {
					return fmt.Errorf("allowed tool type %q reached the service", it)
				}
			}
		}
	default: // chat completions
		a, err := limit("max_tokens", false)
		if err != nil {
			return err
		}
		b, err := limit("max_completion_tokens", false)
		if err != nil {
			return err
		}
		_, hasA := obj["max_tokens"]
		_, hasB := obj["max_completion_tokens"]
		if !hasA && !hasB {
			return fmt.Errorf("no output limit (max_tokens %d, max_completion_tokens %d)", a, b)
		}
		if err := absent("web_search_options"); err != nil {
			return err
		}
		if u := si57URLMedia(obj["messages"], "image_url"); u != "" {
			return fmt.Errorf("media by URL reached the service: %s", u)
		}
		if up.OpenRouter {
			if err := absent("plugins", "models"); err != nil {
				return err
			}
		}
		if err := openAIRules(); err != nil {
			return err
		}
		if n, ok := obj["n"]; ok && fmt.Sprint(n) != "1" {
			return fmt.Errorf("n = %v", n)
		}
		types, err := toolTypes(obj["tools"])
		if err != nil {
			return err
		}
		for _, typ := range types {
			if typ != "function" {
				return fmt.Errorf("chat tool type %q reached the service", typ)
			}
		}
		if tc, ok := obj["tool_choice"].(map[string]any); ok {
			typ, _ := tc["type"].(string)
			if typ != "function" && typ != "allowed_tools" {
				return fmt.Errorf("tool_choice %q reached the service", typ)
			}
			listed := tc["tools"]
			if at, ok := tc["allowed_tools"].(map[string]any); ok && listed == nil {
				listed = at["tools"]
			}
			inner, err := toolTypes(listed)
			if err != nil {
				return err
			}
			for _, it := range inner {
				if it != "function" {
					return fmt.Errorf("allowed tool type %q reached the service", it)
				}
			}
		}
	}
	return nil
}

// si57AnthropicURLMedia finds an Anthropic content block — in system, in a message, in a tool result or
// a document's content, at any depth — whose source is a URL the service would fetch (BP-5).
func si57AnthropicURLMedia(obj map[string]any) string {
	var blocks func(v any) string
	blocks = func(v any) string {
		l, _ := v.([]any)
		for _, b := range l {
			m, _ := b.(map[string]any)
			src, _ := m["source"].(map[string]any)
			if src != nil && src["type"] == "url" {
				return fmt.Sprint(src["url"], " ")
			}
			for _, inner := range []any{m["content"], src["content"]} {
				if u := blocks(inner); u != "" {
					return u
				}
			}
		}
		return ""
	}
	if u := blocks(obj["system"]); u != "" {
		return u
	}
	msgs, _ := obj["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if u := blocks(mm["content"]); u != "" {
			return u
		}
	}
	return ""
}

// si57URLMedia looks through v, everywhere, for media the model service would fetch (BP-5), and returns
// its URL, or "" when there is none. kind names the dialect: "input_image" (Responses: an input_image
// with an image_url that is not data:, or any file_url of an input_file), "image_url" (chat: any
// object with an image_url, video_url or audio_url whose URL is not data:, or a file whose file_data is
// a URL).
func si57URLMedia(v any, kind string) string {
	notData := func(u any) bool {
		s, ok := u.(string)
		return !ok || len(s) < 5 || !strings.EqualFold(s[:5], "data:")
	}
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if u := si57URLMedia(e, kind); u != "" {
				return u
			}
		}
	case map[string]any:
		switch kind {
		case "input_image":
			if x["type"] == "input_image" && x["image_url"] != nil && notData(x["image_url"]) {
				return fmt.Sprint(x["image_url"], " ")
			}
			if x["type"] == "input_file" && x["file_url"] != nil {
				return fmt.Sprint(x["file_url"], " ")
			}
		case "image_url":
			// Whatever the part's type: a compatible server may go by the member alone.
			for _, k := range []string{"image_url", "video_url", "audio_url"} {
				u := x[k]
				if u == nil {
					continue
				}
				if m, ok := u.(map[string]any); ok {
					u = m["url"]
				}
				if notData(u) {
					return fmt.Sprint(u, " ")
				}
			}
			// A file's file_data is base64, which has no colon, or a data: URL.
			if f, ok := x["file"].(map[string]any); ok {
				if s, ok := f["file_data"].(string); ok && strings.ContainsRune(s, ':') && notData(s) {
					return s
				}
			}
		}
		for _, e := range x {
			if u := si57URLMedia(e, kind); u != "" {
				return u
			}
		}
	}
	return ""
}

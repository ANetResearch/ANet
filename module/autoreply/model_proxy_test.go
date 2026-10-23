package autoreply

// model_proxy_test.go — auto_reply.model_proxy (design v0.3.9 §D): the upstream's key added and the run's
// key dropped, unknown keys refused with 401, a key valid only while its run is under way, the request
// policy and the per-run and admitted budgets.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/module"
)

// fakeUpstream is a model service that records what it received and answers a fixed completion.
type fakeUpstream struct {
	mu   sync.Mutex
	reqs []fakeUpstreamReq
	srv  *httptest.Server
}

type fakeUpstreamReq struct {
	Path   string
	Header http.Header
	Body   map[string]any
}

func newFakeUpstream(t testing.TB, content string) *fakeUpstream {
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.reqs = append(f.reqs, fakeUpstreamReq{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "upstream=1")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": content}}}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) received() []fakeUpstreamReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeUpstreamReq(nil), f.reqs...)
}

// writeKeyFile writes a key file and returns its path.
func writeKeyFile(t testing.TB, key string) string {
	p := filepath.Join(t.TempDir(), "upstream.key")
	if err := os.WriteFile(p, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// newTestProxy is a model proxy on a free loopback port with one upstream "plan" at up, keyed with key
// and pinned to model when it is set.
func newTestProxy(t testing.TB, up *fakeUpstream, key, model string) *modelProxy {
	t.Helper()
	p, err := newModelProxy(AutoReplyModelProxyConfig{Listen: "127.0.0.1:0", Upstreams: []ModelProxyUpstream{
		{Name: "plan", APIBase: up.srv.URL + "/v1", APIKeyFile: writeKeyFile(t, key), Model: model}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// post sends a chat completion to the proxy with the credential given, and returns the status and body.
func post(t testing.TB, url, cred, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "agent-cookie=1")
	req.Header.Set("OpenAI-Organization", "org-agent")
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func errorCode(b []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	return e.Error.Code
}

const chatBody = `{"model":"agent-model","messages":[{"role":"user","content":"hi"}]}`

// The proxy forwards a run's request with the upstream's key in place of the run's, drops the agent's
// other credentials and the upstream's cookies, pins the model and bounds the output; the key is in no log.
func TestModelProxyInjectsTheUpstreamKey(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	up := newFakeUpstream(t, "hello from upstream")
	p := newTestProxy(t, up, "upstream-secret-1", "pinned-model")
	run, err := p.begin(proxyRunSpec{Peer: "aidA", Model: true, MaxRequests: 10, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer run.end()
	status, body := post(t, p.modelBase()+"/plan/chat/completions", run.key, chatBody)
	if status != 200 || !strings.Contains(string(body), "hello from upstream") {
		t.Fatalf("%d %s", status, body)
	}
	got := up.received()
	if len(got) != 1 {
		t.Fatalf("%d requests upstream", len(got))
	}
	r := got[0]
	if r.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer upstream-secret-1" {
		t.Errorf("forwarded %s with Authorization %q", r.Path, r.Header.Get("Authorization"))
	}
	if r.Header.Get("Cookie") != "" || r.Header.Get("Openai-Organization") != "" {
		t.Errorf("the agent's credentials were forwarded: %v", r.Header)
	}
	if r.Body["model"] != "pinned-model" || r.Body["max_tokens"] == nil {
		t.Errorf("forwarded body %v", r.Body)
	}
	for _, s := range []string{"upstream-secret-1", run.key} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("a key is in the log: %s", logs.String())
		}
	}
	for _, kv := range run.env() {
		if strings.Contains(kv, "upstream-secret-1") {
			t.Errorf("the run's environment holds the upstream's key: %s", kv)
		}
	}
}

// Without a key of a run under way a request is refused with 401 in the model service's error format, and
// nothing reaches the upstream: no key, a made-up key, a turn token, a key of a run that ended or whose
// time is past.
func TestModelProxyRefusesKeysNotUnderWay(t *testing.T) {
	quietLog(t)
	up := newFakeUpstream(t, "x")
	p := newTestProxy(t, up, "upstream-secret-2", "")
	url := p.modelBase() + "/plan/chat/completions"
	live, _ := p.begin(proxyRunSpec{Peer: "aidA", Model: true, Turn: true, MaxRequests: 10, Timeout: time.Minute})
	ended, _ := p.begin(proxyRunSpec{Peer: "aidA", Model: true, MaxRequests: 10, Timeout: time.Minute})
	ended.end()
	stale, _ := p.begin(proxyRunSpec{Peer: "aidA", Model: true, MaxRequests: 10, Timeout: -2 * time.Minute})
	defer live.end()
	defer stale.end()
	for name, cred := range map[string]string{"none": "", "made up": "mpk-" + strings.Repeat("0", 64),
		"turn token": live.token, "ended run": ended.key, "past its time": stale.key, "upstream key": "upstream-secret-2"} {
		status, body := post(t, url, cred, chatBody)
		if status != http.StatusUnauthorized || errorCode(body) != "model_proxy_unauthorized" {
			t.Errorf("%s: %d %s", name, status, body)
		}
	}
	if n := len(up.received()); n != 0 {
		t.Errorf("%d requests reached the upstream", n)
	}
	if status, _ := post(t, url, live.key, chatBody); status != 200 {
		t.Errorf("the live run's key: %d", status)
	}
}

// A run makes at most its requests; the request policy of the sandbox's route applies, in the proxy's
// words; an upstream the configuration does not name is refused.
func TestModelProxyLimitsAndPolicy(t *testing.T) {
	quietLog(t)
	up := newFakeUpstream(t, "x")
	p := newTestProxy(t, up, "k", "")
	run, _ := p.begin(proxyRunSpec{Peer: "aidA", Model: true, MaxRequests: 2, Timeout: time.Minute})
	defer run.end()
	url := p.modelBase() + "/plan/chat/completions"
	status, body := post(t, url, run.key, `{"model":"m","messages":[],"tools":[{"type":"web_search"}]}`)
	if status != http.StatusForbidden || errorCode(body) != "policy_server_tool" || !strings.Contains(string(body), "model proxy") {
		t.Errorf("a server-side tool: %d %s", status, body)
	}
	if status, body := post(t, p.modelBase()+"/other/chat/completions", run.key, chatBody); status != http.StatusForbidden || errorCode(body) != "policy_path" {
		t.Errorf("an upstream not configured: %d %s", status, body)
	}
	for i := 0; i < 2; i++ {
		if status, _ := post(t, url, run.key, chatBody); status != 200 {
			t.Fatalf("request %d: %d", i+1, status)
		}
	}
	status, body = post(t, url, run.key, chatBody)
	if status != http.StatusTooManyRequests || errorCode(body) != "run_budget_requests" || !strings.Contains(string(body), "its 2 model requests") {
		t.Errorf("the third request of a run of two: %d %s", status, body)
	}
}

// A key file that cannot be read stops the proxy from starting, with the key of the configuration named.
func TestModelProxyKeyFileMissing(t *testing.T) {
	quietLog(t)
	_, err := newModelProxy(AutoReplyModelProxyConfig{Listen: "127.0.0.1:0", Upstreams: []ModelProxyUpstream{
		{Name: "plan", APIBase: "https://ark.example/v3", APIKeyFile: filepath.Join(t.TempDir(), "absent")}}})
	if err == nil || !strings.Contains(err.Error(), "auto_reply.model_proxy.upstreams[0].api_key_file") {
		t.Fatalf("got %v", err)
	}
}

// The admitted budget: an admitted peer's runs take their place in the day's counts, a run past them is
// given a key whose every request is model_budget_spent, a trusted peer's runs are not counted, and the
// counts are rebuilt from the evidence chain after a restart.
func TestAdmittedBudget(t *testing.T) {
	quietLog(t)
	up := newFakeUpstream(t, "x")
	fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: agentHermes}, nil)
	m := fx.m
	m.proxy = newTestProxy(t, up, "k", "")
	cfg := AutoReplyConfig{Backend: "exec", Agent: agentHermes,
		AdmittedBudget: &AutoReplyAdmittedBudget{RunsPerDay: Budget(2), RunsPerPeerPerDay: Budget(1), RequestsPerRun: Budget(3)}}
	url := m.proxy.modelBase() + "/plan/chat/completions"
	admitted := func(peer string) module.Turn {
		return module.Turn{Ix: "ix-" + peer, Role: roleInbound, Peer: peer, Admitted: true}
	}
	run1, t1 := m.beginDirectRun(cfg, admitted("peerA"))
	if run1 == nil || run1.refusal != "" || run1.stats.maxRequests != 3 {
		t.Fatalf("the first run: %+v", run1)
	}
	if status, _ := post(t, url, run1.key, chatBody); status != 200 {
		t.Errorf("the first run's request: %d", status)
	}
	run2, t2 := m.beginDirectRun(cfg, admitted("peerA"))
	status, body := post(t, url, run2.key, chatBody)
	if status != http.StatusTooManyRequests || errorCode(body) != "model_budget_spent" {
		t.Errorf("the peer's second run of the day: %d %s", status, body)
	}
	run3, t3 := m.beginDirectRun(cfg, admitted("peerB"))
	if run3.refusal != "" {
		t.Errorf("another peer's first run was refused")
	}
	trusted, tt := m.beginDirectRun(cfg, module.Turn{Ix: "ix-t", Role: roleInbound, Peer: "peerC", Trusted: true})
	if trusted.refusal != "" || tt != nil || trusted.stats.maxRequests != trustedRequestsPerRun {
		t.Errorf("a trusted peer's run: %+v ticket %v", trusted, tt)
	}
	for _, r := range []*proxyRun{run1, run2, run3, trusted} {
		r.end()
	}
	t1.release(true)
	t2.release(true)
	t3.release(true)
	// A restart: the counts come from the records of the admitted runs.
	_ = fx.h.RecordEvidence(EvAutoReplyInvoked, autoReplyRun{Agent: agentHermes, InteractionID: "ix-peerA", Peer: "peerA",
		Mode: modeDirect, Exit: exitOK}.payload())
	m.admitted = runBudget{}
	again, _ := m.beginDirectRun(cfg, admitted("peerA"))
	defer again.end()
	if again.refusal != "model_budget_spent" {
		t.Errorf("after a restart the peer's day was given back: %q", again.refusal)
	}
}

// What a direct run's environment gets of the run: ANET_TURN_* and ANET_MODEL_* only, and never one of
// the daemon's own ANET_* variables.
func TestDirectRunEnv(t *testing.T) {
	environ := []string{"PATH=/bin", "ANET_TURN_URL=http://daemon-own", "ANET_DATA_DIR=/data", "ANET_MODEL_KEY=daemon-own"}
	base := execEnvFrom(environ, AutoReplyConfig{Agent: agentHermes}, nil, "/ob")
	got := strings.Join(withRunEnv(base, []string{"ANET_TURN_URL=http://127.0.0.1:1/anet-turn/v1", "ANET_MODEL_KEY=mpk-x",
		"ANET_DATA_DIR=/elsewhere", "PATH=/evil", "LD_PRELOAD=/evil.so", "ANET_MODEL_BASE=http://127.0.0.1:1/anet-upstream"}), "\n")
	for _, want := range []string{"ANET_TURN_URL=http://127.0.0.1:1/anet-turn/v1", "ANET_MODEL_KEY=mpk-x", "ANET_OUTBOX=/ob",
		"ANET_MODEL_BASE=http://127.0.0.1:1/anet-upstream"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %s:\n%s", want, got)
		}
	}
	for _, never := range []string{"daemon-own", "ANET_DATA_DIR", "/evil"} {
		if strings.Contains(got, never) {
			t.Errorf("holds %s:\n%s", never, got)
		}
	}
	// A sandboxed run is unchanged: the run's variables never reach it.
	r := &execReplier{cfg: AutoReplyConfig{Backend: "exec", Agent: agentHermes}, sandbox: &sandboxPlan{}}
	o, err := r.baseOpts(replyContext{InteractionID: "ix", RunEnv: []string{"ANET_MODEL_KEY=mpk-x"}})
	if err == nil && strings.Contains(strings.Join(o.Env, "\n"), "mpk-x") {
		t.Error("a sandboxed run got the run's model key")
	}
}

// A run gets a key only when the proxy has upstreams, and a turn token only for an inbound turn of a
// trusted or admitted peer while the service has agent tools.
func TestBeginDirectRunDecides(t *testing.T) {
	quietLog(t)
	fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: agentHermes}, nil)
	m := fx.m
	cfg := AutoReplyConfig{Backend: "exec", Agent: agentHermes}
	if run, _ := m.beginDirectRun(cfg, module.Turn{Role: roleInbound, Peer: "p", Trusted: true}); run != nil {
		t.Error("a run without a proxy got one")
	}
	p, err := newModelProxy(AutoReplyModelProxyConfig{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	m.proxy = p
	if run, _ := m.beginDirectRun(cfg, module.Turn{Role: roleInbound, Peer: "p", Trusted: true}); run != nil {
		t.Error("a run got a key from a proxy without upstreams and a token without agent tools")
	}
	m.svc = &fakeSvc{tools: []module.AgentTool{{Capability: "board.post"}}}
	run, _ := m.beginDirectRun(cfg, module.Turn{Role: roleInbound, Peer: "p", Trusted: true})
	if run == nil || run.token == "" || run.key != "" {
		t.Fatalf("a trusted inbound run: %+v", run)
	}
	run.end()
	if run, _ := m.beginDirectRun(cfg, module.Turn{Role: "outbound", Peer: "p", Trusted: true}); run != nil {
		t.Error("an outbound turn got a turn token")
	}
	if run, _ := m.beginDirectRun(cfg, module.Turn{Role: roleInbound, Peer: "p"}); run != nil {
		t.Error("a peer neither trusted nor admitted got a turn token")
	}
}

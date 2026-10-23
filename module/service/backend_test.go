//go:build !no_service

package service_test

// The kernel's way to the service backends (module.ServiceBackend, design
// v0.3.9 §A, §B): an internal capability is never offered to the network
// and is reached only through CallInternal; an agent tool is called as the
// peer with X-ANet-Caller and X-ANet-Via; anything else is refused before a
// byte is sent; a refusal keeps the service's code.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// recorder is a service that records each request and answers with status
// and body.
type recorder struct {
	mu     sync.Mutex
	hits   []*http.Request
	bodies []string
	status int
	body   string
}

func (r *recorder) handler(w http.ResponseWriter, req *http.Request) {
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := req.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	r.mu.Lock()
	r.hits = append(r.hits, req.Clone(context.Background()))
	r.bodies = append(r.bodies, b.String())
	status, body := r.status, r.body
	r.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hits)
}

func (r *recorder) last() (*http.Request, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[len(r.hits)-1], r.bodies[len(r.bodies)-1]
}

// backendOf is the ServiceBackend of the registered service provider.
func backendOf(t *testing.T, reg *provider.Registry) module.ServiceBackend {
	t.Helper()
	p, ok := reg.Provider("service")
	if !ok {
		t.Fatal("the service provider did not register")
	}
	sb, ok := p.(module.ServiceBackend)
	if !ok {
		t.Fatal("the service provider is not a module.ServiceBackend")
	}
	return sb
}

const backendTok = "0123456789abcdef0123456789abcdef"

func startKinds(t *testing.T, url string) (*provider.Registry, module.ServiceBackend) {
	t.Helper()
	tf := writeToken(t, backendTok, 0o600)
	reg := start(t, `{"allow_tcp":true,"token_file":"`+tf+`","capabilities":[
		{"id":"org.admit","url":"`+url+`","internal":true},
		{"id":"board.post","url":"`+url+`","agent_tool":true,"description":"Post to the board",
		 "input_schema":{"type":"object","properties":{"text":{"type":"string"}}}},
		{"id":"org.join","url":"`+url+`"}]}`)
	return reg, backendOf(t, reg)
}

// An internal capability is on no list the network reads, resolves for no
// call, and a call that names it anyway is answered as for a capability this
// node does not have, before anything is sent.
func TestAnInternalCapabilityIsNeverOfferedOrCalledFromTheNetwork(t *testing.T) {
	rec := &recorder{body: `{"admit":true}`}
	svc := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer svc.Close()
	reg, sb := startKinds(t, svc.URL)

	for _, id := range reg.Capabilities() {
		if id == "org.admit" {
			t.Fatal("the internal capability is listed among the capabilities")
		}
	}
	if _, ok := reg.Resolve("org.admit"); ok {
		t.Fatal("the internal capability resolves for a network call")
	}
	p, _ := reg.Provider("service")
	if _, err := p.Invoke(context.Background(), provider.Call{Capability: "org.admit", CallerAID: "aid-x", Via: provider.ViaRelay}); err == nil ||
		!strings.Contains(err.Error(), "no capability") {
		t.Fatalf("Invoke of the internal capability: %v, want no such capability", err)
	}
	if d, ok := p.(provider.Described); ok {
		if _, has := d.SkillInfo("org.admit"); has {
			t.Error("the internal capability has a skill description")
		}
	}
	if fr, ok := p.(provider.FileReceiver); ok && fr.AcceptsFiles("org.admit") {
		t.Error("the internal capability takes files")
	}
	if rec.count() != 0 {
		t.Fatalf("%d requests reached the service from the network side", rec.count())
	}
	if !sb.InternalCapability("org.admit") || sb.InternalCapability("org.join") || sb.InternalCapability("board.post") {
		t.Error("InternalCapability does not tell the internal capability from the others")
	}

	// The kernel's door: the caller named, the body {}, the token, no Via.
	out, err := sb.CallInternal(context.Background(), "aid-peer", "org.admit", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"admit":true}` {
		t.Errorf("reply = %s", out)
	}
	req, body := rec.last()
	if got := req.Header.Get("X-ANet-Caller"); got != "aid-peer" {
		t.Errorf("X-ANet-Caller = %q", got)
	}
	if _, ok := req.Header["X-Anet-Via"]; ok {
		t.Errorf("an internal call carries X-ANet-Via %q", req.Header.Get("X-ANet-Via"))
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+backendTok {
		t.Errorf("Authorization = %q, want the module's token", got)
	}
	if got := req.Header.Get("X-ANet-Capability"); got != "org.admit" {
		t.Errorf("X-ANet-Capability = %q", got)
	}
	if body != "{}" {
		t.Errorf("body = %q, want {}", body)
	}

	// CallInternal reaches internal capabilities only.
	n := rec.count()
	for _, id := range []string{"org.join", "board.post", "nope"} {
		if _, err := sb.CallInternal(context.Background(), "aid-peer", id, nil); err == nil {
			t.Errorf("CallInternal(%s) was accepted", id)
		}
	}
	if rec.count() != n {
		t.Error("a refused CallInternal reached the service")
	}
}

// Only an agent_tool capability is called as the peer: X-ANet-Caller is the
// peer, X-ANet-Via the given word, the token the module's. Any other
// capability is ErrNotAgentTool and nothing is sent.
func TestCallAgentToolOnlyForAgentTools(t *testing.T) {
	rec := &recorder{body: `{"id":"item-7"}`}
	svc := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer svc.Close()
	_, sb := startKinds(t, svc.URL)

	for _, id := range []string{"org.join", "org.admit", "missing"} {
		if _, err := sb.CallAgentTool(context.Background(), "aid-peer", id, nil, "agent"); !errors.Is(err, module.ErrNotAgentTool) {
			t.Errorf("CallAgentTool(%s) = %v, want ErrNotAgentTool", id, err)
		}
	}
	if rec.count() != 0 {
		t.Fatalf("a refused agent call reached the service (%d requests)", rec.count())
	}
	out, err := sb.CallAgentTool(context.Background(), "aid-peer", "board.post", json.RawMessage(`{"text":"help"}`), "agent")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"id":"item-7"}` {
		t.Errorf("reply = %s", out)
	}
	req, body := rec.last()
	for h, want := range map[string]string{"X-ANet-Caller": "aid-peer", "X-ANet-Via": "agent",
		"X-ANet-Capability": "board.post", "Authorization": "Bearer " + backendTok} {
		if got := req.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	if body != `{"text":"help"}` {
		t.Errorf("body = %q", body)
	}
	if _, err := sb.CallAgentTool(context.Background(), "", "board.post", nil, "agent"); err == nil {
		t.Error("an agent call without a caller was accepted")
	}
	// Arguments are one JSON object without anet's "$" keys.
	for _, args := range []string{`[1]`, `"x"`, `{"$files":[]}`} {
		var se *module.ServiceError
		if _, err := sb.CallAgentTool(context.Background(), "aid-peer", "board.post", json.RawMessage(args), "agent"); !errors.As(err, &se) {
			t.Errorf("args %s: %v, want a ServiceError", args, err)
		}
	}

	tools := sb.AgentTools()
	if len(tools) != 1 || tools[0].Capability != "board.post" || tools[0].Description != "Post to the board" ||
		!strings.Contains(string(tools[0].InputSchema), `"text"`) {
		t.Errorf("AgentTools = %+v", tools)
	}
}

// A refusal answered by the service keeps its code and message; what is not
// a refusal is a reason code, and the service's own words stay in the log.
func TestAgentToolErrorsKeepTheServiceShape(t *testing.T) {
	rec := &recorder{}
	svc := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer svc.Close()
	_, sb := startKinds(t, svc.URL)
	for _, c := range []struct {
		status        int
		body          string
		code, message string
	}{
		{403, `{"error":"not_member","message":"Join with an invite first."}`, "not_member", "Join with an invite first."},
		{404, `{"error":"Not A Code"}`, "backend_failed", ""},
		{429, `{"error":"slow_down"}`, "busy", ""},
		{503, ``, "busy", ""},
		{500, `{"error":"secret stack trace"}`, "backend_failed", ""},
		{200, `not json`, "backend_failed", ""},
		{200, `[1,2]`, "backend_failed", ""},
	} {
		rec.mu.Lock()
		rec.status, rec.body = c.status, c.body
		rec.mu.Unlock()
		_, err := sb.CallAgentTool(context.Background(), "aid-peer", "board.post", nil, "agent")
		var se *module.ServiceError
		if !errors.As(err, &se) {
			t.Errorf("HTTP %d %s: %v, want a ServiceError", c.status, c.body, err)
			continue
		}
		if se.Code != c.code || se.Message != c.message {
			t.Errorf("HTTP %d %s: %+v, want %s %q", c.status, c.body, se, c.code, c.message)
		}
	}
	// A service nobody listens for: nothing was sent, unavailable. (A pooled
	// connection the far side closed may instead be connection_lost, which
	// errs on the side of "it may have gone out"; provider.TrackSent.)
	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close()
	_, sbDown := startKinds(t, url)
	_, err := sbDown.CallAgentTool(context.Background(), "aid-peer", "board.post", nil, "agent")
	var se *module.ServiceError
	if !errors.As(err, &se) || se.Code != "unavailable" {
		t.Errorf("a service that is down: %v, want unavailable", err)
	}
}

func TestCapabilityKindsAreChecked(t *testing.T) {
	for _, c := range []string{
		`{"id":"x","url":"http://127.0.0.1:1","internal":true,"agent_tool":true}`,
		`{"id":"x","url":"http://127.0.0.1:1","internal":true,"price":5}`,
		`{"id":"x","url":"http://127.0.0.1:1","input_schema":[1]}`,
		`{"id":"x","url":"http://127.0.0.1:1","input_schema":"object"}`,
		`{"id":"x","url":"http://127.0.0.1:1","agent_tool":"yes"}`,
		`{"id":"x","url":"http://127.0.0.1:1","input_schema":{"d":"` + strings.Repeat("a", 17<<10) + `"}}`,
	} {
		if _, err := module.Build(map[string][]byte{"service": []byte(`{"allow_tcp":true,"capabilities":[` + c + `]}`)}); err == nil {
			t.Errorf("capability %.80s was accepted", c)
		}
	}
	if _, err := module.Build(map[string][]byte{"service": []byte(`{"allow_tcp":true,"capabilities":[` +
		`{"id":"x","url":"http://127.0.0.1:1","agent_tool":true,"input_schema":{"type":"object"}}]}`)}); err != nil {
		t.Errorf("a valid agent tool was refused: %v", err)
	}
}

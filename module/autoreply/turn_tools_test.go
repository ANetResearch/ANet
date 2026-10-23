package autoreply

// turn_tools_test.go — the turn tools on the model proxy's listener (design v0.3.9 §E): a token is bound
// to its run's caller, nothing in a request changes whose call it is, and the token ends with the run.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/turntools"
	"github.com/ANetResearch/ANet/module"
)

// fakeSvc is a ServiceCaller: its agent tools, and the calls made through it.
type fakeSvc struct {
	tools []module.AgentTool
	mu    sync.Mutex
	calls []fakeSvcCall
}

type fakeSvcCall struct {
	Caller, Capability, Via string
	Args                    json.RawMessage
}

func (f *fakeSvc) AgentTools() []module.AgentTool { return f.tools }

func (f *fakeSvc) CallServiceAs(_ context.Context, caller, capability string, args json.RawMessage, via string) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeSvcCall{Caller: caller, Capability: capability, Via: via, Args: args})
	f.mu.Unlock()
	switch capability {
	case "board.fail":
		return nil, &module.ServiceError{Code: "not_member", Message: "the caller is not a member"}
	case "board.secret":
		return nil, module.ErrNotAgentTool
	}
	return json.RawMessage(`{"id":"item-1"}`), nil
}

func (f *fakeSvc) recorded() []fakeSvcCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeSvcCall(nil), f.calls...)
}

func orgSvc() *fakeSvc {
	return &fakeSvc{tools: []module.AgentTool{
		{Capability: "board.post", Description: "Post a request for help.", InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`)},
		{Capability: "org.info", Description: "The organisation."},
		{Capability: "board.fail"},
		{Capability: "board.secret", InputSchema: json.RawMessage(`{"type":"array"}`)},
	}}
}

func turnClient(t testing.TB, p *modelProxy, token string) *turntools.Client {
	t.Helper()
	c, err := turntools.NewClient(p.turnURL(), token)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func callCode(err error) string {
	var ce *turntools.CallError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

func TestTurnToolsAreBoundToTheRunsCaller(t *testing.T) {
	quietLog(t)
	p, err := newModelProxy(AutoReplyModelProxyConfig{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	svc := orgSvc()
	p.setService(svc)
	runA, _ := p.begin(proxyRunSpec{Peer: "aidA", Ix: "ixA", Turn: true, Model: true, Timeout: time.Minute})
	runB, _ := p.begin(proxyRunSpec{Peer: "aidB", Ix: "ixB", Turn: true, Timeout: time.Minute})
	defer runB.end()
	ctx := context.Background()
	a, b := turnClient(t, p, runA.token), turnClient(t, p, runB.token)

	tools, err := a.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
		if tl.Capability == "org.info" || tl.Capability == "board.secret" {
			if string(tl.InputSchema) != `{"type":"object"}` {
				t.Errorf("%s: schema %s, want the empty object schema", tl.Capability, tl.InputSchema)
			}
		}
	}
	if strings.Join(names, ",") != "board_fail,board_post,board_secret,org_info" {
		t.Errorf("tools %v", names)
	}
	if _, err := a.Call(ctx, "board.post", json.RawMessage(`{"title":"help"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(ctx, "board.post", nil); err != nil {
		t.Fatal(err)
	}
	got := svc.recorded()
	if len(got) != 2 || got[0].Caller != "aidA" || got[1].Caller != "aidB" || got[0].Via != "agent" || string(got[1].Args) != "{}" {
		t.Fatalf("calls %+v", got)
	}
	// Nothing in the request names the caller: a member for it is refused, before the service is asked.
	for name, body := range map[string]string{
		"a caller member":   `{"capability":"board.post","args":{},"caller":"aidB"}`,
		"args not object":   `{"capability":"board.post","args":[1]}`,
		"no capability":     `{"args":{}}`,
		"not a JSON object": `[]`,
	} {
		req, _ := http.NewRequest(http.MethodPost, p.turnURL()+turntools.CallPath, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+runA.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var cr turntools.CallResponse
		_ = json.NewDecoder(resp.Body).Decode(&cr)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || cr.Error == nil || cr.Error.Code != "turn_call_invalid" {
			t.Errorf("%s: %d %+v", name, resp.StatusCode, cr.Error)
		}
	}
	if n := len(svc.recorded()); n != 2 {
		t.Errorf("the service was asked %d times, want 2", n)
	}
	// The service's refusal keeps its own code; a capability that is not a tool is the node's refusal.
	if _, err := a.Call(ctx, "board.fail", nil); callCode(err) != "not_member" {
		t.Errorf("a service refusal: %v", err)
	}
	if _, err := a.Call(ctx, "admin.remove", nil); callCode(err) != "turn_tool_unknown" {
		t.Errorf("a capability that is not listed: %v", err)
	}
	if _, err := a.Call(ctx, "board.secret", nil); callCode(err) != "turn_tool_unknown" {
		t.Errorf("a capability the service does not take as a tool: %v", err)
	}
	// The model key is not a turn token; a token ends with its run.
	if _, err := turnClient(t, p, runA.key).Tools(ctx); callCode(err) != "turn_unauthorized" {
		t.Errorf("the model key on the turn route: %v", err)
	}
	runA.end()
	if _, err := a.Call(ctx, "board.post", nil); callCode(err) != "turn_unauthorized" {
		t.Errorf("a token after its run ended: %v", err)
	}
	if _, err := a.Tools(ctx); callCode(err) != "turn_unauthorized" {
		t.Errorf("listing after the run ended: %v", err)
	}
}

// A run makes at most turnMaxCallsPerRun calls.
func TestTurnToolsCallsPerRun(t *testing.T) {
	quietLog(t)
	p, err := newModelProxy(AutoReplyModelProxyConfig{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.setService(orgSvc())
	run, _ := p.begin(proxyRunSpec{Peer: "aidA", Turn: true, Timeout: time.Minute})
	defer run.end()
	c := turnClient(t, p, run.token)
	for i := 0; i < turnMaxCallsPerRun; i++ {
		if _, err := c.Call(context.Background(), "org.info", nil); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if _, err := c.Call(context.Background(), "org.info", nil); callCode(err) != "turn_calls_spent" {
		t.Errorf("a call past the bound: %v", err)
	}
}

// The client sends its token to this machine's loopback only, and an unexpanded ${VAR} is no value.
func TestTurnToolsClientAddress(t *testing.T) {
	for _, u := range []string{"https://127.0.0.1:1/anet-turn/v1", "http://example.org/anet-turn/v1", "http://10.0.0.1:1/x", "http://u:p@127.0.0.1:1/x"} {
		if _, err := turntools.NewClient(u, "tok"); !errors.Is(err, turntools.ErrNotLoopback) {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"http://127.0.0.1:1/anet-turn/v1", "http://[::1]:1/anet-turn/v1", "http://localhost:1/anet-turn/v1"} {
		if _, err := turntools.NewClient(u, "tok"); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	env := map[string]string{turntools.EnvURL: "${ANET_TURN_URL}", turntools.EnvToken: "${ANET_TURN_TOKEN}"}
	if c, err := turntools.FromEnv(func(k string) string { return env[k] }); c != nil || err != nil {
		t.Errorf("unexpanded variables: %v %v", c, err)
	}
}

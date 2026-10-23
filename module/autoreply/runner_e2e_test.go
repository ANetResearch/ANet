//go:build linux || darwin

package autoreply

// runner_e2e_test.go — the whole path of design v0.3.9 §C to §E on one machine: a peer's task reaches the
// module, the turn goes to the runner as a job, the runner starts the agent (testdata/fakeagent, as
// hermes), the agent calls a turn tool, which the service sees as the peer's call, and the model through
// the proxy, which adds the upstream's key; the reply and the agent's files come back to the peer. The
// upstream's key is nowhere the agent can read, and the run's key and token stop working with the run.

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/turntools"
	"github.com/ANetResearch/ANet/module"
)

const e2ePeer = "bafyreiexamplepeeraid0000000000000000000000000000peer0001"

func TestRunnerEndToEnd(t *testing.T) {
	bin := fakeAgent(t)
	r := startTestRunner(t, RunnerOptions{Agents: map[string]string{agentHermes: bin}})
	up := newFakeUpstream(t, "model says hi")
	const upstreamKey = "upstream-secret-e2e-0123456789"
	cfg := AutoReplyConfig{Backend: "exec", Agent: agentHermes, Model: "deepseek-test", Admitted: AdmittedDirect,
		Runner: &AutoReplyRunnerConfig{Socket: r.sock, ExpectedUser: strconv.Itoa(os.Getuid())},
		ModelProxy: &AutoReplyModelProxyConfig{Listen: "127.0.0.1:0", Upstreams: []ModelProxyUpstream{
			{Name: "plan", APIBase: up.srv.URL + "/v1", APIKeyFile: writeKeyFile(t, upstreamKey)}}},
		AdmittedBudget: &AutoReplyAdmittedBudget{RunsPerPeerPerDay: Budget(1)}}
	if ref := checkRunnerConfig(&cfg); ref != nil {
		t.Fatal(ref)
	}
	fx := newTurnFx(t, cfg, nil)
	svc := orgSvc()
	fx.m.svc = svc
	if err := fx.m.ensureModelProxy(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fx.m.closeModelProxy)

	// A trusted peer: the scan runs the turn directly, which goes to the runner.
	ix := fx.deliver(e2ePeer, true, "Please post a help request to the board.")
	fx.tick()
	x := fx.ix(ix)
	if len(x.sent) != 1 {
		t.Fatalf("sent %+v (refusal %q)", x.sent, x.reason)
	}
	reply := x.sent[0].Text
	for _, want := range []string{"tools:board_fail,board_post,board_secret,org_info", `call:{"id":"item-1"}`, "model:model says hi"} {
		if !strings.Contains(reply, want) {
			t.Errorf("the reply lacks %s: %s", want, reply)
		}
	}
	atts := map[string]string{}
	for _, a := range x.sent[0].Atts {
		atts[a.Name] = string(a.Data)
	}
	env := atts["env.txt"]
	if env == "" {
		t.Fatalf("no environment file among %v", x.sent[0].Atts)
	}
	calls := svc.recorded()
	if len(calls) != 1 || calls[0].Caller != e2ePeer || calls[0].Via != "agent" || calls[0].Capability != "board.post" ||
		string(calls[0].Args) != `{"title":"help wanted"}` {
		t.Errorf("service calls %+v", calls)
	}
	ups := up.received()
	if len(ups) != 1 || ups[0].Header.Get("Authorization") != "Bearer "+upstreamKey {
		t.Fatalf("upstream requests %+v", ups)
	}
	var runKey, turnToken, turnURL, modelBase string
	for _, line := range strings.Split(env, "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case turntools.EnvModelKey:
			runKey = v
		case turntools.EnvToken:
			turnToken = v
		case turntools.EnvURL:
			turnURL = v
		case turntools.EnvModelBase:
			modelBase = v
		}
	}
	if !strings.HasPrefix(runKey, "mpk-") || !strings.HasPrefix(turnToken, "ttk-") ||
		!strings.HasSuffix(turnURL, turntools.RoutePrefix) || !strings.HasSuffix(modelBase, "/anet-upstream") {
		t.Fatalf("the run's variables: key %q token %q url %q base %q", runKey, turnToken, turnURL, modelBase)
	}
	if strings.Contains(ups[0].Header.Get("Authorization"), runKey) {
		t.Error("the run's key reached the upstream")
	}
	// The upstream's key is not in the agent's environment, its files or the runner's work directories.
	if strings.Contains(env, upstreamKey) || strings.Contains(reply, upstreamKey) {
		t.Error("the agent saw the upstream's key")
	}
	_ = filepath.WalkDir(r.root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), upstreamKey) {
				t.Errorf("%s holds the upstream's key", p)
			}
		}
		return nil
	})
	// The run ended: its key and token are refused.
	if status, body := post(t, modelBase+"/plan/chat/completions", runKey, chatBody); status != http.StatusUnauthorized {
		t.Errorf("the run's key after the run: %d %s", status, body)
	}
	if c, err := turntools.NewClient(turnURL, turnToken); err != nil {
		t.Fatal(err)
	} else if _, err := c.Tools(context.Background()); callCode(err) != "turn_unauthorized" {
		t.Errorf("the run's token after the run: %v", err)
	}
	if rec := fx.invoked(); len(rec) != 1 || rec[0]["mode"] != modeDirect || rec[0]["exit"] != exitOK {
		t.Errorf("records %v", rec)
	}

	// An admitted peer (inbound.admit let it in; not on the trust list), with auto_reply.admitted=direct:
	// the same path, with the admitted budget of one run a day. The second task of the day is refused with
	// the budget's reason before any agent starts.
	admittedTurn := func() fxIx {
		ix := fx.deliver(e2ePeer+"x", false, "Another request.")
		fx.h.mu.Lock()
		fx.h.ixs[ix].turn.Admitted = true
		fx.h.mu.Unlock()
		fx.tick()
		return fx.ix(ix)
	}
	first := admittedTurn()
	if len(first.sent) != 1 || !strings.Contains(first.sent[0].Text, "model:model says hi") || !strings.Contains(first.sent[0].Text, "tools:") {
		t.Errorf("the admitted peer's first run: %+v", first.sent)
	}
	second := admittedTurn()
	if len(second.sent) != 0 || second.reason != module.ReasonBudget || second.retryAfter <= 0 {
		t.Errorf("the admitted peer's second task of the day: sent %+v, refused %q after %s", second.sent, second.reason, second.retryAfter)
	}
	var cs []fakeSvcCall
	for _, c := range svc.recorded() {
		if c.Caller == e2ePeer+"x" {
			cs = append(cs, c)
		}
	}
	if len(cs) != 1 {
		t.Errorf("the admitted peer's tool calls: %+v", cs)
	}
}

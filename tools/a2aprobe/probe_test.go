package main

// probe_test.go runs the probe's scenario against a stand-in for the local
// A2A interface — a2a-go's own server (a2asrv) over a small in-memory agent
// that answers the way the design says anet answers — so that a probe that
// misreads a2a-go, or the design, fails here rather than on the test hosts.
// It does not test anet: module/a2a has its own tests, and the joint run
// is the test of the whole.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

const (
	fakeToken = "0123456789abcdef0123456789abcdef"
	fakeAID   = "bafyfakeprovider"
)

// fakeInterface is the local interface's HTTP face: the bearer on every
// route, the card at the base URL and the well-known one, the two bindings.
type fakeInterface struct {
	srv    *httptest.Server
	mu     sync.Mutex
	agents map[string]*fakeAgent
	// race makes a text task's blocking answer arrive as input-required
	// with the answer, the task completing a moment later.
	race bool
	// waitAnyway makes a send with returnImmediately wait like a blocking
	// one (with race: until the answer, at input-required).
	waitAnyway bool
	// overRefusal, when set, is the error a payment above the agent tier
	// gets instead of the spending policy's.
	overRefusal string
	// noX402 leaves a2a-x402 off the card, as when the hub serves no
	// registry: the activator then asks for nothing.
	noX402 bool
}

func startFake(t *testing.T, race bool) *fakeInterface {
	t.Helper()
	f := &fakeInterface{agents: map[string]*fakeAgent{}, race: race}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+agentsPath+"/{aid}", f.card)
	mux.HandleFunc("GET "+agentsPath+"/{aid}/.well-known/agent-card.json", f.card)
	mux.HandleFunc(agentsPath+"/{aid}/", func(w http.ResponseWriter, r *http.Request) {
		a := f.agent(r.PathValue("aid"))
		rest := strings.TrimPrefix(r.URL.Path, agentsPath+"/"+a.aid)
		switch {
		case rest == "/jsonrpc":
			a2asrv.NewJSONRPCHandler(a).ServeHTTP(w, r)
		case strings.HasPrefix(rest, "/rest/"):
			http.StripPrefix(agentsPath+"/"+a.aid+"/rest", a2asrv.NewRESTHandler(a)).ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			w.Header().Set("WWW-Authenticate", `Bearer realm="anet-local-a2a"`)
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeInterface) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeInterface) agent(aid string) *fakeAgent {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[aid]
	if !ok {
		a = &fakeAgent{aid: aid, race: f.race, waitAnyway: f.waitAnyway, overRefusal: f.overRefusal,
			tasks: map[a2a.TaskID]*a2a.Task{}, calls: map[string]int{}}
		f.agents[aid] = a
	}
	return a
}

func (f *fakeInterface) card(w http.ResponseWriter, r *http.Request) {
	aid := r.PathValue("aid")
	base := f.srv.URL + agentsPath + "/" + aid
	verification := "UNVERIFIED"
	ext := []a2a.AgentExtension{}
	if aid == fakeAID && !f.noX402 {
		verification = "VERIFIED"
		ext = append(ext, a2a.AgentExtension{URI: x402URI, Params: map[string]any{"signer": "anet-daemon", "clientPayload": false}})
	}
	ext = append(ext, a2a.AgentExtension{URI: originURI, Params: map[string]any{"aid": aid, "originVerification": verification}})
	card := a2a.AgentCard{
		Name: "Fake " + aid, Description: "a stand-in", Version: "1",
		SupportedInterfaces: []*a2a.AgentInterface{
			{URL: base + "/jsonrpc", ProtocolBinding: a2a.TransportProtocolJSONRPC, ProtocolVersion: "1.0"},
			{URL: base + "/rest", ProtocolBinding: a2a.TransportProtocolHTTPJSON, ProtocolVersion: "1.0"},
		},
		Capabilities:         a2a.AgentCapabilities{Streaming: true, Extensions: ext},
		DefaultInputModes:    []string{"text/plain"},
		DefaultOutputModes:   []string{"text/plain"},
		Skills:               []a2a.AgentSkill{{ID: "chat", Name: "chat", Description: "chat", Tags: []string{"chat"}}},
		SecuritySchemes:      a2a.NamedSecuritySchemes{"anetLocal": a2a.HTTPAuthSecurityScheme{Scheme: "Bearer"}},
		SecurityRequirements: a2a.SecurityRequirementsOptions{{"anetLocal": a2a.SecuritySchemeScopes{"a2a"}}},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(card)
}

// fakeAgent is one remote agent's tasks, answering as the design says anet
// does: text tasks answered "echo: <text>" (never those that say [hold]),
// capability calls to "paid" and "pricey" quoted, the §8.7 payment
// message and its refusals.
type fakeAgent struct {
	aid         string
	race        bool
	waitAnyway  bool
	overRefusal string
	mu          sync.Mutex
	tasks       map[a2a.TaskID]*a2a.Task
	order       []a2a.TaskID
	calls       map[string]int
}

var fakeOption = map[string]any{"scheme": "anet-credit", "network": "hub:x", "amount": "5", "payTo": fakeAID, "maxTimeoutSeconds": 600}

func (a *fakeAgent) copyOf(t *a2a.Task) *a2a.Task {
	b, _ := json.Marshal(t)
	var out a2a.Task
	_ = json.Unmarshal(b, &out)
	return &out
}

func (a *fakeAgent) complete(t *a2a.Task, text string) {
	reply := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(echoPrefix+text))
	t.History = append(t.History, reply)
	t.Artifacts = []*a2a.Artifact{{ID: artifactReply, Parts: a2a.ContentParts{a2a.NewTextPart(echoPrefix + text)}}}
	t.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
	t.Metadata[keyReceiptOK] = "verified"
}

func (a *fakeAgent) SendMessage(ctx context.Context, r *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := r.Message
	if m.TaskID != "" {
		t, ok := a.tasks[m.TaskID]
		if !ok {
			return nil, a2a.ErrTaskNotFound
		}
		if st, ok := m.Metadata[keyX402Status].(string); ok {
			return a.pay(t, st, m.Metadata)
		}
		if t.Status.State.Terminal() {
			return nil, a2a.NewError(a2a.ErrUnsupportedOperation, "the task has ended")
		}
		a.complete(t, partsText(m.Parts))
		return a.copyOf(t), nil
	}
	ctxID := m.ContextID
	if ctxID == "" {
		ctxID = "ctx-minted-" + randHex(4)
	}
	t := &a2a.Task{ID: a2a.TaskID("task-" + randHex(6)), ContextID: ctxID, History: []*a2a.Message{m},
		Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, Metadata: map[string]any{keyPeerAID: a.aid}}
	a.tasks[t.ID] = t
	a.order = append(a.order, t.ID)
	for _, p := range m.Parts {
		if d, ok := p.Content.(a2a.Data); ok {
			obj, _ := d.Value.(map[string]any)
			skill, _ := obj["skill"].(string)
			args, _ := obj["args"].(map[string]any)
			t.Metadata[keySkill] = skill
			t.Metadata["fake.text"] = args["text"]
			opt := cloneJSON(fakeOption)
			if skill == "pricey" {
				opt["amount"] = "15"
			}
			req := map[string]any{"x402Version": 2, "accepts": []any{opt}}
			msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Payment is required."))
			msg.Metadata = map[string]any{keyX402Status: payRequired, keyX402Required: req}
			t.Status = a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: msg}
			t.Metadata[keyX402Status] = payRequired
			t.Metadata[keyReason] = reasonNeedsOperatorApproval
			return a.copyOf(t), nil
		}
	}
	text := partsText(m.Parts)
	switch {
	case strings.Contains(text, holdMark):
	case r.Config != nil && r.Config.ReturnImmediately && !a.waitAnyway:
		go func() {
			time.Sleep(50 * time.Millisecond)
			a.mu.Lock()
			a.complete(t, text)
			a.mu.Unlock()
		}()
	case a.race:
		msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(echoPrefix+text))
		t.Status = a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: msg}
		out := a.copyOf(t)
		go func() {
			time.Sleep(50 * time.Millisecond)
			a.mu.Lock()
			a.complete(t, text)
			a.mu.Unlock()
		}()
		return out, nil
	default:
		a.complete(t, text)
	}
	return a.copyOf(t), nil
}

// pay is §8.7 on the agent's side.
func (a *fakeAgent) pay(t *a2a.Task, status string, meta map[string]any) (a2a.SendMessageResult, error) {
	refuse := func(reason string) (a2a.SendMessageResult, error) {
		out := a.copyOf(t)
		msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("payment not submitted"))
		msg.Metadata = map[string]any{keyX402Status: payFailed, keyX402Error: codeSettlementFailed, keyReason: reason}
		out.Status.Message = msg
		return out, nil
	}
	if _, ok := meta[keyX402Payload]; ok {
		return refuse(reasonClientPayload)
	}
	switch status {
	case payRejected:
		t.Status = a2a.TaskStatus{State: a2a.TaskStateCanceled}
		t.Metadata[keyX402Status] = payRejected
		return a.copyOf(t), nil
	case paySubmitted:
	default:
		return nil, a2a.ErrInvalidParams
	}
	want := paymentRequired(t)
	got, _ := json.Marshal(meta[keyAccept])
	offered, _ := json.Marshal(accepts(want)[0])
	if !bytes.Equal(got, offered) {
		return refuse(reasonOptionNotOffered)
	}
	if t.Metadata[keySkill] == "pricey" {
		msg := "task " + string(t.ID) + ": spending policy refused a task-agent payment (" + spendOverSingle +
			"): 15 is above the task-agent limit of 10 per payment; an operator can pay it with `anet pay`"
		if a.overRefusal != "" {
			msg = a.overRefusal
		}
		return nil, a2a.NewError(a2a.ErrUnsupportedOperation, msg)
	}
	text, _ := t.Metadata["fake.text"].(string)
	a.calls[str(t.Metadata[keySkill])]++
	t.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
	t.Metadata[keyX402Status] = payCompleted
	t.Metadata[keyX402Receipts] = []any{map[string]any{"success": true, "transaction": "tx1"}}
	t.Metadata[keyEffect] = "OK"
	delete(t.Metadata, keyReason)
	t.Artifacts = []*a2a.Artifact{{ID: "anet.deliverable", Parts: a2a.ContentParts{a2a.NewDataPart(map[string]any{"digest": digestHex(text)})}}}
	return a.copyOf(t), nil
}

func (a *fakeAgent) GetTask(ctx context.Context, r *a2a.GetTaskRequest) (*a2a.Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tasks[r.ID]
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	return a.copyOf(t), nil
}

func (a *fakeAgent) ListTasks(ctx context.Context, r *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := &a2a.ListTasksResponse{Tasks: []*a2a.Task{}, PageSize: 50}
	for _, id := range a.order {
		t := a.tasks[id]
		if r.ContextID != "" && t.ContextID != r.ContextID {
			continue
		}
		c := a.copyOf(t)
		if !r.IncludeArtifacts {
			c.Artifacts = nil
		}
		out.Tasks = append(out.Tasks, c)
	}
	out.TotalSize = len(out.Tasks)
	return out, nil
}

func (a *fakeAgent) CancelTask(ctx context.Context, r *a2a.CancelTaskRequest) (*a2a.Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tasks[r.ID]
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	t.Status = a2a.TaskStatus{State: a2a.TaskStateCanceled}
	return a.copyOf(t), nil
}

func (a *fakeAgent) SendStreamingMessage(ctx context.Context, r *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		text := partsText(r.Message.Parts)
		a.mu.Lock()
		t := &a2a.Task{ID: a2a.TaskID("task-" + randHex(6)), ContextID: "ctx-minted-" + randHex(4),
			History: []*a2a.Message{r.Message}, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted},
			Metadata: map[string]any{keyPeerAID: a.aid}}
		a.tasks[t.ID] = t
		a.order = append(a.order, t.ID)
		first := a.copyOf(t)
		a.complete(t, text)
		a.mu.Unlock()
		if !yield(first, nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(t, a2a.TaskStateWorking, nil), nil) {
			return
		}
		if !yield(a2a.NewArtifactUpdateEvent(t, artifactReply, a2a.NewTextPart(echoPrefix+text)), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(t, a2a.TaskStateCompleted, nil), nil)
	}
}

func (a *fakeAgent) SubscribeToTask(context.Context, *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) { yield(nil, a2a.ErrUnsupportedOperation) }
}

func (a *fakeAgent) GetTaskPushConfig(context.Context, *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

func (a *fakeAgent) ListTaskPushConfigs(context.Context, *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

func (a *fakeAgent) CreateTaskPushConfig(context.Context, *a2a.PushConfig) (*a2a.PushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

func (a *fakeAgent) DeleteTaskPushConfig(context.Context, *a2a.DeleteTaskPushConfigRequest) error {
	return a2a.ErrPushNotificationNotSupported
}

func (a *fakeAgent) GetExtendedAgentCard(context.Context, *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	return nil, a2a.ErrUnsupportedOperation
}

// runProbe runs the scenario and the restart check against f and returns
// the report's output.
func runProbe(t *testing.T, f *fakeInterface) (string, *probe) {
	t.Helper()
	dir := t.TempDir()
	var out bytes.Buffer
	rep := newReport(&out)
	p := &probe{rep: rep, agent: fakeAID, token: fakeToken, nonce: "nonce42", timeout: 10 * time.Second,
		base: "http://" + f.addr() + agentsPath + "/" + fakeAID,
		st: &runState{Agent: fakeAID, Nonce: "nonce42", Contexts: map[string]string{}, Blocking: map[string]string{},
			Immediate: map[string]string{}, Stream: map[string]string{}},
		rec: loadRecord(""),
	}
	client := filepath.Join(dir, "client.json")
	if !p.run(context.Background(), runOpts{registry: true, paid: "paid", pricey: "pricey", clientOut: client}) {
		t.Fatalf("run found nothing to run against:\n%s", out.String())
	}
	record := filepath.Join(dir, "record.json")
	state := filepath.Join(dir, "state.json")
	p.save(state, record)

	// again, from the files run wrote, as a new process would.
	var cc clientConfig
	var st runState
	if err := readJSONFile(client, &cc); err != nil {
		t.Fatal(err)
	}
	if err := readJSONFile(state, &st); err != nil {
		t.Fatal(err)
	}
	if cc.URL != p.base || cc.Auth.Type != "bearer" || cc.Auth.Token != fakeToken {
		t.Fatalf("client configuration %+v", cc)
	}
	if fi, err := os.Stat(client); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("client configuration mode: %v %v", fi.Mode(), err)
	}
	q := &probe{rep: rep, agent: st.Agent, base: cc.URL, token: cc.Auth.Token, nonce: st.Nonce,
		timeout: 10 * time.Second, st: &st, rec: loadRecord(record)}
	q.again(context.Background())
	q.save("", record)
	return out.String(), q
}

func TestScenarioAgainstAStandIn(t *testing.T) {
	f := startFake(t, false)
	out, q := runProbe(t, f)
	t.Log(out)
	if q.rep.fails != 0 || strings.Contains(out, "FAIL ") {
		t.Fatalf("%d failures:\n%s", q.rep.fails, out)
	}
	for _, id := range []string{"card-no-token", "card-resolver-no-token", "card-origin", "card-x402",
		"jsonrpc-send-blocking", "rest-send-blocking", "jsonrpc-send-immediate-done", "rest-stream",
		"jsonrpc-list-context", "cancel", "cancel-then-send", "no-security-requirements", "other-agent",
		"pay-quote", "pay-option-not-offered", "pay-client-payload", "pay-quote-stands", "pay", "pay-deliverable",
		"pay-over-limit", "pay-over-limit-task", "pay-reject", "restart-card", "restart-get", "restart-send", "restart-list"} {
		if !strings.Contains(out, "PASS "+id+":") {
			t.Errorf("no PASS %s:\n%s", id, out)
		}
	}
	if strings.Contains(out, "NOTE jsonrpc-send-blocking-state") {
		t.Errorf("a completed blocking answer was noted:\n%s", out)
	}
	names := map[string]hermesCase{}
	for _, c := range q.rec.Cases {
		names[c.Name] = c
	}
	for _, n := range []string{"text-blocking", "text-blocking-after-restart", "capability-payment-required", "capability-paid"} {
		c, ok := names[n]
		if !ok {
			t.Errorf("record has no case %s", n)
			continue
		}
		var resp struct {
			Result struct {
				Task json.RawMessage `json:"task"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(c.Response), &resp); err != nil || len(resp.Result.Task) == 0 {
			t.Errorf("%s: not a JSON-RPC SendMessage response: %s", n, c.Response)
		}
	}
	if c := names["text-blocking"]; c.WantState != "completed" || c.WantReplyContains != "echo: joint-a2a jsonrpc blocking nonce42" {
		t.Errorf("text-blocking case: %+v", c)
	}
	if c := names["capability-paid"]; c.WantReplyContains != digestHex("joint-a2a paid nonce42") {
		t.Errorf("capability-paid case: %+v", c)
	}
	if f.agent(fakeAID).calls["pricey"] != 0 || f.agent(fakeAID).calls["paid"] != 1 {
		t.Errorf("calls %v", f.agent(fakeAID).calls)
	}
}

// When the answer comes back at input-required, the client still has it;
// that is noted, not failed, and the task is followed to its end.
func TestAnswerAtInputRequiredIsNoted(t *testing.T) {
	f := startFake(t, true)
	out, q := runProbe(t, f)
	if q.rep.fails != 0 {
		t.Fatalf("%d failures:\n%s", q.rep.fails, out)
	}
	for _, s := range []string{"NOTE jsonrpc-send-blocking-state:", "PASS jsonrpc-send-blocking-done:", "NOTE restart-send-state:"} {
		if !strings.Contains(out, s) {
			t.Errorf("no %q:\n%s", s, out)
		}
	}
	for _, c := range q.rec.Cases {
		if c.Name == "text-blocking" && c.WantState != "input-required" {
			t.Errorf("text-blocking recorded as %s", c.WantState)
		}
	}
}

// A send with returnImmediately that waits for the answer anyway ends at
// input-required, which is not terminal: it must fail all the same.
func TestImmediateThatWaitsFails(t *testing.T) {
	f := startFake(t, true)
	f.waitAnyway = true
	out, _ := runProbe(t, f)
	for _, tag := range []string{"jsonrpc", "rest"} {
		if !strings.Contains(out, "FAIL "+tag+"-send-immediate:") {
			t.Errorf("no FAIL %s-send-immediate:\n%s", tag, out)
		}
	}
}

// A payment above the agent tier refused for some other reason — no quote,
// a payee not allowed — says nothing about the limit and must fail.
func TestOverLimitRefusedForAnotherReasonFails(t *testing.T) {
	f := startFake(t, false)
	f.overRefusal = "task x: spending policy refused a task-agent payment (payee_not_allowed)"
	out, _ := runProbe(t, f)
	if !strings.Contains(out, "FAIL pay-over-limit:") {
		t.Errorf("no FAIL pay-over-limit:\n%s", out)
	}
}

// Without a2a-x402 on the card the client does not activate it, and §8.7
// gives the quote another reason than the one the kernel gives today: a
// NOTE, and the flow still runs.
func TestNotActivatedIsNoted(t *testing.T) {
	f := startFake(t, false)
	f.noX402 = true
	var out bytes.Buffer
	p := &probe{rep: newReport(&out), agent: fakeAID, token: fakeToken, nonce: "n", timeout: 10 * time.Second,
		base: "http://" + f.addr() + agentsPath + "/" + fakeAID,
		st: &runState{Contexts: map[string]string{}, Blocking: map[string]string{}, Immediate: map[string]string{},
			Stream: map[string]string{}}, rec: loadRecord("")}
	card := p.cardChecks(context.Background(), false)
	if card == nil {
		t.Fatalf("no card:\n%s", out.String())
	}
	p.payFlow(context.Background(), card, "paid", "pricey")
	if p.rep.fails != 0 {
		t.Fatalf("%d failures:\n%s", p.rep.fails, out.String())
	}
	for _, s := range []string{"NOTE pay-not-activated:", "PASS pay-over-limit-task:", "PASS pay:"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("no %q:\n%s", s, out.String())
		}
	}
}

// A probe against an interface that takes requests without a token must
// fail, not pass quietly.
func TestAnOpenInterfaceFails(t *testing.T) {
	f := startFake(t, false)
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+fakeToken)
		f.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer open.Close()
	var out bytes.Buffer
	p := &probe{rep: newReport(&out), agent: fakeAID, token: fakeToken, nonce: "n", timeout: 5 * time.Second,
		base: open.URL + agentsPath + "/" + fakeAID, st: &runState{}, rec: loadRecord("")}
	p.cardChecks(context.Background(), true)
	for _, id := range []string{"card-no-token", "card-resolver-no-token"} {
		if !strings.Contains(out.String(), "FAIL "+id+":") {
			t.Errorf("no FAIL %s:\n%s", id, out.String())
		}
	}
}

func TestRecorderCopiesOnlyWhenAsked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "body") }))
	defer srv.Close()
	hc := &http.Client{Transport: recorder{next: http.DefaultTransport}}
	var got []byte
	req, _ := http.NewRequestWithContext(recordInto(context.Background(), &got), http.MethodGet, srv.URL, nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "body" || string(b) != "body" {
		t.Fatalf("recorded %q, read %q", got, b)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL, nil)
	if resp, err = hc.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestResponderAnswersOncePerMessage(t *testing.T) {
	var mu sync.Mutex
	var replies []map[string]any
	fail := 1 // the first reply fails with a 503: retried
	tasks := `{"tasks":[
	 {"id":"t1","status":{"state":"TASK_STATE_SUBMITTED"},"history":[{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hello"}]}]},
	 {"id":"t2","status":{"state":"TASK_STATE_SUBMITTED"},"history":[{"messageId":"m2","role":"ROLE_USER","parts":[{"text":"[hold] wait"}]}]},
	 {"id":"t3","status":{"state":"TASK_STATE_COMPLETED"},"history":[{"messageId":"m3","role":"ROLE_USER","parts":[{"text":"done"}]}]},
	 {"id":"t4","status":{"state":"TASK_STATE_INPUT_REQUIRED"},"metadata":{"anet.skill":"x"},"history":[{"messageId":"m4","role":"ROLE_USER","parts":[{"data":{}}]}]},
	 {"id":"t5","status":{"state":"TASK_STATE_INPUT_REQUIRED"},"history":[{"messageId":"m5","role":"ROLE_AGENT","parts":[{"text":"?"}]}]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/tasks/list":
			io.WriteString(w, tasks)
		case "/tasks/reply":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			defer mu.Unlock()
			if fail > 0 {
				fail--
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			replies = append(replies, body)
			io.WriteString(w, "{}")
		}
	}))
	defer srv.Close()
	r := &responder{ctl: strings.TrimPrefix(srv.URL, "http://"), token: "tok", hold: holdMark,
		answered: map[string]string{}, hc: srv.Client()}
	for i := 0; i < 3; i++ {
		if err := r.once(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(replies) != 1 || replies[0]["task_id"] != "t1" || replies[0]["text"] != "echo: hello" || replies[0]["state"] != "completed" {
		t.Fatalf("replies %v", replies)
	}
	if r.answered["t2"] != "m2" {
		t.Fatalf("the held task is not marked: %v", r.answered)
	}
}

func TestBackendDigestsAndCounts(t *testing.T) {
	b := &backend{token: "svc", calls: map[string]int{}}
	srv := httptest.NewServer(b)
	defer srv.Close()
	post := func(auth string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"text":"abc"}`))
		req.Header.Set("X-ANet-Capability", "joint.digest.paid")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	for _, auth := range []string{"", "Bearer other", "Basic svc"} {
		if resp := post(auth); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST with %q: HTTP %d", auth, resp.StatusCode)
		} else {
			resp.Body.Close()
		}
	}
	resp := post("Bearer svc")
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out["digest"] != digestHex("abc") || out["length"] != float64(3) {
		t.Fatalf("reply %v", out)
	}
	resp, err := http.Get(srv.URL + "/calls")
	if err != nil {
		t.Fatal(err)
	}
	var calls map[string]int
	json.NewDecoder(resp.Body).Decode(&calls)
	resp.Body.Close()
	if calls["joint.digest.paid"] != 1 || len(calls) != 1 {
		t.Fatalf("calls %v", calls)
	}
}

func TestSmallPieces(t *testing.T) {
	if short(a2a.TaskStateInputRequired) != "input-required" || short(a2a.TaskStateCompleted) != "completed" {
		t.Error("short")
	}
	if o := otherAID("bafya"); o == "bafya" || len(o) != 5 {
		t.Errorf("otherAID %q", o)
	}
	if otherAID("bafyb") != "bafya" || otherAID("bafya") != "bafyb" {
		t.Error("otherAID")
	}
	if !sameIDs([]string{"a", "b"}, []string{"b", "a"}) || sameIDs([]string{"a"}, []string{"a", ""}) || sameIDs([]string{"a", "b"}, []string{"a"}) {
		t.Error("sameIDs")
	}
	for addr, ok := range map[string]bool{"127.0.0.1:1": true, "[::1]:1": true, "localhost:1": true, "0.0.0.0:1": false, "10.0.0.1:1": false, "x": false} {
		if (loopbackAddr(addr) == nil) != ok {
			t.Errorf("loopbackAddr(%q)", addr)
		}
	}
	card := &a2a.AgentCard{SecuritySchemes: a2a.NamedSecuritySchemes{
		"b": a2a.HTTPAuthSecurityScheme{Scheme: "bearer"}, "k": a2a.APIKeySecurityScheme{Name: "X-Key"}}}
	creds := credentials(card, "tok")
	if c, err := creds.Get(context.Background(), session, "b"); err != nil || c != "tok" {
		t.Errorf("bearer credential: %q %v", c, err)
	}
	if _, err := creds.Get(context.Background(), session, "k"); err == nil {
		t.Error("an API key scheme got the bearer token")
	}
	var r report
	r.w = io.Discard
	r.pass("a", "x\ny")
	r.fail("b", "z")
	if r.exitCode() != 1 {
		t.Error("exit code after a failure")
	}
}

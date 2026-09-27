//go:build !no_a2a

package a2a

// server_test.go drives the local A2A interface with a2a-go's own,
// unmodified client — card resolution with a bearer, AuthInterceptor and
// CredentialsService, both bindings — over a real HTTP listener, against a
// fake TaskSeam. What it pins is the A2A face: the routes, the checks of
// §11.4, the operation mapping of §11.5 and the card of §11.3. The kernel's
// side of the seam has its own tests.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/a2a/kelresolver"
)

const (
	testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	agentA    = "bafyagenta"
	agentB    = "bafyagentb"
	selfAID   = "bafyself"
)

// testSigner signs proxy cards the way the daemon does, with a controller
// of its own.
type testSigner struct{ c *identity.Controller }

func (s testSigner) SignProxyCard(card []byte) ([]byte, error) {
	return a2acard.SignWithController(card, s.c, "")
}

type env struct {
	t      *testing.T
	seam   *fakeSeam
	srv    *httptest.Server
	port   string
	signer testSigner
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctl, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, seam: newFakeSeam(), signer: testSigner{ctl}}
	e.srv = httptest.NewUnstartedServer(nil)
	_, e.port, _ = net.SplitHostPort(e.srv.Listener.Addr().String())
	e.srv.Config.Handler = newServer(e.seam, serverConfig{token: testToken, port: e.port, self: selfAID, signer: e.signer})
	e.srv.Start()
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) base(aid string) string { return e.srv.URL + agentsPath + "/" + aid }

// card resolves a proxy card the way a client configured with the agent's
// base URL does.
func (e *env) card(aid string) *a2a.AgentCard {
	e.t.Helper()
	c, err := agentcard.DefaultResolver.Resolve(context.Background(), e.base(aid),
		agentcard.WithRequestHeader("Authorization", "Bearer "+testToken))
	if err != nil {
		e.t.Fatalf("resolve card: %v", err)
	}
	return c
}

// client is an unmodified a2a-go client for aid over one binding, with the
// token in a CredentialsService.
func (e *env) client(aid string, binding a2a.TransportProtocol) (*a2aclient.Client, context.Context) {
	e.t.Helper()
	creds := a2aclient.NewInMemoryCredentialsStore()
	creds.Set("s1", securityScheme, testToken)
	hc := &http.Client{} // no timeout: blocking sends and streams
	cl, err := a2aclient.NewFromCard(context.Background(), e.card(aid),
		a2aclient.WithCallInterceptors(&a2aclient.AuthInterceptor{Service: creds}),
		a2aclient.WithConfig(a2aclient.Config{PreferredTransports: []a2a.TransportProtocol{binding}}),
		a2aclient.WithJSONRPCTransport(hc), a2aclient.WithRESTTransport(hc))
	if err != nil {
		e.t.Fatalf("client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	e.t.Cleanup(cancel)
	return cl, a2aclient.AttachSessionID(ctx, "s1")
}

func textMessage(s string) *a2a.Message {
	return a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(s))
}

func replyText(t *testing.T, task *a2a.Task) string {
	t.Helper()
	for _, a := range task.Artifacts {
		if a.ID == a2ashape.ArtifactReply && len(a.Parts) > 0 {
			return a.Parts[0].Text()
		}
	}
	return ""
}

var bindings = []a2a.TransportProtocol{a2a.TransportProtocolJSONRPC, a2a.TransportProtocolHTTPJSON}

func TestOperationsThroughBothBindings(t *testing.T) {
	for _, binding := range bindings {
		t.Run(string(binding), func(t *testing.T) {
			e := newEnv(t)
			cl, ctx := e.client(agentA, binding)

			// Blocking send: answers at the terminal state, with the reply.
			res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("hi")})
			if err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			task, ok := res.(*a2a.Task)
			if !ok || task.Status.State != a2a.TaskStateCompleted || replyText(t, task) != "echo: hi" {
				t.Fatalf("blocking send: %#v", res)
			}

			// Non-blocking send: answers at once; the task completes later.
			res, err = cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("later"),
				Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
			if err != nil {
				t.Fatalf("SendMessage returnImmediately: %v", err)
			}
			quick := res.(*a2a.Task)
			if quick.Status.State != a2a.TaskStateSubmitted {
				t.Fatalf("returnImmediately answered %s", quick.Status.State)
			}
			got := waitState(t, ctx, cl, quick.ID, a2a.TaskStateCompleted)
			if replyText(t, got) != "echo: later" {
				t.Fatalf("GetTask: %#v", got)
			}

			// Stream: the Task, working, the reply, completed — the reply
			// before the terminal status.
			var kinds []string
			for ev, err := range cl.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("stream")}) {
				if err != nil {
					t.Fatalf("stream: %v", err)
				}
				switch v := ev.(type) {
				case *a2a.Task:
					kinds = append(kinds, "task")
				case *a2a.TaskStatusUpdateEvent:
					kinds = append(kinds, string(v.Status.State))
				case *a2a.TaskArtifactUpdateEvent:
					kinds = append(kinds, "artifact:"+string(v.Artifact.ID))
				}
			}
			want := []string{"task", string(a2a.TaskStateWorking), "artifact:" + a2ashape.ArtifactReply, string(a2a.TaskStateCompleted)}
			if !slices.Equal(kinds, want) {
				t.Fatalf("stream events %v, want %v", kinds, want)
			}

			// ListTasks: this agent's tasks, artifacts left out by default.
			list, err := cl.ListTasks(ctx, &a2a.ListTasksRequest{})
			if err != nil {
				t.Fatalf("ListTasks: %v", err)
			}
			if list.TotalSize != 3 || len(list.Tasks) != 3 || list.PageSize != a2ashape.DefaultPageSize {
				t.Fatalf("ListTasks: %+v", list)
			}
			for _, lt := range list.Tasks {
				if len(lt.Artifacts) != 0 {
					t.Fatalf("ListTasks without includeArtifacts returned artifacts: %+v", lt)
				}
			}
			list, err = cl.ListTasks(ctx, &a2a.ListTasksRequest{IncludeArtifacts: true, ContextID: task.ContextID})
			if err != nil || len(list.Tasks) != 1 || replyText(t, list.Tasks[0]) != "echo: hi" {
				t.Fatalf("ListTasks by context with artifacts: %+v %v", list, err)
			}

			// SubscribeToTask on a finished task is UnsupportedOperation.
			for _, err := range cl.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: task.ID}) {
				if !errors.Is(err, a2a.ErrUnsupportedOperation) {
					t.Fatalf("subscribe to a completed task: %v", err)
				}
				break
			}

			// CancelTask: a task waiting for the client can be canceled,
			// once.
			e.seam.quote = map[string]any{"x402Version": 2, "accepts": []any{map[string]any{"scheme": "credit"}}}
			res, err = cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("quote me")})
			if err != nil {
				t.Fatal(err)
			}
			waiting := res.(*a2a.Task)
			if waiting.Status.State != a2a.TaskStateInputRequired {
				t.Fatalf("quoted task is %s", waiting.Status.State)
			}
			canceled, err := cl.CancelTask(ctx, &a2a.CancelTaskRequest{ID: waiting.ID})
			if err != nil || canceled.Status.State != a2a.TaskStateCanceled {
				t.Fatalf("CancelTask: %+v %v", canceled, err)
			}
			if _, err := cl.CancelTask(ctx, &a2a.CancelTaskRequest{ID: waiting.ID}); !errors.Is(err, a2a.ErrTaskNotCancelable) {
				t.Fatalf("second cancel: %v", err)
			}

			// Push notifications and the extended card are not offered.
			if _, err := cl.GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{TaskID: task.ID, ID: "x"}); !errors.Is(err, a2a.ErrPushNotificationNotSupported) {
				t.Fatalf("push config: %v", err)
			}
		})
	}
}

func waitState(t *testing.T, ctx context.Context, cl *a2aclient.Client, id a2a.TaskID, want a2a.TaskState) *a2a.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := cl.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Status.State == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s is %s, want %s", id, got.Status.State, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A client holding agent B's URL cannot reach agent A's tasks, and cannot
// tell them from tasks that do not exist [C17].
func TestAnotherAgentsTaskIsNotFound(t *testing.T) {
	for _, binding := range bindings {
		t.Run(string(binding), func(t *testing.T) {
			e := newEnv(t)
			clA, ctxA := e.client(agentA, binding)
			res, err := clA.SendMessage(ctxA, &a2a.SendMessageRequest{Message: textMessage("mine")})
			if err != nil {
				t.Fatal(err)
			}
			id := res.(*a2a.Task).ID
			clB, ctxB := e.client(agentB, binding)
			_, errOther := clB.GetTask(ctxB, &a2a.GetTaskRequest{ID: id})
			_, errNone := clB.GetTask(ctxB, &a2a.GetTaskRequest{ID: "task999"})
			if !errors.Is(errOther, a2a.ErrTaskNotFound) || !errors.Is(errNone, a2a.ErrTaskNotFound) {
				t.Fatalf("GetTask across agents: %v / %v", errOther, errNone)
			}
			if errOther.Error() != errNone.Error() {
				t.Fatalf("another agent's task reads differently from none: %q vs %q", errOther, errNone)
			}
			if _, err := clB.CancelTask(ctxB, &a2a.CancelTaskRequest{ID: id}); !errors.Is(err, a2a.ErrTaskNotFound) {
				t.Fatalf("CancelTask across agents: %v", err)
			}
			msg := textMessage("follow-up")
			msg.TaskID = id
			if _, err := clB.SendMessage(ctxB, &a2a.SendMessageRequest{Message: msg}); !errors.Is(err, a2a.ErrTaskNotFound) {
				t.Fatalf("SendMessage to another agent's task: %v", err)
			}
			list, err := clB.ListTasks(ctxB, &a2a.ListTasksRequest{})
			if err != nil || len(list.Tasks) != 0 {
				t.Fatalf("B lists A's tasks: %+v %v", list, err)
			}
		})
	}
}

func TestURLPartsAreRefused(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "https://example.com/x", "data:text/plain,hi"} {
		e := newEnv(t)
		cl, ctx := e.client(agentA, a2a.TransportProtocolJSONRPC)
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("read this"), a2a.NewFileURLPart(a2a.URL(u), "text/plain"))
		_, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg})
		if !errors.Is(err, a2a.ErrInvalidParams) {
			t.Fatalf("url part %s: %v", u, err)
		}
		if len(e.seam.sends) != 0 {
			t.Fatalf("url part %s reached the kernel", u)
		}
	}
}

// raw sends one HTTP request as a client without an SDK would.
func (e *env) raw(method, path string, hdr map[string]string, body string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range hdr {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

const rpcGet = `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"task1"}}`

func TestRequestChecks(t *testing.T) {
	e := newEnv(t)
	bearer := "Bearer " + testToken
	rpc := agentsPath + "/" + agentA + "/jsonrpc"
	cardPath := agentsPath + "/" + agentA + "/.well-known/agent-card.json"
	cases := []struct {
		name   string
		method string
		path   string
		hdr    map[string]string
		body   string
		code   int
	}{
		{"card without a token", "GET", cardPath, nil, "", 401},
		{"card with another token", "GET", cardPath, map[string]string{"Authorization": "Bearer control-token-is-not-this"}, "", 401},
		{"list without a token", "GET", agentsPath, nil, "", 401},
		{"rpc without a token", "POST", rpc, map[string]string{"Content-Type": "application/json"}, rpcGet, 401},
		{"basic auth", "GET", cardPath, map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("a:"+testToken))}, "", 401},
		{"foreign host", "GET", cardPath, map[string]string{"Authorization": bearer, "Host": "evil.example:" + e.port}, "", 421},
		{"loopback name, wrong port", "GET", cardPath, map[string]string{"Authorization": bearer, "Host": "127.0.0.1:1"}, "", 421},
		{"rebinding name", "GET", cardPath, map[string]string{"Authorization": bearer, "Host": "localtest.me:" + e.port}, "", 421},
		{"origin", "GET", cardPath, map[string]string{"Authorization": bearer, "Origin": "http://127.0.0.1:" + e.port}, "", 403},
		{"text/plain rpc", "POST", rpc, map[string]string{"Authorization": bearer, "Content-Type": "text/plain"}, rpcGet, 415},
		{"form rpc", "POST", rpc, map[string]string{"Authorization": bearer, "Content-Type": "application/x-www-form-urlencoded"}, rpcGet, 415},
		{"bad aid", "GET", agentsPath + "/NOT_AN_AID/.well-known/agent-card.json", map[string]string{"Authorization": bearer}, "", 404},
		{"this node itself", "GET", agentsPath + "/" + selfAID, map[string]string{"Authorization": bearer}, "", 404},
		// Every route, the REST binding and the bare card alias included,
		// is behind the same checks (SI-7).
		{"rest without a token", "GET", agentsPath + "/" + agentA + "/rest/tasks/task1", nil, "", 401},
		{"alias card without a token", "GET", agentsPath + "/" + agentA, nil, "", 401},
		{"console cookie, no bearer", "GET", cardPath, map[string]string{"Cookie": "anet_s_" + e.port + "=session"}, "", 401},
		{"control token", "POST", rpc, map[string]string{"Authorization": "Bearer " + strings.Repeat("c", 64), "Content-Type": "application/json"}, rpcGet, 401},
		{"list, foreign host", "GET", agentsPath, map[string]string{"Authorization": bearer, "Host": "evil.example:" + e.port}, "", 421},
		{"rest, foreign host", "GET", agentsPath + "/" + agentA + "/rest/tasks/task1", map[string]string{"Authorization": bearer, "Host": "evil.example:" + e.port}, "", 421},
		{"text/plain rest", "POST", agentsPath + "/" + agentA + "/rest/message:send", map[string]string{"Authorization": bearer, "Content-Type": "text/plain"}, `{"message":{}}`, 415},
		{"card", "GET", cardPath, map[string]string{"Authorization": bearer}, "", 200},
		{"card at localhost", "GET", cardPath, map[string]string{"Authorization": bearer, "Host": "localhost:" + e.port}, "", 200},
		{"rpc", "POST", rpc, map[string]string{"Authorization": bearer, "Content-Type": "application/json; charset=utf-8"}, rpcGet, 200},
	}
	for _, c := range cases {
		resp, body := e.raw(c.method, c.path, c.hdr, c.body)
		if resp.StatusCode != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, resp.StatusCode, body, c.code)
		}
		if c.code == 401 && resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: 401 without WWW-Authenticate", c.name)
		}
	}
}

// A client that has the token but does not attach it (no session for the
// AuthInterceptor) is refused, and a card without securityRequirements
// would leave every client in that position — so the card must carry
// them.
func TestClientWithoutCredentialsIsRefused(t *testing.T) {
	e := newEnv(t)
	card := e.card(agentA)
	if len(card.SecurityRequirements) == 0 || card.SecuritySchemes[securityScheme] == nil {
		t.Fatalf("proxy card has no security requirement: %+v", card)
	}
	cl, err := a2aclient.NewFromCard(context.Background(), card,
		a2aclient.WithCallInterceptors(&a2aclient.AuthInterceptor{Service: a2aclient.NewInMemoryCredentialsStore()}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = cl.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: textMessage("hi")})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("SendMessage without a token: %v", err)
	}
	if len(e.seam.sends) != 0 {
		t.Fatal("an unauthenticated message reached the kernel")
	}
}

func TestVersion(t *testing.T) {
	e := newEnv(t)
	hdr := func(v string) map[string]string {
		h := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json"}
		if v != "" {
			h["A2A-Version"] = v
		}
		return h
	}
	rpc := agentsPath + "/" + agentA + "/jsonrpc"
	rpcCode := func(path string, h map[string]string) int {
		resp, body := e.raw("POST", path, h, rpcGet)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: HTTP %d %s", path, resp.StatusCode, body)
		}
		var out struct {
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		if out.Error == nil {
			return 0
		}
		return out.Error.Code
	}
	// The task does not exist: -32001 means the version was accepted.
	for _, v := range []string{"", "1.0", "1", "1.2"} {
		if c := rpcCode(rpc, hdr(v)); c != -32001 {
			t.Errorf("A2A-Version %q: code %d, want -32001 (accepted)", v, c)
		}
	}
	for _, v := range []string{"0.3", "2.0", "1.0.0.0", "abc"} {
		if c := rpcCode(rpc, hdr(v)); c != -32009 {
			t.Errorf("A2A-Version %q: code %d, want -32009", v, c)
		}
	}
	if c := rpcCode(rpc+"?A2A-Version=0.3", hdr("")); c != -32009 {
		t.Errorf("query A2A-Version=0.3: code %d, want -32009", c)
	}
	resp, body := e.raw("GET", agentsPath+"/"+agentA+"/rest/tasks/task1", hdr("0.3"), "")
	if resp.StatusCode != 400 || !strings.Contains(string(body), "VERSION_NOT_SUPPORTED") {
		t.Errorf("REST A2A-Version 0.3: %d %s", resp.StatusCode, body)
	}
}

// verifiedRemote is a network card for agent A that this node verified,
// declaring a2a-x402 and anet-pricing.
func verifiedRemote(aid string) module.RemoteAgent {
	card := map[string]any{
		"name": "Echo", "description": "Echoes things.", "version": "1.2.0",
		"supportedInterfaces": []any{map[string]any{"url": "https://hub.example/relay", "protocolBinding": a2acard.BindingRelayURI,
			"protocolVersion": "1.0", "tenant": aid}},
		"capabilities": map[string]any{"streaming": false, "pushNotifications": false, "extensions": []any{
			map[string]any{"uri": a2ashape.X402ExtensionURI, "required": true},
			map[string]any{"uri": extPricingURI, "params": map[string]any{"network": "hub:x", "prices": []any{
				map[string]any{"skillId": "text.stats", "amount": "5"}}, "empty": ""}},
			map[string]any{"uri": a2acard.ExtCardURI, "params": map[string]any{"aid": aid, "seq": "1"}},
		}},
		"defaultInputModes": []any{"text/plain"}, "defaultOutputModes": []any{"application/json"},
		"skills": []any{map[string]any{"id": "text.stats", "name": "", "description": "", "tags": []any{}}},
	}
	b, _ := json.Marshal(card)
	return module.RemoteAgent{AID: aid, Card: b, Verification: verified}
}

func TestProxyCard(t *testing.T) {
	e := newEnv(t)
	e.seam.cards[agentA] = verifiedRemote(agentA)

	resp, raw := e.raw("GET", agentsPath+"/"+agentA+"/.well-known/agent-card.json", map[string]string{"Authorization": "Bearer " + testToken}, "")
	if resp.StatusCode != 200 {
		t.Fatalf("card: %d %s", resp.StatusCode, raw)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	if card.Name != "Echo" || card.Version != "1.2.0" || len(card.Skills) != 1 || card.Skills[0].ID != "text.stats" ||
		card.Skills[0].Name != "text.stats" || card.Skills[0].Description == "" || len(card.Skills[0].Tags) == 0 {
		t.Fatalf("copied fields: %+v", card)
	}
	if !card.Capabilities.Streaming || card.Capabilities.PushNotifications {
		t.Fatalf("capabilities: %+v", card.Capabilities)
	}
	for _, i := range card.SupportedInterfaces {
		if !strings.HasPrefix(i.URL, "http://127.0.0.1:"+e.port+agentsPath+"/"+agentA+"/") || i.Tenant != "" {
			t.Fatalf("interface %+v does not point here", i)
		}
	}
	uris := map[string]a2a.AgentExtension{}
	for _, x := range card.Capabilities.Extensions {
		uris[x.URI] = x
	}
	x402, ok := uris[a2ashape.X402ExtensionURI]
	if !ok || x402.Required || x402.Params["signer"] != "anet-daemon" || x402.Params["clientPayload"] != false {
		t.Fatalf("x402 declaration: %+v", x402)
	}
	if _, ok := uris[a2acard.ExtCardURI]; ok {
		t.Fatal("the remote's anet-card extension was copied into the proxy card")
	}
	if p, ok := uris[extPricingURI]; !ok || p.Params["network"] != "hub:x" || p.Params["empty"] != nil {
		t.Fatalf("anet-pricing: %+v", p)
	}
	origin := uris[ExtOriginURI].Params
	if origin["aid"] != agentA || origin["originVerification"] != verified {
		t.Fatalf("anet-origin: %+v", origin)
	}
	if b, err := base64.RawURLEncoding.DecodeString(origin["originCard"].(string)); err != nil || string(b) != string(verifiedRemote(agentA).Card) {
		t.Fatalf("originCard does not carry the remote card: %v", err)
	}
	// Publish form: nothing at its default value where it could be left
	// out (§10.1) — "required": false above all.
	if strings.Contains(string(raw), `"required":false`) || strings.Contains(string(raw), `""`) || strings.Contains(string(raw), `[]`) {
		t.Fatalf("card is not in publish form: %s", raw)
	}

	// Signed by this node, and a2a-go verifies it with the KEL resolver,
	// also after its own parse-and-reserialize.
	if len(card.Signatures) != 1 {
		t.Fatalf("signatures: %+v", card.Signatures)
	}
	res := kelresolver.Resolver{KEL: func(_ context.Context, aid string) ([]identity.SignedEvent, error) {
		if aid != e.signer.c.AID() {
			return nil, kelresolver.ErrUnknownAID
		}
		return e.signer.c.KEL(), nil
	}}
	v := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: res})
	if err := v.Verify(context.Background(), raw, &card.Signatures[0]); err != nil {
		t.Fatalf("a2a-go does not verify the proxy card: %v", err)
	}
	again, _ := json.Marshal(card)
	if err := v.Verify(context.Background(), again, &card.Signatures[0]); err != nil {
		t.Fatalf("a2a-go does not verify its own re-serialization: %v", err)
	}

	// No card, or one this node could not verify: a placeholder that says
	// so, carrying whatever bytes there were.
	e.seam.cards[agentB] = module.RemoteAgent{AID: agentB, Name: "hub name", Card: []byte(`{"name":"claims"}`), Verification: unverified}
	pb := e.card(agentB)
	if pb.Name != "hub name" || len(pb.Skills) != 1 || pb.Skills[0].ID != "chat" || !strings.Contains(pb.Description, "verify") {
		t.Fatalf("placeholder: %+v", pb)
	}
	for _, x := range pb.Capabilities.Extensions {
		if x.URI == a2ashape.X402ExtensionURI {
			t.Fatal("placeholder declares x402")
		}
		if x.URI == ExtOriginURI && (x.Params["originVerification"] != unverified || x.Params["originCard"] == nil) {
			t.Fatalf("placeholder origin: %+v", x.Params)
		}
	}
}

func TestExtensionsAreMergedAndEchoed(t *testing.T) {
	e := newEnv(t)
	e.seam.cards[agentA] = verifiedRemote(agentA)
	body := `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`
	resp, raw := e.raw("POST", agentsPath+"/"+agentA+"/jsonrpc", map[string]string{
		"Authorization": "Bearer " + testToken, "Content-Type": "application/json",
		"A2A-Extensions":   ExtOriginURI + ", https://unknown.example/ext",
		"X-A2A-Extensions": a2ashape.X402ExtensionURI,
	}, body)
	if resp.StatusCode != 200 {
		t.Fatalf("send: %d %s", resp.StatusCode, raw)
	}
	echo := strings.Split(resp.Header.Get("A2A-Extensions"), ", ")
	if !slices.Equal(echo, []string{ExtOriginURI, a2ashape.X402ExtensionURI}) {
		t.Fatalf("echoed extensions %q", resp.Header.Get("A2A-Extensions"))
	}
	if len(e.seam.sends) != 1 || !slices.Equal(e.seam.sends[0].Extensions, echo) {
		t.Fatalf("the kernel was told %v", e.seam.sends)
	}
	// An agent that declares nothing but anet-origin echoes only that.
	resp, _ = e.raw("POST", agentsPath+"/"+agentB+"/jsonrpc", map[string]string{
		"Authorization": "Bearer " + testToken, "Content-Type": "application/json",
		"X-A2A-Extensions": a2ashape.X402ExtensionURI + "," + ExtOriginURI,
	}, body)
	if got := resp.Header.Get("A2A-Extensions"); got != ExtOriginURI {
		t.Fatalf("agent B echoed %q", got)
	}
}

func TestAgentList(t *testing.T) {
	e := newEnv(t)
	e.seam.agents = []module.RemoteAgent{{AID: agentA, Name: "Echo", Verification: verified}, {AID: selfAID}, {AID: "../x"}}
	resp, raw := e.raw("GET", agentsPath, map[string]string{"Authorization": "Bearer " + testToken}, "")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var out agentList
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Agents) != 1 || out.Agents[0].AID != agentA || out.Agents[0].URL != "http://127.0.0.1:"+e.port+agentsPath+"/"+agentA ||
		!strings.HasSuffix(out.Agents[0].CardURL, "/.well-known/agent-card.json") {
		t.Fatalf("agents: %+v", out)
	}
}

// §8.7: the local client says payment-submitted and the daemon signs.
func TestPaymentMessages(t *testing.T) {
	option := map[string]any{"scheme": "credit", "network": "hub:x", "amount": "5", "payTo": agentA, "maxTimeoutSeconds": 60}
	start := func(t *testing.T) (*env, *a2aclient.Client, context.Context, *a2a.Task) {
		e := newEnv(t)
		e.seam.quote = map[string]any{"x402Version": 2, "accepts": []any{option}}
		cl, ctx := e.client(agentA, a2a.TransportProtocolJSONRPC)
		res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("paid work")})
		if err != nil {
			t.Fatal(err)
		}
		task := res.(*a2a.Task)
		if task.Status.State != a2a.TaskStateInputRequired || task.Status.Message.Metadata[a2ashape.KeyX402Status] != a2ashape.PaymentRequired {
			t.Fatalf("quote: %+v", task.Status)
		}
		return e, cl, ctx, task
	}
	payMsg := func(task *a2a.Task, meta map[string]any) *a2a.Message {
		m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("paying"))
		m.TaskID, m.ContextID, m.Metadata = task.ID, task.ContextID, meta
		return m
	}

	t.Run("submitted", func(t *testing.T) {
		e, cl, ctx, task := start(t)
		res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: payMsg(task, map[string]any{
			a2ashape.KeyX402Status: a2ashape.PaymentSubmitted, a2ashape.KeyPaymentAccept: option})})
		if err != nil {
			t.Fatal(err)
		}
		done := res.(*a2a.Task)
		if done.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("a blocking payment answered at %s", done.Status.State)
		}
		if len(e.seam.pays) != 1 || e.seam.pays[0].Decision != module.PaySubmit {
			t.Fatalf("pays: %+v", e.seam.pays)
		}
		var accept map[string]any
		if err := json.Unmarshal(e.seam.pays[0].Accept, &accept); err != nil || accept["payTo"] != agentA {
			t.Fatalf("accept passed on as %s", e.seam.pays[0].Accept)
		}
		if len(e.seam.sends) != 1 {
			t.Fatal("the payment message was also sent as a chat message")
		}
	})
	t.Run("client payload", func(t *testing.T) {
		e, cl, ctx, task := start(t)
		res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: payMsg(task, map[string]any{
			a2ashape.KeyX402Status: a2ashape.PaymentSubmitted, a2ashape.KeyX402Payload: map[string]any{"x402Version": 2}})})
		if err != nil {
			t.Fatal(err)
		}
		checkRefused(t, res.(*a2a.Task), reasonClientPayloadUnsupported)
		if len(e.seam.pays) != 0 {
			t.Fatal("a client payload reached Pay")
		}
	})
	t.Run("option not offered", func(t *testing.T) {
		e, cl, ctx, task := start(t)
		other := map[string]any{"scheme": "credit", "network": "hub:x", "amount": "1", "payTo": agentA, "maxTimeoutSeconds": 60}
		res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: payMsg(task, map[string]any{
			a2ashape.KeyX402Status: a2ashape.PaymentSubmitted, a2ashape.KeyPaymentAccept: other})})
		if err != nil {
			t.Fatal(err)
		}
		checkRefused(t, res.(*a2a.Task), reasonOptionNotOffered)
		if len(e.seam.pays) != 0 {
			t.Fatal("an option not offered reached Pay")
		}
	})
	t.Run("rejected", func(t *testing.T) {
		e, cl, ctx, task := start(t)
		res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: payMsg(task, map[string]any{
			a2ashape.KeyX402Status: a2ashape.PaymentRejected})})
		if err != nil {
			t.Fatal(err)
		}
		if res.(*a2a.Task).Status.State != a2a.TaskStateCanceled || len(e.seam.pays) != 1 || e.seam.pays[0].Decision != module.PayReject {
			t.Fatalf("reject: %+v %+v", res, e.seam.pays)
		}
	})
	t.Run("other status", func(t *testing.T) {
		_, cl, ctx, task := start(t)
		_, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: payMsg(task, map[string]any{
			a2ashape.KeyX402Status: a2ashape.PaymentCompleted})})
		if !errors.Is(err, a2a.ErrInvalidParams) {
			t.Fatalf("payment-completed from a client: %v", err)
		}
	})
}

func checkRefused(t *testing.T, task *a2a.Task, reason string) {
	t.Helper()
	m := task.Status.Message
	if task.Status.State != a2a.TaskStateInputRequired || m == nil ||
		m.Metadata[a2ashape.KeyX402Status] != a2ashape.PaymentFailed ||
		m.Metadata[a2ashape.KeyX402Error] != codeSettlementFailed || m.Metadata[a2ashape.KeyReason] != reason {
		t.Fatalf("refusal (%s): %+v", reason, task.Status)
	}
}

// A stream whose kernel sends the terminal status without the artifacts
// still delivers the reply first.
func TestStreamSendsArtifactsBeforeTheEnd(t *testing.T) {
	e := newEnv(t)
	cl, ctx := e.client(agentA, a2a.TransportProtocolHTTPJSON)
	e.seam.quote = map[string]any{"accepts": []any{}}
	res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("q")})
	if err != nil {
		t.Fatal(err)
	}
	id := string(res.(*a2a.Task).ID)
	go func() {
		time.Sleep(50 * time.Millisecond)
		e.seam.mu.Lock()
		defer e.seam.mu.Unlock()
		ft := e.seam.tasks[id]
		ft.t.Artifacts = []a2ashape.Artifact{{ID: a2ashape.ArtifactReply, Parts: []a2ashape.Part{a2ashape.TextPart("late")}}}
		e.seam.setState(ft, a2ashape.TaskStateCompleted, nil)
		e.seam.status(ft)
	}()
	var kinds []string
	for ev, err := range cl.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: a2a.TaskID(id)}) {
		if err != nil {
			t.Fatal(err)
		}
		switch v := ev.(type) {
		case *a2a.Task:
			kinds = append(kinds, "task")
		case *a2a.TaskArtifactUpdateEvent:
			kinds = append(kinds, "artifact")
		case *a2a.TaskStatusUpdateEvent:
			kinds = append(kinds, string(v.Status.State))
		}
	}
	if want := []string{"task", "artifact", string(a2a.TaskStateCompleted)}; !slices.Equal(kinds, want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
}

// A file carried inline reaches the kernel as a raw part, with its name
// and type, where it becomes an attachment [C44].
func TestRawPartsReachTheKernel(t *testing.T) {
	e := newEnv(t)
	cl, ctx := e.client(agentA, a2a.TransportProtocolHTTPJSON)
	raw := a2a.NewRawPart([]byte{0, 1, 2, 0xff})
	raw.Filename, raw.MediaType = "b.bin", "application/octet-stream"
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("see attached"), raw)
	if _, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg, Metadata: map[string]any{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	got := e.seam.sends[0]
	p := got.Message.Parts[1]
	if p.Kind != a2ashape.PartRaw || string(p.Raw) != "\x00\x01\x02\xff" || p.Filename != "b.bin" || p.MediaType != "application/octet-stream" {
		t.Fatalf("raw part arrived as %+v", p)
	}
	if got.Message.ID != msg.ID || got.Metadata["k"] != "v" {
		t.Fatalf("message id or request metadata lost: %+v", got)
	}
}

// A body over the limit is refused before a binding reads it (§11.4).
func TestBodyLimit(t *testing.T) {
	e := newEnv(t)
	s := e.srv.Config.Handler
	for _, path := range []string{agentsPath + "/" + agentA + "/jsonrpc", agentsPath + "/" + agentA + "/rest/message:send"} {
		req := httptest.NewRequest("POST", "http://127.0.0.1:"+e.port+path, strings.NewReader(rpcGet))
		req.Host = "127.0.0.1:" + e.port
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = maxBody + 1
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: %d %s, want 413", path, rec.Code, rec.Body)
		}
	}
	if len(e.seam.sends) != 0 {
		t.Fatal("an oversized request reached the kernel")
	}
}

// An agent the kernel calls verified but whose card bytes are missing gets
// a placeholder, and the placeholder does not call its origin verified.
func TestProxyCardVerifiedWithoutBytes(t *testing.T) {
	long := strings.Repeat("é", 100) // 200 bytes: cut to 128 between characters
	card, _ := cardBuilder{port: "1"}.build(module.RemoteAgent{AID: agentA, Name: long, Verification: verified})
	if n := card["name"].(string); len(n) > maxNameBytes || !utf8.ValidString(n) || !strings.HasPrefix(long, n) {
		t.Fatalf("placeholder name %q (%d bytes)", n, len(n))
	}
	if card["skills"].([]any)[0].(map[string]any)["id"] != "chat" {
		t.Fatalf("not a placeholder: %+v", card)
	}
	for _, x := range card["capabilities"].(map[string]any)["extensions"].([]any) {
		x := x.(map[string]any)
		if x["uri"] == ExtOriginURI && x["params"].(map[string]any)["originVerification"] != unverified {
			t.Fatalf("a placeholder claims a verified origin: %+v", x)
		}
	}
}

// The URLs in a card reach the listener where it is bound: an interface on
// [::1] is not at 127.0.0.1.
func TestCardURLsNameTheListener(t *testing.T) {
	card, _ := cardBuilder{host: "::1", port: "43811"}.build(module.RemoteAgent{AID: agentA})
	for _, i := range card["supportedInterfaces"].([]any) {
		u := i.(map[string]any)["url"].(string)
		if !strings.HasPrefix(u, "http://[::1]:43811"+agentsPath+"/"+agentA+"/") {
			t.Fatalf("interface %s", u)
		}
	}
	s := &server{cfg: serverConfig{host: "::1", port: "43811"}}
	if got := s.baseURL(agentA); got != "http://[::1]:43811"+agentsPath+"/"+agentA {
		t.Fatalf("base URL %s", got)
	}
}

// Only the A2A error reaches the client, not the text of what wraps it.
func TestWrappedErrorsDoNotLeak(t *testing.T) {
	err := toSDKError(fmt.Errorf("store at /home/x/.anet: %w", a2a.NewError(a2a.ErrInvalidParams, "bad input")))
	if !errors.Is(err, a2a.ErrInvalidParams) || err.Error() != "bad input" {
		t.Fatalf("%v", err)
	}
	err = toSDKError(fmt.Errorf("hub said %q: %w", "secret", a2ashape.Errorf(a2ashape.ErrTaskNotFound, "ix at peer")))
	if !errors.Is(err, a2a.ErrTaskNotFound) || err.Error() != "task not found" {
		t.Fatalf("%v", err)
	}
}

// A new task that is already waiting for the client when the send returns —
// the agent asked, or quoted, before the stream began — ends the stream at
// once with that task, rather than holding it open for a newer state.
func TestStreamOfATaskThatAlreadyAsked(t *testing.T) {
	for _, binding := range bindings {
		t.Run(string(binding), func(t *testing.T) {
			e := newEnv(t)
			e.seam.quote = map[string]any{"x402Version": 2, "accepts": []any{map[string]any{"scheme": "credit"}}}
			cl, ctx := e.client(agentA, binding)
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			var states []a2a.TaskState
			for ev, err := range cl.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("quote me")}) {
				if err != nil {
					t.Fatalf("stream: %v (after %v)", err, states)
				}
				if task, ok := ev.(*a2a.Task); ok {
					states = append(states, task.Status.State)
				}
			}
			if !slices.Equal(states, []a2a.TaskState{a2a.TaskStateInputRequired}) {
				t.Fatalf("stream %v", states)
			}
		})
	}
}

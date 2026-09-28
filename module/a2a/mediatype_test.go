//go:build !no_a2a

package a2a

// The media types of the two bindings (A2A v1.0.1): a request body may be
// application/json or application/a2a+json, parameters such as charset
// allowed, on either binding (§9.1, §11.1, §14.1.1); anything else is
// ContentTypeNotSupportedError with HTTP 400 (§5.4). The REST binding's
// JSON answers take application/a2a+json when the client prefers it
// (§11.1 SHOULD) and application/json otherwise, which is what a2a-go's
// client asks for and the only type its REST client reads an error from;
// JSON-RPC answers are application/json (§9.1).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

const restSend = `{"message":{"messageId":"m-media","role":"ROLE_USER","parts":[{"text":"hi"}]}}`

func TestRESTAcceptsA2AJSON(t *testing.T) {
	e := newEnv(t)
	bearer := "Bearer " + testToken
	send := agentsPath + "/" + agentA + "/rest/message:send"
	for i, c := range []struct{ contentType, want string }{
		{"application/a2a+json", "application/a2a+json"},
		{"application/a2a+json; charset=utf-8", "application/a2a+json"},
		{"APPLICATION/A2A+JSON", "application/a2a+json"},
		{"application/json", "application/json"},
		{"application/json; charset=UTF-8", "application/json"},
	} {
		body := strings.Replace(restSend, "m-media", "m-media-"+string(rune('a'+i)), 1)
		resp, out := e.raw("POST", send, map[string]string{"Authorization": bearer, "Content-Type": c.contentType}, body)
		var res struct {
			Task *struct {
				ID     string `json:"id"`
				Status struct {
					State string `json:"state"`
				} `json:"status"`
			} `json:"task"`
		}
		if resp.StatusCode != 200 || json.Unmarshal(out, &res) != nil || res.Task == nil || res.Task.Status.State != "TASK_STATE_COMPLETED" {
			t.Errorf("%s: %d %s, want the completed task", c.contentType, resp.StatusCode, out)
			continue
		}
		// No Accept: the answer is of the type the request was.
		if got := resp.Header.Get("Content-Type"); got != c.want {
			t.Errorf("%s: answered as %q, want %q", c.contentType, got, c.want)
		}
	}

	// A body of either type, with an Accept that rates both JSON types the
	// same: the one Accept names decides, application/a2a+json if it names
	// both, and the body's type only if it names neither.
	for i, c := range []struct{ contentType, accept, want string }{
		{"application/a2a+json", "*/*", "application/a2a+json"},
		{"application/a2a+json", "application/*", "application/a2a+json"},
		{"application/a2a+json", "application/json, */*", "application/json"},
		{"application/a2a+json", "application/json, text/plain, */*", "application/json"},
		{"application/a2a+json", "application/json, application/*", "application/json"},
		{"application/a2a+json", "application/json;q=0.5, application/*;q=0.5", "application/json"},
		{"application/json", "application/a2a+json, */*", "application/a2a+json"},
		{"application/json", "application/a2a+json, application/json, */*", "application/a2a+json"},
		{"application/json", "*/*", "application/json"},
	} {
		body := strings.Replace(restSend, "m-media", "m-media-tie-"+string(rune('a'+i)), 1)
		resp, out := e.raw("POST", send, map[string]string{"Authorization": bearer, "Content-Type": c.contentType, "Accept": c.accept}, body)
		if resp.StatusCode != 200 || !strings.Contains(string(out), `"TASK_STATE_COMPLETED"`) {
			t.Errorf("%s, Accept %q: %d %s", c.contentType, c.accept, resp.StatusCode, out)
			continue
		}
		if got := resp.Header.Get("Content-Type"); got != c.want {
			t.Errorf("%s, Accept %q: answered as %q, want %q", c.contentType, c.accept, got, c.want)
		}
	}
}

func TestRESTAnswerMediaType(t *testing.T) {
	e := newEnv(t)
	bearer := "Bearer " + testToken
	cl, ctx := e.client(agentA, a2a.TransportProtocolHTTPJSON)
	res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	get := agentsPath + "/" + agentA + "/rest/tasks/" + string(res.(*a2a.Task).ID)

	for _, c := range []struct{ accept, want string }{
		{"", "application/json"},
		{"*/*", "application/json"},
		{"application/*", "application/json"},
		{"application/json", "application/json"},
		{"application/a2a+json", "application/a2a+json"},
		{"application/a2a+json, application/json", "application/a2a+json"},
		{"application/json, application/a2a+json", "application/a2a+json"},
		{"application/*, application/a2a+json", "application/a2a+json"},
		{"application/json, */*;q=0.1", "application/json"},
		{"application/a2a+json;q=0.5, application/json", "application/json"},
		{"application/json;q=0.5, application/a2a+json", "application/a2a+json"},
		{"application/a2a+json;q=0, */*", "application/json"},
		{"text/html", "application/json"},
		{"application/a2a+json;q=nonsense, application/json", "application/json"},
	} {
		hdr := map[string]string{"Authorization": bearer}
		if c.accept != "" {
			hdr["Accept"] = c.accept
		}
		resp, out := e.raw("GET", get, hdr, "")
		if resp.StatusCode != 200 || !strings.Contains(string(out), `"TASK_STATE_COMPLETED"`) {
			t.Errorf("Accept %q: %d %s", c.accept, resp.StatusCode, out)
			continue
		}
		if got := resp.Header.Get("Content-Type"); got != c.want {
			t.Errorf("Accept %q: answered as %q, want %q", c.accept, got, c.want)
		}
	}

	// Errors take the same type, whoever writes them: a2a-go's REST binding
	// (a task that does not exist), this package before the binding (a body
	// type it does not take), and the precheck of a stream (0017 Q31).
	a2aJSON := map[string]string{"Authorization": bearer, "Accept": "application/a2a+json"}
	for _, c := range []struct {
		name, method, path, contentType, body string
		code                                  int
		reason                                string
	}{
		{"binding", "GET", agentsPath + "/" + agentA + "/rest/tasks/nope", "", "", 404, "TASK_NOT_FOUND"},
		{"body type", "POST", agentsPath + "/" + agentA + "/rest/message:send", "text/plain", restSend, 400, "CONTENT_TYPE_NOT_SUPPORTED"},
		{"stream precheck", "POST", agentsPath + "/" + agentA + "/rest/tasks/nope:subscribe", "", "", 404, "TASK_NOT_FOUND"},
		{"no such agent", "GET", agentsPath + "/" + selfAID + "/rest/tasks/x", "", "", 404, "INVALID_REQUEST"},
	} {
		hdr := map[string]string{}
		for k, v := range a2aJSON {
			hdr[k] = v
		}
		if c.contentType != "" {
			hdr["Content-Type"] = c.contentType
		}
		resp, out := e.raw(c.method, c.path, hdr, c.body)
		var st struct {
			Error struct {
				Code    int `json:"code"`
				Details []struct {
					Reason string `json:"reason"`
				} `json:"details"`
			} `json:"error"`
		}
		if resp.StatusCode != c.code || json.Unmarshal(out, &st) != nil || st.Error.Code != c.code ||
			len(st.Error.Details) == 0 || st.Error.Details[0].Reason != c.reason {
			t.Errorf("%s: %d %s, want %d %s", c.name, resp.StatusCode, out, c.code, c.reason)
		}
		if got := resp.Header.Get("Content-Type"); got != "application/a2a+json" {
			t.Errorf("%s: error answered as %q, want application/a2a+json", c.name, got)
		}
	}

	// A stream stays a stream.
	resp, out := e.raw("POST", agentsPath+"/"+agentA+"/rest/message:stream",
		map[string]string{"Authorization": bearer, "Content-Type": "application/a2a+json", "Accept": "text/event-stream"},
		strings.Replace(restSend, "m-media", "m-media-stream", 1))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || !strings.Contains(string(out), "data:") {
		t.Errorf("stream: %d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), out)
	}
}

// The JSON-RPC binding takes application/a2a+json too, and answers
// application/json whatever the client accepts (A2A §9.1).
func TestJSONRPCAnswersJSON(t *testing.T) {
	e := newEnv(t)
	rpc := agentsPath + "/" + agentA + "/jsonrpc"
	for i, accept := range []string{"", "application/a2a+json", "application/json"} {
		hdr := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/a2a+json"}
		if accept != "" {
			hdr["Accept"] = accept
		}
		send := `{"jsonrpc":"2.0","id":7,"method":"SendMessage","params":` +
			strings.Replace(restSend, "m-media", "m-media-rpc-"+string(rune('a'+i)), 1) + `}`
		resp, out := e.raw("POST", rpc, hdr, send)
		var res struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if resp.StatusCode != 200 || json.Unmarshal(out, &res) != nil || res.ID != 7 || !strings.Contains(string(res.Result), `"TASK_STATE_COMPLETED"`) {
			t.Errorf("Accept %q: %d %s", accept, resp.StatusCode, out)
		}
		if got := resp.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Accept %q: answered as %q, want application/json", accept, got)
		}
	}
}

// retype is an http.RoundTripper that makes a2a-go's client send its
// requests with other media types — as a client that follows A2A v1.0.1
// §11.1 does — and records the Content-Type of each answer.
type retype struct {
	contentType string // replaces a2a-go's application/json request body type
	accept      string // replaces a2a-go's "Accept: application/json"; "" keeps it
	mu          sync.Mutex
	answers     []string
}

func (rt *retype) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if req.Header.Get("Content-Type") != "" {
		req.Header.Set("Content-Type", rt.contentType)
	}
	if rt.accept != "" && req.Header.Get("Accept") == "application/json" {
		req.Header.Set("Accept", rt.accept)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil {
		rt.mu.Lock()
		rt.answers = append(rt.answers, resp.Header.Get("Content-Type"))
		rt.mu.Unlock()
	}
	return resp, err
}

func (rt *retype) seen() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return slices.Clone(rt.answers)
}

// clientVia is e.client over hc.
func (e *env) clientVia(aid string, binding a2a.TransportProtocol, hc *http.Client) (*a2aclient.Client, context.Context) {
	e.t.Helper()
	creds := a2aclient.NewInMemoryCredentialsStore()
	creds.Set("s1", securityScheme, testToken)
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

// a2a-go's own client, its request bodies sent as application/a2a+json,
// round-trips on both bindings: sends, reads, streams, and reads a named
// error. Its REST client asks for application/json and is answered so —
// it reads an error only from a body of that type.
func TestA2AGoClientSendingA2AJSON(t *testing.T) {
	for _, binding := range bindings {
		t.Run(string(binding), func(t *testing.T) {
			e := newEnv(t)
			rt := &retype{contentType: "application/a2a+json; charset=utf-8"}
			cl, ctx := e.clientVia(agentA, binding, &http.Client{Transport: rt})

			res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("hi")})
			if err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			task, ok := res.(*a2a.Task)
			if !ok || task.Status.State != a2a.TaskStateCompleted || replyText(t, task) != "echo: hi" {
				t.Fatalf("SendMessage: %#v", res)
			}
			got, err := cl.GetTask(ctx, &a2a.GetTaskRequest{ID: task.ID})
			if err != nil || replyText(t, got) != "echo: hi" {
				t.Fatalf("GetTask: %#v %v", got, err)
			}
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
			if _, err := cl.CancelTask(ctx, &a2a.CancelTaskRequest{ID: task.ID}); !errors.Is(err, a2a.ErrTaskNotCancelable) {
				t.Fatalf("cancel a completed task: %v", err)
			}
			if _, err := cl.GetTask(ctx, &a2a.GetTaskRequest{ID: "nope"}); !errors.Is(err, a2a.ErrTaskNotFound) {
				t.Fatalf("GetTask of no task: %v", err)
			}
			for _, ct := range rt.seen() {
				if ct != "application/json" && ct != "text/event-stream" {
					t.Errorf("answered as %q", ct)
				}
			}
		})
	}
}

// Asked for application/a2a+json, the REST binding answers in it, and
// a2a-go's REST client, which decodes a success whatever its type, reads
// the answers.
func TestA2AGoClientAcceptingA2AJSON(t *testing.T) {
	e := newEnv(t)
	rt := &retype{contentType: "application/a2a+json", accept: "application/a2a+json"}
	cl, ctx := e.clientVia(agentA, a2a.TransportProtocolHTTPJSON, &http.Client{Transport: rt})
	res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("hi")})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := res.(*a2a.Task)
	got, err := cl.GetTask(ctx, &a2a.GetTaskRequest{ID: task.ID})
	if err != nil || replyText(t, got) != "echo: hi" {
		t.Fatalf("GetTask: %#v %v", got, err)
	}
	list, err := cl.ListTasks(ctx, &a2a.ListTasksRequest{})
	if err != nil || len(list.Tasks) != 1 {
		t.Fatalf("ListTasks: %+v %v", list, err)
	}
	if seen := rt.seen(); len(seen) != 3 || slices.ContainsFunc(seen, func(ct string) bool { return ct != "application/a2a+json" }) {
		t.Fatalf("answered as %q, want application/a2a+json three times", seen)
	}
}

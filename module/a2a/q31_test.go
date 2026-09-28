//go:build !no_a2a

package a2a

// 0017 Q31 (a2a-tck STREAM-SUB-003/004): a streaming call that cannot start
// is answered with an ordinary error in the binding's form — a JSON-RPC
// error response, or the HTTP+JSON binding's google.rpc.Status — and not
// with an SSE stream whose one event is the error. a2a-go writes a stream's
// headers before it asks the handler for anything, so the checks run
// before the binding (precheck.go).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestAStreamThatCannotStartIsAnOrdinaryError(t *testing.T) {
	e := newEnv(t)
	cl, ctx := e.client(agentA, a2a.TransportProtocolJSONRPC)
	res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	done := string(res.(*a2a.Task).ID) // the fake agent answers at once: completed
	// agentB's task, which agentA's endpoint must not find.
	clB, ctxB := e.client(agentB, a2a.TransportProtocolJSONRPC)
	resB, err := clB.SendMessage(ctxB, &a2a.SendMessageRequest{Message: textMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	other := string(resB.(*a2a.Task).ID)

	hdr := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json",
		"Accept": "text/event-stream"}
	rpc := agentsPath + "/" + agentA + "/jsonrpc"
	rest := agentsPath + "/" + agentA + "/rest"
	cases := []struct {
		name         string
		method, path string
		body         string
		code         int // JSON-RPC error code; 0 for the HTTP+JSON binding
		status       int
		reason       string
	}{
		{"subscribe, no such task (rpc)", "POST", rpc,
			`{"jsonrpc":"2.0","id":"r1","method":"SubscribeToTask","params":{"id":"nope"}}`, -32001, 404, "TASK_NOT_FOUND"},
		{"subscribe, another agent's task (rpc)", "POST", rpc,
			`{"jsonrpc":"2.0","id":"r2","method":"SubscribeToTask","params":{"id":"` + other + `"}}`, -32001, 404, "TASK_NOT_FOUND"},
		{"subscribe, ended (rpc)", "POST", rpc,
			`{"jsonrpc":"2.0","id":3,"method":"SubscribeToTask","params":{"id":"` + done + `"}}`, -32004, 400, "UNSUPPORTED_OPERATION"},
		{"stream a message to no such task (rpc)", "POST", rpc,
			`{"jsonrpc":"2.0","id":"r4","method":"SendStreamingMessage","params":{"message":{"messageId":"m4","role":"ROLE_USER",` +
				`"taskId":"nope","parts":[{"text":"x"}]}}}`, -32001, 404, "TASK_NOT_FOUND"},
		{"stream a message with a url part (rpc)", "POST", rpc,
			`{"jsonrpc":"2.0","id":"r5","method":"SendStreamingMessage","params":{"message":{"messageId":"m5","role":"ROLE_USER",` +
				`"parts":[{"url":"file:///etc/passwd"}]}}}`, -32602, 400, "INVALID_PARAMS"},
		{"subscribe, no such task (rest)", "GET", rest + "/tasks/nope:subscribe", "", 0, 404, "TASK_NOT_FOUND"},
		{"subscribe, ended (rest, POST)", "POST", rest + "/tasks/" + done + ":subscribe", "", 0, 400, "UNSUPPORTED_OPERATION"},
		{"stream a message to an ended task (rest)", "POST", rest + "/message:stream",
			`{"message":{"messageId":"m8","role":"ROLE_USER","taskId":"` + done + `","parts":[{"text":"x"}]}}`, 0, 400, "UNSUPPORTED_OPERATION"},
	}
	for _, c := range cases {
		resp, body := e.raw(c.method, c.path, hdr, c.body)
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Errorf("%s: answered with a stream: %s", c.name, body)
			continue
		}
		if resp.StatusCode != c.status || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Errorf("%s: %d %s: %s", c.name, resp.StatusCode, resp.Header.Get("Content-Type"), body)
			continue
		}
		if c.code != 0 {
			var r struct {
				JSONRPC string `json:"jsonrpc"`
				ID      any    `json:"id"`
				Error   struct {
					Code int `json:"code"`
					Data []struct {
						Reason string `json:"reason"`
					} `json:"data"`
				} `json:"error"`
			}
			var req struct {
				ID any `json:"id"`
			}
			_ = json.Unmarshal([]byte(c.body), &req)
			if json.Unmarshal(body, &r) != nil || r.JSONRPC != "2.0" || r.ID != req.ID || r.Error.Code != c.code ||
				len(r.Error.Data) != 1 || r.Error.Data[0].Reason != c.reason {
				t.Errorf("%s: %s", c.name, body)
			}
			continue
		}
		var r struct {
			Error struct {
				Code    int `json:"code"`
				Details []struct {
					Reason string `json:"reason"`
				} `json:"details"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &r) != nil || r.Error.Code != c.status || len(r.Error.Details) != 1 ||
			r.Error.Details[0].Reason != c.reason {
			t.Errorf("%s: %s", c.name, body)
		}
	}

	// A call that can start still streams, and a streaming send is sent
	// once: the check that carried it out hands its task to the stream.
	sent := func() int {
		e.seam.mu.Lock()
		defer e.seam.mu.Unlock()
		return len(e.seam.sends)
	}
	sends := sent()
	resp, body := e.raw("POST", rpc, hdr, `{"jsonrpc":"2.0","id":"s1","method":"SendStreamingMessage","params":`+
		`{"message":{"messageId":"ms","role":"ROLE_USER","parts":[{"text":"stream"}]}}}`)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(string(body), "echo: stream") {
		t.Fatalf("a streaming send: %d %s: %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if got := sent() - sends; got != 1 {
		t.Fatalf("the streaming send reached the kernel %d times", got)
	}
	resp, body = e.raw("POST", rest+"/message:stream", hdr,
		`{"message":{"messageId":"mr","role":"ROLE_USER","parts":[{"text":"rest stream"}]}}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "echo: rest stream") {
		t.Fatalf("a REST streaming send: %d: %s", resp.StatusCode, body)
	}
	if got := sent() - sends; got != 2 {
		t.Fatalf("the two streaming sends reached the kernel %d times", got)
	}
}

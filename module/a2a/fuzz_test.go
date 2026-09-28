//go:build !no_a2a

package a2a

// Fuzz target for the local A2A interface's request handling
// (docs/notes/0033): both bindings, the media-type rule, the service
// parameters, and the request bodies a2a-go and precheck.go read, against
// the fake TaskSeam. Under plain `go test` it runs its seeds only.

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
)

var fuzzMethods = []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodPut, http.MethodPatch}

// logSink collects what the server logs, where a recovered panic shows up.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) take() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.buf.String()
	l.buf.Reset()
	return s
}

// FuzzServer: any request reaches an answer without a panic (recovered or
// not); a POST whose body is not application/json is never served; a
// JSON-RPC answer, and any answer that says it is JSON, is JSON; and a
// request without the bearer is refused before anything reads its body.
func FuzzServer(f *testing.F) {
	const rpc = agentA + "/jsonrpc"
	send := `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`
	seeds := []struct {
		method       uint8
		path, ct     string
		version, ext string
		body         string
		auth         bool
	}{
		{0, rpc, "application/json", "", "", send, true},
		{0, rpc + "/", "application/json; charset=utf-8", "1.0", "", send, true},
		{0, rpc, "application/json", "", x402URI, `{"jsonrpc":"2.0","id":"a","method":"SendStreamingMessage","params":{"message":{"messageId":"m2","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`, true},
		{0, rpc, "application/json", "", "", `{"jsonrpc":"2.0","id":2,"method":"GetTask","params":{"id":"task1","historyLength":3}}`, true},
		{0, rpc, "application/json", "", "", `{"jsonrpc":"2.0","id":3,"method":"ListTasks","params":{"pageSize":101}}`, true},
		{0, rpc, "application/json", "", "", `{"jsonrpc":"2.0","id":4,"method":"CancelTask","params":{"id":"task1"}}`, true},
		{0, rpc, "application/json", "", "", `{"jsonrpc":"2.0","id":5,"method":"SubscribeToTask","params":{"id":"task1"}}`, true},
		{0, rpc, "application/json", "0.3", "", send, true},
		{0, rpc, "text/plain", "", "", send, true},
		{0, rpc, "application/a2a+json", "", "", send, true},
		{0, rpc, "application/json", "", "", `[` + send + `]`, true},
		{0, rpc, "application/json", "", "", send + `{}`, true},
		{0, rpc, "application/json", "", "", send, false},
		{0, agentA + "/rest/message:send", "application/json", "", "", `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}`, true},
		{0, agentA + "/rest/message:stream", "application/json", "", "", `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"raw":"aGk=","filename":"a"}]}}`, true},
		{1, agentA + "/rest/tasks/task1", "", "", "", "", true},
		{1, agentA + "/rest/tasks?pageSize=0&status=TASK_STATE_WORKING", "", "", "", "", true},
		{0, agentA + "/rest/tasks/task1:cancel", "application/json", "", "", "{}", true},
		{1, agentA + "/rest/tasks/task1:subscribe", "", "", "", "", true},
		{1, agentA + "/.well-known/agent-card.json", "", "", "", "", true},
		{1, "", "", "", "", "", true},
		{1, "?skill=x&limit=-1&cursor=zz", "", "", "", "", true},
	}
	for _, s := range seeds {
		f.Add(s.method, s.path, s.ct, s.version, s.ext, []byte(s.body), s.auth)
	}
	ctl, err := identity.Incept()
	if err != nil {
		f.Fatal(err)
	}
	sink := &logSink{}
	log.SetOutput(sink)
	f.Fuzz(func(t *testing.T, method uint8, path, ct, version, ext string, body []byte, auth bool) {
		defer func() {
			if s := sink.take(); strings.Contains(s, "panic") {
				t.Fatalf("the server recovered a panic: %s", s)
			}
		}()
		seam := newFakeSeam()
		s := newServer(seam, serverConfig{token: testToken, port: "4242", self: selfAID, signer: testSigner{ctl}})
		m := fuzzMethods[int(method)%len(fuzzMethods)]
		target := agentsPath
		if path != "" {
			target += "/" + path
		}
		req, err := http.NewRequest(m, "http://127.0.0.1:4242"+target, bytes.NewReader(body))
		if err != nil {
			return // not a request a client could send
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req = req.WithContext(ctx)
		req.Host = "127.0.0.1:4242"
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		if version != "" {
			req.Header.Set("A2A-Version", version)
		}
		if ext != "" {
			req.Header.Set("A2A-Extensions", ext)
		}
		read := &countingBody{r: req.Body}
		req.Body = read
		if auth {
			req.Header.Set("Authorization", "Bearer "+testToken)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		resp := w.Result()
		out := w.Body.Bytes()

		if !auth && (resp.StatusCode != http.StatusUnauthorized || read.n > 0) {
			t.Fatalf("a request without the bearer: %d, %d body bytes read", resp.StatusCode, read.n)
		}
		mt, _, perr := mime.ParseMediaType(ct)
		isRPC := strings.Contains(req.URL.Path, "/jsonrpc")
		if m == http.MethodPost && (isRPC || len(body) > 0) && (perr != nil || mt != "application/json") &&
			resp.StatusCode < 300 {
			t.Fatalf("a %q body was served: %d %s", ct, resp.StatusCode, out)
		}
		if resp.StatusCode >= 500 {
			t.Fatalf("%s %s: %d %s", m, target, resp.StatusCode, out)
		}
		rct := resp.Header.Get("Content-Type")
		if strings.HasPrefix(rct, "application/json") && len(out) > 0 && !json.Valid(out) {
			t.Fatalf("an answer labelled JSON is not: %s", out)
		}
		if strings.HasPrefix(rct, "text/event-stream") {
			for _, ev := range strings.Split(string(out), "\n") {
				if d, ok := strings.CutPrefix(ev, "data: "); ok && !json.Valid([]byte(d)) {
					t.Fatalf("a stream event is not JSON: %s", d)
				}
			}
		}
	})
}

const x402URI = "https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2"

// countingBody counts the body bytes the server read.
type countingBody struct {
	r interface {
		Read([]byte) (int, error)
		Close() error
	}
	n int
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func (c *countingBody) Close() error { return c.r.Close() }

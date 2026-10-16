package main

// UX-C10: the CLI sends one way, through /tasks/send — the route MCP
// send_message and the local A2A interface take — and CUX-F4: for the same
// call, the CLI, MCP and the local A2A interface send the same request.

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// sendRecorder is a control plane that records each request and answers /tasks/get with a task of
// role, everything else with a task.
type sendRecorder struct {
	mu     sync.Mutex
	paths  []string
	bodies []map[string]any
}

func newSendRecorder(t *testing.T, role string) (*client, *sendRecorder) {
	t.Helper()
	sr := &sendRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		sr.mu.Lock()
		sr.paths = append(sr.paths, r.URL.Path)
		sr.bodies = append(sr.bodies, body)
		sr.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"ix_1","contextId":"c","status":{"state":"TASK_STATE_SUBMITTED"},"metadata":{"anet.role":"` + role + `"}}`))
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, token: "t", timeout: 5 * time.Second}, sr
}

// last is the last request: its path and body.
func (sr *sendRecorder) last() (string, map[string]any) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if len(sr.paths) == 0 {
		return "", nil
	}
	return sr.paths[len(sr.paths)-1], sr.bodies[len(sr.bodies)-1]
}

// writeFile writes b to name in a temporary directory and returns its path.
func writeFile(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// rawPart is the JSON form of a raw part carrying b as name.
func rawPart(b []byte, name string) map[string]any {
	return map[string]any{"raw": base64.StdEncoding.EncodeToString(b), "filename": name}
}

// `anet delegate` is /tasks/send with return_immediately (UX-C10): text as text, a capability call as
// skill and args, and with files the A2A message itself, the files read here as raw parts (CUX-F4).
// `anet message` on a task this node sent is /tasks/send with the task's id; on one sent to it, the
// provider's answer, /tasks/reply. Neither asks the console routes /delegate or /message any more.
func TestSendsGoThroughTasksSend(t *testing.T) {
	img := []byte("\x89PNG\r\n\x1a\nimage bytes")
	pic := writeFile(t, "pic.png", img)
	for _, tc := range []struct {
		name string
		run  func(c *client) error
		path string
		want map[string]any
	}{
		{"delegate a text task", func(c *client) error { return delegateCommand(c, []string{wholeAID, "translate", "this"}) },
			"/tasks/send", map[string]any{"to": wholeAID, "text": "translate this", "return_immediately": true,
				"max_task_bytes": float64(cliTaskBytes)}},
		{"delegate a capability call", func(c *client) error {
			return delegateCommand(c, []string{wholeAID, "--capability", "translate", "--args", `{"text":"你好"}`})
		}, "/tasks/send", map[string]any{"to": wholeAID, "skill": "translate", "args": map[string]any{"text": "你好"},
			"return_immediately": true, "max_task_bytes": float64(cliTaskBytes)}},
		{"delegate with a file", func(c *client) error {
			return delegateCommand(c, []string{wholeAID, "describe", "it", "--attach", pic})
		}, "/tasks/send", map[string]any{"to": wholeAID, "return_immediately": true, "max_task_bytes": float64(cliTaskBytes),
			"message": map[string]any{"messageId": "", "role": "ROLE_USER",
				"parts": []any{map[string]any{"text": "describe it"}, rawPart(img, "pic.png")}}}},
		{"a message on a task this node sent", func(c *client) error {
			return messageCommand(c, []string{"ix_1", "one", "more"})
		}, "/tasks/send", map[string]any{"task_id": "ix_1", "text": "one more", "return_immediately": true,
			"max_task_bytes": float64(cliTaskBytes)}},
		{"a message with a file", func(c *client) error {
			return messageCommand(c, []string{"ix_1", "see", "--attach", pic})
		}, "/tasks/send", map[string]any{"return_immediately": true, "max_task_bytes": float64(cliTaskBytes),
			"message": map[string]any{"messageId": "", "role": "ROLE_USER", "taskId": "ix_1",
				"parts": []any{map[string]any{"text": "see"}, rawPart(img, "pic.png")}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, sr := newSendRecorder(t, a2ashape.RoleRequester)
			_, _ = captureCLI(t, func() {
				if err := tc.run(c); err != nil {
					t.Fatal(err)
				}
			})
			path, body := sr.last()
			if path != tc.path || !reflect.DeepEqual(body, tc.want) {
				t.Fatalf("sent %s %v\nwant %s %v", path, body, tc.path, tc.want)
			}
		})
	}
	// On a task sent to this node, a message is the provider's answer that asks for input.
	c, sr := newSendRecorder(t, a2ashape.RoleProvider)
	_, _ = captureCLI(t, func() {
		if err := messageCommand(c, []string{"ix_1", "which", "language?"}); err != nil {
			t.Fatal(err)
		}
	})
	if path, body := sr.last(); path != "/tasks/reply" || !reflect.DeepEqual(body, map[string]any{"task_id": "ix_1",
		"text": "which language?", "max_task_bytes": float64(cliTaskBytes)}) {
		t.Fatalf("the provider's message: %s %v", path, body)
	}
	// What is malformed is a usage error and asks nothing.
	c, sr = newSendRecorder(t, a2ashape.RoleRequester)
	for _, args := range [][]string{{}, {wholeAID}, {wholeAID, "--args", `{}`}, {wholeAID, "--capability", "x", "--args", "[1]"},
		{wholeAID, "--capability", "x", "text"}} {
		if err := delegateCommand(c, args); !isUsage(err) {
			t.Errorf("delegate %v: %v, want a usage error", args, err)
		}
	}
	for _, args := range [][]string{{}, {"ix_1"}, {"ix_1", "--file"}} {
		if err := messageCommand(c, args); !isUsage(err) {
			t.Errorf("message %v: %v, want a usage error", args, err)
		}
	}
	if p, _ := sr.last(); p != "" {
		t.Fatalf("a malformed command asked the daemon %s", p)
	}
}

//go:build !no_mcp

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/mcpserv"
)

// mcpSession runs the MCP server over c against an in-memory client.
func mcpSession(t *testing.T, c mcpserv.Control) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := mcpserv.New(c, "test").Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// mcpRecorder is the control plane MCP send_message talks to: it records the /tasks/send body.
type mcpRecorder struct {
	mu   sync.Mutex
	body map[string]any
}

func (m *mcpRecorder) Call(_ context.Context, path string, body, out any) error {
	b, _ := json.Marshal(body)
	m.mu.Lock()
	_ = json.Unmarshal(b, &m.body)
	m.mu.Unlock()
	if out != nil {
		return json.Unmarshal([]byte(`{"id":"ix_1","contextId":"c","status":{"state":"TASK_STATE_SUBMITTED"}}`), out)
	}
	return nil
}

// CUX-F4: the CLI's `delegate <aid> --capability <id> --args J --attach P` sends what MCP send_message
// sends for to, skill, args and files — and what a local A2A client sends, a DataPart {skill, args}
// and a raw FilePart — so all three make the same request (the daemon reads one message, the same
// way, into the one TaskDoc whose CID is the request's). Only how long to wait differs. [mut] the CLI
// sends its files as paths, or out of order → red.
func TestCLIAndMCPSendTheSameRequest(t *testing.T) {
	img := []byte("\x89PNG\r\n\x1a\nface")
	img2 := []byte("GIF89a second")
	p1, p2 := writeFile(t, "a.png", img), writeFile(t, "b.gif", img2)
	for _, tc := range []struct {
		name  string
		cli   []string
		tool  map[string]any
		parts []a2ashape.Part // the A2A message a local client sends; nil when the call has no file
	}{
		{"a capability call", []string{wholeAID, "--capability", "face.detect", "--args", `{"max_faces":3}`},
			map[string]any{"to": wholeAID, "skill": "face.detect", "args": map[string]any{"max_faces": 3}}, nil},
		{"a capability call with files", []string{wholeAID, "--capability", "face.compare", "--args", `{"pick":"largest"}`,
			"--attach", p1, "--attach", p2},
			map[string]any{"to": wholeAID, "skill": "face.compare", "args": map[string]any{"pick": "largest"},
				"files": []any{map[string]any{"name": "a.png", "content_base64": base64.StdEncoding.EncodeToString(img)},
					map[string]any{"name": "b.gif", "content_base64": base64.StdEncoding.EncodeToString(img2)}}},
			[]a2ashape.Part{a2ashape.DataPart(map[string]any{"skill": "face.compare", "args": map[string]any{"pick": "largest"}}),
				a2ashape.RawPart(img, "a.png", ""), a2ashape.RawPart(img2, "b.gif", "")}},
		{"a text task with a file", []string{wholeAID, "what", "is", "this", "--attach", p1},
			map[string]any{"to": wholeAID, "text": "what is this",
				"files": []any{map[string]any{"name": "a.png", "content_base64": base64.StdEncoding.EncodeToString(img)}}},
			[]a2ashape.Part{a2ashape.TextPart("what is this"), a2ashape.RawPart(img, "a.png", "")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, sr := newSendRecorder(t, a2ashape.RoleRequester)
			_, _ = captureCLI(t, func() {
				if err := delegateCommand(c, tc.cli); err != nil {
					t.Fatal(err)
				}
			})
			_, cliBody := sr.last()

			rec := &mcpRecorder{}
			sess := mcpSession(t, rec)
			res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message", Arguments: tc.tool})
			if err != nil || res.IsError {
				t.Fatalf("send_message: %v %+v", err, res)
			}
			rec.mu.Lock()
			mcpBody := rec.body
			rec.mu.Unlock()
			// How long to wait is each entry's own: the CLI returns at once, MCP waits.
			for _, k := range []string{"return_immediately", "timeout_ms", "max_task_bytes", "history_length"} {
				delete(cliBody, k)
				delete(mcpBody, k)
			}
			if !reflect.DeepEqual(cliBody, mcpBody) {
				t.Fatalf("the CLI sent\n%v\nMCP sent\n%v", cliBody, mcpBody)
			}
			if tc.parts != nil {
				// The local A2A interface: the same parts, in the same order.
				want, _ := json.Marshal(a2ashape.Message{Role: a2ashape.RoleUser, Parts: tc.parts})
				got, _ := json.Marshal(cliBody["message"])
				var w, g any
				_ = json.Unmarshal(want, &w)
				_ = json.Unmarshal(got, &g)
				if !reflect.DeepEqual(w, g) {
					t.Fatalf("the CLI's message\n%s\nis not the A2A client's\n%s", got, want)
				}
			}
		})
	}
}

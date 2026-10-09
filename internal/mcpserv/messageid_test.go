//go:build !no_mcp

package mcpserv

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// correctness-ux CUX-D9: send_message takes no message_id. A model does not
// manage idempotency keys well (an internal note: ids made from the request
// and the date, reused by a second session, returned an old task and the
// new request was never sent), so MCP sends no client id and takes no part
// in the daemon's retry check: the same call made twice is two tasks, which
// the description says.
func TestNoMessageIDInMCPSchema(t *testing.T) {
	f := &fakeControl{}
	sess := connect(t, f)
	tool := listTools(t, sess)["send_message"]
	if tool == nil {
		t.Fatal("no send_message tool")
	}
	schema, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Properties) == 0 {
		t.Fatalf("send_message has no input properties: %s", schema)
	}
	for k := range s.Properties {
		if strings.Contains(strings.ToLower(k), "message_id") || strings.Contains(strings.ToLower(k), "messageid") {
			t.Errorf("send_message takes %q", k)
		}
	}
	if d := strings.ToLower(tool.Description); strings.Contains(d, "message_id") || strings.Contains(d, "messageid") {
		t.Errorf("send_message's description still speaks of a message id: %s", tool.Description)
	}
	if !strings.Contains(tool.Description, "a resend is a second task") {
		t.Error("send_message must say that sending again makes a second task")
	}
	// A model that sends one anyway is told so by the schema, and nothing
	// reaches the daemon.
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message",
		Arguments: map[string]any{"to": "aid-1", "text": "hi", "message_id": "m-1"}})
	if msg, isErr := resultText(t, res, err); !isErr || !strings.Contains(msg, "message_id") || len(f.calls) != 0 {
		t.Fatalf("a call with message_id: %q (error %v), %d calls to the daemon", msg, isErr, len(f.calls))
	}
	// Nor does a message with files carry one.
	if _, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"to": "aid-1", "text": "see attached",
		"files": []any{map[string]any{"name": "a.txt", "media_type": "text/plain", "content_base64": "aGk="}},
	}}); err != nil {
		t.Fatal(err)
	}
	last := f.calls[len(f.calls)-1].body
	if m, _ := last["message"].(map[string]any); m == nil || m["messageId"] != "" {
		t.Fatalf("the message sent with files carries a client id: %v", last["message"])
	}
}

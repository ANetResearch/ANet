//go:build !no_mcp

package mcpserv

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// capCallMessage is the message a capability call with one file is, from
// every entry (CUX-F4): the CLI's --attach (cmd/anet TestAttachMessageIsTheSameAsMCPs)
// and the local A2A endpoint send exactly this, so the daemon builds one
// request, with one request CID, whichever door the call came through.
const capCallMessage = `{"messageId":"","role":"ROLE_USER","parts":[{"data":{"args":{"max_faces":3},"skill":"face.detect"},"mediaType":"application/json"},{"raw":"aGVsbG8=","filename":"a.txt"}]}`

func TestCapabilityCallWithFilesIsTheSharedMessage(t *testing.T) {
	f := &fakeControl{}
	sess := connect(t, f)
	if _, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"to": "aid-1", "skill": "face.detect", "args": map[string]any{"max_faces": 3},
		"files": []any{map[string]any{"name": "a.txt", "content_base64": "aGVsbG8="}},
	}}); err != nil {
		t.Fatal(err)
	}
	body := f.calls[len(f.calls)-1].body
	got, err := json.Marshal(body["message"])
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	_ = json.Unmarshal(got, &a)
	_ = json.Unmarshal([]byte(capCallMessage), &b)
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	if string(ga) != string(gb) {
		t.Fatalf("message\n got %s\nwant %s", ga, gb)
	}
}

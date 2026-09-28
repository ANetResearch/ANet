//go:build !no_mcp

package mcpserv

// Fuzz target for the MCP surface (docs/notes/0033): the arguments a model
// passes to a tool, and the answer the daemon gives back, which carries
// what peers wrote. Under plain `go test` it runs its seeds only.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fuzzControl answers every call with the fuzzed reply and records the
// calls, safely: the server runs the tool on its own goroutine.
type fuzzControl struct {
	mu    sync.Mutex
	calls []call
	reply []byte
}

func (f *fuzzControl) Call(_ context.Context, path string, body, out any) error {
	var m map[string]any
	b, _ := json.Marshal(body)
	_ = json.Unmarshal(b, &m)
	f.mu.Lock()
	f.calls = append(f.calls, call{path, m})
	reply := f.reply
	f.mu.Unlock()
	if out != nil {
		return json.Unmarshal(reply, out)
	}
	return nil
}

func (f *fuzzControl) take(reply []byte) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls, f.reply = nil, reply
	return c
}

// FuzzTools: any tool, any arguments, any daemon answer. The server
// answers the call (a result or a tool error) without panicking; every
// control-plane call it makes is on the allowlist; a payment is made only
// by submit_payment, with decision submit and no purpose; and
// inbound_pending passes on only the metadata fields it names.
func FuzzTools(f *testing.F) {
	for _, rs := range requestShapes {
		args, _ := json.Marshal(rs.args)
		idx := 0
		for i, n := range theTools {
			if n == rs.tool {
				idx = i
			}
		}
		f.Add(uint8(idx), args, []byte(`{"ok":true}`))
	}
	f.Add(uint8(13), []byte(`{}`), []byte(`{"pending":[{"interaction_id":"i","goal":"secret task text","requester":"r","attachments":[{"name":"a"}]}]}`))
	f.Add(uint8(7), []byte(`{}`), []byte(`{"tasks":[{"id":"t1","metadata":{}}],"nextPageToken":"n"}`))
	f.Add(uint8(2), []byte(`{"to":"aid-1","text":"hi","files":[{"name":"a","content_base64":"aGk="}],"skill":"x.y","args":{"a":1}}`), []byte(`[]`))
	f.Add(uint8(3), []byte(`{"task_id":"t"}`), []byte(`"string"`))
	f.Add(uint8(5), []byte(`{"task_id":"t","timeout_seconds":-5,"after_seq":-1}`), []byte(`null`))

	fc := &fuzzControl{}
	ct, st := mcp.NewInMemoryTransports()
	srv := New(fc, "fuzz")
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		f.Fatal(err)
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "fuzz-client"}, nil)
	sess, err := cli.Connect(context.Background(), ct, nil)
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = sess.Close() })

	f.Fuzz(func(t *testing.T, tool uint8, args, reply []byte) {
		name := theTools[int(tool)%len(theTools)]
		if !json.Valid(args) {
			return // the client could not have sent it
		}
		fc.take(reply)
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(args)})
		calls := fc.take(nil)
		if err == nil && res == nil {
			t.Fatal("no result and no error")
		}
		for _, c := range calls {
			if !allowedPaths[c.path] {
				t.Fatalf("%s called %s, outside the allowlist", name, c.path)
			}
			if c.path == "/tasks/pay" {
				want := map[string]string{"submit_payment": "submit", "reject_payment": "reject"}[name]
				if want == "" || c.body["decision"] != want || c.body["purpose"] != nil {
					t.Fatalf("%s paid: %v", name, c.body)
				}
			}
			if _, ok := c.body["pay"]; ok {
				t.Fatalf("%s sent a pay flag: %v", name, c.body)
			}
		}
		if name == "inbound_pending" && err == nil && !res.IsError {
			b, _ := json.Marshal(res.StructuredContent)
			var out struct {
				Pending []map[string]json.RawMessage `json:"pending"`
			}
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatalf("inbound_pending answered %s", b)
			}
			for _, p := range out.Pending {
				for k := range p {
					if !contains(pendingFields, k) {
						t.Fatalf("inbound_pending passed on %q: %s", k, b)
					}
				}
			}
		}
	})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

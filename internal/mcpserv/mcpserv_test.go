//go:build !no_mcp

package mcpserv

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeControl records what the tools ask the daemon for, and answers.
type fakeControl struct {
	calls []call
	reply map[string]string
	err   error
}

type call struct {
	path string
	body map[string]any
}

func (f *fakeControl) Call(_ context.Context, path string, body, out any) error {
	var m map[string]any
	if body != nil {
		b, _ := json.Marshal(body)
		_ = json.Unmarshal(b, &m)
	}
	f.calls = append(f.calls, call{path, m})
	if f.err != nil {
		return f.err
	}
	if r, ok := f.reply[path]; ok && out != nil {
		return json.Unmarshal([]byte(r), out)
	}
	return nil
}

// connect runs the server against an in-memory client, which is the only
// honest way to test an MCP surface: the tool schemas, the dispatch and
// the argument decoding are the SDK's, and a test that called the
// closures directly would exercise none of them.
func connect(t *testing.T, c Control) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	srv := New(c, "test")
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
	sess, err := cli.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// The surface an agent actually sees. If a tool is renamed or dropped,
// every client configuration referencing it breaks silently — the model
// simply stops having the ability and never says so.
func TestTheToolSurfaceIsWhatWePromise(t *testing.T) {
	sess := connect(t, &fakeControl{})
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool.Description
	}
	want := []string{
		"agents_find", "task_delegate", "task_results",
		"task_inbox", "task_message", "task_end", "node_status",
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("tool %q is missing — client configurations naming it break silently", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("tool count = %d, want %d: %v", len(got), len(want), got)
	}

	// Descriptions are the interface. A model reads them and decides what
	// to do, so the honesty the rest of the system enforces has to survive
	// into the prose.
	if d := got["task_results"]; !strings.Contains(d, "does not mean forged") {
		t.Error("task_results must explain that an unverified receipt is not a forged one")
	}
	if d := got["task_delegate"]; !strings.Contains(d, "cannot be repudiated") {
		t.Error("task_delegate must say the delegation is signed and attributable")
	}
}

func TestDelegateSendsAGoalOrACapabilityNeverBoth(t *testing.T) {
	f := &fakeControl{reply: map[string]string{
		"/delegate": `{"interaction_id":"ix-1","status":"queued"}`}}
	sess := connect(t, f)
	ctx := context.Background()

	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "task_delegate",
		Arguments: map[string]any{"provider": "aid-1", "goal": "translate this"},
	}); err != nil {
		t.Fatal(err)
	}
	last := f.calls[len(f.calls)-1]
	if last.path != "/delegate" || last.body["goal"] != "translate this" {
		t.Fatalf("prose delegation sent %v", last)
	}
	if _, has := last.body["capability"]; has {
		t.Error("a prose goal must not also carry a capability")
	}

	// A capability call carries no goal text at all. The control API had a
	// defect where the capability ended up inside the goal, where no
	// resolver looks; the shape is worth pinning here too.
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "task_delegate",
		Arguments: map[string]any{"provider": "aid-1", "capability": "cas.put",
			"args": map[string]any{"body": "aGk="}},
	}); err != nil {
		t.Fatal(err)
	}
	last = f.calls[len(f.calls)-1]
	if last.body["capability"] != "cas.put" {
		t.Fatalf("capability call sent %v", last)
	}
	if _, has := last.body["goal"]; has {
		t.Error("a capability call must leave no goal text")
	}
}

// A tool that needs an argument says which one. An MCP error goes back to
// the model, which can fix a named omission and cannot fix "bad request".
//
// Two mechanisms, and both are wanted. The SDK derives a JSON schema from
// the input struct and rejects a missing required property before the
// handler runs — naming the property, which is the better error because a
// client can often correct it without asking the model again. What a
// schema cannot express is a choice between two optional fields, so
// task_delegate still checks by hand that exactly one of goal/capability
// arrived. The test asserts the outcome, not which layer produced it.
func TestMissingArgumentsAreNamed(t *testing.T) {
	sess := connect(t, &fakeControl{})
	ctx := context.Background()
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"task_delegate", map[string]any{"goal": "x"}, "provider"},
		{"task_delegate", map[string]any{"provider": "aid-1"}, "goal (prose) or a capability"},
		{"task_message", map[string]any{"interaction_id": "ix-1"}, "body"},
		{"task_end", map[string]any{}, "interaction_id"},
	} {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
		if err == nil && (res == nil || !res.IsError) {
			t.Errorf("%s%v: expected a named refusal", tc.tool, tc.args)
			continue
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		} else {
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					msg += tc.Text
				}
			}
		}
		if !strings.Contains(msg, tc.want) {
			t.Errorf("%s: message %q does not name the problem (%q)", tc.tool, msg, tc.want)
		}
	}
}

// The daemon's own error text reaches the model rather than a status code.
func TestDaemonErrorsReachTheCaller(t *testing.T) {
	sess := connect(t, &fakeControl{err: errNoHub})
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "agents_find", Arguments: map[string]any{"query": "translate"}})
	msg := ""
	if err != nil {
		msg = err.Error()
	} else if res != nil {
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msg += tc.Text
			}
		}
	}
	if !strings.Contains(msg, "no hub configured") {
		t.Errorf("the daemon's explanation must survive to the model, got %q", msg)
	}
}

var errNoHub = errNoHubType{}

type errNoHubType struct{}

func (errNoHubType) Error() string { return "anet: no hub configured (run `anet hub-register` first)" }

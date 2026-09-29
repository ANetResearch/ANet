//go:build !no_mcp

package mcpserv

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// fakeControl records what the tools ask the daemon for, and answers.
type fakeControl struct {
	calls []call
	reply map[string]string
	errs  map[string]error
	err   error
	// answer, when set, answers a call by its body; ok false falls back to
	// reply.
	answer func(path string, body map[string]any) (string, bool)
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
	if e := f.errs[path]; e != nil {
		return e
	}
	r, ok := "", false
	if f.answer != nil {
		r, ok = f.answer(path, m)
	}
	if !ok {
		r, ok = f.reply[path]
	}
	if !ok {
		r = `{"ok":true}`
	}
	if out != nil {
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

func listTools(t *testing.T, sess *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool
	}
	return got
}

// resultText is what a call told the model: the error text, or the JSON.
func resultText(t *testing.T, res *mcp.CallToolResult, err error) (string, bool) {
	t.Helper()
	if err != nil {
		return err.Error(), true
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

var theTools = []string{
	"list_agents", "get_agent_card", "send_message", "get_task", "list_tasks", "wait_task",
	"cancel_task", "reply_task", "submit_payment", "reject_payment", "get_balance", "audit",
	"node_status", "inbound_pending",
}

// The surface an agent actually sees (A2A-DESIGN §12). If a tool is
// renamed or dropped, every client configuration referencing it breaks
// silently — the model simply stops having the ability and never says so.
func TestTheToolSurfaceIsWhatWePromise(t *testing.T) {
	sess := connect(t, &fakeControl{})
	got := listTools(t, sess)
	for _, name := range theTools {
		if _, ok := got[name]; !ok {
			t.Errorf("tool %q is missing — client configurations naming it break silently", name)
		}
	}
	if len(got) != len(theTools) {
		names := make([]string, 0, len(got))
		for n := range got {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Errorf("tool count = %d, want %d: %v", len(got), len(theTools), names)
	}
	// The replaced names are gone, not aliased: task_delegate's pay=true was
	// the gateway tier and agents_find sent free text to the hub.
	for _, old := range []string{"agents_find", "task_delegate", "task_results", "task_inbox",
		"task_message", "task_end", "evidence_read", "credit_balance"} {
		if _, ok := got[old]; ok {
			t.Errorf("replaced tool %q is still registered", old)
		}
	}

	// Descriptions are the interface. A model reads them and decides what
	// to do, so the honesty the rest of the system enforces has to survive
	// into the prose.
	desc := func(n string) string { return got[n].Description }
	for _, n := range []string{"send_message", "get_task", "list_tasks", "wait_task"} {
		if !strings.Contains(desc(n), "anet.effect_status=UNVERIFIED is not success") {
			t.Errorf("%s must say that completed with effect_status=UNVERIFIED is not success", n)
		}
		if !strings.Contains(desc(n), "not the same as forged") {
			t.Errorf("%s must say that an unverified receipt is not a forged one", n)
		}
		// Red-team F10: failed + UNVERIFIED is "may have happened", and the
		// agent is the one that would send it again.
		if !strings.Contains(desc(n), "do not resend it as if it had not run") {
			t.Errorf("%s must say that a failed task with effect_status=UNVERIFIED is not to be resent", n)
		}
	}
	if !strings.Contains(desc("send_message"), "cannot be repudiated") {
		t.Error("send_message must say the request is signed and attributable")
	}
	if !strings.Contains(desc("send_message"), "a resend is a second task") {
		t.Error("send_message must warn that sending again makes a second task")
	}
	// docs/notes/0041: Claude Code made message ids from the request and the
	// date ("book-meeting-room-20260929-1"); the same id in a second session,
	// while the first session's task was still open, returned that old task
	// and the new request was never sent.
	for _, want := range []string{"a fresh random one (a UUID) for every new message",
		"never one made from the text or the date", "returns that task and sends nothing"} {
		if !strings.Contains(desc("send_message"), want) {
			t.Errorf("send_message must say %q about message_id", want)
		}
	}
	// docs/notes/0041: told "the limits are 0 until the operator raises
	// them", the model told the user a priced skill needed `anet pay` on a
	// node whose limits covered it. No tool shows the limits.
	for _, n := range []string{"send_message", "submit_payment"} {
		if !strings.Contains(desc(n), "no tool shows") {
			t.Errorf("%s must say that no tool shows the spending limits", n)
		}
	}
	if !strings.Contains(desc("list_agents"), "never sent to the hub") {
		t.Error("list_agents must say free text stays on this machine")
	}
	if !strings.Contains(desc("list_agents"), "`anet.official: true`") ||
		!strings.Contains(desc("list_agents"), "A name that looks official is not") {
		t.Error("list_agents must name the anet.official mark and say a name is not it")
	}
	// 0017 Q24, Q27: what an UNVERIFIED and a NONE entry are, and that the
	// fields of the latter are the hub's word.
	for _, want := range []string{"include_uncarded", "verification NONE", "only what the hub says",
		"UNVERIFIED entry", "nothing of the card"} {
		if !strings.Contains(desc("list_agents"), want) {
			t.Errorf("list_agents must say %q", want)
		}
	}
	if !strings.Contains(desc("get_agent_card"), "Only a VERIFIED card is returned") {
		t.Error("get_agent_card must say an unverified card is not returned")
	}
	// Paying spends the operator's money. The model has to learn whose
	// credit it is, what caps it, and what to do when it is refused.
	for _, want := range []string{"agent_max", "payees.allow", "anet pay <task_id>", "never try to raise a limit",
		"needs_operator_approval"} {
		if !strings.Contains(desc("submit_payment"), want) {
			t.Errorf("submit_payment must mention %q", want)
		}
	}
	// A quarantined chain is the one fact a reader most needs and would
	// never think to ask for.
	if !strings.Contains(desc("audit"), "QUARANTINED") {
		t.Error("audit must warn that a forked chain cannot be relied on")
	}
	if !strings.Contains(desc("get_balance"), "custodian") {
		t.Error("get_balance must say the hub holds the balance, not this node")
	}
	if !strings.Contains(desc("inbound_pending"), "never the content") {
		t.Error("inbound_pending must say it returns metadata only")
	}

	// The short rules go to every client on connect.
	ins := sess.InitializeResult().Instructions
	for _, want := range []string{"anet.effect_status=UNVERIFIED is not success", "wait_task", "submit_payment", "untrusted",
		"do not resend it as if it had not run", "no tool shows them", "do not tell the user what they are"} {
		if !strings.Contains(ins, want) {
			t.Errorf("server instructions must mention %q; got %q", want, ins)
		}
	}
	// The limits' default is not this node's value (docs/notes/0041).
	if strings.Contains(ins, "which are 0 until the operator raises them") {
		t.Error("server instructions state the spending limits as 0 again")
	}
}

// Every tool states every hint: MCP's defaults (destructive, open world)
// would otherwise make a read look dangerous and a payment look ordinary.
func TestToolAnnotations(t *testing.T) {
	type hints struct {
		readOnly, destructive, idempotent, openWorld bool
	}
	want := map[string]hints{
		"list_agents":     {readOnly: true, openWorld: true},
		"get_agent_card":  {readOnly: true, openWorld: true},
		"send_message":    {openWorld: true},
		"get_task":        {readOnly: true},
		"list_tasks":      {readOnly: true},
		"wait_task":       {readOnly: true},
		"cancel_task":     {destructive: true, idempotent: true, openWorld: true}, // cannot be taken back
		"reply_task":      {openWorld: true},
		"submit_payment":  {destructive: true, openWorld: true},
		"reject_payment":  {idempotent: true, openWorld: true},
		"get_balance":     {readOnly: true, openWorld: true},
		"audit":           {readOnly: true},
		"node_status":     {readOnly: true},
		"inbound_pending": {readOnly: true},
	}
	got := listTools(t, connect(t, &fakeControl{}))
	for name, w := range want {
		tool := got[name]
		if tool == nil {
			t.Errorf("%s missing", name)
			continue
		}
		a := tool.Annotations
		if a == nil {
			t.Errorf("%s has no annotations", name)
			continue
		}
		if a.OpenWorldHint == nil {
			t.Errorf("%s: openWorldHint unset (defaults to true)", name)
			continue
		}
		h := hints{readOnly: a.ReadOnlyHint, idempotent: a.IdempotentHint, openWorld: *a.OpenWorldHint}
		if !a.ReadOnlyHint {
			if a.DestructiveHint == nil {
				t.Errorf("%s: destructiveHint unset (defaults to true)", name)
				continue
			}
			h.destructive = *a.DestructiveHint
		}
		if h != w {
			t.Errorf("%s annotations = %+v, want %+v", name, h, w)
		}
	}
}

// requestShapes is one valid call of every tool and the control-plane
// request it must make.
var requestShapes = []struct {
	tool string
	args map[string]any
	path string
	body map[string]any
}{
	{"list_agents", map[string]any{"skill": "text.digest", "query": "summaries", "limit": 5},
		"/agents/list", map[string]any{"skill": "text.digest", "q": "summaries", "limit": 5.0}},
	{"list_agents", map[string]any{"tag": "docs", "cursor": "c2"},
		"/agents/list", map[string]any{"tag": "docs", "cursor": "c2"}},
	{"list_agents", map[string]any{"skill": "code.write", "include_uncarded": true},
		"/agents/list", map[string]any{"skill": "code.write", "include_uncarded": true}},
	{"list_agents", map[string]any{"skill": "code.write", "include_uncarded": false},
		"/agents/list", map[string]any{"skill": "code.write"}},
	{"get_agent_card", map[string]any{"aid": "aid-1"},
		"/agents/card", map[string]any{"aid": "aid-1"}},
	{"send_message", map[string]any{"to": "aid-1", "text": "translate this"},
		"/tasks/send", map[string]any{"to": "aid-1", "text": "translate this", "timeout_ms": 30000.0, "max_task_bytes": 24576.0}},
	{"send_message", map[string]any{"to": "aid-1", "skill": "text.digest", "args": map[string]any{"text": "x"},
		"message_id": "m-1", "context_id": "ctx-1", "return_immediately": true},
		"/tasks/send", map[string]any{"to": "aid-1", "skill": "text.digest", "args": map[string]any{"text": "x"},
			"message_id": "m-1", "context_id": "ctx-1", "return_immediately": true, "max_task_bytes": 24576.0}},
	{"send_message", map[string]any{"task_id": "ix-1", "text": "yes", "timeout_seconds": 1000, "history_length": 2},
		"/tasks/send", map[string]any{"task_id": "ix-1", "text": "yes", "timeout_ms": 300000.0, "history_length": 2.0,
			"max_task_bytes": 24576.0}},
	{"get_task", map[string]any{"task_id": "ix-1", "history_length": 0},
		"/tasks/get", map[string]any{"task_id": "ix-1", "history_length": 0.0, "max_task_bytes": 24576.0}},
	{"list_tasks", map[string]any{"role": "provider", "state": "input-required", "context_id": "ctx-1",
		"peer": "aid-2", "page_size": 10, "page_token": "p2", "include_artifacts": true},
		"/tasks/list", map[string]any{"role": "inbound", "state": "input-required", "context_id": "ctx-1",
			"peer": "aid-2", "page_size": 10.0, "page_token": "p2", "include_artifacts": true, "history_length": 1.0,
			"max_task_bytes": 8192.0}},
	{"list_tasks", map[string]any{"role": "requester"},
		"/tasks/list", map[string]any{"role": "outbound", "history_length": 1.0, "max_task_bytes": 8192.0}},
	{"list_tasks", map[string]any{"history_length": 0},
		"/tasks/list", map[string]any{"history_length": 0.0, "max_task_bytes": 8192.0}},
	{"list_tasks", map[string]any{"context_id": "ctx-1", "history_length": 40},
		"/tasks/list", map[string]any{"context_id": "ctx-1", "history_length": 40.0, "max_task_bytes": 8192.0}},
	{"wait_task", map[string]any{"task_id": "ix-1"},
		"/tasks/wait", map[string]any{"task_id": "ix-1", "timeout_ms": 30000.0, "max_task_bytes": 24576.0}},
	{"wait_task", map[string]any{"task_id": "ix-1", "after_seq": 7, "timeout_seconds": 120},
		"/tasks/wait", map[string]any{"task_id": "ix-1", "after_seq": 7.0, "timeout_ms": 120000.0, "max_task_bytes": 24576.0}},
	{"cancel_task", map[string]any{"task_id": "ix-1"},
		"/tasks/cancel", map[string]any{"task_id": "ix-1", "max_task_bytes": 24576.0}},
	{"reply_task", map[string]any{"task_id": "ix-2", "text": "done", "state": "completed"},
		"/tasks/reply", map[string]any{"task_id": "ix-2", "text": "done", "state": "completed", "max_task_bytes": 24576.0}},
	{"submit_payment", map[string]any{"task_id": "ix-1"},
		"/tasks/pay", map[string]any{"task_id": "ix-1", "decision": "submit"}},
	{"submit_payment", map[string]any{"task_id": "ix-1", "accept": map[string]any{"scheme": "credit", "amount": "5"}},
		"/tasks/pay", map[string]any{"task_id": "ix-1", "decision": "submit", "accept": map[string]any{"scheme": "credit", "amount": "5"}}},
	{"reject_payment", map[string]any{"task_id": "ix-1"},
		"/tasks/pay", map[string]any{"task_id": "ix-1", "decision": "reject"}},
	{"get_balance", map[string]any{},
		"/balance", map[string]any{}},
	{"audit", map[string]any{"event_type": "anet.capability.effect", "since": 3, "limit": 10},
		"/evidence", map[string]any{"event_type": "anet.capability.effect", "since": 3.0, "limit": 10.0}},
	{"node_status", map[string]any{},
		"/status", map[string]any{}},
	{"inbound_pending", map[string]any{},
		"/inbound/pending", map[string]any{}},
}

// Each tool makes exactly the control-plane request the routes expect —
// the body is compared whole, so a field that leaks in (a pay flag, a
// payload) fails as surely as one that goes missing.
func TestEachToolSendsTheRequestTheRouteExpects(t *testing.T) {
	f := &fakeControl{reply: map[string]string{"/inbound/pending": `{"pending":[]}`}}
	sess := connect(t, f)
	covered := map[string]bool{}
	for _, tc := range requestShapes {
		n := len(f.calls)
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
		if msg, isErr := resultText(t, res, err); isErr {
			t.Errorf("%s%v: %s", tc.tool, tc.args, msg)
			continue
		}
		if len(f.calls) != n+1 {
			t.Errorf("%s made %d calls, want 1", tc.tool, len(f.calls)-n)
			continue
		}
		got := f.calls[n]
		if got.path != tc.path {
			t.Errorf("%s called %s, want %s", tc.tool, got.path, tc.path)
		}
		if !reflect.DeepEqual(got.body, tc.body) {
			t.Errorf("%s sent %v, want %v", tc.tool, got.body, tc.body)
		}
		covered[tc.tool] = true
	}
	for _, name := range theTools {
		if !covered[name] {
			t.Errorf("no request shape pinned for %s", name)
		}
	}
}

// A message with files goes as an A2A message with raw parts; this process
// never reads a local path, so a file leaves only if the model put its
// bytes in the call.
func TestFilesTravelAsRawParts(t *testing.T) {
	f := &fakeControl{}
	sess := connect(t, f)
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"task_id": "ix-1", "message_id": "m-2", "text": "see attached",
		"files": []any{map[string]any{"name": "a.txt", "media_type": "text/plain", "content_base64": "aGk="}},
	}})
	if msg, isErr := resultText(t, res, err); isErr {
		t.Fatal(msg)
	}
	body := f.calls[len(f.calls)-1].body
	for _, k := range []string{"text", "task_id", "message_id"} {
		if _, has := body[k]; has {
			t.Errorf("with a message, the short field %s must not be sent too: %v", k, body)
		}
	}
	msg, _ := body["message"].(map[string]any)
	want := map[string]any{
		"messageId": "m-2", "taskId": "ix-1", "role": "ROLE_USER",
		"parts": []any{
			map[string]any{"text": "see attached"},
			map[string]any{"raw": "aGk=", "filename": "a.txt", "mediaType": "text/plain"},
		},
	}
	if !reflect.DeepEqual(msg, want) {
		t.Errorf("message = %v, want %v", msg, want)
	}

	res, err = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"to": "aid-1", "files": []any{map[string]any{"name": "a", "content_base64": "not base64!"}}}})
	if msg, isErr := resultText(t, res, err); !isErr || !strings.Contains(msg, "not base64") {
		t.Errorf("bad base64 must be refused by name, got %q", msg)
	}
}

// §8.6: the MCP server never calls the task-manual, gateway or redeem
// routes. Pinned three ways: the allowlist does not hold them, the guard
// refuses them, and no tool — called with every shape above — reaches a
// path outside the allowlist.
func TestMCPNeverCallsManualGatewayOrRedeemRoutes(t *testing.T) {
	forbidden := []string{"/tasks/pay-manual", "/x402-authorize", "/redeem", "/delegate", "/message",
		"/end", "/payments/limits", "/peers/allow", "/inbound/approve", "/shutdown"}
	for _, p := range forbidden {
		if allowedPaths[p] {
			t.Errorf("%s is on the MCP allowlist", p)
		}
		f := &fakeControl{}
		if err := (guarded{f}).Call(context.Background(), p, map[string]any{}, nil); err == nil {
			t.Errorf("the guard let %s through", p)
		}
		if len(f.calls) != 0 {
			t.Errorf("%s reached the daemon", p)
		}
	}

	f := &fakeControl{reply: map[string]string{"/inbound/pending": `{"pending":[]}`, "/tasks/list": `{"tasks":[]}`}}
	sess := connect(t, f)
	for _, tc := range requestShapes {
		_, _ = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
	}
	_, _ = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "reply_task", Arguments: map[string]any{}})
	for _, c := range f.calls {
		if !allowedPaths[c.path] {
			t.Errorf("a tool called %s, outside the allowlist", c.path)
		}
		if c.path == "/tasks/pay" && c.body["purpose"] != nil {
			t.Errorf("/tasks/pay must not carry a purpose (the route decides it): %v", c.body)
		}
	}

	// No tool can be asked to pay by a flag: payment is submit_payment's
	// alone, and it has no purpose or payload to choose.
	for name, tool := range listTools(t, sess) {
		raw, _ := json.Marshal(tool.InputSchema)
		var s struct {
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(raw, &s)
		for _, p := range []string{"pay", "purpose", "payload"} {
			if _, has := s.Properties[p]; has {
				t.Errorf("%s takes a %q argument", name, p)
			}
		}
	}
}

// The control plane's projection is forwarded byte for byte: a number past
// 2^53 and the daemon's key order survive, which a decode into a Go map
// would lose.
func TestProjectionIsForwardedUnchanged(t *testing.T) {
	daemonJSON := `{"id":"ix-1","contextId":"ctx-1","status":{"state":"TASK_STATE_WORKING"},` +
		`"metadata":{"zeta":1,"anet.state_seq":9007199254740993,"alpha":"a"}}`
	f := &fakeControl{reply: map[string]string{"/tasks/get": daemonJSON}}
	sess := connect(t, f)
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_task",
		Arguments: map[string]any{"task_id": "ix-1"}})
	text, isErr := resultText(t, res, err)
	if isErr {
		t.Fatal(text)
	}
	var want bytes.Buffer
	_ = json.Compact(&want, []byte(daemonJSON))
	if text != want.String() {
		t.Errorf("forwarded\n %s\nwant\n %s", text, want.String())
	}
	if res.StructuredContent == nil {
		t.Error("the task must also be structured content")
	}

	// An empty answer is the daemon failing, not a null result.
	f.reply["/tasks/get"] = `null`
	res, err = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_task",
		Arguments: map[string]any{"task_id": "ix-1"}})
	if msg, isErr := resultText(t, res, err); !isErr || !strings.Contains(msg, "empty answer") {
		t.Errorf("a null answer must be an error, got %q", msg)
	}
}

// reply_task is always registered (a node that answers nothing still has
// it), and says plainly when there is nothing to answer.
func TestReplyTaskExplainsWhenThereIsNothingToAnswer(t *testing.T) {
	open := map[string]string{}
	f := &fakeControl{answer: func(path string, body map[string]any) (string, bool) {
		if path != "/tasks/list" {
			return "", false
		}
		st, _ := body["state"].(string)
		if tasks, ok := open[st]; ok {
			return tasks, true
		}
		return `{"tasks":[],"totalSize":0,"pageSize":20,"nextPageToken":""}`, true
	}}
	sess := connect(t, f)
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "reply_task",
		Arguments: map[string]any{"text": "hello"}})
	msg, isErr := resultText(t, res, err)
	if !isErr || !strings.Contains(msg, "there is no task to reply to") || !strings.Contains(msg, "inbound_pending") {
		t.Errorf("reply_task with nothing to answer: %q", msg)
	}
	// It asks for each open state of the tasks sent here. Reading the newest
	// page of all of them would miss an open task behind finished ones (the
	// list is ordered by the last state change) and wrongly say "none".
	var asked []string
	for _, c := range f.calls {
		if c.path != "/tasks/list" {
			t.Errorf("reply_task without a task id called %s", c.path)
			continue
		}
		if c.body["role"] != "inbound" {
			t.Errorf("reply_task looked for tasks with %v", c.body)
		}
		st, _ := c.body["state"].(string)
		asked = append(asked, st)
	}
	if !reflect.DeepEqual(asked, replyableStates) {
		t.Errorf("reply_task asked for states %v, want %v", asked, replyableStates)
	}

	// Open tasks are named; capability calls, which complete by themselves
	// and cannot be replied to, are not.
	open["working"] = `{"tasks":[{"id":"ix-c","contextId":"c","status":{"state":"TASK_STATE_WORKING"}}]}`
	open["input-required"] = `{"tasks":[` +
		`{"id":"ix-a","contextId":"c","status":{"state":"TASK_STATE_INPUT_REQUIRED"}},` +
		`{"id":"ix-k","contextId":"c","status":{"state":"TASK_STATE_INPUT_REQUIRED"},"metadata":{"anet.skill":"text.digest"}}]}`
	res, err = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "reply_task",
		Arguments: map[string]any{"text": "hello"}})
	msg, _ = resultText(t, res, err)
	if !strings.Contains(msg, "ix-c, ix-a") || strings.Contains(msg, "ix-k") || strings.Contains(msg, "and more") {
		t.Errorf("reply_task must name the open tasks only: %q", msg)
	}

	// More than one page: say so, and where to look.
	open["submitted"] = `{"tasks":[{"id":"ix-s","contextId":"c","status":{"state":"TASK_STATE_SUBMITTED"}}],"nextPageToken":"p2"}`
	res, err = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "reply_task",
		Arguments: map[string]any{"text": "hello"}})
	msg, _ = resultText(t, res, err)
	if !strings.Contains(msg, "ix-s, ix-c, ix-a") || !strings.Contains(msg, "and more") {
		t.Errorf("reply_task must say the list goes on: %q", msg)
	}

	// An id that is not a task sent here: the daemon's answer, and where
	// to look.
	f.errs = map[string]error{"/tasks/reply": &DaemonError{Status: 404, Message: "task not found: ix-z", Code: "TaskNotFoundError"}}
	res, err = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "reply_task",
		Arguments: map[string]any{"task_id": "ix-z", "text": "hello"}})
	msg, isErr = resultText(t, res, err)
	if !isErr || !strings.Contains(msg, "task not found: ix-z") || !strings.Contains(msg, "role=provider") {
		t.Errorf("reply_task on an unknown task: %q", msg)
	}
}

// inbound_pending passes on metadata only, whatever the daemon's list
// grows to carry.
func TestInboundPendingIsMetadataOnly(t *testing.T) {
	f := &fakeControl{reply: map[string]string{"/inbound/pending": `{"pending":[{"interaction_id":"ix-1",` +
		`"requester":"aid-r","arrived_at":1700000000000,"bytes":321,"request_cid":"bafy1","capability":"text.digest",` +
		`"attachments":0,"followups":1,"expires_at":1700086400000,"goal":"SECRET TASK TEXT"}]}`}}
	sess := connect(t, f)
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "inbound_pending", Arguments: map[string]any{}})
	text, isErr := resultText(t, res, err)
	if isErr {
		t.Fatal(text)
	}
	if strings.Contains(text, "SECRET") {
		t.Errorf("inbound_pending passed content on: %s", text)
	}
	var out struct {
		Pending []map[string]any `json:"pending"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || len(out.Pending) != 1 {
		t.Fatalf("pending = %s (%v)", text, err)
	}
	for _, k := range []string{"requester", "arrived_at", "bytes", "request_cid", "capability"} {
		if _, ok := out.Pending[0][k]; !ok {
			t.Errorf("inbound_pending dropped %s", k)
		}
	}
}

// A tool that needs an argument says which one. An MCP error goes back to
// the model, which can fix a named omission and cannot fix "bad request".
func TestMissingArgumentsAreNamed(t *testing.T) {
	sess := connect(t, &fakeControl{})
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"send_message", map[string]any{"text": "x"}, "`to`"},
		{"send_message", map[string]any{"to": "aid-1"}, "the message is empty"},
		{"send_message", map[string]any{"to": "aid-1", "args": map[string]any{"a": 1}}, "the skill id"},
		{"get_agent_card", map[string]any{}, "aid"},
		{"get_task", map[string]any{}, "task_id"},
		{"wait_task", map[string]any{}, "task_id"},
		{"cancel_task", map[string]any{}, "task_id"},
		{"submit_payment", map[string]any{}, "task_id"},
		{"reject_payment", map[string]any{}, "task_id"},
		{"list_tasks", map[string]any{"role": "boss"}, "requester"},
	} {
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
		msg, isErr := resultText(t, res, err)
		if !isErr {
			t.Errorf("%s%v: expected a named refusal", tc.tool, tc.args)
			continue
		}
		if !strings.Contains(msg, tc.want) {
			t.Errorf("%s: message %q does not name the problem (%q)", tc.tool, msg, tc.want)
		}
	}
}

// The daemon's own error text reaches the model rather than a status code,
// with the machine-readable names it gave.
func TestDaemonErrorsReachTheCaller(t *testing.T) {
	sess := connect(t, &fakeControl{err: &DaemonError{Status: 403,
		Message: "anet: payment refused: over the agent limit", Reason: "agent_max"}})
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "submit_payment", Arguments: map[string]any{"task_id": "ix-1"}})
	msg, isErr := resultText(t, res, err)
	if !isErr || !strings.Contains(msg, "over the agent limit") || !strings.Contains(msg, "reason: agent_max") {
		t.Errorf("the daemon's explanation must survive to the model, got %q", msg)
	}
}

// Every tool that returns one task keeps its result under what Claude Code
// takes from an MCP tool (MAX_MCP_OUTPUT_TOKENS, 25 000 by default): the
// daemon holds the task to max_task_bytes (a2ashape.TaskReadWithin, which
// the fake below applies as the daemon does), and the result, which carries
// it twice — as text and as structured content — stays under 50 000 bytes,
// 25 000 tokens even at two bytes a token (docs/notes/0035 §7.4: a peer's
// 3 MiB reply made get_task, wait_task and send_message unusable). A reply
// just under the bound is carried whole; a larger one is a notice that
// names the task and how to read it whole.
func TestASingleTaskToolResultStaysUnderTheClientLimit(t *testing.T) {
	mk := func(reply string) a2ashape.Task {
		return a2ashape.Task{ID: "ix-1", ContextID: "c", Status: a2ashape.TaskStatus{State: a2ashape.TaskStateCompleted},
			History:   []a2ashape.Message{{ID: "a", Role: a2ashape.RoleAgent, Parts: []a2ashape.Part{a2ashape.TextPart(reply)}}},
			Artifacts: []a2ashape.Artifact{{ID: a2ashape.ArtifactReply, Parts: []a2ashape.Part{a2ashape.TextPart(reply)}}},
			Metadata:  map[string]any{"anet.role": "requester"}}
	}
	for _, reply := range []string{strings.Repeat("字", 1<<20), strings.Repeat("字", 7800), strings.Repeat("0", 22<<10)} {
		task := mk(reply)
		f := &fakeControl{answer: func(path string, body map[string]any) (string, bool) {
			out := task
			if n, ok := body["max_task_bytes"].(float64); ok && n > 0 {
				out = a2ashape.TaskReadWithin(task, int(n))
			}
			b, _ := json.Marshal(out)
			return string(b), true
		}}
		sess := connect(t, f)
		for _, tc := range []struct {
			tool string
			args map[string]any
		}{
			{"get_task", map[string]any{"task_id": "ix-1"}},
			{"wait_task", map[string]any{"task_id": "ix-1"}},
			{"send_message", map[string]any{"task_id": "ix-1", "text": "more"}},
			{"cancel_task", map[string]any{"task_id": "ix-1"}},
			{"reply_task", map[string]any{"task_id": "ix-1", "text": "ok"}},
		} {
			res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			text, isErr := resultText(t, res, err)
			if isErr {
				t.Fatalf("%s: %s", tc.tool, text)
			}
			structured, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(text) + len(structured); n > 50_000 {
				t.Errorf("%s, a %d-byte reply: the model is handed %d bytes", tc.tool, len(reply), n)
			}
			if !strings.Contains(text, `"ix-1"`) {
				t.Errorf("%s: the result does not carry the task id: %.300s", tc.tool, text)
			}
			if len(reply) > 1<<20 && !strings.Contains(text, "anet task get ix-1 --full") {
				t.Errorf("%s: the cut result does not say how to read the task whole: %.300s", tc.tool, text)
			}
		}
	}
}

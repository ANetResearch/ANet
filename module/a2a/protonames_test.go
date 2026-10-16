package a2a

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestNormalizeRPCParams(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"top level", `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"t","history_length":1}}`,
			`{"id":1,"jsonrpc":"2.0","method":"GetTask","params":{"historyLength":1,"id":"t"}}`},
		{"list", `{"method":"ListTasks","params":{"context_id":"c","page_size":5,"include_artifacts":true}}`,
			`{"method":"ListTasks","params":{"contextId":"c","includeArtifacts":true,"pageSize":5}}`},
		{"configuration and message", `{"params":{"configuration":{"history_length":0,"return_immediately":true},"message":{"message_id":"m","task_id":"x","parts":[{"text":"a"}]}}}`,
			`{"params":{"configuration":{"historyLength":0,"returnImmediately":true},"message":{"messageId":"m","parts":[{"text":"a"}],"taskId":"x"}}}`},
		{"the JSON name wins", `{"params":{"id":"t","historyLength":2,"history_length":9}}`,
			`{"params":{"historyLength":2,"id":"t"}}`},
		{"metadata is free-form", `{"params":{"message":{"messageId":"m","metadata":{"history_length":7}}}}`,
			`{"params":{"message":{"messageId":"m","metadata":{"history_length":7}}}}`},
		{"numbers are kept as written", `{"params":{"message":{"message_id":"m","metadata":{"n":9007199254740993}}}}`,
			`{"params":{"message":{"messageId":"m","metadata":{"n":9007199254740993}}}}`},
		{"nothing to rename", `{"params":{"id":"t","historyLength":1}}`, `{"params":{"id":"t","historyLength":1}}`},
		{"not an object", `[1,2]`, `[1,2]`},
		{"not JSON", `{"params":`, `{"params":`},
	}
	for _, c := range cases {
		if got := string(normalizeRPCParams([]byte(c.in))); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

// A client that names the history limit by its proto field name (the
// reference conformance kit does) gets it honoured on JSON-RPC, as on REST.
func TestJSONRPCReadsProtoFieldNames(t *testing.T) {
	e := newEnv(t)
	cl, ctx := e.client(agentA, a2a.TransportProtocolJSONRPC)
	res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	id := string(res.(*a2a.Task).ID)

	hdr := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json", "A2A-Version": "1.0"}
	historyLen := func(params string) int {
		t.Helper()
		_, body := e.raw("POST", agentsPath+"/"+agentA+"/jsonrpc", hdr,
			`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"`+id+`"`+params+`}}`)
		var out struct {
			Result struct {
				History []json.RawMessage `json:"history"`
			} `json:"result"`
			Error any `json:"error"`
		}
		if err := json.Unmarshal(body, &out); err != nil || out.Error != nil {
			t.Fatalf("GetTask: %s %v", body, err)
		}
		return len(out.Result.History)
	}
	if full := historyLen(""); full < 1 {
		t.Fatalf("the task has %d history messages, want at least 1", full)
	}
	for _, p := range []string{`,"historyLength":0`, `,"history_length":0`} {
		if n := historyLen(p); n != 0 {
			t.Errorf("params %s: %d history messages, want none", strings.TrimPrefix(p, ","), n)
		}
	}
}

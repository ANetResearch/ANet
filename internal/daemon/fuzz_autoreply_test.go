package daemon

// Fuzz target for POST /autoreply's decoding and merge (LEC §2.3.4, EXE-5): any current configuration
// and any request body. Under plain `go test` it runs its seeds only.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// FuzzMergeAutoReply: mergeAutoReply never panics; what it accepts is a configuration checkAutoReply
// accepts, with a key that is either kept, removed (null) or the request's own without surrounding white
// space, never empty; a member the request does not name keeps its value; and applying the same request
// again changes nothing.
func FuzzMergeAutoReply(f *testing.F) {
	for _, s := range [][2]string{
		{`{"backend":"exec","agent":"claude","api_key":"sk-1"}`, `{"model":"haiku"}`},
		{`{"backend":"exec","agent":"claude"}`, `{"api_key":"  sk-2\n"}`},
		{`{"backend":"exec","agent":"claude","api_key":"sk-1"}`, `{"api_key":""}`},
		{`{"backend":"exec","agent":"claude","api_key":"sk-1"}`, `{"api_key":null,"work_dir":null}`},
		{`null`, `{"backend":"openai","api_base":"http://127.0.0.1:1/v1","model":"m"}`},
		{`{"backend":"openai","api_base":"https://m.example/v1","model":"m"}`, `{"api_base":"http://192.0.2.1/v1"}`},
		{`{"backend":"exec","agent":"codex","untrusted":"sandbox"}`, `{"untrusted":"maybe","off":false}`},
		{`{"backend":"exec","agent":"hermes"}`, `{"extra_args":["-v"],"max_history":"x"}`},
		{`{}`, `{"unknown":{"a":1}}`},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, curJSON, body string) {
		var cur *AutoReplyConfig
		if c := new(AutoReplyConfig); json.Unmarshal([]byte(curJSON), c) == nil {
			cur = c
		}
		members := map[string]json.RawMessage{}
		if json.Unmarshal([]byte(body), &members) != nil || members == nil {
			return
		}
		again := map[string]json.RawMessage{}
		for k, v := range members {
			again[k] = v
		}
		named := func(k string) bool { _, ok := members[k]; return ok }
		rawKey, keyNamed := members["api_key"]
		next, ref := mergeAutoReply(cur, members)
		if (next == nil) == (ref == nil) {
			t.Fatalf("merge returned %v and %v", next, ref)
		}
		if ref != nil {
			if ref.Code() == "" || strings.Contains(ref.Error(), "{key}") || strings.Contains(ref.Error(), "{rule}") {
				t.Fatalf("refusal %q has no code or an unfilled placeholder", ref.Error())
			}
			return
		}
		if r := checkAutoReply(*next); r != nil {
			t.Fatalf("accepted a configuration the check refuses: %v", r)
		}
		switch {
		case !keyNamed:
			if cur != nil && next.APIKey != cur.APIKey {
				t.Fatal("a key the request did not name changed")
			}
		case string(bytes.TrimSpace(rawKey)) == "null":
			if next.APIKey != "" {
				t.Fatal("a null key was kept")
			}
		default:
			if next.APIKey == "" || next.APIKey != strings.TrimSpace(next.APIKey) {
				t.Fatalf("stored key %q", next.APIKey)
			}
		}
		if cur != nil {
			for k, v := range jsonMembers(t, cur) {
				if !named(k) && !bytes.Equal(jsonMembers(t, next)[k], v) {
					t.Fatalf("member %s, not named, changed from %s to %s", k, v, jsonMembers(t, next)[k])
				}
			}
		}
		twice, ref := mergeAutoReply(next, again)
		if ref != nil {
			t.Fatalf("the same request on its own result was refused: %v", ref)
		}
		a, _ := json.Marshal(next)
		b, _ := json.Marshal(twice)
		if !bytes.Equal(a, b) {
			t.Fatalf("not idempotent:\n%s\n%s", a, b)
		}
	})
}

func jsonMembers(t *testing.T, c *AutoReplyConfig) map[string]json.RawMessage {
	t.Helper()
	raw, _ := json.Marshal(c)
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &m)
	return m
}

package autoreply

// LEC §2.3.3, §2.3.4, AV-8 and AV-10 (EXE-5): POST /autoreply merges what it names into the current
// configuration and never shows the key; /status says only whether one is set; rotateLocalTokens
// replaces both local tokens and the old ones stop working at once.

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func sameAutoReply(a, b AutoReplyConfig) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func readTokenFile(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v %v", path, fi, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// The exact-name rule holds inside a block of settings too (an object member whose field is a struct,
// merged member by member): "Hosts" sent next to a stored hosts would be overridden by it in silence.
func TestAutoReplyMemberNamesAreExactAtEveryDepth(t *testing.T) {
	type block struct {
		Hosts []string `json:"hosts,omitempty"`
		Limit int      `json:"limit,omitempty"`
	}
	type conf struct {
		Name  string `json:"name,omitempty"`
		Block *block `json:"block,omitempty"`
	}
	for _, c := range []struct{ body, want string }{
		{`{"name":"x","block":{"hosts":["a"],"limit":1}}`, ""},
		{`{"block":null}`, ""},
		{`{"block":{"Hosts":["a"]}}`, "block.Hosts"},
		{`{"block":{"hosts":["a"],"other":1}}`, "block.other"},
		{`{"Name":"x","block":{"hosts":[]}}`, "Name"},
		{`{"name":{"Hosts":1}}`, ""}, // not a block: its type is checked when the merge is decoded
	} {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(c.body), &m); err != nil {
			t.Fatal(err)
		}
		if got := inexactMember(reflect.TypeFor[conf](), m); got != c.want {
			t.Errorf("%s: %q, want %q", c.body, got, c.want)
		}
	}
}

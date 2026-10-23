package autoreply

import (
	"encoding/json"
	"strings"
	"testing"
)

// The model proxy's upstreams bound a request's output by their own limit, and the sandbox's custom
// upstream keeps 8192 (BP-3).
func TestChatPolicyOutputBoundPerUpstream(t *testing.T) {
	cases := []struct {
		name string
		up   *egressUpstream
		in   string
		want string
	}{
		{"sandbox default added", &egressUpstream{Name: "custom"}, `{"messages":[]}`, "8192"},
		{"sandbox clamps", &egressUpstream{Name: "custom"}, `{"messages":[],"max_tokens":20000}`, "8192"},
		{"proxy default added", &egressUpstream{Name: "plan", MaxOutput: ModelProxyUpstream{}.outputBound()}, `{"messages":[]}`, "32768"},
		{"proxy keeps within", &egressUpstream{Name: "plan", MaxOutput: 32768}, `{"messages":[],"max_tokens":20000}`, "20000"},
		{"proxy clamps over", &egressUpstream{Name: "plan", MaxOutput: 32768}, `{"messages":[],"max_tokens":100000}`, "32768"},
		{"proxy configured", &egressUpstream{Name: "plan", MaxOutput: ModelProxyUpstream{MaxOutputTokens: 65536}.outputBound()}, `{"messages":[]}`, "65536"},
	}
	for _, c := range cases {
		var obj map[string]any
		d := json.NewDecoder(strings.NewReader(c.in))
		d.UseNumber()
		if err := d.Decode(&obj); err != nil {
			t.Fatal(err)
		}
		if r := chatBodyPolicy(obj, c.up); r != nil {
			t.Fatalf("%s: refused: %v", c.name, r)
		}
		if got := obj["max_tokens"].(json.Number).String(); got != c.want {
			t.Errorf("%s: max_tokens %s, want %s", c.name, got, c.want)
		}
	}
}

func TestModelProxyMaxOutputTokensValidated(t *testing.T) {
	for _, n := range []int64{0, 1, proxyMaxOutputTokensCap} {
		p := &AutoReplyModelProxyConfig{Listen: "127.0.0.1:0", Upstreams: []ModelProxyUpstream{{Name: "plan", APIBase: "https://models.example/v1", MaxOutputTokens: n}}}
		if r := checkModelProxy(p); r != nil {
			t.Errorf("max_output_tokens %d refused: %v", n, r)
		}
	}
	for _, n := range []int64{-1, proxyMaxOutputTokensCap + 1} {
		p := &AutoReplyModelProxyConfig{Listen: "127.0.0.1:0", Upstreams: []ModelProxyUpstream{{Name: "plan", APIBase: "https://models.example/v1", MaxOutputTokens: n}}}
		if r := checkModelProxy(p); r == nil {
			t.Errorf("max_output_tokens %d accepted", n)
		}
	}
}

package a2a

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/module"
)

// UX-10 (CUX-P30, CUX-P10, CUX-A3): the preview extension on the proxy
// card, its activation by local capability, and what reaches the kernel.

func remoteWithPreview(aid string, declared bool) module.RemoteAgent {
	ra := verifiedRemote(aid)
	if !declared {
		return ra
	}
	var card map[string]any
	_ = json.Unmarshal(ra.Card, &card)
	caps := card["capabilities"].(map[string]any)
	caps["extensions"] = append(caps["extensions"].([]any), map[string]any{"uri": module.ExtPreviewURI})
	ra.Card, _ = json.Marshal(card)
	return ra
}

func proxyExtensions(t *testing.T, e *env, aid string) map[string]a2a.AgentExtension {
	t.Helper()
	resp, raw := e.raw("GET", agentsPath+"/"+aid+"/.well-known/agent-card.json", map[string]string{"Authorization": "Bearer " + testToken}, "")
	if resp.StatusCode != 200 {
		t.Fatalf("card: %d %s", resp.StatusCode, raw)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	if !card.Capabilities.Streaming {
		t.Fatal("the proxy card must always declare streaming")
	}
	out := map[string]a2a.AgentExtension{}
	for _, x := range card.Capabilities.Extensions {
		out[x.URI] = x
	}
	return out
}

// TestProxyCardPreviewFollowsRemote: declared by a verified remote card
// only, not required, and streaming is true whatever the remote says.
func TestProxyCardPreviewFollowsRemote(t *testing.T) {
	e := newEnv(t)
	e.seam.cards[agentA] = remoteWithPreview(agentA, true)
	e.seam.cards[agentB] = remoteWithPreview(agentB, false)
	unverified := remoteWithPreview(agentB, true)
	unverified.Verification = "UNVERIFIED"
	e.seam.cards["bafyagentc"] = unverified

	x, ok := proxyExtensions(t, e, agentA)[module.ExtPreviewURI]
	if !ok || x.Required || len(x.Params) != 0 {
		t.Fatalf("a remote that declares it: %+v %v", x, ok)
	}
	if _, ok := proxyExtensions(t, e, agentB)[module.ExtPreviewURI]; ok {
		t.Fatal("declared for a remote that does not declare it")
	}
	if _, ok := proxyExtensions(t, e, "bafyagentc")[module.ExtPreviewURI]; ok {
		t.Fatal("declared from a card that is not verified")
	}
}

// TestActivationWithoutRemoteDeclaration: the extension is activated by the
// node's own capability, echoed, and a streaming send asks for previews;
// a plain send does not.
func TestActivationWithoutRemoteDeclaration(t *testing.T) {
	e := newEnv(t)
	e.seam.cards[agentB] = remoteWithPreview(agentB, false)
	hdr := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json",
		"A2A-Extensions": module.ExtPreviewURI}
	send := func(method string) *module.TaskSend {
		before := len(e.seam.sends)
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{"message":{"messageId":"m-` + method +
			`","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`
		resp, raw := e.raw("POST", agentsPath+"/"+agentB+"/jsonrpc", hdr, body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", method, resp.StatusCode, raw)
		}
		if got := resp.Header.Get("A2A-Extensions"); got != hdr["A2A-Extensions"] {
			t.Fatalf("%s echoed %q", method, got)
		}
		if len(e.seam.sends) != before+1 {
			t.Fatalf("%s: the kernel was not asked", method)
		}
		return &e.seam.sends[before]
	}
	if r := send("SendStreamingMessage"); r.Preview != "on" || !slices.Contains(r.Extensions, module.ExtPreviewURI) {
		t.Fatalf("streaming send: %+v", r)
	}
	if r := send("SendMessage"); r.Preview != "" {
		t.Fatalf("a blocking send asked for previews: %+v", r)
	}
	// Without the header: no preview.
	delete(hdr, "A2A-Extensions")
	if r := send("SendStreamingMessage"); r.Preview != "" {
		t.Fatalf("no activation, yet previews: %+v", r)
	}
}

func TestWatchOptsFollowActivation(t *testing.T) {
	e := newEnv(t)
	e.seam.cards[agentA] = remoteWithPreview(agentA, false)
	for _, tc := range []struct {
		hdr  string
		want bool
	}{{module.ExtPreviewURI, true}, {"", false}, {"https://unknown.example/ext", false}} {
		hdr := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json"}
		if tc.hdr != "" {
			hdr["A2A-Extensions"] = tc.hdr
		}
		body := `{"jsonrpc":"2.0","id":1,"method":"SendStreamingMessage","params":{"message":{"messageId":"m-` +
			strings.ReplaceAll(tc.hdr, "/", "") + `","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`
		resp, raw := e.raw("POST", agentsPath+"/"+agentA+"/jsonrpc", hdr, body)
		if resp.StatusCode != 200 {
			t.Fatalf("%q: %d %s", tc.hdr, resp.StatusCode, raw)
		}
		e.seam.mu.Lock()
		if len(e.seam.watches) == 0 || e.seam.watches[len(e.seam.watches)-1].Preview != tc.want {
			t.Errorf("header %q: Watch opts %+v, want preview %v", tc.hdr, e.seam.watches, tc.want)
		}
		e.seam.mu.Unlock()
	}
}

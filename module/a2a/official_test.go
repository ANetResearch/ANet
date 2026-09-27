//go:build !no_a2a

package a2a

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/a2a/kelresolver"
)

// originParams reads the anet-origin params of a proxy card as served.
func originParams(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var card a2a.AgentCard
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	for _, x := range card.Capabilities.Extensions {
		if x.URI == ExtOriginURI {
			return x.Params
		}
	}
	t.Fatalf("no anet-origin extension in %s", raw)
	return nil
}

// The proxy card of an agent the kernel calls official says so in its
// anet-origin params, under the key "anet.official" and the value true;
// the card of any other agent has no such key (not false: §10.1 keeps
// default values out of params). The mark is this node's statement and
// travels under this node's signature, which a2a-go still verifies. It does
// not depend on the remote card: a placeholder for an official agent
// without a verified card carries it too.
func TestProxyCardMarksAnOfficialAgent(t *testing.T) {
	e := newEnv(t)
	official := verifiedRemote(agentA)
	official.Official = true
	e.seam.cards[agentA] = official
	e.seam.cards[agentB] = verifiedRemote(agentB) // the same card, not official
	const placeholderAID = "bafyofficialnocard"
	e.seam.cards[placeholderAID] = module.RemoteAgent{AID: placeholderAID, Verification: unverified, Official: true}

	get := func(aid string) []byte {
		t.Helper()
		resp, raw := e.raw("GET", agentsPath+"/"+aid+"/.well-known/agent-card.json",
			map[string]string{"Authorization": "Bearer " + testToken}, "")
		if resp.StatusCode != 200 {
			t.Fatalf("card %s: %d %s", aid, resp.StatusCode, raw)
		}
		return raw
	}

	raw := get(agentA)
	if !strings.Contains(string(raw), `"anet.official":true`) {
		t.Fatalf("the official agent's proxy card does not spell \"anet.official\":true: %s", raw)
	}
	if p := originParams(t, raw); p["anet.official"] != true || p["aid"] != agentA {
		t.Fatalf("anet-origin params %+v", p)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(raw, &card); err != nil || len(card.Signatures) != 1 {
		t.Fatalf("signatures: %v %+v", err, card.Signatures)
	}
	res := kelresolver.Resolver{KEL: func(_ context.Context, aid string) ([]identity.SignedEvent, error) {
		if aid != e.signer.c.AID() {
			return nil, kelresolver.ErrUnknownAID
		}
		return e.signer.c.KEL(), nil
	}}
	v := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: res})
	if err := v.Verify(context.Background(), raw, &card.Signatures[0]); err != nil {
		t.Fatalf("a2a-go does not verify the marked proxy card: %v", err)
	}

	if p := originParams(t, get(agentB)); p["anet.official"] != nil {
		t.Fatalf("a card like the official one, for another AID, carries anet.official=%v", p["anet.official"])
	}
	if strings.Contains(string(get(agentB)), "anet.official") {
		t.Fatal("a card for an agent that is not official mentions anet.official")
	}
	if p := originParams(t, get(placeholderAID)); p["anet.official"] != true || p["originVerification"] != unverified {
		t.Fatalf("placeholder for an official agent: %+v", p)
	}
}

// The local agent list marks the same agents, under the same key.
func TestAgentListMarksOfficialAgents(t *testing.T) {
	e := newEnv(t)
	e.seam.agents = []module.RemoteAgent{
		{AID: agentA, Name: "anet-tools", Verification: verified, Official: true},
		{AID: agentB, Name: "anet-tools", Verification: verified},
	}
	resp, raw := e.raw("GET", agentsPath, map[string]string{"Authorization": "Bearer " + testToken}, "")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var out struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Agents) != 2 {
		t.Fatalf("agents: %v %s", err, raw)
	}
	for _, a := range out.Agents {
		v, has := a["anet.official"]
		switch a["aid"] {
		case agentA:
			if v != true {
				t.Errorf("the official agent: anet.official=%v", v)
			}
		case agentB:
			if has {
				t.Errorf("the namesake: anet.official=%v, want the key absent", v)
			}
		}
	}
}

// The mark is shown here and used for nothing else: card.go writes it into
// the proxy card and server.go into the agent list, and no other file of
// this module reads module.RemoteAgent.Official — not the backend
// forwarding, not payments, not the task handlers. A use elsewhere would
// be a channel A2A-DESIGN §15 rules out, and has to be added here on
// purpose to pass.
func TestTheOfficialMarkIsOnlyShown(t *testing.T) {
	allowed := map[string]bool{"card.go": true, "server.go": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	uses := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			var name string
			switch x := n.(type) {
			case *ast.SelectorExpr:
				name = x.Sel.Name
			case *ast.Ident:
				name = x.Name
			default:
				return true
			}
			if name != "Official" && name != "originOfficial" {
				return true
			}
			if _, isIdent := n.(*ast.Ident); isIdent && name == "Official" {
				return true // a field name in a declaration or composite literal key, not a read
			}
			uses++
			if !allowed[f] {
				t.Errorf("%s: %s reads the official mark; it is only shown (card.go, server.go)", f, fset.Position(n.Pos()))
			}
			return true
		})
	}
	if uses == 0 {
		t.Fatal("no file reads the mark: the proxy card and the agent list have lost it")
	}
}

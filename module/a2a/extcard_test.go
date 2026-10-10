//go:build !no_a2a

package a2a

// HPP §4.7 and core CORE-K23 rules 3 and 4: GetExtendedAgentCard over both
// bindings, row by row — the JSON-RPC code, the REST status, Retry-After
// and the two ErrorInfo, and what a2a-go's JSON-RPC and REST clients make
// of each refusal — and the proxy cards' extendedAgentCard.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/netcard"
	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/a2a/kelresolver"
)

// extAgent is a remote agent with a public card declaring a2a-x402 and an
// extended card pricing translate at 3 credits, both signed by ctl.
type extAgent struct {
	ctl      *identity.Controller
	pub, ext []byte
}

func newExtAgent(t *testing.T) extAgent {
	t.Helper()
	ctl, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	x402 := a2acard.ExtensionDecl(module.ExtX402URI, "Payments.", false, nil)
	in := netcard.Input{AID: ctl.AID(), Name: "Translator", Description: "Translates.", HubURL: "https://hub.example",
		Seq: 2, IssuedAtMs: 1, NotBeforeMs: 1,
		Skills:     []netcard.Skill{{ID: "translate", Name: "translate", Description: "translates"}, {ID: "chat", Name: "chat", Description: "chats"}},
		Extensions: []map[string]any{x402}}
	pubUnsigned, err := netcard.Build(in)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := a2acard.SignWithSnapshot(pubUnsigned, ctl, netcard.JKU("https://hub.example", ctl.AID()))
	if err != nil {
		t.Fatal(err)
	}
	price := a2acard.ExtensionDecl(module.ExtPriceURI, "Prices.", false, map[string]any{"network": "hub:" + ctl.AID(),
		"asset": "credit", "prices": []any{map[string]any{"skillId": "translate", "amount": "3"}}})
	extUnsigned, _, err := netcard.BuildExtended(in, module.VariantGeneral, []map[string]any{x402, price})
	if err != nil {
		t.Fatal(err)
	}
	ext, err := a2acard.SignWithSnapshot(extUnsigned, ctl, "")
	if err != nil {
		t.Fatal(err)
	}
	return extAgent{ctl: ctl, pub: pub, ext: ext}
}

// seed gives e's kernel the agent: its verified public card, its extended
// card offered.
func (a extAgent) seed(e *env) module.RemoteAgent {
	ra := module.RemoteAgent{AID: a.ctl.AID(), Card: a.pub, Verification: verified, Name: "Translator",
		Charge: module.ChargePartial, ExtendedOffered: true}
	e.seam.mu.Lock()
	defer e.seam.mu.Unlock()
	e.seam.cards[ra.AID] = ra
	if e.seam.ext == nil {
		e.seam.ext = map[string]fakeExt{}
	}
	e.seam.ext[ra.AID] = fakeExt{agent: module.ExtendedAgent{Agent: ra, Card: a.ext}}
	return ra
}

// headerTransport adds the bearer and the version to every request.
type headerTransport struct{}

func (headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("A2A-Version", "1.0")
	return http.DefaultTransport.RoundTrip(r)
}

// extTransports are a2a-go's two transports for aid's interfaces, used
// directly: a2a-go's Client refuses GetExtendedAgentCard itself when the
// card it holds does not declare one, and this is about the server's
// answer.
func (e *env) extTransports(aid string) map[string]a2aclient.Transport {
	hc := &http.Client{Transport: headerTransport{}}
	u, _ := url.Parse(e.base(aid) + "/rest")
	return map[string]a2aclient.Transport{
		"jsonrpc": a2aclient.NewJSONRPCTransport(e.base(aid)+"/jsonrpc", hc),
		"rest":    a2aclient.NewRESTTransport(u, hc),
	}
}

func TestGetExtendedAgentCardRefusals(t *testing.T) {
	a := newExtAgent(t)
	hdr := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json", "A2A-Version": "1.0"}
	for _, c := range []struct {
		name    string
		err     error
		key     string
		sdk     error
		rpc     int
		rest    int
		retry   string
		variant string
	}{
		{"rule 2 does not hold", a2ashape.Refuse("extended_card_not_offered", nil), "extended_card_not_offered",
			a2a.ErrUnsupportedOperation, -32004, 400, "", ""},
		{"not configured", a2ashape.Refuse("extended_card_not_configured", nil), "extended_card_not_configured",
			a2a.ErrExtendedCardNotConfigured, -32007, 400, "", ""},
		{"busy", &a2ashape.Refusal{Key: "extended_card_unavailable", RetryAfter: 60 * time.Second}, "extended_card_unavailable",
			a2a.ErrInternalError, -32603, 503, "60", ""},
		{"no answer in time", &a2ashape.Refusal{Key: "extended_card_unavailable", RetryAfter: 15 * time.Second}, "extended_card_unavailable",
			a2a.ErrInternalError, -32603, 503, "15", ""},
		{"bad signature", a2ashape.Refuse("extended_card_unavailable.bad_signature", nil), "extended_card_unavailable.bad_signature",
			a2a.ErrInvalidAgentResponse, -32006, 500, "", "bad_signature"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			ra := a.seed(e)
			e.seam.mu.Lock()
			e.seam.ext[ra.AID] = fakeExt{err: c.err}
			e.seam.mu.Unlock()
			sentence := a2ashape.Sentence(c.key, a2ashape.Lang(), nil)
			for name, tr := range e.extTransports(ra.AID) {
				_, err := tr.GetExtendedAgentCard(context.Background(), a2aclient.ServiceParams{}, &a2a.GetExtendedAgentCardRequest{})
				if !errors.Is(err, c.sdk) {
					t.Errorf("a2a-go %s: %v, want %v", name, err, c.sdk)
				}
			}
			checkInfo := func(where string, details []a2ashape.ErrorInfo) {
				if len(details) != 2 || details[0].Domain != a2ashape.DomainA2A || details[1].Domain != a2ashape.DomainANet ||
					details[1].Metadata["reason"] != "extended_card_unavailable" && c.variant != "" ||
					details[1].Metadata["reason"] != a2ashape.ReasonCode(c.key) || details[1].Metadata["variant"] != c.variant {
					t.Errorf("%s: ErrorInfo %+v", where, details)
				}
			}
			resp, body := e.raw(http.MethodPost, agentsPath+"/"+ra.AID+"/jsonrpc", hdr,
				`{"jsonrpc":"2.0","id":"x7","method":"GetExtendedAgentCard","params":{}}`)
			var rpc struct {
				ID    any `json:"id"`
				Error struct {
					Code    int                  `json:"code"`
					Message string               `json:"message"`
					Data    []a2ashape.ErrorInfo `json:"data"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &rpc); err != nil || resp.StatusCode != 200 || rpc.Error.Code != c.rpc ||
				rpc.Error.Message != sentence || rpc.ID != "x7" {
				t.Errorf("JSON-RPC: %d %s", resp.StatusCode, body)
			}
			checkInfo("JSON-RPC", rpc.Error.Data)
			if got := resp.Header.Get("Retry-After"); got != c.retry {
				t.Errorf("JSON-RPC Retry-After %q, want %q", got, c.retry)
			}
			resp, body = e.raw(http.MethodGet, agentsPath+"/"+ra.AID+"/rest/extendedAgentCard", hdr, "")
			var rest struct {
				Error struct {
					Code    int                  `json:"code"`
					Message string               `json:"message"`
					Details []a2ashape.ErrorInfo `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &rest); err != nil || resp.StatusCode != c.rest || rest.Error.Message != sentence {
				t.Errorf("REST: %d %s", resp.StatusCode, body)
			}
			checkInfo("REST", rest.Error.Details)
			if got := resp.Header.Get("Retry-After"); got != c.retry {
				t.Errorf("REST Retry-After %q, want %q", got, c.retry)
			}
		})
	}
}

// The row that answers with a card: the proxy extended card, signed by this
// node, written as signed on both bindings with the proxy card's caching
// headers; its skills, prices and a2a-x402 declaration are the extended
// card's, and anet-origin carries both of the agent's cards, the extended
// one verifying in a2a-go under the agent's key.
func TestGetExtendedAgentCardAnswersTheProxyExtendedCard(t *testing.T) {
	a := newExtAgent(t)
	e := newEnv(t)
	ra := a.seed(e)
	hdr := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json", "A2A-Version": "1.0"}

	resp, rest := e.raw(http.MethodGet, agentsPath+"/"+ra.AID+"/rest/extendedAgentCard", hdr, "")
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "private, max-age=300" || resp.Header.Get("ETag") == "" {
		t.Fatalf("REST: %d %v %s", resp.StatusCode, resp.Header, rest)
	}
	resp2, _ := e.raw(http.MethodGet, agentsPath+"/"+ra.AID+"/rest/extendedAgentCard",
		map[string]string{"Authorization": "Bearer " + testToken, "If-None-Match": resp.Header.Get("ETag")}, "")
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", resp2.StatusCode)
	}
	resp, body := e.raw(http.MethodPost, agentsPath+"/"+ra.AID+"/jsonrpc", hdr,
		`{"jsonrpc":"2.0","id":3,"method":"GetExtendedAgentCard","params":{}}`)
	var rpc struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil || resp.StatusCode != 200 || !bytes.Equal(rpc.Result, bytes.TrimSpace(rest)) ||
		resp.Header.Get("Cache-Control") != "private, max-age=300" {
		t.Fatalf("JSON-RPC: %d %s", resp.StatusCode, body)
	}

	// Signed by this node, as a proxy card is: a2a-go verifies it with the
	// KEL resolver.
	var card a2a.AgentCard
	if err := json.Unmarshal(rest, &card); err != nil || len(card.Signatures) != 1 {
		t.Fatalf("%v %+v", err, card.Signatures)
	}
	res := kelresolver.Resolver{KEL: func(_ context.Context, aid string) ([]identity.SignedEvent, error) {
		if aid != e.signer.c.AID() {
			return nil, kelresolver.ErrUnknownAID
		}
		return e.signer.c.KEL(), nil
	}}
	if err := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: res}).Verify(context.Background(), rest, &card.Signatures[0]); err != nil {
		t.Fatalf("the proxy extended card does not verify under this node's key: %v", err)
	}
	if !card.Capabilities.ExtendedAgentCard || len(card.Skills) != 2 {
		t.Fatalf("the proxy extended card: %+v", card.Capabilities)
	}
	var origin, x402, price map[string]any
	for _, x := range card.Capabilities.Extensions {
		switch x.URI {
		case ExtOriginURI:
			origin = x.Params
		case module.ExtX402URI:
			x402 = x.Params
			if x.Required {
				t.Error("the proxy's a2a-x402 declaration is required")
			}
		case module.ExtPriceURI:
			price = x.Params
		}
	}
	if x402["signer"] != "anet-daemon" || x402["clientPayload"] != false {
		t.Errorf("a2a-x402 params %v", x402)
	}
	want, _ := json.Marshal(map[string]any{"network": "hub:" + ra.AID, "asset": "credit",
		"prices": []any{map[string]any{"skillId": "translate", "amount": "3"}}})
	if got, _ := json.Marshal(price); !bytes.Equal(got, want) {
		t.Errorf("price params %s, want %s", got, want)
	}
	extB, err1 := base64.RawURLEncoding.DecodeString(origin["originExtendedCard"].(string))
	pubB, err2 := base64.RawURLEncoding.DecodeString(origin["originCard"].(string))
	if err1 != nil || err2 != nil || !bytes.Equal(extB, a.ext) || !bytes.Equal(pubB, a.pub) || origin["originVerification"] != verified {
		t.Fatalf("anet-origin %v", origin)
	}
	// a2a-go verifies the agent's own signature on originExtendedCard.
	kel := a.ctl.KEL()
	key := ed25519.PublicKey(kel[len(kel)-1].Event.Keys[0])
	v := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: a2acrypto.KeyResolverFunc(
		func(_ context.Context, kid string) (crypto.PublicKey, error) { return key, nil })})
	var ext a2a.AgentCard
	if err := json.Unmarshal(extB, &ext); err != nil || len(ext.Signatures) != 1 {
		t.Fatalf("originExtendedCard: %v", err)
	}
	if err := v.Verify(context.Background(), json.RawMessage(extB), &ext.Signatures[0]); err != nil {
		t.Fatalf("a2a-go: %v", err)
	}

	// And through an unmodified a2a-go client on either binding.
	for _, binding := range bindings {
		cl, ctx := e.client(ra.AID, binding)
		got, err := cl.GetExtendedAgentCard(ctx, &a2a.GetExtendedAgentCardRequest{})
		if err != nil || got.Name != "Translator" || len(got.Skills) != 2 {
			t.Errorf("%s: %v %+v", binding, err, got)
		}
	}
}

// CORE-K23 rule 3: a proxy card declares an extended card when, and only
// when, this node would ask the agent for one; a placeholder never does.
func TestTheProxyCardDeclaresAnExtendedCardOnlyWhenOffered(t *testing.T) {
	a := newExtAgent(t)
	for _, offered := range []bool{true, false} {
		e := newEnv(t)
		ra := a.seed(e)
		e.seam.mu.Lock()
		ra.ExtendedOffered = offered
		e.seam.cards[ra.AID] = ra
		e.seam.mu.Unlock()
		if got := e.card(ra.AID).Capabilities.ExtendedAgentCard; got != offered {
			t.Errorf("offered %t: the proxy card says %t", offered, got)
		}
	}
	e := newEnv(t)
	e.seam.mu.Lock()
	e.seam.cards[agentA] = module.RemoteAgent{AID: agentA, Verification: unverified, ExtendedOffered: true}
	e.seam.mu.Unlock()
	if e.card(agentA).Capabilities.ExtendedAgentCard {
		t.Error("a placeholder declares an extended card")
	}
	// Reading the proxy card asks the kernel for no extended card.
	e.seam.mu.Lock()
	calls := e.seam.extCalls
	e.seam.mu.Unlock()
	if calls != 0 {
		t.Errorf("%d extended card fetches for proxy cards", calls)
	}
}

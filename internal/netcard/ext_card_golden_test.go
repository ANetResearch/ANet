package netcard

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
)

// G-PAY-EXT-1 (HPP §7.1): testdata/ext-card-1.json holds, for two nodes,
// the public card and the extended cards as this package builds them and
// a2acard signs them under the frozen conformance agent identity
// (identity.SuiteAgentController): each card's bytes (RFC 8785, with its
// JWS) and its signing payload.
//
//	priced            translate (3 credits) and chat public, x (2 credits)
//	                  for the lists: both variants carry prices; the public
//	                  card declares a2a-x402 without required
//	list_only_priced  echo public and free, y (2 credits) for the lists: the
//	                  public card declares no a2a-x402, the general variant
//	                  is the public card (not_configured), the list variant
//	                  declares a2a-x402 without required
//
// The Go test rebuilds every card and compares bytes, and runs the full anet
// check (a2acard.Verify) on each; ext_card_a2ago_test.go has a2a-go verify
// them, and testdata/check_ext_card_1.py a2a-python
// (TestExtCard1VerifiesInA2APython). Rewrite with -update-ext-card.
var updateExtCard = flag.Bool("update-ext-card", false, "rewrite testdata/ext-card-1.json")

const extCard1Path = "testdata/ext-card-1.json"

type extCard1 struct {
	Format      string         `json:"format"`
	About       string         `json:"about"`
	ProviderAID string         `json:"provider_aid"`
	HubAID      string         `json:"hub_aid"`
	Kid         string         `json:"kid"`
	NowMS       uint64         `json:"now_ms"`
	Nodes       []extCard1Node `json:"nodes"`
}

type extCard1Node struct {
	Name          string            `json:"name"`
	Public        []string          `json:"public"`
	ListOnly      []string          `json:"list_only"`
	Prices        map[string]uint64 `json:"prices"`
	Cards         []extCard1Card    `json:"cards"`
	NotConfigured []string          `json:"not_configured"`
}

type extCard1Card struct {
	Variant string `json:"variant"` // public, general or list
	Card    string `json:"card"`    // the signed card, byte for byte
	Payload string `json:"payload"` // the JWS payload: RFC 8785 of the card without signatures
	Jku     string `json:"jku,omitempty"`
}

func extCard1Nodes(hub string) []testNode {
	return []testNode{
		{aid: "priced", hub: hub, public: []string{"translate", module.ChatSkillID}, listOnly: []string{"x"},
			prices: map[string]uint64{"translate": 3, "x": 2}},
		{aid: "list_only_priced", hub: hub, public: []string{"echo"}, listOnly: []string{"y"},
			prices: map[string]uint64{"y": 2}},
	}
}

// buildExtCard1 builds the vector from scratch.
func buildExtCard1(t *testing.T) extCard1 {
	t.Helper()
	signer := identity.SuiteAgentController()
	hub := identity.SuiteController().AID()
	out := extCard1{
		Format: "anet-ext-card-vectors/1",
		About: "G-PAY-EXT-1: public and extended cards of two nodes, signed by the conformance agent identity " +
			"(ANetCore identity.SuiteAgentController; Ed25519 seed SHA-256(\"anet-suite-identity-v1/cur\")). " +
			"card is the signed card byte for byte, payload its JWS payload. The public card names the hub's JWKS " +
			"(jku); an extended card names none. A variant in not_configured is the public card: there is no " +
			"extended card to give.",
		ProviderAID: signer.AID(), HubAID: hub, Kid: a2acard.KID(signer.AID(), 0), NowMS: testT0,
	}
	for _, n := range extCard1Nodes(hub) {
		name := n.aid
		n.aid = signer.AID()
		in := n.input(t)
		node := extCard1Node{Name: name, Public: n.public, ListOnly: n.listOnly, Prices: n.prices, NotConfigured: []string{}}
		jku := JKU(testHubURL, n.aid)
		add := func(variant string, unsigned []byte, jku string) {
			signed, err := a2acard.SignWithSnapshot(unsigned, signer, jku)
			if err != nil {
				t.Fatalf("%s %s: %v", name, variant, err)
			}
			payload, err := a2acard.SigningPayload(signed)
			if err != nil {
				t.Fatal(err)
			}
			node.Cards = append(node.Cards, extCard1Card{Variant: variant, Card: string(signed), Payload: string(payload), Jku: jku})
		}
		add("public", mustBuild(t, in), jku)
		for _, v := range []module.CardVariant{module.VariantGeneral, module.VariantList} {
			card, same, err := n.variant(t, in, v)
			if err != nil {
				t.Fatalf("%s %s: %v", name, v, err)
			}
			if same {
				node.NotConfigured = append(node.NotConfigured, string(v))
				continue
			}
			add(string(v), card, "")
		}
		out.Nodes = append(out.Nodes, node)
	}
	return out
}

func loadExtCard1(t *testing.T) extCard1 {
	t.Helper()
	b, err := os.ReadFile(extCard1Path)
	if err != nil {
		t.Fatal(err)
	}
	var v extCard1
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestExtCard1(t *testing.T) {
	built := buildExtCard1(t)
	if *updateExtCard {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", " ")
		if err := enc.Encode(built); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(extCard1Path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden := loadExtCard1(t)
	if !reflect.DeepEqual(golden, built) {
		gb, _ := json.Marshal(golden)
		bb, _ := json.Marshal(built)
		t.Fatalf("the cards changed (rewrite with -update-ext-card if that is intended)\n golden %s\n built  %s", gb, bb)
	}

	signer := identity.SuiteAgentController()
	resolve := func(aid string) ([]identity.SignedEvent, error) {
		if aid != signer.AID() {
			t.Fatalf("resolved %s", aid)
		}
		return signer.KEL(), nil
	}
	want := map[string]struct {
		cards         []string
		notConfigured []string
		declared      map[string][2]bool // variant -> (declared, required)
		prices        map[string]map[string]uint64
	}{
		"priced": {[]string{"public", "general", "list"}, []string{},
			map[string][2]bool{"public": {true, false}, "general": {true, false}, "list": {true, false}},
			map[string]map[string]uint64{"public": {}, "general": {"translate": 3}, "list": {"translate": 3, "x": 2}}},
		"list_only_priced": {[]string{"public", "list"}, []string{"general"},
			map[string][2]bool{"public": {false, false}, "list": {true, false}},
			map[string]map[string]uint64{"public": {}, "list": {"y": 2}}},
	}
	for _, n := range golden.Nodes {
		w, ok := want[n.Name]
		if !ok {
			t.Fatalf("unexpected node %s", n.Name)
		}
		var variants []string
		for _, c := range n.Cards {
			variants = append(variants, c.Variant)
			raw := []byte(c.Card)
			if canon, err := a2acard.Canonicalize(raw); err != nil || !bytes.Equal(canon, raw) {
				t.Errorf("%s %s: not RFC 8785 (%v)", n.Name, c.Variant, err)
			}
			if err := a2acard.CheckPublishForm(raw); err != nil {
				t.Errorf("%s %s: %v", n.Name, c.Variant, err)
			}
			v, err := a2acard.Verify(raw, resolve, golden.NowMS)
			if err != nil {
				t.Errorf("%s %s: %v", n.Name, c.Variant, err)
				continue
			}
			if v.AID != signer.AID() || v.Seq != testSeq || v.KeyStateSeq != 0 {
				t.Errorf("%s %s: verified %+v", n.Name, c.Variant, v)
			}
			if ok, _ := DeclaresExtended(raw); !ok {
				t.Errorf("%s %s: no extendedAgentCard", n.Name, c.Variant)
			}
			d, r, _ := X402Declaration(raw)
			if [2]bool{d, r} != w.declared[c.Variant] {
				t.Errorf("%s %s: a2a-x402 declared %v required %v, want %v", n.Name, c.Variant, d, r, w.declared[c.Variant])
			}
			if p, err := PricesOf(raw); err != nil || !reflect.DeepEqual(p, w.prices[c.Variant]) {
				t.Errorf("%s %s: prices %v (%v), want %v", n.Name, c.Variant, p, err, w.prices[c.Variant])
			}
			hdr := protectedHeader(t, raw)
			wantJku := ""
			if c.Variant == "public" {
				wantJku = JKU(testHubURL, signer.AID())
			}
			if hdr["kid"] != golden.Kid || hdr["alg"] != "EdDSA" || (hdr["jku"] != nil) != (wantJku != "") || c.Jku != wantJku {
				t.Errorf("%s %s: protected header %v", n.Name, c.Variant, hdr)
			}
		}
		if !reflect.DeepEqual(variants, w.cards) || !reflect.DeepEqual(n.NotConfigured, w.notConfigured) {
			t.Errorf("%s: cards %v, not configured %v", n.Name, variants, n.NotConfigured)
		}
	}
}

func protectedHeader(t *testing.T, card []byte) map[string]any {
	t.Helper()
	var c struct {
		Signatures []a2acard.Signature `json:"signatures"`
	}
	if err := json.Unmarshal(card, &c); err != nil || len(c.Signatures) != 1 {
		t.Fatalf("signatures: %v", err)
	}
	b, err := base64.RawURLEncoding.DecodeString(c.Signatures[0].Protected)
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

// TestExtCard1VerifiesInA2APython has the a2a-python reference SDK parse
// and verify every card of the vector (G-PAY-EXT-1), with
// testdata/check_ext_card_1.py. It needs a Python with a2a-sdk[signing]:
// ANET_A2A_PYTHON names it.
func TestExtCard1VerifiesInA2APython(t *testing.T) {
	py := os.Getenv("ANET_A2A_PYTHON")
	if py == "" {
		t.Skip("needs ANET_A2A_PYTHON: a Python interpreter with a2a-sdk[signing] installed")
	}
	script, err := filepath.Abs("testdata/check_ext_card_1.py")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, script, extCard1Path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a2a-python: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "verified 5 cards") {
		t.Fatalf("a2a-python did not verify every card:\n%s", out)
	}
	t.Logf("%s", out)
}

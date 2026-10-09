package netcard

import (
	"encoding/json"
	"sort"
	"strconv"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
)

// testNode is a node as these tests build its cards: public and allow_only
// skills, and what each costs. contribute stands in for the payment
// module's CardExtensions (module/x402, whose own tests hold it to the same
// rules): a2a-x402 for the skills a card lists, and on an extended card the
// prices.
type testNode struct {
	aid      string
	hub      string // the hub's AID, the ledger the prices are in
	public   []string
	listOnly []string
	prices   map[string]uint64
	// extra are contributions a module makes to every card regardless; the
	// kernel must still drop a price list from the public card.
	extra []map[string]any
}

const (
	testHubURL = "https://hub.example.org"
	testT0     = 1767225600000 // 2026-01-01T00:00:00Z
	testSeq    = 7
)

func suiteHubAID() string { return identity.SuiteController().AID() }

func (n testNode) contribute(c module.CardContext) []map[string]any {
	var prices []any
	for _, id := range c.Skills {
		if p, ok := n.prices[id]; ok {
			prices = append(prices, map[string]any{"skillId": id, "amount": strconv.FormatUint(p, 10)})
		}
	}
	out := append([]map[string]any(nil), n.extra...)
	if len(prices) == 0 {
		return out
	}
	x := map[string]any{"uri": module.ExtX402URI, "description": "priced skills take payment inside the same task"}
	if len(prices) == len(c.Skills) {
		x["required"] = true
	}
	out = append(out, x)
	if c.Extended {
		out = append(out, map[string]any{"uri": module.ExtPriceURI, "description": "prices in credits",
			"params": map[string]any{"network": payment.CreditNetwork(n.hub), "asset": payment.AssetCredit, "prices": prices}})
	}
	return out
}

// accept puts contributions through Extension, as the daemon does.
func accept(t testing.TB, exts []map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range exts {
		x, err := Extension(e)
		if err != nil {
			t.Fatalf("contribution %v: %v", e, err)
		}
		out = append(out, x)
	}
	return out
}

func skillsOf(ids []string) []Skill {
	out := make([]Skill, 0, len(ids))
	for _, id := range ids {
		if id == module.ChatSkillID {
			out = append(out, ChatSkill())
			continue
		}
		out = append(out, Skill{ID: id, Name: id, Description: "The " + id + " skill.", Tags: []string{"test"}})
	}
	return out
}

// input is the public card's input of n, with its contributions.
func (n testNode) input(t testing.TB) Input {
	t.Helper()
	pub := append([]string(nil), n.public...)
	sort.Strings(pub)
	lo := append([]string(nil), n.listOnly...)
	sort.Strings(lo)
	in := Input{AID: n.aid, Name: "Test Agent", Description: "A node of the extended card tests.", Version: "1.0.0",
		HubURL: testHubURL, Seq: testSeq, IssuedAtMs: testT0, NotBeforeMs: testT0 - 60_000,
		Skills: skillsOf(pub), ListOnly: skillsOf(lo)}
	in.Extensions = accept(t, n.contribute(module.CardContext{AID: n.aid, HubURL: testHubURL, Skills: pub}))
	return in
}

// variant builds variant v of n's extended card as the daemon does: the
// modules asked with that variant's skills.
func (n testNode) variant(t testing.TB, in Input, v module.CardVariant) ([]byte, bool, error) {
	t.Helper()
	skills, err := VariantSkills(in, v)
	if err != nil {
		t.Fatal(err)
	}
	cc := module.CardContext{AID: n.aid, HubURL: testHubURL, Skills: SkillIDs(skills), Extended: true, Variant: v}
	return BuildExtended(in, v, accept(t, n.contribute(cc)))
}

// cardJSON decodes a card for a test to look at.
func cardJSON(t testing.TB, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("card is not JSON: %v\n%s", err, raw)
	}
	return m
}

// extURIs are the extension URIs a card carries.
func extURIs(t testing.TB, raw []byte) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	caps, _ := cardJSON(t, raw)["capabilities"].(map[string]any)
	exts, _ := caps["extensions"].([]any)
	for _, e := range exts {
		m := e.(map[string]any)
		out[m["uri"].(string)] = m
	}
	return out
}

// skillIDsOf are the skill ids a card lists.
func skillIDsOf(t testing.TB, raw []byte) []string {
	t.Helper()
	var out []string
	sk, _ := cardJSON(t, raw)["skills"].([]any)
	for _, s := range sk {
		out = append(out, s.(map[string]any)["id"].(string))
	}
	return out
}

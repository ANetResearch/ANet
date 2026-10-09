package netcard

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
)

// The four kinds of node a public card must not tell apart by its
// extendedAgentCard flag (PAY-C2, CORE-K23 rule 6): free with nothing for
// its lists, free with an allow_only capability, some skills priced, every
// skill priced. Each public card says an extended card may be asked for, so
// the flag reveals neither prices nor capabilities for the lists.
func TestPublicCardAlwaysDeclaresExtended(t *testing.T) {
	hub := suiteHubAID()
	for name, n := range map[string]testNode{
		"free, nothing for the lists": {aid: "bafyaid", hub: hub, public: []string{"face.detect"}},
		"free, an allow_only skill":   {aid: "bafyaid", hub: hub, public: []string{"face.detect"}, listOnly: []string{"face.compare"}},
		"some skills priced": {aid: "bafyaid", hub: hub, public: []string{"translate", "chat"},
			prices: map[string]uint64{"translate": 3}},
		"every skill priced": {aid: "bafyaid", hub: hub, public: []string{"vision.describe"},
			prices: map[string]uint64{"vision.describe": 2}},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := Build(n.input(t))
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := DeclaresExtended(raw); err != nil || !ok {
				t.Fatalf("the public card does not declare an extended card (%v):\n%s", err, raw)
			}
			caps := cardJSON(t, raw)["capabilities"].(map[string]any)
			if caps["extendedAgentCard"] != true {
				t.Fatalf("capabilities %v", caps)
			}
			if err := a2acard.CheckPublishForm(raw); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// PAY-C1: the public card names no price, whatever a module contributes —
// a price list in the extended card's form, or the public price list of
// 0.2 — and keeps the rest of what the module said.
func TestThePublicCardDropsEveryPriceList(t *testing.T) {
	n := testNode{aid: "bafyaid", hub: suiteHubAID(), public: []string{"translate", "chat"},
		prices: map[string]uint64{"translate": 3},
		extra: []map[string]any{
			{"uri": module.ExtPriceURI, "params": map[string]any{"network": payment.CreditNetwork(suiteHubAID()),
				"asset": "credit", "prices": []any{map[string]any{"skillId": "translate", "amount": "3"}}}},
			{"uri": retiredPricingURI, "params": map[string]any{"prices": []any{map[string]any{"skillId": "translate", "amount": "3"}}}},
		}}
	raw, err := Build(n.input(t))
	if err != nil {
		t.Fatal(err)
	}
	exts := extURIs(t, raw)
	for _, u := range []string{module.ExtPriceURI, retiredPricingURI} {
		if _, ok := exts[u]; ok {
			t.Errorf("the public card carries %s", u)
		}
	}
	if _, ok := exts[module.ExtX402URI]; !ok {
		t.Errorf("the a2a-x402 declaration went with the price lists: %v", exts)
	}
	for _, leak := range []string{`"prices"`, `"amount"`} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("the public card carries %s:\n%s", leak, raw)
		}
	}
	if p, err := PricesOf(raw); err != nil || len(p) != 0 {
		t.Fatalf("PricesOf(public) = %v, %v", p, err)
	}
	// The retired list is dropped from an extended card too; the payment
	// module's own is what it carries.
	n.extra = n.extra[1:]
	card, _, err := n.variant(t, n.input(t), module.VariantGeneral)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := extURIs(t, card)[retiredPricingURI]; ok {
		t.Fatalf("an extended card carries the 0.2 price list:\n%s", card)
	}
	if p, err := PricesOf(card); err != nil || !reflect.DeepEqual(p, map[string]uint64{"translate": 3}) {
		t.Fatalf("prices %v, %v", p, err)
	}
}

// The two variants of a node with a priced public skill, a free one and a
// priced allow_only one (the provider of J-PAY-4): the general card lists the public
// skills and their prices; the list card adds the allow_only skill and its
// price; both declare a2a-x402 without required; neither is the public
// card.
func TestTheTwoVariants(t *testing.T) {
	n := testNode{aid: "bafyaid", hub: suiteHubAID(), public: []string{"translate", "chat"}, listOnly: []string{"x"},
		prices: map[string]uint64{"translate": 3, "x": 2}}
	in := n.input(t)
	for _, c := range []struct {
		v      module.CardVariant
		skills []string
		prices map[string]uint64
	}{
		{module.VariantGeneral, []string{"chat", "translate"}, map[string]uint64{"translate": 3}},
		{module.VariantList, []string{"chat", "translate", "x"}, map[string]uint64{"translate": 3, "x": 2}},
	} {
		card, same, err := n.variant(t, in, c.v)
		if err != nil || same {
			t.Fatalf("%s: same %v, %v", c.v, same, err)
		}
		if got := skillIDsOf(t, card); !reflect.DeepEqual(got, c.skills) {
			t.Errorf("%s lists %v, want %v", c.v, got, c.skills)
		}
		if got, err := PricesOf(card); err != nil || !reflect.DeepEqual(got, c.prices) {
			t.Errorf("%s prices %v (%v), want %v", c.v, got, err, c.prices)
		}
		if d, r, err := X402Declaration(card); err != nil || !d || r {
			t.Errorf("%s: a2a-x402 declared %v required %v (%v)", c.v, d, r, err)
		}
		if ok, _ := DeclaresExtended(card); !ok {
			t.Errorf("%s does not declare extendedAgentCard", c.v)
		}
		if err := a2acard.CheckPublishForm(card); err != nil {
			t.Errorf("%s: %v", c.v, err)
		}
		// The rest is the public card's: name, interfaces, the anet-card
		// params (seq included), the evidence extension.
		pub := cardJSON(t, mustBuild(t, in))
		got := cardJSON(t, card)
		for _, k := range []string{"name", "description", "version", "supportedInterfaces", "defaultInputModes"} {
			if !reflect.DeepEqual(got[k], pub[k]) {
				t.Errorf("%s: %s %v, the public card's %v", c.v, k, got[k], pub[k])
			}
		}
		if !reflect.DeepEqual(extURIs(t, card)[a2acard.ExtCardURI], extURIs(t, mustBuild(t, in))[a2acard.ExtCardURI]) {
			t.Errorf("%s: anet-card params differ from the public card's", c.v)
		}
	}
}

func mustBuild(t testing.TB, in Input) []byte {
	t.Helper()
	raw, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A free node with an allow_only skill (a face-detection node, J-PAY-4):
// the general variant is the public card (not_configured), the list variant
// lists the allow_only skill with no price and no a2a-x402. A node with
// only allow_only skills priced (J-PAY-15): the public card and the
// general variant declare nothing, the list variant declares a2a-x402
// without required.
func TestVariantsOfFreeAndListPricedNodes(t *testing.T) {
	face := testNode{aid: "bafyaid", hub: suiteHubAID(), public: []string{"face.detect"}, listOnly: []string{"face.compare"}}
	in := face.input(t)
	if _, same, err := face.variant(t, in, module.VariantGeneral); err != nil || !same {
		t.Fatalf("face node general: same %v, %v", same, err)
	}
	card, same, err := face.variant(t, in, module.VariantList)
	if err != nil || same {
		t.Fatalf("face node list: same %v, %v", same, err)
	}
	if got := skillIDsOf(t, card); !reflect.DeepEqual(got, []string{"face.compare", "face.detect"}) {
		t.Fatalf("list skills %v", got)
	}
	if d, _, _ := X402Declaration(card); d {
		t.Fatal("a free list variant declares a2a-x402")
	}
	if p, _ := PricesOf(card); len(p) != 0 {
		t.Fatalf("a free list variant has prices %v", p)
	}

	listPriced := testNode{aid: "bafyaid", hub: suiteHubAID(), public: []string{"echo"}, listOnly: []string{"y"},
		prices: map[string]uint64{"y": 2}}
	in = listPriced.input(t)
	if d, _, _ := X402Declaration(mustBuild(t, in)); d {
		t.Fatal("the public card of a node whose public skills are free declares a2a-x402")
	}
	if _, same, err := listPriced.variant(t, in, module.VariantGeneral); err != nil || !same {
		t.Fatalf("list priced, general: same %v, %v", same, err)
	}
	card, _, err = listPriced.variant(t, in, module.VariantList)
	if err != nil {
		t.Fatal(err)
	}
	if d, r, _ := X402Declaration(card); !d || r {
		t.Fatalf("list priced, list: declared %v required %v; want declared, required left out", d, r)
	}
	// Every skill of the list priced: required.
	all := testNode{aid: "bafyaid", hub: suiteHubAID(), public: []string{"a"}, listOnly: []string{"b"},
		prices: map[string]uint64{"a": 1, "b": 2}}
	card, _, err = all.variant(t, all.input(t), module.VariantList)
	if err != nil {
		t.Fatal(err)
	}
	if d, r, _ := X402Declaration(card); !d || !r {
		t.Fatalf("all priced: declared %v required %v", d, r)
	}
}

// A node with nothing to add has no extended card: both variants are the
// public card.
func TestAFreeNodeHasNoExtendedCard(t *testing.T) {
	n := testNode{aid: "bafyaid", hub: suiteHubAID(), public: []string{"echo", "chat"}}
	in := n.input(t)
	for _, v := range []module.CardVariant{module.VariantGeneral, module.VariantList} {
		card, same, err := n.variant(t, in, v)
		if err != nil || !same || !bytes.Equal(card, mustBuild(t, in)) {
			t.Fatalf("%s: same %v, %v", v, same, err)
		}
	}
	if _, _, err := BuildExtended(in, "", nil); err == nil {
		t.Fatal("an empty variant was built")
	}
	if _, _, err := BuildExtended(in, "everyone", nil); err == nil {
		t.Fatal("an unknown variant was built")
	}
}

// A price list that does not hold is an error, never a card: every member
// checked as HPP §2.6 says, and the card's a2a-x402 declaration held to its
// prices.
func TestBuildExtendedRefusesAPriceListThatDoesNotHold(t *testing.T) {
	hub := suiteHubAID()
	price := func(params map[string]any) map[string]any {
		return map[string]any{"uri": module.ExtPriceURI, "params": params}
	}
	good := func() map[string]any {
		return map[string]any{"network": payment.CreditNetwork(hub), "asset": "credit",
			"prices": []any{map[string]any{"skillId": "translate", "amount": "3"}}}
	}
	with := func(k string, v any) map[string]any {
		m := good()
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	entry := func(id, amount string) map[string]any {
		return with("prices", []any{map[string]any{"skillId": id, "amount": amount}})
	}
	x402 := map[string]any{"uri": module.ExtX402URI}
	in := testNode{aid: "bafyaid", hub: hub, public: []string{"translate", "chat"}}.input(t)
	for name, exts := range map[string][]map[string]any{
		"network not a hub":       {x402, price(with("network", "anet"))},
		"network hub not an AID":  {x402, price(with("network", "hub:nope"))},
		"no network":              {x402, price(with("network", nil))},
		"asset":                   {x402, price(with("asset", "usd"))},
		"no prices":               {x402, price(with("prices", nil))},
		"prices not an array":     {x402, price(with("prices", "3"))},
		"unknown member":          {x402, price(with("currency", "credit"))},
		"skill not on the card":   {x402, price(entry("ghost", "3"))},
		"amount zero":             {x402, price(entry("translate", "0"))},
		"amount leading zero":     {x402, price(entry("translate", "03"))},
		"amount negative":         {x402, price(entry("translate", "-3"))},
		"amount over 2^63-1":      {x402, price(entry("translate", "9223372036854775808"))},
		"amount not decimal":      {x402, price(entry("translate", "3.0"))},
		"amount spaced":           {x402, price(entry("translate", " 3"))},
		"entry with extra member": {x402, price(with("prices", []any{map[string]any{"skillId": "translate", "amount": "3", "unit": "credit"}}))},
		"same skill twice": {x402, price(with("prices", []any{map[string]any{"skillId": "translate", "amount": "3"},
			map[string]any{"skillId": "translate", "amount": "4"}}))},
		"required price list":    {x402, {"uri": module.ExtPriceURI, "required": true, "params": good()}},
		"prices, no a2a-x402":    {price(good())},
		"a2a-x402, no prices":    {x402},
		"every skill, not req'd": {x402, price(with("prices", []any{map[string]any{"skillId": "translate", "amount": "3"}, map[string]any{"skillId": "chat", "amount": "1"}}))},
		"some skills, required":  {{"uri": module.ExtX402URI, "required": true}, price(good())},
	} {
		if card, _, err := BuildExtended(in, module.VariantGeneral, accept(t, exts)); err == nil {
			t.Errorf("%s: built\n%s", name, card)
		} else if !errors.Is(err, ErrBadPrices) {
			t.Errorf("%s: %v, not ErrBadPrices", name, err)
		}
	}
	// The largest price is 2^63-1.
	card, _, err := BuildExtended(in, module.VariantGeneral, accept(t, []map[string]any{x402, price(entry("translate", "9223372036854775807"))}))
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := PricesOf(card); p["translate"] != 1<<63-1 {
		t.Fatalf("prices %v", p)
	}
	if ValidPrice(0) || !ValidPrice(1) || !ValidPrice(1<<63-1) || ValidPrice(1<<63) {
		t.Fatal("ValidPrice bounds")
	}
}

// The reads are by exact member names: a member spelled in another case is
// not the one a2acard.Verify and a2a-python read.
func TestCardReadsUseExactNames(t *testing.T) {
	raw := []byte(`{"capabilities":{"ExtendedAgentCard":true,"extensions":[{"URI":"` + module.ExtX402URI + `"}]},"skills":[]}`)
	if ok, err := DeclaresExtended(raw); err != nil || ok {
		t.Fatalf("DeclaresExtended = %v, %v", ok, err)
	}
	if d, _, err := X402Declaration(raw); err != nil || d {
		t.Fatalf("X402Declaration = %v, %v", d, err)
	}
	if _, err := PricesOf([]byte(`[1]`)); err == nil {
		t.Fatal("a card that is not an object was read")
	}
	two := []byte(`{"capabilities":{"extensions":[{"uri":"` + module.ExtX402URI + `"},{"uri":"` + module.ExtX402URI + `"}]}}`)
	if _, _, err := X402Declaration(two); err == nil {
		t.Fatal("a double a2a-x402 declaration was read")
	}
}

func TestVariantSkillsKeepsTheListsApart(t *testing.T) {
	in := Input{Skills: skillsOf([]string{"b", "a"}), ListOnly: skillsOf([]string{"c", "0"})}
	g, err := VariantSkills(in, module.VariantGeneral)
	if err != nil || !slices.Equal(SkillIDs(g), []string{"a", "b"}) {
		t.Fatalf("general %v %v", SkillIDs(g), err)
	}
	l, err := VariantSkills(in, module.VariantList)
	if err != nil || !slices.Equal(SkillIDs(l), []string{"0", "a", "b", "c"}) {
		t.Fatalf("list %v %v", SkillIDs(l), err)
	}
	// A ListOnly skill never reaches the public card.
	n := testNode{aid: "bafyaid", hub: suiteHubAID(), public: []string{"a"}, listOnly: []string{"secret.thing"}}
	if raw := mustBuild(t, n.input(t)); strings.Contains(string(raw), "secret.thing") {
		t.Fatalf("the public card lists an allow_only skill:\n%s", raw)
	}
}

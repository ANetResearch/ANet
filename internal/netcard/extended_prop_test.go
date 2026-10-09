package netcard

import (
	"bytes"
	"math/rand/v2"
	"reflect"
	"strconv"
	"testing"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
)

// P-PAY-9 (HPP §7.2): over random capability sets — public and allow_only,
// priced and free mixed, the chat skill or not, and a module that also
// offers price lists to every card —
//
//   - the public card carries no price list and always declares
//     extendedAgentCard;
//   - the general variant lists no allow_only skill, the list variant all
//     of them;
//   - each variant's prices are exactly Price() of its skills;
//   - each card (public and both variants) declares a2a-x402 iff a skill it
//     lists is priced, required iff all are;
//   - a variant is reported the same as the public card (not_configured)
//     iff its content is the public card's.
func TestExtendedCardProperties(t *testing.T) {
	seed := uint64(20261009)
	r := rand.New(rand.NewPCG(seed, seed^0x5eed))
	hub := suiteHubAID()
	for i := 0; i < 600; i++ {
		n := randomNode(r, hub)
		in := n.input(t)
		pub, err := Build(in)
		if err != nil {
			t.Fatalf("case %d %+v: %v", i, n, err)
		}
		checkCard(t, i, "public", n, pub, n.public)
		exts := extURIs(t, pub)
		if _, ok := exts[module.ExtPriceURI]; ok {
			t.Fatalf("case %d: the public card carries prices", i)
		}
		if _, ok := exts[retiredPricingURI]; ok {
			t.Fatalf("case %d: the public card carries the 0.2 price list", i)
		}
		if ok, _ := DeclaresExtended(pub); !ok {
			t.Fatalf("case %d: the public card does not declare extendedAgentCard", i)
		}
		for _, v := range []module.CardVariant{module.VariantGeneral, module.VariantList} {
			skills := n.public
			if v == module.VariantList {
				skills = append(append([]string(nil), n.public...), n.listOnly...)
			}
			card, same, err := n.variant(t, in, v)
			if err != nil {
				t.Fatalf("case %d %s %+v: %v", i, v, n, err)
			}
			if want := sameContent(t, pub, card); same != want {
				t.Fatalf("case %d %s: reported same %v, content same %v\npublic %s\nvariant %s", i, v, same, want, pub, card)
			}
			if wantSame := !anyPriced(n, skills) && (v == module.VariantGeneral || len(n.listOnly) == 0); same != wantSame {
				t.Fatalf("case %d %s: same %v; it has prices or skills beyond the public card: %v", i, v, same, !wantSame)
			}
			checkCard(t, i, string(v), n, card, skills)
			if v == module.VariantGeneral {
				for _, id := range skillIDsOf(t, card) {
					for _, lo := range n.listOnly {
						if id == lo {
							t.Fatalf("case %d: the general variant lists the allow_only skill %s", i, id)
						}
					}
				}
			}
			if err := a2acard.CheckPublishForm(card); err != nil {
				t.Fatalf("case %d %s: %v", i, v, err)
			}
		}
	}
}

// checkCard holds one card to its skills and prices: it lists exactly
// skills; its prices are Price() of them (none on the public card); and its
// a2a-x402 declaration follows them.
func checkCard(t *testing.T, i int, which string, n testNode, card []byte, skills []string) {
	t.Helper()
	want := map[string]bool{}
	for _, id := range skills {
		want[id] = true
	}
	got := map[string]bool{}
	for _, id := range skillIDsOf(t, card) {
		got[id] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("case %d %s lists %v, want %v", i, which, got, want)
	}
	prices, err := PricesOf(card)
	if err != nil {
		t.Fatalf("case %d %s: %v", i, which, err)
	}
	wantPrices := map[string]uint64{}
	if which != "public" {
		for _, id := range skills {
			if p, ok := n.prices[id]; ok {
				wantPrices[id] = p
			}
		}
	}
	if !reflect.DeepEqual(prices, wantPrices) {
		t.Fatalf("case %d %s prices %v, want %v", i, which, prices, wantPrices)
	}
	pricedN := 0
	for _, id := range skills {
		if _, ok := n.prices[id]; ok {
			pricedN++
		}
	}
	d, r, err := X402Declaration(card)
	if err != nil {
		t.Fatalf("case %d %s: %v", i, which, err)
	}
	if d != (pricedN > 0) || (d && r != (pricedN == len(skills))) {
		t.Fatalf("case %d %s: a2a-x402 declared %v required %v with %d of %d skills priced", i, which, d, r, pricedN, len(skills))
	}
}

func anyPriced(n testNode, skills []string) bool {
	for _, id := range skills {
		if _, ok := n.prices[id]; ok {
			return true
		}
	}
	return false
}

// sameContent compares two cards as statements: the same JSON value.
func sameContent(t *testing.T, a, b []byte) bool {
	t.Helper()
	ca, err := a2acard.Canonicalize(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := a2acard.Canonicalize(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(ca, cb)
}

func randomNode(r *rand.Rand, hub string) testNode {
	n := testNode{aid: "bafyaid", hub: hub, prices: map[string]uint64{}}
	nPub := 1 + r.IntN(5)
	nList := r.IntN(4)
	id := 0
	next := func() string { id++; return "cap" + strconv.Itoa(id) + ".do" }
	price := func(s string) {
		switch r.IntN(3) {
		case 0: // free
		case 1:
			n.prices[s] = 1 + r.Uint64N(1000)
		default:
			n.prices[s] = 1 + r.Uint64N(1<<63-1)
		}
	}
	for i := 0; i < nPub; i++ {
		s := next()
		n.public = append(n.public, s)
		price(s)
	}
	if r.IntN(3) == 0 {
		n.public = append(n.public, module.ChatSkillID) // never priced
	}
	for i := 0; i < nList; i++ {
		s := next()
		n.listOnly = append(n.listOnly, s)
		price(s)
	}
	if r.IntN(2) == 0 {
		// A module that offers the 0.2 price list to every card: the kernel
		// keeps it off all of them. (A stray anet-price list is kept off the
		// public card too, and refused on an extended card:
		// TestAStrayPriceListIsKeptOffThePublicCardAndRefusedElsewhere.)
		pl := []any{map[string]any{"skillId": n.public[0], "amount": "5"}}
		n.extra = []map[string]any{
			{"uri": retiredPricingURI, "params": map[string]any{"prices": pl}},
		}
	}
	return n
}

// A price list a module offers beside the payment module's own: kept off
// the public card like any other, and on an extended card refused (two
// lists, or a list without a2a-x402) rather than put on a signed card.
func TestAStrayPriceListIsKeptOffThePublicCardAndRefusedElsewhere(t *testing.T) {
	hub := suiteHubAID()
	pl := []any{map[string]any{"skillId": "echo", "amount": "5"}}
	stray := map[string]any{"uri": module.ExtPriceURI,
		"params": map[string]any{"network": payment.CreditNetwork(hub), "asset": "credit", "prices": pl}}
	for name, n := range map[string]testNode{
		"free node":   {aid: "bafyaid", hub: hub, public: []string{"echo"}, extra: []map[string]any{stray}},
		"priced node": {aid: "bafyaid", hub: hub, public: []string{"echo"}, prices: map[string]uint64{"echo": 3}, extra: []map[string]any{stray}},
	} {
		in := n.input(t)
		if raw := mustBuild(t, in); bytes.Contains(raw, []byte("anet-price")) || bytes.Contains(raw, []byte(`"prices"`)) {
			t.Fatalf("%s: the public card carries the stray list:\n%s", name, raw)
		}
		if card, _, err := n.variant(t, in, module.VariantGeneral); err == nil {
			t.Fatalf("%s: an extended card took the stray list:\n%s", name, card)
		}
	}
}

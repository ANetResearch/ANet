package netcard

import (
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANetCore/a2acard"

	"github.com/ANetResearch/ANet/module"
)

// FuzzCardReads feeds arbitrary bytes to the readers of a card (PricesOf,
// X402Declaration, DeclaresExtended), which also read the extended cards of
// other nodes: they never panic, a parse failure is a failure of all three,
// a price list they accept holds only amounts of 1..2^63-1 for skills the
// card lists, required is never read without the declaration, and a card
// read twice — as given and in its RFC 8785 form — reads the same.
func FuzzCardReads(f *testing.F) {
	for _, n := range loadExtCard1(f).Nodes {
		for _, c := range n.Cards {
			f.Add([]byte(c.Card))
		}
	}
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"capabilities":{"extensions":[{"uri":"` + module.ExtPriceURI + `","params":{"prices":[]}}]}}`))
	f.Add([]byte(`{"skills":[{"id":"a"}],"capabilities":{"extendedAgentCard":true,"extensions":[{"uri":"` +
		module.ExtPriceURI + `","params":{"network":"hub:x","asset":"credit","prices":[{"skillId":"a","amount":"1"}]}}]}}`))
	f.Add([]byte(`{"capabilities":{"extensions":[{"URI":"x","Required":true}]}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		prices, perr := PricesOf(b)
		declared, required, xerr := X402Declaration(b)
		_, eerr := DeclaresExtended(b)
		_, parseErr := parseCard(b)
		if parseErr != nil && (perr == nil || xerr == nil || eerr == nil) {
			t.Fatalf("an unreadable card was read: %v / %v %v %v", parseErr, perr, xerr, eerr)
		}
		if required && !declared {
			t.Fatal("required without the declaration")
		}
		if perr == nil {
			c, _ := parseCard(b)
			skills := map[string]bool{}
			for _, s := range c.Skills {
				skills[s.ID] = true
			}
			for id, n := range prices {
				if !ValidPrice(n) || !skills[id] {
					t.Fatalf("accepted price %q = %d", id, n)
				}
			}
		}
		canon, err := a2acard.Canonicalize(b)
		if err != nil {
			return
		}
		p2, perr2 := PricesOf(canon)
		if (perr == nil) != (perr2 == nil) || (perr == nil && !equalPrices(prices, p2)) {
			t.Fatalf("the canonical form reads differently: %v %v / %v %v", prices, perr, p2, perr2)
		}
		d2, r2, _ := X402Declaration(canon)
		if d2 != declared || r2 != required {
			t.Fatal("the canonical form declares differently")
		}
		var v any
		if json.Unmarshal(canon, &v) != nil {
			t.Fatal("canonical form is not JSON")
		}
	})
}

func equalPrices(a, b map[string]uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

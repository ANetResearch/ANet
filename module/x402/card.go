//go:build !no_x402

package x402

import (
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
)

// CardExtensions declares payment on this node's network card
// (A2A-DESIGN §8.1, §10.4): the a2a-x402 v0.2 extension and the signed
// price list, both only when at least one published skill has a price.
//
// a2a-x402 is required when every published skill is priced: a client
// that cannot pay can use nothing here. Otherwise required is left out,
// which in the card's publish form is how false is written.
//
// The price list sits inside the card signature so a hub cannot quote a
// price the provider never agreed to. Amounts are decimal strings of the
// hub's credit unit; network names the ledger they settle on.
//
// It is also what lets the hub name the skill a settlement paid for: the
// settle body carries the payee and the exact amount (and no resource),
// and where the payee's published prices differ per skill, the amount
// picks the skill. With c.WithholdPrices (payments.publish_prices=false)
// the list is left out and only a2a-x402 is declared; the price reaches
// the payer in the quote, end to end encrypted (A2A-DESIGN §21)
// [redteam:F1].
func (m *Module) CardExtensions(c module.CardContext) []map[string]any {
	network := m.HomeNetwork()
	if network == "" {
		// No hub, no ledger: nothing on this node can be paid for.
		return nil
	}
	prices := []any{}
	for _, id := range c.Skills {
		if amount, ok := m.Price(id); ok {
			prices = append(prices, map[string]any{"skillId": id, "amount": payment.Amount(amount)})
		}
	}
	if len(prices) == 0 {
		return nil
	}
	x402 := map[string]any{
		"uri":         module.ExtX402URI,
		"description": "priced skills take payment inside the same task (a2a-x402 v0.2 metadata, x402 v2 objects)",
	}
	if len(prices) == len(c.Skills) {
		x402["required"] = true
	}
	if c.WithholdPrices {
		return []map[string]any{x402}
	}
	pricing := map[string]any{
		"uri":         module.ExtPricingURI,
		"description": "prices of the priced skills, in credits of the named hub ledger",
		"params":      map[string]any{"network": network, "prices": prices},
	}
	return []map[string]any{x402, pricing}
}

// CardInterfaces adds no interface: payment rides the task's own binding.
func (m *Module) CardInterfaces(module.CardContext) []map[string]any { return nil }

var _ module.CardContributor = (*Module)(nil)

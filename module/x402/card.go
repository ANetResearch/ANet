//go:build !no_x402

package x402

import (
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
)

// CardExtensions declares payment on this node's cards (A2A-DESIGN §8.1,
// §10.4; HPP §2.6): the a2a-x402 v0.2 extension when at least one skill
// the card lists has a price, and on an extended card the prices
// themselves.
//
// Each card states a2a-x402 for the skills it lists (c.Skills), the public
// card and each variant of the extended card alike. a2a-x402 is required
// when every listed skill is priced: a client that cannot pay can use
// nothing there. Otherwise required is left out, which in the card's
// publish form is how false is written. So a node whose public skills are
// free and whose allow_only skill is priced declares nothing on its public
// card and a2a-x402 on the list variant, which is the AgentCard a caller on
// its lists replaces the public card with (A2A §13.3).
//
// The public card carries no price. A price per skill on it let anyone who
// sees a settlement's payee and amount — the hub, and readers of the
// public issuance chain for a cross-hub payment — name the skill that was
// paid for (A2A-DESIGN §21) [redteam:F1]. An extended card, which this node
// signs and gives only to a caller that asks it end to end, lists them
// (module.ExtPriceURI): every price comes from Price, the same answer the
// quote is made from, so an honest provider never quotes above its signed
// price (PAY-C5).
func (m *Module) CardExtensions(c module.CardContext) []map[string]any {
	network := m.HomeNetwork()
	if network == "" {
		// No hub, no ledger: nothing on this node can be paid for.
		return nil
	}
	var prices []any
	for _, id := range c.Skills {
		if p, ok := m.Price(id); ok {
			prices = append(prices, map[string]any{"skillId": id, "amount": payment.Amount(p)})
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
	out := []map[string]any{x402}
	if c.Extended {
		out = append(out, map[string]any{
			"uri": module.ExtPriceURI,
			"description": "the price of each skill in credits of the named hub's ledger, signed by this agent; " +
				"a skill not listed is free, and a task's quote above its price here is not paid automatically",
			"params": map[string]any{"network": network, "asset": payment.AssetCredit, "prices": prices},
		})
	}
	return out
}

// HasPrices reports whether this node prices any capability it serves: the
// grant's pay flag (core X-SS-8), from the same answers as the cards' prices
// and the quotes (Price).
func (m *Module) HasPrices() bool {
	if m.host == nil {
		return false
	}
	for _, id := range m.host.Providers().Capabilities() {
		if _, ok := m.Price(id); ok {
			return true
		}
	}
	return false
}

// CardInterfaces adds no interface: payment rides the task's own binding.
func (m *Module) CardInterfaces(module.CardContext) []map[string]any { return nil }

var _ module.CardContributor = (*Module)(nil)

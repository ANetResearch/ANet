//go:build !no_x402

package x402

import (
	"github.com/ANetResearch/ANet/module"
)

// CardExtensions declares payment on this node's network card
// (A2A-DESIGN §8.1, §10.4): the a2a-x402 v0.2 extension, only when at
// least one published skill has a price.
//
// a2a-x402 is required when every published skill is priced: a client
// that cannot pay can use nothing here. Otherwise required is left out,
// which in the card's publish form is how false is written.
//
// The public card carries no price. A price per skill on it let anyone who
// sees a settlement's payee and amount — the hub, and readers of the
// public issuance chain for a cross-hub payment — name the skill that was
// paid for (A2A-DESIGN §21) [redteam:F1]. What a skill costs reaches the
// payer in the quote, end to end encrypted, as a2a-x402 defines it.
func (m *Module) CardExtensions(c module.CardContext) []map[string]any {
	if m.HomeNetwork() == "" {
		// No hub, no ledger: nothing on this node can be paid for.
		return nil
	}
	priced := 0
	for _, id := range c.Skills {
		if _, ok := m.Price(id); ok {
			priced++
		}
	}
	if priced == 0 {
		return nil
	}
	x402 := map[string]any{
		"uri":         module.ExtX402URI,
		"description": "priced skills take payment inside the same task (a2a-x402 v0.2 metadata, x402 v2 objects)",
	}
	if priced == len(c.Skills) {
		x402["required"] = true
	}
	return []map[string]any{x402}
}

// CardInterfaces adds no interface: payment rides the task's own binding.
func (m *Module) CardInterfaces(module.CardContext) []map[string]any { return nil }

var _ module.CardContributor = (*Module)(nil)

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// payments.publish_prices=false (A2A-DESIGN §21) [redteam:F1]. A
// settlement's payee and amount reach the hub, and a cross-hub one's the
// public issuance chain; a price per skill on the payee's cards lets
// either name the skill that was paid for. With prices withheld, neither
// card carries one: the A2A card still declares a2a-x402, the ADP card has
// no price list (so the hub gateway cannot sell the skill), and the price
// is given only in the quote. Absent, the option is true.
func TestWithheldPricesAreOnNeitherCard(t *testing.T) {
	compiled := false
	for _, n := range module.Compiled() {
		compiled = compiled || n == "x402"
	}
	if !compiled {
		t.Skip("no payment module in this build")
	}
	for _, c := range []struct {
		name    string
		publish *bool
		want    bool
	}{{"absent", nil, true}, {"true", publishPricesOpt(true), true}, {"false", publishPricesOpt(false), false}} {
		t.Run(c.name, func(t *testing.T) {
			h := newFakeHub(t)
			d := newCardDaemon(t, "Worker", "")
			if err := d.Providers().Register(context.Background(), &cardPricedWork{}); err != nil {
				t.Fatal(err)
			}
			setPublic(d, "work.priced")
			d.mu.Lock()
			d.cfg.HubURL = h.URL
			p := defaultPayments()
			p.PublishPrices = c.publish
			d.cfg.Payments = &p
			d.mu.Unlock()

			raw, _, err := d.networkCard(h.URL)
			if err != nil {
				t.Fatal(err)
			}
			verifyOwn(t, d, raw)
			exts := map[string]bool{}
			for _, e := range decodeCard(t, raw)["capabilities"].(map[string]any)["extensions"].([]any) {
				exts[e.(map[string]any)["uri"].(string)] = true
			}
			if !exts[module.ExtX402URI] {
				t.Errorf("the card does not declare a2a-x402:\n%s", raw)
			}
			if exts[module.ExtPricingURI] != c.want {
				t.Errorf("anet-pricing on the A2A card = %v, want %v:\n%s", exts[module.ExtPricingURI], c.want, raw)
			}

			adpRaw, err := d.signedCard("Worker", []string{"work.priced"})
			if err != nil {
				t.Fatal(err)
			}
			var adpCard struct {
				Extensions map[string]any `json:"extensions"`
			}
			if err := json.Unmarshal(adpRaw, &adpCard); err != nil {
				t.Fatal(err)
			}
			if _, ok := adpCard.Extensions[ExtPricing]; ok != c.want {
				t.Errorf("%s on the ADP card = %v, want %v: %s", ExtPricing, ok, c.want, adpRaw)
			}
			if !c.want && bytes.Contains(adpRaw, []byte(`"work.priced":5`)) {
				t.Errorf("the ADP card carries the price: %s", adpRaw)
			}
		})
	}
}

func publishPricesOpt(b bool) *bool { return &b }

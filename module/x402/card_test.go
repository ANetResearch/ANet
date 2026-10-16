//go:build !no_x402

package x402

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
)

// With a priced skill among free ones the card declares a2a-x402 without
// "required" (the publish form of false), and nothing else: no price.
func TestCardExtensionsDeclarePaymentAndLeaveRequiredOut(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	withWork(t, h, 7)

	got := m.CardExtensions(module.CardContext{AID: h.AID(), Skills: []string{"free.thing", "work.do"}})
	if len(got) != 1 || got[0]["uri"] != module.ExtX402URI {
		t.Fatalf("want a2a-x402 alone, got %v", got)
	}
	if _, ok := got[0]["required"]; ok {
		t.Fatalf("required must be left out when a skill is free, got %v", got[0]["required"])
	}
}

// Every published skill priced: a client that cannot pay can use nothing,
// so the extension is required.
func TestCardExtensionsRequireX402WhenEverySkillIsPriced(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	withWork(t, h, 3)
	got := m.CardExtensions(module.CardContext{AID: h.AID(), Skills: []string{"work.do"}})
	if len(got) != 1 || got[0]["required"] != true {
		t.Fatalf("want a2a-x402 with required true, got %v", got)
	}
}

// Nothing priced, or no ledger: no payment URI at all.
func TestCardExtensionsAreEmptyWithoutAPriceOrALedger(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	if got := m.CardExtensions(module.CardContext{AID: h.AID(), Skills: []string{"free.thing"}}); len(got) != 0 {
		t.Fatalf("no priced skill, got %v", got)
	}
	withWork(t, h, 3)
	unhubbed := &Module{host: h}
	if got := unhubbed.CardExtensions(module.CardContext{AID: h.AID(), Skills: []string{"work.do"}}); len(got) != 0 {
		t.Fatalf("no hub, so no ledger to price on; got %v", got)
	}
	if got := m.CardInterfaces(module.CardContext{AID: h.AID(), Skills: []string{"work.do"}}); got != nil {
		t.Fatalf("payment adds no interface, got %v", got)
	}
}

// The public card carries no price (A2A-DESIGN §21) [redteam:F1]:
// whatever the skills cost, the contribution says only that this node takes
// payment in the task. There is no switch that puts prices back: the
// public-price extension and payments.publish_prices are gone.
func TestCardExtensionsCarryNoPrice(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	withWork(t, h, 7919)
	for _, skills := range [][]string{{"free.thing", "work.do"}, {"work.do"}} {
		got := m.CardExtensions(module.CardContext{AID: h.AID(), Skills: skills})
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{"7919", "anet-pricing", "anet-price", "prices", "params", "network"} {
			if strings.Contains(string(raw), leak) {
				t.Errorf("skills %v: the card contribution carries %q: %s", skills, leak, raw)
			}
		}
	}
}

// An extended card (HPP §2.6) carries the prices, as the quote states them
// (Price): only the priced skills it lists, amounts as decimal strings, on
// this node's hub ledger in credits. Its a2a-x402 declaration is the same
// rule as the public card's, applied to its own skills.
func TestExtendedCardExtensionsCarryThePrices(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	withWork(t, h, 7919)
	for _, v := range []module.CardVariant{module.VariantGeneral, module.VariantList} {
		got := m.CardExtensions(module.CardContext{AID: h.AID(), Skills: []string{"free.thing", "work.do"}, Extended: true, Variant: v})
		if len(got) != 2 || got[0]["uri"] != module.ExtX402URI || got[1]["uri"] != module.ExtPriceURI {
			t.Fatalf("%s: want a2a-x402 and the price list, got %v", v, got)
		}
		if _, ok := got[0]["required"]; ok {
			t.Fatalf("%s: required with a free skill listed", v)
		}
		want := map[string]any{"network": payment.CreditNetwork(h.hub.AID()), "asset": payment.AssetCredit,
			"prices": []any{map[string]any{"skillId": "work.do", "amount": "7919"}}}
		if !reflect.DeepEqual(got[1]["params"], want) {
			t.Fatalf("%s: params %v, want %v", v, got[1]["params"], want)
		}
		if price, ok := m.Price("work.do"); !ok || price != 7919 {
			t.Fatalf("Price = %d %v: the card and the quote must say the same", price, ok)
		}
	}
	// Each card declares for what it lists: a variant whose only priced
	// skill is beyond the public card declares a2a-x402 where the public
	// card does not.
	if got := m.CardExtensions(module.CardContext{AID: h.AID(), Skills: []string{"free.thing"}}); len(got) != 0 {
		t.Fatalf("public card with only free skills: %v", got)
	}
	got := m.CardExtensions(module.CardContext{AID: h.AID(), Skills: []string{"work.do"}, Extended: true, Variant: module.VariantList})
	if len(got) != 2 || got[0]["required"] != true {
		t.Fatalf("every listed skill priced: %v", got)
	}
	// Nothing priced on the extended card either: nothing at all.
	if got := m.CardExtensions(module.CardContext{AID: h.AID(), Skills: []string{"free.thing"}, Extended: true, Variant: module.VariantGeneral}); len(got) != 0 {
		t.Fatalf("free extended card: %v", got)
	}
}

// HasPrices (core X-SS-8): whether any served capability is priced, from
// Price; without a ledger nothing is.
func TestHasPrices(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	if m.HasPrices() {
		t.Fatal("no capability served, yet prices")
	}
	withWork(t, h, 3)
	if !m.HasPrices() {
		t.Fatal("a priced capability served, yet no prices")
	}
	if (&Module{}).HasPrices() || (&Module{host: h}).HasPrices() {
		t.Fatal("prices without a host or a ledger")
	}
}

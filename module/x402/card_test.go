//go:build !no_x402

package x402

import (
	"reflect"
	"testing"

	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
)

// With a priced skill among free ones the card declares a2a-x402 without
// "required" (the publish form of false) and prices only the priced skill,
// as a decimal string on the hub's ledger.
func TestCardExtensionsPriceOnlyPricedSkillsAndLeaveRequiredOut(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	withWork(t, h, 7)

	got := m.CardExtensions(module.CardContext{AID: h.AID(), Skills: []string{"free.thing", "work.do"}})
	if len(got) != 2 {
		t.Fatalf("want a2a-x402 and anet-pricing, got %v", got)
	}
	x, pricing := got[0], got[1]
	if x["uri"] != module.ExtX402URI {
		t.Fatalf("first extension is %v, want a2a-x402", x["uri"])
	}
	if _, ok := x["required"]; ok {
		t.Fatalf("required must be left out when a skill is free, got %v", x["required"])
	}
	if pricing["uri"] != module.ExtPricingURI {
		t.Fatalf("second extension is %v, want anet-pricing", pricing["uri"])
	}
	want := map[string]any{
		"network": payment.CreditNetwork(h.hub.AID()),
		"prices":  []any{map[string]any{"skillId": "work.do", "amount": "7"}},
	}
	if !reflect.DeepEqual(pricing["params"], want) {
		t.Fatalf("pricing params\n got  %#v\n want %#v", pricing["params"], want)
	}
}

// Every published skill priced: a client that cannot pay can use nothing,
// so the extension is required.
func TestCardExtensionsRequireX402WhenEverySkillIsPriced(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	withWork(t, h, 3)
	got := m.CardExtensions(module.CardContext{AID: h.AID(), Skills: []string{"work.do"}})
	if len(got) != 2 || got[0]["required"] != true {
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
	unhubbed := &Module{spent: newSpentVouchers(), host: h}
	if got := unhubbed.CardExtensions(module.CardContext{AID: h.AID(), Skills: []string{"work.do"}}); len(got) != 0 {
		t.Fatalf("no hub, so no ledger to price on; got %v", got)
	}
	if got := m.CardInterfaces(module.CardContext{AID: h.AID(), Skills: []string{"work.do"}}); got != nil {
		t.Fatalf("payment adds no interface, got %v", got)
	}
}

// payments.publish_prices=false (A2A-DESIGN §21) [redteam:F1]: the card
// still says this node takes payment in the task, and publishes no price
// per skill. required is decided as before: it says whether every skill
// is priced, not what any costs.
func TestCardExtensionsWithheldPricesDeclarePaymentOnly(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	withWork(t, h, 7)
	for _, c := range []struct {
		skills   []string
		required bool
	}{{[]string{"free.thing", "work.do"}, false}, {[]string{"work.do"}, true}} {
		got := m.CardExtensions(module.CardContext{AID: h.AID(), Skills: c.skills, WithholdPrices: true})
		if len(got) != 1 || got[0]["uri"] != module.ExtX402URI {
			t.Fatalf("skills %v: want a2a-x402 only, got %v", c.skills, got)
		}
		if _, ok := got[0]["required"]; ok != c.required {
			t.Errorf("skills %v: required present = %v, want %v", c.skills, ok, c.required)
		}
		for _, e := range got {
			if _, ok := e["params"]; ok {
				t.Errorf("an extension carries params: %v", e)
			}
		}
	}
}

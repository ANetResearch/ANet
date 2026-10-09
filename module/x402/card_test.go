//go:build !no_x402

package x402

import (
	"encoding/json"
	"strings"
	"testing"

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

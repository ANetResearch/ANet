//go:build !no_x402

package x402

// Red team si1 (F1; A2A-DESIGN §1 SI-1, §2 X4, §21), rerun for 0.3.0 (§7.4).
//
// SI-1 lists the x402 `resource` among the task content the hub must not
// see, and X4 says the settlement sent to the hub carries no capability
// id. Structurally that holds: the /x402/settle body has no resource,
// description or extra. Up to 0.2 the same node also published, to the same
// hub, a signed price list per skill (anet-pricing/v1 on the A2A card, and
// the ADP card's price list the hub gateway sold from), while the settle
// body names the payee and the exact amount: whenever the payee's prices
// were distinct per skill, the hub recovered which capability was bought
// from nothing but what it was sent (the red team's PoC, cd73df0).
//
// 0.3.0 has one path: no price leaves this node in public. The card
// contribution declares a2a-x402 and nothing about amounts; the price is in
// the quote, inside the end-to-end encrypted task. So the hub's lookup has
// nothing to look the amount up in. The other half of the rerun — anyone,
// a probe identity included, can ask the provider for its extended card and
// read the prices there, which is stated rather than prevented (PAY-N2) —
// needs two daemons and a hub between them: it is internal/daemon's
// TestAProbeIdentityCanReadThePricesFromTheExtendedCard.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// twoPriced serves two priced capabilities with different prices, which is
// what a provider with more than one paid skill looks like.
type twoPriced struct{}

func (twoPriced) ID() string { return "two-priced" }
func (twoPriced) Capabilities(context.Context) ([]string, error) {
	return []string{"legal.contract.review", "text.summarize"}, nil
}
func (twoPriced) Describe(context.Context) (string, error) { return "", nil }
func (twoPriced) Health(context.Context) error             { return nil }
func (twoPriced) Price(capID string) (uint64, bool) {
	switch capID {
	case "text.summarize":
		return 3, true
	case "legal.contract.review":
		return 11, true
	}
	return 0, false
}
func (twoPriced) Invoke(context.Context, provider.Call) (effect.Effect, error) {
	return effect.Effect{Status: effect.OK}, nil
}

func TestTheHubCannotNameThePaidSkillFromWhatItIsSent(t *testing.T) {
	var mu sync.Mutex
	var settleBodies [][]byte
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x402/supported":
			_ = json.NewEncoder(w).Encode(payment.Supported{})
		case "/x402/settle":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			settleBodies = append(settleBodies, b)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(payment.SettlementResponse{Success: true, Transaction: "tx-1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	// The provider, with its two priced skills.
	prov := newHost(t)
	prov.url = hub.URL
	if err := prov.reg.Register(context.Background(), twoPriced{}); err != nil {
		t.Fatal(err)
	}
	pm := newModule(t, prov)

	// What the provider publishes to the hub about payment: its card's
	// contribution (A2A-DESIGN §10.1; module/x402/card.go). The hub stores
	// and serves it.
	skills := []string{"legal.contract.review", "text.summarize"}
	published, err := json.Marshal(pm.CardExtensions(module.CardContext{AID: prov.AID(), HubURL: hub.URL, Skills: skills}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(published), module.ExtX402URI) {
		t.Fatalf("precondition: the card does not say this node takes payment: %s", published)
	}

	// Inside the sealed envelope (the hub never sees this): the provider
	// quotes the capability it was asked for, and the quote's resource
	// names it.
	const secretCap = "legal.contract.review"
	price, _ := pm.Price(secretCap)
	quote := pm.Quote(secretCap, price)

	// The payer (another node on the same hub) authorizes the quoted
	// option, bound to the task; the provider settles it at the hub.
	payer := newHost(t)
	payer.hub = prov.hub
	payer.url = hub.URL
	paym := newModule(t, payer)
	raw, err := paym.Authorize(quote.Accepts[0], "ix-secret", "bind-secret", module.PurposeTaskAuto)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := pm.Settle(context.Background(), raw, quote.Accepts[0]); err != nil || st.Failed != "" {
		t.Fatalf("settle: %+v %v", st, err)
	}

	mu.Lock()
	if len(settleBodies) != 1 {
		mu.Unlock()
		t.Fatalf("%d settle calls", len(settleBodies))
	}
	body := settleBodies[0]
	mu.Unlock()

	// The structural half of SI-1 holds: nothing in the body names the work.
	for _, s := range []string{secretCap, "resource", "description", "extra"} {
		if strings.Contains(string(body), s) {
			t.Fatalf("the settle body carries %q: %s", s, body)
		}
	}
	var fr struct {
		PaymentRequirements payment.PaymentRequirements `json:"paymentRequirements"`
	}
	if err := json.Unmarshal(body, &fr); err != nil {
		t.Fatal(err)
	}
	// The hub sees whom and how much: the price is still what was paid.
	if fr.PaymentRequirements.PayTo != prov.AID() || fr.PaymentRequirements.Amount != "11" {
		t.Fatalf("settled %s to %s, want the quoted 11 to the provider", fr.PaymentRequirements.Amount, fr.PaymentRequirements.PayTo)
	}

	// ---- the hub's side: only what it was sent ----
	// The 0.2 lookup read the amount in the provider's published prices.
	// There are none: neither the amount, nor a skill beside a number, nor
	// the price list's members.
	for _, leak := range []string{`"11"`, `"3"`, ":11", ":3", "prices", "amount", "skillId", "anet-pricing", "anet-price/"} {
		if strings.Contains(string(published), leak) {
			t.Fatalf("the published card carries %s, from which the amount names the skill: %s", leak, published)
		}
	}
	for _, s := range skills {
		if strings.Contains(string(published), s) {
			t.Fatalf("the payment contribution names the skill %s: %s", s, published)
		}
	}
}

//go:build !no_x402

package x402

// Red team si1 (F1; A2A-DESIGN §1 SI-1, §2 X4, §21).
//
// SI-1 lists the x402 `resource` among the task content the hub must not
// see, and X4 says the settlement sent to the hub carries no capability
// id. Structurally that holds: the /x402/settle body has no resource,
// description or extra. But the same node publishes, to the same hub, a
// signed price list per skill (anet-pricing/v1 on the A2A card, and the ADP
// card's price list), and the settle body names the payee and the exact
// amount. Whenever the payee's prices are distinct per skill (or it prices
// a single skill), the hub recovers which capability was bought, and so
// the exact `resource` the quote carried end to end, from nothing but what
// it is sent.
//
// That is a design-level inference, and it is now stated as a known
// limitation (§21) rather than denied (the settle code's comment and note
// 0006 said the hub learns nothing about the work). An operator who wants
// it closed sets payments.publish_prices=false: the cards carry no price
// per skill, the price is given only in the quote, and the same settlement
// no longer names the skill. The test pins both: the inference with
// prices published (so a change to it is noticed and §21 updated), and
// its absence with prices withheld (the red team's PoC, cd73df0, turned
// round).

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

func TestSettleAmountAndPublishedPricesNameTheSkillUnlessPricesAreWithheld(t *testing.T) {
	t.Run("prices published (the default; A2A-DESIGN §21)", func(t *testing.T) {
		recovered, secret := settleAndInfer(t, false)
		if recovered != secret {
			t.Fatalf("with prices published the hub no longer recovers the resource (got %q, quoted %q): "+
				"update A2A-DESIGN §21 and this test", recovered, secret)
		}
	})
	t.Run("payments.publish_prices=false", func(t *testing.T) {
		if recovered, secret := settleAndInfer(t, true); recovered != "" {
			t.Fatalf("with prices withheld the hub still recovers %q (quoted %q)", recovered, secret)
		}
	})
}

// settleAndInfer runs one priced settlement and plays the hub: from the
// settle body and the card the provider published, it returns the
// resource it can name, or "" when the amount names no skill. secret is
// the resource the quote carried end to end.
func settleAndInfer(t *testing.T, withhold bool) (recovered, secret string) {
	t.Helper()
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

	// What the provider publishes to the hub: its card's pricing extension
	// (A2A-DESIGN §10.1; module/x402/card.go). The hub stores and serves it.
	skills := []string{"legal.contract.review", "text.summarize"}
	exts := pm.CardExtensions(module.CardContext{AID: prov.AID(), HubURL: hub.URL, Skills: skills,
		WithholdPrices: withhold})
	published, err := json.Marshal(exts)
	if err != nil {
		t.Fatal(err)
	}

	// Inside the sealed envelope (the hub never sees this): the provider
	// quotes the capability it was asked for, and the quote's resource
	// names it.
	const secretCap = "legal.contract.review"
	price, _ := pm.Price(secretCap)
	quote := pm.Quote(secretCap, price)
	secret = quote.Resource.URL

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
			t.Fatalf("precondition: the settle body carries %q (a different defect): %s", s, body)
		}
	}

	// ---- the hub's side: only what it was sent ----
	var fr struct {
		PaymentRequirements payment.PaymentRequirements `json:"paymentRequirements"`
	}
	if err := json.Unmarshal(body, &fr); err != nil {
		t.Fatal(err)
	}
	var cardExts []struct {
		URI    string `json:"uri"`
		Params struct {
			Prices []struct {
				SkillID string `json:"skillId"`
				Amount  string `json:"amount"`
			} `json:"prices"`
		} `json:"params"`
	}
	if err := json.Unmarshal(published, &cardExts); err != nil {
		t.Fatal(err)
	}
	byAmount := map[string][]string{}
	for _, e := range cardExts {
		if e.URI != module.ExtPricingURI {
			continue
		}
		for _, p := range e.Params.Prices {
			byAmount[p.Amount] = append(byAmount[p.Amount], p.SkillID)
		}
	}
	// payTo names whose card to read; the amount picks the skill.
	if fr.PaymentRequirements.PayTo != prov.AID() {
		t.Fatalf("payTo %q is not the provider", fr.PaymentRequirements.PayTo)
	}
	// The price is still what was paid: withholding the list does not
	// change the quote.
	if fr.PaymentRequirements.Amount != "11" {
		t.Fatalf("settled amount %s, want the quoted 11", fr.PaymentRequirements.Amount)
	}
	cands := byAmount[fr.PaymentRequirements.Amount]
	if len(cands) != 1 {
		return "", secret
	}
	return "anet:capability/" + cands[0], secret
}

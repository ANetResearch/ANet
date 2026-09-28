//go:build !no_x402

package x402

// Red-team PoC (lens si1, A2A-DESIGN §1 SI-1, §2 X4).
//
// SI-1 lists the x402 `resource` among the task content the hub must not
// see, and X4 says the settlement sent to the hub carries no capability id.
// Structurally that holds: the /x402/settle body has no resource,
// description or extra. But the same node publishes, to the same hub, a
// signed price list per skill (anet-pricing/v1 on the A2A card, and the ADP
// card's pricing extension), and the settle body names the payee and the
// exact amount. Whenever the payee's prices are distinct per skill (or the
// payee prices a single skill, as the official anet-paid-demo does), the
// hub recovers which capability was bought, and so the exact `resource`
// the quote carried end to end, from nothing but what it is sent.
//
// The test passes when the attack succeeds: the hub-side reconstruction
// equals the resource the provider quoted inside the sealed envelope.

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

func TestRedteamSI1SettleAmountAndPublishedPricesRevealTheResource(t *testing.T) {
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
	exts := pm.CardExtensions(module.CardContext{AID: prov.AID(), HubURL: hub.URL, Skills: skills})
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
	secretResource := quote.Resource.URL

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
	cands := byAmount[fr.PaymentRequirements.Amount]
	if len(cands) != 1 {
		t.Fatalf("attack failed: amount %s matches %v", fr.PaymentRequirements.Amount, cands)
	}
	recovered := "anet:capability/" + cands[0]
	if recovered != secretResource {
		t.Fatalf("attack failed: recovered %q, the quote said %q", recovered, secretResource)
	}
	t.Logf("SI-1 inference: the hub recovers resource %q from payTo=%s amount=%s and the payee's published prices",
		recovered, fr.PaymentRequirements.PayTo, fr.PaymentRequirements.Amount)
}

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/module"
)

// publicPricingURI is the public price list extension of 0.2 (anet-pricing/v1).
// 0.3.0 removed it with the setting that turned it off (payments.publish_prices):
// nothing a node publishes names a price (A2A-DESIGN §21) [redteam:F1].
const publicPricingURI = "https://agentnetwork.org.cn/a2a/ext/anet-pricing/v1"

// assertNoPublicPrice fails when raw, something this node published, carries a
// price: the amount amount as a JSON number or decimal string, or a price
// extension or member.
func assertNoPublicPrice(t *testing.T, raw []byte, amount string) {
	t.Helper()
	for _, leak := range []string{`"` + amount + `"`, `:` + amount, `anet-pricing`, `anet-price/`, `"pricing"`, `"prices"`} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("published bytes carry %s:\n%s", leak, raw)
		}
	}
}

// distinctPricedWork is a public capability with a price no other number in a
// request is likely to spell.
type distinctPricedWork struct{ lampProvider }

func (*distinctPricedWork) ID() string { return "distinctpriced" }
func (*distinctPricedWork) Capabilities(context.Context) ([]string, error) {
	return []string{"work.priced"}, nil
}
func (*distinctPricedWork) Price(c string) (uint64, bool) { return 7919, c == "work.priced" }

// F1, rewritten for 0.3.0 (§7.4): a node with a priced public skill names its
// price nowhere it publishes. Its network card declares a2a-x402 and no price;
// its registration carries no signed ADP card (whose price list the gateway
// sold from) and its profile no pricing; a registry entry has no price member.
// What a skill costs reaches the payer in the quote, inside the task. A hub
// that sees a settlement's payee and amount cannot look the amount up.
func TestNoPriceLeavesThisNode(t *testing.T) {
	compiled := false
	for _, n := range module.Compiled() {
		compiled = compiled || n == "x402"
	}
	if !compiled {
		t.Skip("no payment module in this build: a priced skill is not published at all (TestPaymentOnTheCardFollowsTheBuild)")
	}
	h := newFakeHub(t)
	d := newCardDaemon(t, "Worker", "")
	if err := d.Providers().Register(context.Background(), &distinctPricedWork{}); err != nil {
		t.Fatal(err)
	}
	setPublic(d, "work.priced")
	ctx := context.Background()
	if err := d.HubRegister(ctx, h.URL, "Worker", []string{"work.priced"}); err != nil {
		t.Fatal(err)
	}
	fh := fakeHubAt(t, h.URL)
	deadline := time.Now().Add(5 * time.Second)
	for {
		fh.mu.Lock()
		card := fh.a2aCards[d.AID()]
		fh.mu.Unlock()
		if bytes.Contains(card, []byte(module.ExtX402URI)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the card never declared a2a-x402:\n%s", card)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := d.SetProfile(ctx, Profile{Summary: "I check contracts", Readme: "# Worker"}); err != nil {
		t.Fatal(err)
	}

	registrations, profiles := 0, 0
	for _, r := range hubRequests(t, h.URL) {
		switch r.path {
		case "/register":
			registrations++
			var m map[string]json.RawMessage
			if err := json.Unmarshal(r.body, &m); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"card", "pricing"} {
				if _, ok := m[k]; ok {
					t.Errorf("a registration carries %q: %s", k, r.body)
				}
			}
		case "/profile":
			profiles++
		}
		assertNoPublicPrice(t, r.body, "7919")
	}
	if registrations == 0 || profiles == 0 {
		t.Fatalf("%d registrations and %d profile publications seen; the scan would prove nothing", registrations, profiles)
	}
	// The registry entry, as the hub serves it.
	rec := httptest.NewRecorder()
	fh.hAgents(rec, httptest.NewRequest(http.MethodGet, "/agents", nil))
	if !strings.Contains(rec.Body.String(), d.AID()) {
		t.Fatalf("the registry does not list the node: %s", rec.Body)
	}
	assertNoPublicPrice(t, rec.Body.Bytes(), "7919")

	// And the price is still what the quote says.
	if p := d.payer(); p == nil {
		t.Fatal("no payment module")
	} else if price, ok := p.Price("work.priced"); !ok || price != 7919 {
		t.Fatalf("Price = %d %v, want the provider's 7919", price, ok)
	}
}

// A control-plane caller that still sends a removed member is told so, in the
// sentence of the CLI flag that sent it (ux §4.3), and nothing happens: /profile
// with "pricing" changes no setting, and /delegate with "pay" sends nothing.
func TestTheControlPlaneRefusesTheRemovedPaymentMembers(t *testing.T) {
	srv := newFakeHub(t)
	d := newTestDaemon(t, srv.URL, false)
	token, err := loadOrGenControlToken(d.layout)
	if err != nil {
		t.Fatal(err)
	}
	p := newPlaneFor(t, d, token)
	for _, c := range []struct {
		path, body string
		want       *a2ashape.Refusal
		hubPath    string // what the request would have sent the hub
	}{
		{"/profile", `{"summary":"s","pricing":"3 credits"}`, removedProfilePricing, "/profile"},
		{"/delegate", `{"provider":"bafyreixtplpft7v4seh2kvj72ceuvw75efr6edt4sywb5wkh7d6kx2wxyz","capability":"work.do","pay":true}`,
			removedDelegatePay, hubapi.RelaySendPath},
	} {
		sent := func() int {
			n := 0
			for _, r := range hubRequests(t, srv.URL) {
				if r.path == c.hubPath {
					n++
				}
			}
			return n
		}
		before := sent()
		resp, b := p.req(t, http.MethodPost, c.path, c.body, p.bearer)
		var got a2ashape.ErrorBody
		_ = json.Unmarshal(b, &got)
		if want := c.want.Body(a2ashape.Lang()); resp.StatusCode != http.StatusBadRequest || got != want {
			t.Errorf("%s: %d %s, want 400 %+v", c.path, resp.StatusCode, b, want)
		}
		if got.Reason != "cli_removed" {
			t.Errorf("%s: reason %q, want cli_removed", c.path, got.Reason)
		}
		if n := sent() - before; n != 0 {
			t.Errorf("%s: %d requests to %s reached the hub", c.path, n, c.hubPath)
		}
	}
	if s := d.config().Summary; s != "" {
		t.Errorf("the refused /profile changed the summary to %q", s)
	}
	// The routes of the gateway tier and the v1 search are gone.
	for _, path := range []string{"/x402-authorize", "/find"} {
		if resp, b := p.req(t, http.MethodPost, path, `{}`, p.bearer); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d %s, want 404 (the route is gone)", path, resp.StatusCode, b)
		}
	}
}

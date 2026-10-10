//go:build !no_x402

package daemon

// PAY-D5 and J-PAY-13 (HPP §4.3.4, §7.5): a signed price that comes late.
// The provider does not answer the price request within its 10 seconds:
// the first decision is card_price_unavailable (timeout), and the task
// waits. When the card arrives after all, the decision is taken again, once,
// and the quote is paid automatically. M-PAY-42 (the second decision gone)
// turns this red.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

func TestJPAY13ALateSignedPricePays(t *testing.T) {
	srv, req, prov := registeredPair(t)
	work := &meteredWork{price: 3}
	if err := prov.Providers().Register(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	setPublic(prov, "work.do")
	grantOn(srv.URL, req.AID(), 50)
	payPolicy(t, req, PaymentsConfig{AutoMax: 5, DailyMax: u64(defaultDailyMax)}, prov.AID())
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov) // quoted
	start := time.Now()
	poll(t, req) // the quote arrives; no card is kept, so the decision asks for one
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the receive path waited %s for the price card", time.Since(start))
	}
	// The provider reads nothing: no answer within the 10 seconds.
	waitLong(t, "the first decision", func() bool {
		_, ok := storedPayNote(getIX(t, req, id))
		return ok
	})
	n, s := sentenceOf(t, req, id)
	rix := getIX(t, req, id)
	if n.Code != "card_price_unavailable" || n.Why != priceWhyTimeout || rix.PayState != interactions.PayRequired ||
		rix.State != interactions.StateInputRequired {
		t.Fatalf("without an answer: %+v, %s / %q", n, rix.State, rix.PayState)
	}
	if chainEvents(t, req, EvPaymentAuthorized) != 0 {
		t.Fatal("paid without a signed price")
	}
	if r := PaymentReason(rix); r != "card_price_unavailable" {
		t.Fatalf("anet.reason %q", r)
	}
	sameEverywhere(t, req, id, prov.AID(), s, true)
	// The answer comes late: kept, and the quote decided again and paid.
	waitLong(t, "the payment after the late card", func() bool {
		_ = prov.pollOnce(context.Background())
		_ = req.pollOnce(context.Background())
		n, ok := storedPayNote(getIX(t, req, id))
		return ok && n.Code == "paid.auto"
	})
	n, _ = sentenceOf(t, req, id)
	if rix := getIX(t, req, id); rix.PayState != interactions.PaySubmitted && rix.PayState != interactions.PayCompleted {
		t.Fatalf("after the late card: %q", rix.PayState)
	}
	if n.Tier != a2ashape.PayTierAuto || n.Price == nil || *n.Price != 3 || n.Left != defaultDailyMax-3 {
		t.Fatalf("the second decision: %+v", n)
	}
	if got := chainEvents(t, req, EvPaymentAuthorized); got != 1 {
		t.Fatalf("%d authorizations", got)
	}
	// Once: a later card for the same agent decides nothing again.
	req.signedPriceArrived(prov.AID())
	time.Sleep(200 * time.Millisecond)
	if got := chainEvents(t, req, EvPaymentAuthorized); got != 1 {
		t.Fatalf("%d authorizations after another card", got)
	}
}

// Only a decision that had no answer (timeout) waits for a late card: one
// that had none for another reason, a quote that has lapsed, a task no
// longer waiting and a decision of the manual tier do not.
func TestOnlyATimedOutDecisionWaitsForALateCard(t *testing.T) {
	now := time.Now().UnixMilli()
	note := func(code, why, tier string) []byte {
		n := a2ashape.PayNote{Code: code, Amount: 3, Tier: tier, Why: why, At: time.Now().UTC()}
		b, _ := json.Marshal(n)
		return b
	}
	waiting := func(b []byte, expires int64, st interactions.State) *interactions.Interaction {
		return &interactions.Interaction{ID: "ix", Role: interactions.RoleOutbound, State: st,
			PayState: interactions.PayRequired, QuoteExpiresAt: expires, PayNote: b}
	}
	for _, c := range []struct {
		ix   *interactions.Interaction
		want bool
	}{
		{waiting(note("card_price_unavailable", "timeout", "auto"), now+60_000, interactions.StateInputRequired), true},
		{waiting(note("card_price_unavailable", "timeout", "agent"), 0, interactions.StateInputRequired), true},
		{waiting(note("card_price_unavailable", "not_offered", "auto"), now+60_000, interactions.StateInputRequired), false},
		{waiting(note("card_price_unavailable", "strangers_busy", "auto"), now+60_000, interactions.StateInputRequired), false},
		{waiting(note("card_price_unavailable", "timeout", "auto"), now-1, interactions.StateInputRequired), false},
		{waiting(note("card_price_unavailable", "timeout", "auto"), now+60_000, interactions.StateWorking), false},
		{waiting(note("quote_above_card_price", "", "auto"), now+60_000, interactions.StateInputRequired), false},
		{waiting(nil, now+60_000, interactions.StateInputRequired), false},
	} {
		if got := lateCardWaits(c.ix, now); got != c.want {
			t.Errorf("%s: %v, want %v", c.ix.PayNote, got, c.want)
		}
	}
}

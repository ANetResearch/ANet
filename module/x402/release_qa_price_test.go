package x402

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"
)

// mintVoucherAmount is mintVoucher with the amount opened up. The shared
// helper pins 120, which happens to equal the test provider's price, so
// every existing redeem test is blind to whether the amount is compared
// at all.
func mintVoucherAmount(t *testing.T, signer *identity.Controller,
	payTo, capID, payer, nonce, network string, amount uint64) string {
	t.Helper()
	v := &payment.Voucher{
		AuthID: "auth-" + nonce, Payer: payer, PayTo: payTo, Capability: capID,
		Amount: amount, Network: network,
		NotAfter: time.Now().Add(time.Minute).UnixMilli(), Nonce: nonce,
	}
	if err := v.Sign(signer); err != nil {
		t.Fatal(err)
	}
	raw, err := v.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// A voucher's signature is the hub's, and it certifies that the hub moved
// that much credit — not that the amount is the price. The price reaches
// the buyer through the hub, and the voucher is minted by the hub, so a
// hub quoting below the published price is caught by nobody unless the
// seller compares against its own price on the way in.
func TestRedeemRefusesAVoucherBelowThePublishedPrice(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	w := withWork(t, h, 120)

	v := mintVoucherAmount(t, h.hub, h.AID(), "work.do", "buyer-short", "nonce-short",
		payment.CreditNetwork(h.hub.AID()), 119)
	res, code := m.RedeemVoucher(context.Background(), redeemRequest{
		Voucher: v, Capability: "work.do", Args: map[string]any{"n": 1}})
	if code != 402 {
		t.Fatalf("underpaid voucher: code = %d %v, want 402", code, res)
	}
	if msg, _ := res["error"].(string); !strings.Contains(msg, "119") || !strings.Contains(msg, "120") {
		t.Errorf("refusal does not say what was paid and what it costs: %v", res["error"])
	}
	if w.invoked != 0 {
		t.Errorf("the work ran %d times for an underpaid voucher", w.invoked)
	}

	// Paying more than the price is the payer's business, not an error:
	// refusing it would turn a rounding-up buyer into a failed sale.
	over := mintVoucherAmount(t, h.hub, h.AID(), "work.do", "buyer-over", "nonce-over",
		payment.CreditNetwork(h.hub.AID()), 500)
	if _, code := m.RedeemVoucher(context.Background(), redeemRequest{
		Voucher: over, Capability: "work.do", Args: map[string]any{"n": 1}}); code != 200 {
		t.Fatalf("overpaid voucher refused: %d", code)
	}
	if w.invoked != 1 {
		t.Errorf("work ran %d times after one valid redemption", w.invoked)
	}
}

// "This node has no price for that" must not be the cheapest way to buy.
// Serving an unpriced capability for whatever the voucher happens to say
// would make every capability the provider forgot to price free to the
// first buyer who names it.
func TestRedeemRefusesWhenThisNodePublishesNoPrice(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	withWork(t, h, 120)

	v := mintVoucherAmount(t, h.hub, h.AID(), "work.free", "buyer-unpriced", "nonce-unpriced",
		payment.CreditNetwork(h.hub.AID()), 120)
	res, code := m.RedeemVoucher(context.Background(), redeemRequest{
		Voucher: v, Capability: "work.free", Args: nil})
	if code == 200 {
		t.Fatalf("an unpriced capability was served for a voucher: %v", res)
	}
}

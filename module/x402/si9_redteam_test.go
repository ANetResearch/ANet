//go:build !no_x402

package x402

// Red-team PoCs for SI-9 (payment), merchant side. Each test asserts that
// the ATTACK SUCCEEDS: a passing test means the defect is present.

import (
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
)

// The provider's merchant check (§8.4) passes an authorization whose
// amount is above MaxInt64. The hub stores and moves amounts as
// int64(auth.Amount), so such an amount settles as a negative number: the
// "payer" is credited and the provider debited (see ANetHub
// TestRedteamSI9_SettleNegativeAmountDrainsVictim). The check here is the
// provider's own line of defence and does not stop it: the provider
// presents the payment, the hub answers success, and the provider then
// runs the paid call — having lost the money it thinks it received.
func TestRedteamSI9_MerchantCheckAcceptsOverflowingAmount(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	payer, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	quote := m.Quote("work.do", 120)
	opt := quote.Accepts[0]
	now := time.Now().UnixMilli()
	huge := ^uint64(0) - 5000 + 1 // int64(huge) == -5000
	a := payment.Authorization{PayTo: h.AID(), Amount: huge, Network: opt.Network,
		IssuedAt: now, NotAfter: now + 300_000, InteractionID: "bind-1"}
	o := opt
	o.Amount = payment.Amount(huge)
	terms := module.PaymentTerms{Quoted: quote, Bind: "bind-1", Payer: payer.AID(),
		QuoteExpiresAt: now + 3600_000, Now: now}
	chk := m.CheckPayment(signAuth(t, payer, a, o), terms)
	if chk.Reason != "" {
		t.Fatalf("defect absent: the merchant check refused it: %+v", chk)
	}
	t.Logf("ATTACK OK: merchant check accepted amount %d (int64 %d) for a quote of 120", huge, int64(huge))
}

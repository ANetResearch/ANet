//go:build !no_x402

package x402

import (
	"math"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
)

// The merchant check compares the authorization's window without adding
// the skew to the payer's NotAfter (docs/notes/0033, the review of F9).
// NotAfter + ClockSkew wrapped negative for a NotAfter near
// math.MaxInt64, so an authorization ANetCore's payment.Authorization.
// Verify accepts, and the hub with it, was refused here as expired at
// every moment.
func TestTheMerchantCheckWindowDoesNotOverflow(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	payer, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	quote := m.Quote("work.do", 120)
	opt := quote.Accepts[0]
	now := time.Now().UnixMilli()
	terms := module.PaymentTerms{Quoted: quote, Bind: "bind-1", Payer: payer.AID(),
		QuoteExpiresAt: now + 3600_000, Now: now}
	check := func(issuedAt, notAfter int64) module.PaymentCheck {
		a := payment.Authorization{PayTo: h.AID(), Amount: 120, Network: opt.Network,
			IssuedAt: issuedAt, NotAfter: notAfter, InteractionID: "bind-1"}
		return m.CheckPayment(signAuth(t, payer, a, opt), terms)
	}
	for _, notAfter := range []int64{math.MaxInt64, math.MaxInt64 - payment.ClockSkew + 1, math.MaxInt64 - 1} {
		if chk := check(now-1000, notAfter); chk.Reason != "" {
			t.Errorf("an authorization valid until %d refused at %d: %+v", notAfter, now, chk)
		}
	}
	// The window still closes on both sides, at the skew.
	for _, w := range [][2]int64{
		{now - 600_000, now - payment.ClockSkew - 1},
		{now + payment.ClockSkew + 1, now + 600_000},
	} {
		if chk := check(w[0], w[1]); chk.Reason != payment.ReasonExpiredPayment {
			t.Errorf("an authorization for %d..%d at %d: %+v, want %s", w[0], w[1], now, chk, payment.ReasonExpiredPayment)
		}
	}
	for _, w := range [][2]int64{
		{now - 600_000, now - payment.ClockSkew},
		{now + payment.ClockSkew, now + 600_000},
	} {
		if chk := check(w[0], w[1]); chk.Reason != "" {
			t.Errorf("an authorization for %d..%d refused at the skew edge %d: %+v", w[0], w[1], now, chk)
		}
	}
}

//go:build !no_x402

package x402

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
)

// A hub books amounts as int64, and before its range check every amount
// above math.MaxInt64 was booked as a negative one: a payment "for" such
// an amount debited the payee and credited the payer (red team si9; the
// hub fix is ANetHub internal/aghub/amount.go). This node's own lines of
// defence refuse such an amount too, so they do not depend on the hub
// having the fix: the merchant check, the voucher door, the signer, and
// the audit that reads the hub's issuance chain.

// The red team's PoC (b7470a8), kept with its own inputs: 2^64-5000
// against a quote of 120 passed the merchant check.
func TestRedteamSI9MerchantCheckRefusesAnOverflowingAmount(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	payer, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	quote := m.Quote("work.do", 120)
	opt := quote.Accepts[0]
	now := time.Now().UnixMilli()
	for _, huge := range []uint64{^uint64(0) - 5000 + 1, 1 << 63, math.MaxUint64} {
		a := payment.Authorization{PayTo: h.AID(), Amount: huge, Network: opt.Network,
			IssuedAt: now, NotAfter: now + 300_000, InteractionID: "bind-1"}
		o := opt
		o.Amount = payment.Amount(huge)
		terms := module.PaymentTerms{Quoted: quote, Bind: "bind-1", Payer: payer.AID(),
			QuoteExpiresAt: now + 3600_000, Now: now}
		chk := m.CheckPayment(signAuth(t, payer, a, o), terms)
		if chk.Reason != payment.ReasonInvalidAmount || chk.Code != "INVALID_AMOUNT" {
			t.Errorf("amount %d against a quote of 120: %+v, want invalid_amount / INVALID_AMOUNT", huge, chk)
		}
	}
	// The top of the range is still an overpayment the check accepts.
	a := payment.Authorization{PayTo: h.AID(), Amount: math.MaxInt64, Network: opt.Network,
		IssuedAt: now, NotAfter: now + 300_000, InteractionID: "bind-1"}
	o := opt
	o.Amount = payment.Amount(math.MaxInt64)
	if chk := m.CheckPayment(signAuth(t, payer, a, o), module.PaymentTerms{Quoted: quote, Bind: "bind-1",
		Payer: payer.AID(), QuoteExpiresAt: now + 3600_000, Now: now}); chk.Reason != "" {
		t.Errorf("an overpayment of MaxInt64 was refused: %+v", chk)
	}
}

// A voucher is the hub's statement that it moved the amount it names, so
// a voucher for more than any ledger can hold is a statement about a
// movement that ran backwards — the provider would do the work and be the
// one charged.
func TestRedeemRefusesAVoucherForMoreThanALedgerCanHold(t *testing.T) {
	for _, amount := range []uint64{1 << 63, math.MaxUint64, math.MaxUint64 - 999} {
		h := newHost(t)
		m := newModule(t, h)
		w := withWork(t, h, 120)
		v := mintVoucherAmount(t, h.hub, h.AID(), "work.do", "buyer-huge", "nonce-huge",
			payment.CreditNetwork(h.hub.AID()), amount)
		res, code := m.RedeemVoucher(context.Background(), redeemRequest{
			Voucher: v, Capability: "work.do", Args: map[string]any{"n": 1}})
		if code != 402 {
			t.Errorf("voucher for %d: code = %d %v, want 402", amount, code, res)
		}
		if w.invoked != 0 {
			t.Errorf("voucher for %d: the work ran %d times", amount, w.invoked)
		}
	}
}

// The payer's side: a quote for such an amount is not signed, and the
// spending policy is not asked (its daily sums would be adding a number
// no ledger can hold).
func TestAuthorizeRefusesAnAmountNoLedgerCanHold(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	for _, amount := range []uint64{1 << 63, math.MaxUint64} {
		_, err := m.Authorize(payment.PaymentOption{Scheme: payment.SchemeCredit,
			Network: payment.CreditNetwork(h.hub.AID()), Amount: payment.Amount(amount),
			Asset: payment.AssetCredit, PayTo: "did:anet:greedy"}, "ix", "bind", module.PurposeTaskManual)
		if err == nil || !strings.Contains(err.Error(), payment.ReasonInvalidAmount) {
			t.Errorf("amount %d: signed (%v)", amount, err)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.spends) != 0 {
		t.Errorf("the spending policy was asked about %+v", h.spends)
	}
	for _, e := range h.events {
		if e.kind == EvPaymentAuthorized {
			t.Errorf("an authorization was recorded: %+v", e)
		}
	}
}

// issueAmount is fakeIssuer.issue with the amount's Go type left to the
// caller, so a test can put on the chain exactly what an unpatched hub
// wrote (a negative int64) or what a wire amount above math.MaxInt64
// decodes to (a uint64).
func (f *fakeIssuer) issueAmount(t *testing.T, kind, aid string, amount any) {
	t.Helper()
	prev := ael.GenesisPrev()
	seq := uint64(0)
	if n := len(f.records); n > 0 {
		prev = f.records[n-1].ID
		seq = f.records[n-1].Seq + 1
	}
	rec := &ael.EventRecord{
		ChainDID: f.ctrl.AID(), Seq: seq, PrevID: prev, EventType: kind,
		VersionMajor: ael.VersionMajor2,
		Payload:      map[string]any{"aid": aid, "amount": amount, "reason": "test"},
		Timestamp:    time.Now().UnixMilli(), CriticalExtensions: []string{},
	}
	if err := rec.Sign(f.ctrl); err != nil {
		t.Fatal(err)
	}
	f.records = append(f.records, rec)
}

// The audit summed whatever amount a record carried. A retirement of
// -1000 — what an unpatched hub wrote for a redemption of 2^64-1000 —
// lowered "retired" by 1000 and the report still said verified, and a
// uint64 amount above math.MaxInt64 was converted to a negative int64
// silently. Both are the signature of the hub's overflow bug, and the
// audit is the one place a node can see it.
func TestAuditReportsAnAmountNoLedgerShouldHold(t *testing.T) {
	for _, tc := range []struct {
		name   string
		amount any
	}{
		{"negative", int64(-1000)},
		{"zero", int64(0)},
		{"above int64", uint64(math.MaxUint64 - 999)},
		{"exactly 2^63", uint64(1 << 63)},
		{"fractional", 12.5},
		{"missing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, err := identity.Incept()
			if err != nil {
				t.Fatal(err)
			}
			f := &fakeIssuer{ctrl: hub}
			f.issue(t, evCreditIssued, "did:anet:a", 100)
			f.issueAmount(t, evCreditRetired, "did:anet:a", tc.amount)
			f.issue(t, evCreditRetired, "did:anet:a", 40)
			m, _ := hubbedModule(t, f)

			rep, err := m.auditIssuance(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if rep.Verified {
				t.Errorf("a chain with a retirement of %v verified", tc.amount)
			}
			found := false
			for _, p := range rep.Problems {
				if strings.Contains(p, "seq 1") {
					found = true
				}
			}
			if !found {
				t.Errorf("no problem names seq 1: %v", rep.Problems)
			}
			// The bad record is reported, not counted.
			if rep.Issued != 100 || rep.Retired != 40 {
				t.Errorf("issued=%d retired=%d, want 100/40", rep.Issued, rep.Retired)
			}
		})
	}
}

func TestAsInt64RefusesWhatItCannotRepresent(t *testing.T) {
	for _, v := range []any{uint64(1 << 63), uint64(math.MaxUint64), 1e19, -1e19, 1.5, "7", nil} {
		if n, err := asInt64(v); err == nil {
			t.Errorf("asInt64(%v) = %d, want an error", v, n)
		}
	}
	for v, want := range map[any]int64{
		int64(-5): -5, uint64(math.MaxInt64): math.MaxInt64, 3: 3, float64(42): 42,
	} {
		if n, err := asInt64(v); err != nil || n != want {
			t.Errorf("asInt64(%v) = %d, %v; want %d", v, n, err, want)
		}
	}
}

//go:build !no_x402

package x402

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/provider"
)

// lostAnswerWork is priced work whose call went out and whose answer was
// lost (provider.OutcomeUnknownError).
type lostAnswerWork struct{ pricedWork }

func (p *lostAnswerWork) Invoke(context.Context, provider.Call) (effect.Effect, error) {
	p.invoked++
	return effect.Effect{Status: effect.Unverified, Evidence: &effect.Evidence{Protocol: "http", Requested: "work.do"}},
		&provider.OutcomeUnknownError{Reason: provider.ReasonTimeout, Err: context.DeadlineExceeded}
}

// A voucher call whose answer was lost may have had its effect: the door
// says UNVERIFIED with the reason, not FAILED ("it did not happen"), for a
// buyer who paid and must decide whether to ask again (red-team F10).
func TestAVoucherCallWithALostAnswerIsNotReportedFailed(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	w := &lostAnswerWork{pricedWork{price: 120}}
	if err := h.reg.Register(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	v := signVoucher(t, h, h.AID(), "work.do", "buyer-1", "nonce-1")
	res, code := m.RedeemVoucher(context.Background(), redeemRequest{Voucher: v, Capability: "work.do"})
	if code != 200 || w.invoked != 1 {
		t.Fatalf("redeem = %d %v, work ran %d times", code, res, w.invoked)
	}
	if res["status"] != string(effect.Unverified) || res["reason"] != provider.ReasonTimeout {
		t.Fatalf("status %v reason %v: want UNVERIFIED, timeout", res["status"], res["reason"])
	}
	if effs := h.eventsOf(EvCapabilityEffect); len(effs) != 1 || effs[0].payload["status"] != string(effect.Unverified) {
		t.Errorf("the chain records %+v", effs)
	}
}

//go:build !no_x402

package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
)

// A build that cannot charge must not do paid work for free.
//
// The interesting failure is not a crash. It is a provider that priced
// its work at 25 credits, running on a node with no way to collect,
// cheerfully doing the job for nothing — every test green, every call
// succeeding, and the operator discovering months later that the price
// was decorative. So the absence of a payer is a refusal with a reason,
// not a silent free path.
func TestPricedWorkIsRefusedWhenThisBuildCannotCharge(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	work := &pricedProvider{price: 120}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	// The provider loses its payer: this is the -tags no_x402 shape, and
	// the requester keeps one so the test exercises "they cannot charge",
	// not "we cannot pay".
	withoutPayments(prov)
	if err := req.RegisterWithHub(ctx, srv.URL, "Payer", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Worker", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}

	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if work.invoked != 0 {
		t.Fatalf("priced work ran %d times on a node that cannot charge for it", work.invoked)
	}
	if _, err := req.Results(ctx); err != nil {
		t.Fatal(err)
	}
	res := lastResultFor(t, req, id)
	if res.Status != string(effect.Unavailable) {
		t.Fatalf("status = %s, want UNAVAILABLE", res.Status)
	}
	if !strings.Contains(res.Message, "no_x402") {
		t.Errorf("the caller is not told why: %q", res.Message)
	}
}

// And the paying surfaces say so rather than failing obscurely.
func TestThePayingSurfacesSayWhenThereIsNoPaymentSupport(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	d := newTestDaemon(t, srv.URL, false)
	withoutPayments(d)
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"balance", func() error { _, err := d.Balance(ctx); return err }},
		{"redeem", func() error { _, err := d.RedeemCredit(ctx, 10, "ref"); return err }},
		{"pay-and-retry", func() error {
			_, err := d.PayAndRetry(ctx, "did:anet:x", "work.do", nil, nil)
			return err
		}},
		{"delegate-and-pay", func() error {
			_, _, err := d.DelegateAndPay(ctx, "did:anet:x", "work.do", nil)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("this must fail — there is nothing to pay with")
			}
			if !strings.Contains(err.Error(), "no_x402") {
				t.Errorf("the error does not name the cause: %v", err)
			}
		})
	}
}

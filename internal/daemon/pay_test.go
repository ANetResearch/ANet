//go:build !no_x402

package daemon

// The paid loop, tested where both halves meet.
//
// Tagged !no_x402 and importing the module on purpose. The kernel drives
// the delegation and the module handles the money; testing either against
// a stand-in for the other is exactly the arrangement that let this loop
// compile for a month without ever having run. So this file uses the real
// module, and a build that removes the module removes the test with it —
// which is honest, because there is no loop left to close.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/module/x402"
	"github.com/ANetResearch/ANet/provider"
)

// The x402 module registers itself in init(), so importing it — which
// this file does — gives every daemon in this test binary a payer,
// exactly as importing it from cmd/anet does in a real build. That is
// worth knowing when reading these tests: nobody wires the payer up, the
// build does.
//
// withoutPayments is therefore how a -tags no_x402 node is reproduced:
// take the payer away again.
func withoutPayments(d *Daemon) {
	d.mu.Lock()
	d.pay = nil
	d.mu.Unlock()
}

// pricedProvider costs money and says so.
type pricedProvider struct {
	price   uint64
	invoked int
}

func (p *pricedProvider) ID() string { return "priced" }
func (p *pricedProvider) Capabilities(context.Context) ([]string, error) {
	return []string{"work.do"}, nil
}
func (p *pricedProvider) Describe(context.Context) (string, error) { return "", nil }
func (p *pricedProvider) Health(context.Context) error             { return nil }
func (p *pricedProvider) Price(cap string) (uint64, bool) {
	if cap == "work.do" {
		return p.price, true
	}
	return 0, false
}
func (p *pricedProvider) Invoke(context.Context, provider.Call) (effect.Effect, error) {
	p.invoked++
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"done": 1}},
		Evidence: &effect.Evidence{Protocol: "test", Requested: "work.do",
			NativeAck: true, VerifyTrust: 1},
	}, nil
}

// Priced work is quoted, not refused — and the quote is a real answer.
//
// A provider that wants paying has not failed, so PAYMENT_REQUIRED is its
// own status; and the quote is signed, receipted and recorded like any
// other result, because a price nobody can point back at is not a price.
func TestPricedWorkIsQuotedNotRefused(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	work := &pricedProvider{price: 120}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("unpaid work was done %d times", work.invoked)
	}
	if _, err := req.Results(ctx); err != nil {
		t.Fatal(err)
	}
	quote := lastResultFor(t, req, id)
	if quote.Status != string(effect.PaymentRequired) {
		t.Fatalf("status = %s, want PAYMENT_REQUIRED", quote.Status)
	}
	if quote.Payment == nil || len(quote.Payment.Accepts) == 0 {
		t.Fatal("the quote carried no price")
	}
	opt := quote.Payment.Accepts[0]
	if opt.Scheme != payment.SchemeCredit || opt.Amount != "120" || opt.PayTo != prov.AID() {
		t.Errorf("quote = %+v", opt)
	}
	// The quote is on the provider's own chain, like any answer.
	if got := lastLedgerPayload(t, prov, EvCapabilityEffect)["status"]; got != string(effect.PaymentRequired) {
		t.Errorf("the quote is not on the chain as such: %v", got)
	}
}

// The loop that had never run: quote → authorize → settle → work.
//
// Every piece of this existed and compiled for a month, and no test and
// no caller ever put them in a line. That is the same mistake as the
// module seam nothing imported, and it is why this test asserts the whole
// arc rather than each step: a step that passes in isolation is exactly
// what was already true.
func TestThePaidLoopClosesEndToEnd(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	work := &pricedProvider{price: 120}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Payer", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Worker", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	grantOn(srv.URL, req.AID(), 500)

	// 1. Ask, and be quoted.
	quoteID, err := req.DelegateCapability(ctx, prov.AID(), "work.do", map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := req.Results(ctx); err != nil {
		t.Fatal(err)
	}
	quote := lastResultFor(t, req, quoteID)
	if quote.Status != string(effect.PaymentRequired) {
		t.Fatalf("step 1: status = %s, want PAYMENT_REQUIRED", quote.Status)
	}
	if work.invoked != 0 {
		t.Fatalf("step 1: unpaid work was done %d times", work.invoked)
	}

	// 2. Pay it, and ask again. A second interaction, because the first
	// ended with a real answer that stays on both chains.
	paidID, err := req.PayAndRetry(ctx, prov.AID(), "work.do", map[string]any{"n": 1}, quote.Payment)
	if err != nil {
		t.Fatalf("step 2: %v", err)
	}
	if paidID == quoteID {
		t.Error("step 2: paying must not overwrite the interaction the quote lives in")
	}

	// 3. The provider settles before working.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if work.invoked != 1 {
		t.Fatalf("step 3: paid work ran %d times, want 1", work.invoked)
	}
	if _, err := req.Results(ctx); err != nil {
		t.Fatal(err)
	}
	done := lastResultFor(t, req, paidID)
	if done.Status != string(effect.OK) {
		t.Fatalf("step 3: status = %s (%s), want OK", done.Status, done.Message)
	}
	if done.Paid == nil || done.Paid.Transaction == "" {
		t.Fatal("step 3: the result does not say it was paid for")
	}

	// 4. The credit actually moved. A loop that reports success without
	// this is the failure mode worth testing for.
	if got := balanceOf(srv.URL, req.AID()); got != 380 {
		t.Errorf("step 4: payer balance = %d, want 380", got)
	}
	if got := balanceOf(srv.URL, prov.AID()); got != 120 {
		t.Errorf("step 4: provider balance = %d, want 120", got)
	}

	// 5. The hub's signed statement reached the payer. Without it the
	// payer holds a transaction string it cannot check, which is the
	// provider's word for the provider having been paid.
	if done.Paid.Receipt == "" {
		t.Fatal("step 5: the hub's settlement receipt did not travel")
	}
	raw, err := base64.StdEncoding.DecodeString(done.Paid.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := payment.UnmarshalReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Verify(hubKELOf(t, srv.URL), hubAIDOf(srv.URL), time.Now().UnixMilli()); err != nil {
		t.Errorf("step 5: the settlement receipt does not verify: %v", err)
	}
	if rec.Payer != req.AID() || rec.PayTo != prov.AID() || rec.Amount != 120 {
		t.Errorf("step 5: receipt = %+v", rec)
	}

	// 6. Both chains carry it, which is the whole custody bargain: the
	// balance is the hub's, the record is the parties'.
	if got := lastLedgerPayload(t, req, x402.EvPaymentAuthorized)["interaction_id"]; got != paidID {
		t.Errorf("step 6: payer's authorization is not on its chain: %v", got)
	}
	payerSettled := lastLedgerPayload(t, req, EvPaymentSettled)
	if payerSettled["interaction_id"] != paidID {
		t.Errorf("step 6: payer's settlement is not on its chain: %v", payerSettled)
	}
	if payerSettled["verified"] != true {
		t.Errorf("step 6: the payer recorded a settlement it could not verify: %v", payerSettled)
	}
	if got := lastLedgerPayload(t, prov, EvPaymentSettled)["interaction_id"]; got != paidID {
		t.Errorf("step 6: payee's settlement is not on its chain: %v", got)
	}

	// 7. And the payer can read its own standing off the custodian.
	bal, err := req.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bal["balance"] != int64(380) {
		t.Errorf("step 7: reported balance = %v, want 380", bal["balance"])
	}
}

// Paying more than you have must fail as a payment, not as the work.
//
// The distinction is the point: a caller told FAILED would look for a bug
// in the capability. PAYMENT_REQUIRED with a reason tells it the truth,
// which is that the work is fine and the money is not.
func TestPayingWithoutCreditIsRefusedAsAPayment(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	work := &pricedProvider{price: 120}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Payer", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Worker", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	grantOn(srv.URL, req.AID(), 10) // not enough

	quoted := &payment.PaymentRequired{
		X402Version: payment.Version,
		Accepts: []payment.PaymentOption{{
			Scheme: payment.SchemeCredit, Network: payment.CreditNetwork(hubAIDOf(srv.URL)),
			Amount: "120", Asset: payment.AssetCredit, PayTo: prov.AID(),
		}},
	}
	id, err := req.PayAndRetry(ctx, prov.AID(), "work.do", nil, quoted)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if work.invoked != 0 {
		t.Fatalf("work ran %d times for a payment that did not settle", work.invoked)
	}
	if _, err := req.Results(ctx); err != nil {
		t.Fatal(err)
	}
	res := lastResultFor(t, req, id)
	if res.Status != string(effect.PaymentRequired) {
		t.Fatalf("status = %s, want PAYMENT_REQUIRED", res.Status)
	}
	if !strings.Contains(res.Message, payment.ReasonInsufficientFunds) {
		t.Errorf("the payer is not told why: %q", res.Message)
	}
	if got := balanceOf(srv.URL, req.AID()); got != 10 {
		t.Errorf("a refused payment moved credit: balance = %d", got)
	}
}
func lastResultFor(t *testing.T, d *Daemon, ixID string) capabilityResult {
	t.Helper()
	results, err := d.Results(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.InteractionID == ixID {
			var out capabilityResult
			if err := json.Unmarshal([]byte(r.Result), &out); err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	t.Fatalf("no result for %s", ixID)
	return capabilityResult{}
}

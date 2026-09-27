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
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
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

// Priced work is quoted, not refused, on the task itself (A2A-DESIGN
// §8.3): the task waits in input-required with the price, and the quote is
// on the provider's chain as anet.payment.quoted.
func TestPricedWorkIsQuotedNotRefused(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	work := &pricedProvider{price: 120}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Payer", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Worker", nil, ""); err != nil {
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
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	rix, _ := req.ix.Get(id)
	if rix.State != interactions.StateInputRequired || rix.PayState != interactions.PayRequired {
		t.Fatalf("requester: state %s pay_state %q, want input-required / required", rix.State, rix.PayState)
	}
	q := storedQuote(rix)
	if q == nil || len(q.Accepts) == 0 {
		t.Fatal("the quote carried no price")
	}
	opt := q.Accepts[0]
	if opt.Scheme != payment.SchemeCredit || opt.Amount != "120" || opt.PayTo != prov.AID() {
		t.Errorf("quote = %+v", opt)
	}
	if len(rix.Receipt) != 0 {
		t.Error("a quote is not a result: nothing was done, and nothing is receipted")
	}
	got := lastLedgerPayload(t, prov, EvPaymentQuoted)
	if got["interaction_id"] != id || got["amount"] != "120" || got["pay_to"] != prov.AID() {
		t.Errorf("the quote is not on the provider's chain: %v", got)
	}
}

// The prepaid path (DelegateReq.Payment, kept by A2A-DESIGN §8.3):
// quote → authorize → a new task carrying the payment → settle → work.
// The provider checks it against the quote it makes for that task and
// settles it on the same path as a payment on the task.
func TestThePaidLoopClosesEndToEnd(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	work := &pricedProvider{price: 120}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Payer", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Worker", nil, ""); err != nil {
		t.Fatal(err)
	}
	grantOn(srv.URL, req.AID(), 500)
	payPolicy(t, req, PaymentsConfig{ExplicitMax: u64(200), DailyMax: u64(1000)}, prov.AID())

	// 1. Ask, and be quoted.
	quoteID, err := req.DelegateCapability(ctx, prov.AID(), "work.do", map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	qix, _ := req.ix.Get(quoteID)
	quote := storedQuote(qix)
	if quote == nil {
		t.Fatalf("step 1: no quote stored (pay_state %q)", qix.PayState)
	}
	if work.invoked != 0 {
		t.Fatalf("step 1: unpaid work was done %d times", work.invoked)
	}

	// 2. Pay it up front on a new task.
	paidID, err := req.PayAndRetry(ctx, prov.AID(), "work.do", map[string]any{"n": 1}, quote)
	if err != nil {
		t.Fatalf("step 2: %v", err)
	}
	if paidID == quoteID {
		t.Error("step 2: the prepaid call must be a task of its own")
	}

	// 3. The provider settles before working.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if work.invoked != 1 {
		t.Fatalf("step 3: paid work ran %d times, want 1", work.invoked)
	}
	for i := 0; i < 2; i++ {
		if err := req.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	done := lastResultFor(t, req, paidID)
	if done.Status != string(effect.OK) {
		t.Fatalf("step 3: status = %s (%s), want OK", done.Status, done.Message)
	}
	if done.Paid == nil || done.Paid.Transaction == "" {
		t.Fatal("step 3: the result does not say it was paid for")
	}

	// 4. The credit actually moved, once.
	if got := balanceOf(srv.URL, req.AID()); got != 380 {
		t.Errorf("step 4: payer balance = %d, want 380", got)
	}
	if got := balanceOf(srv.URL, prov.AID()); got != 120 {
		t.Errorf("step 4: provider balance = %d, want 120", got)
	}

	// 5. The hub's signed statement reached the payer and verifies.
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

	// 6. Both chains carry it.
	auth := lastLedgerPayload(t, req, x402.EvPaymentAuthorized)
	pix, _ := req.ix.Get(paidID)
	if auth["interaction_id"] != paidID || auth["purpose"] != module.PurposeGateway ||
		auth["pay_bind"] != x402a2a.PayBind(paidID, pix.TaskNonce) {
		t.Errorf("step 6: payer's authorization is not on its chain as signed: %v", auth)
	}
	payerSettled := lastLedgerPayload(t, req, EvPaymentSettled)
	if payerSettled["interaction_id"] != paidID || payerSettled["verified"] != true {
		t.Errorf("step 6: payer's settlement is not on its chain, verified: %v", payerSettled)
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

// Paying more than you have fails as a payment, not as the work: the task
// is told payment-failed with INSUFFICIENT_FUNDS and waits for another
// payment (A2A-DESIGN §8.3, §8.5).
func TestPayingWithoutCreditIsRefusedAsAPayment(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	work := &pricedProvider{price: 120}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Payer", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Worker", nil, ""); err != nil {
		t.Fatal(err)
	}
	grantOn(srv.URL, req.AID(), 10) // not enough
	payPolicy(t, req, PaymentsConfig{ExplicitMax: u64(200), DailyMax: u64(1000)}, prov.AID())

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
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	rix, _ := req.ix.Get(id)
	if rix.State != interactions.StateInputRequired || rix.PayState != interactions.PayFailed {
		t.Fatalf("requester: state %s pay_state %q, want input-required / failed", rix.State, rix.PayState)
	}
	_, meta := lastStatusMeta(t, req, id)
	if meta[x402a2a.KeyStatus] != x402a2a.StatusFailed || meta[x402a2a.KeyError] != "INSUFFICIENT_FUNDS" ||
		meta["anet.reason"] != payment.ReasonInsufficientFunds {
		t.Errorf("the payer is not told why: %v", meta)
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

// Buying through a gateway is something a person does, not only
// something a test does — so the header they need has to be reachable
// without a fixture binary.
//
// The joint run had been signing gateway payments with anetfixture,
// which meant the one path a real buyer would take was the one path
// nothing exercised. A test tool standing in for a user-facing command is
// the same substitution that hid every other defect this month.
func TestANodeCanSignAPaymentForAGateway(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	d := newTestDaemon(t, srv.URL, false)
	if err := d.RegisterWithHub(ctx, srv.URL, "Buyer", nil, ""); err != nil {
		t.Fatal(err)
	}
	payPolicy(t, d, PaymentsConfig{ExplicitMax: u64(30)}, "did:anet:seller")
	p := d.payer()
	if p == nil {
		t.Fatal("no payer")
	}
	raw, err := p.Authorize(payment.PaymentOption{
		Scheme: payment.SchemeCredit, Network: payment.CreditNetwork(hubAIDOf(srv.URL)),
		Amount: "30", Asset: payment.AssetCredit, PayTo: "did:anet:seller",
	}, "gw-1", "gw-1", module.PurposeGateway)
	if err != nil {
		t.Fatal(err)
	}
	var pp payment.PaymentPayload
	if err := json.Unmarshal(raw, &pp); err != nil {
		t.Fatal(err)
	}
	// The terms a gateway checks before it settles: who is being paid and
	// how much. A mismatch here reads as the gateway refusing a correct
	// payment.
	if pp.Accepted.PayTo != "did:anet:seller" || pp.Accepted.Amount != "30" {
		t.Errorf("accepted terms = %+v", pp.Accepted)
	}
	enc, _ := pp.Payload["authorization"].(string)
	authRaw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := payment.UnmarshalAuthorization(authRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.Verify(d.self.KEL(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("the header a buyer would send does not verify: %v", err)
	}
	if auth.Payer != d.AID() {
		t.Errorf("payer = %s, want this node", auth.Payer)
	}
	// Signing is not spending. Nothing has settled until a gateway
	// presents it, and the balance must be untouched.
	if got := balanceOf(srv.URL, d.AID()); got != 0 {
		t.Errorf("signing an authorization moved credit: %d", got)
	}
}

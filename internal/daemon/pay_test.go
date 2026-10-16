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
	"encoding/json"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	_ "github.com/ANetResearch/ANet/module/x402"
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
	if err := req.RegisterWithHub(ctx, srv.URL, "Payer", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Worker", nil); err != nil {
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

// Paying more than you have fails as a payment, not as the work: the task
// is told payment-failed with INSUFFICIENT_FUNDS and waits for another
// payment (A2A-DESIGN §8.3, §8.5). The payment is made on the quoted task;
// nothing pays in the message that asks for the work (0.3.0).
func TestPayingWithoutCreditIsRefusedAsAPayment(t *testing.T) {
	srv, req, prov := registeredPair(t)
	work := &meteredWork{price: 120}
	if err := prov.Providers().Register(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	grantOn(srv.URL, req.AID(), 10) // not enough
	cardPriced(t, req, prov, "work.do")
	payPolicy(t, req, PaymentsConfig{AutoMax: 200, DailyMax: u64(1000)}, prov.AID())
	ctx := context.Background()

	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov) // quoted
	poll(t, req)  // paid within auto_max, on the same task
	if rix := getIX(t, req, id); rix.PayState != interactions.PaySubmitted {
		t.Fatalf("requester after the quote: %s / %q, want a submitted payment", rix.State, rix.PayState)
	}
	poll(t, prov) // checked, presented to the hub, refused
	if n := work.invoked.Load(); n != 0 {
		t.Fatalf("work ran %d times for a payment that did not settle", n)
	}
	poll(t, req)
	rix := getIX(t, req, id)
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
	// The payment that did not settle is the task's payment sentence (HPP
	// PAY-U5, ux D3), with its own next step, and the automatic tier does
	// not pay again by itself (at most once per task).
	n, ok := storedPayNote(getIX(t, req, id))
	if !ok || n.Code != "x402:INSUFFICIENT_FUNDS" || n.Amount != 120 || n.Peer != prov.AID() {
		t.Fatalf("the snapshot of the failure: %+v (%v)", n, ok)
	}
	task := getTaskProjection(t, req, id)
	want := a2ashape.PaymentNote(n, a2ashape.Lang())
	if task.Metadata[a2ashape.KeyPaymentNote] != want || task.Metadata[a2ashape.KeyNext] != "anet balance" ||
		task.Metadata[a2ashape.KeyReason] != x402a2a.ReasonNeedsOperatorApproval ||
		task.Status.Message == nil || task.Status.Message.Parts[0].Text != want {
		t.Fatalf("the task after the failure: %+v", task)
	}
	if n := chainEvents(t, req, EvPaymentAuthorized); n != 1 {
		t.Fatalf("%d authorizations", n)
	}
}

// failureNote picks the row of ux D3 from what the provider said, and only
// from this node's table.
func TestAFailedPaymentIsSaidByARowOfD3(t *testing.T) {
	at := time.Now()
	for _, c := range []struct{ reason, code, want string }{
		{"payee_not_local", "SETTLEMENT_FAILED", "payee_not_local"},
		{"auth_window_too_long", "SETTLEMENT_FAILED", "auth_window_too_long"},
		{"binding_mismatch", "SETTLEMENT_FAILED", "binding_mismatch"},
		{"insufficient_funds", "INSUFFICIENT_FUNDS", "x402:INSUFFICIENT_FUNDS"},
		{"", "DUPLICATE_NONCE", "x402:DUPLICATE_NONCE"},
		{"needs_operator_approval.off", "INVALID_AMOUNT", "x402:INVALID_AMOUNT"}, // a D1 code is no failure
		{"paid.auto", "", "x402:SETTLEMENT_FAILED"},
		{"anything the provider likes", "NOT_A_CODE", "x402:SETTLEMENT_FAILED"},
	} {
		if n := failureNote(c.reason, c.code, 3, "p", at); n.Code != c.want {
			t.Errorf("%q/%q: %s, want %s", c.reason, c.code, n.Code, c.want)
		}
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

//go:build !no_x402

package daemon

// MOD-2: a node that cannot pay says so in one way, on every surface, and
// does nothing about a price it was quoted. The tests take the payer away
// (withoutPayments), as a -tags no_x402 node or `without: [x402]` leaves it.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// TestPaymentNoteWithoutPayer: a quote arrives at a node with no payment
// module. The task stays input-required with anet.reason module_absent and a
// sentence that does not tell the person to run `anet pay` (it would fail)
// but to cancel the task; the provider is sent nothing, and the node signed
// and recorded nothing.
func TestPaymentNoteWithoutPayer(t *testing.T) {
	for _, tc := range []struct{ name, without string }{{"not built in", ""}, {"turned off", "x402"}} {
		t.Run(tc.name, func(t *testing.T) {
			work := &meteredWork{price: 3}
			_, req, prov := paidPair(t, work)
			payPolicy(t, req, PaymentsConfig{AutoMax: 5, ExplicitMax: u64(defaultExplicitMax), DailyMax: u64(defaultDailyMax)}, prov.AID())
			withoutPayments(req)
			if tc.without != "" {
				av, err := module.NewAvailability([]string{tc.without})
				if err != nil {
					t.Fatal(err)
				}
				req.avail = av
			}
			ctx := context.Background()
			id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
			if err != nil {
				t.Fatal(err)
			}
			poll(t, prov, req)

			ix := getIX(t, req, id)
			if ix.State != interactions.StateInputRequired || ix.PayState != interactions.PayRequired {
				t.Fatalf("state %s pay_state %q, want input-required / required", ix.State, ix.PayState)
			}
			if got := PaymentReason(ix); got != "module_absent" {
				t.Errorf("anet.reason %q, want module_absent", got)
			}
			n, text := sentenceOf(t, req, id)
			if n.Code != "module_absent.payment" || n.Amount != 3 {
				t.Fatalf("decision %+v", n)
			}
			if !strings.Contains(text, "anet task cancel "+id) {
				t.Errorf("the sentence does not say how to cancel: %q", text)
			}
			sameEverywhere(t, req, id, prov.AID(), text, true)
			if task := getTaskProjection(t, req, id); task.Metadata[a2ashape.KeyNext] != "anet task cancel "+id {
				t.Errorf("anet.next %v, want anet task cancel %s", task.Metadata[a2ashape.KeyNext], id)
			}
			// Nothing was declined, signed or sent: the provider still waits for
			// a payment, and this node's chain has no payment event.
			if pix := getIX(t, prov, id); pix.PayState != interactions.PayRequired || int(work.invoked.Load()) != 0 {
				t.Errorf("provider: pay_state %q, work done %d times", pix.PayState, int(work.invoked.Load()))
			}
			_, recs := req.ledger.Evidence(EvidenceQuery{Limit: maxEvidenceLimit})
			for _, r := range recs {
				if strings.HasPrefix(r.EventType, "anet.payment.") {
					t.Errorf("payment evidence on a node without a payer: %s", r.EventType)
				}
			}
		})
	}
}

// TestPayEntrancesAnswerModuleAbsent: the entrances that carry a payment
// decision answer module_absent, and never the old English sentence: the
// kernel's payment functions, the agent-tier seam (MCP submit_payment and
// reject_payment, the local A2A payment-submitted), and the status members.
func TestPayEntrancesAnswerModuleAbsent(t *testing.T) {
	d := startWith(t, map[string]any{"without": []string{"x402"}}, nil)
	ctx := context.Background()
	check := func(what string, err error) {
		t.Helper()
		var ref *a2ashape.Refusal
		if !errors.As(err, &ref) || ref.Code() != "module_absent" {
			t.Errorf("%s: %v, want module_absent", what, err)
			return
		}
		if s := ref.Error(); !strings.Contains(s, "payment module (x402") || strings.Contains(s, "no payment support") {
			t.Errorf("%s: %q", what, s)
		}
	}
	ix := &interactions.Interaction{ID: "t1", Role: interactions.RoleOutbound}
	_, err := d.RedeemCredit(ctx, 1, "r", "hub")
	check("RedeemCredit", err)
	for _, dec := range []module.PayDecision{{Decision: module.PaySubmit}, {Decision: module.PayReject}} {
		_, err = d.taskPay(ctx, ix, dec)
		check("taskPay "+string(dec.Decision), err)
	}
	for _, status := range []string{"payment-submitted", "payment-rejected"} {
		_, _, err = d.taskPaymentMessage(ctx, ix, a2ashape.Message{Metadata: map[string]any{"x402.payment.status": status}}, nil)
		check("payment message "+status, err)
	}
	check("PayTask", func() error { _, err := d.PayTask(ctx, PayRequest{TaskID: "none"}); return err }())
	if d.canPay() {
		t.Error("canPay is true without a payer")
	}
}

// TestCanPayIsWorkedOutEachTime (K-11): the seam answers from the payer, the
// hub address and the hub's AID as they are now. A node that registers at its
// hub after it started can pay from then on, and one without a payer never.
func TestCanPayIsWorkedOutEachTime(t *testing.T) {
	if (&DaemonTaskSeam{d: newTestDaemon(t, "", false)}).CanPay() {
		t.Fatal("CanPay with no hub")
	}
	srv := newFakeHub(t)
	d := registered(t, srv.URL, "Payer")
	seam := &DaemonTaskSeam{d: d}
	if !seam.CanPay() {
		t.Fatal("CanPay is false with the payment module, a hub and its AID")
	}
	withoutPayments(d)
	if seam.CanPay() {
		t.Fatal("CanPay without a payer")
	}
}

// TestUnchargeableCount: priced capabilities, public or allow-only, are
// counted on a node without a payer, once each; none on a node that can
// charge (payments.unchargeable).
func TestUnchargeableCount(t *testing.T) {
	_, _, prov := paidPair(t, &meteredWork{price: 3})
	setPublic(prov, "work.do")
	if n := prov.unchargeableCount(); n != 0 {
		t.Errorf("a node that can charge counts %d", n)
	}
	withoutPayments(prov)
	if n := prov.unchargeableCount(); n != 1 {
		t.Errorf("unchargeable = %d, want 1", n)
	}
}

// A priced capability that is not public (it sits only where the allow and
// trust lists reach it) is refused without a payer too, so it is counted.
func TestUnchargeableCountIncludesANonPublicCapability(t *testing.T) {
	_, _, prov := paidPair(t, &meteredWork{price: 3})
	withoutPayments(prov)
	if n := prov.unchargeableCount(); n != 1 {
		t.Errorf("a priced capability that is not public: unchargeable = %d, want 1", n)
	}
}

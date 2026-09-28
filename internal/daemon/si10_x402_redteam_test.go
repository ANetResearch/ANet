//go:build !no_x402

package daemon

// Red-team regressions for SI-10 on the requester's payment path
// ([redteam:F28]); see si10_storefault_redteam_test.go for storeFault.

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// [redteam:F28] regression (was TestRedteamSI10_QuoteWriteFailureIsAckedAndTheTaskCannotBePaid).
// The requester stores a provider's quote in the transaction of the
// payment-required status that carries it, with the status row, its state
// and its replay row. A storage error on the quote used to be only logged
// after the commit: the status acknowledged, its redelivery a duplicate,
// and the task stuck in input-required with no quote, never paid
// automatically and refused by `anet pay`. Now the error rolls the status
// back: it is not acknowledged, and its redelivery stores the quote, which
// the automatic tier pays, and the work runs.
func TestRedteamSI10_QuoteWriteFailureLeavesTheStatusForRedelivery(t *testing.T) {
	work := &meteredWork{price: 120}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 200, AgentDailyMax: 500, DailyMax: u64(1000)}, prov.AID())
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov) // quote
	if q := queuedAt(t, hub, req.AID()); len(q) != 1 {
		t.Fatalf("%d envelopes for the requester, want the quote", len(q))
	}

	recover := storeFault(t, req, "rt_quote_fault",
		`CREATE TRIGGER rt_quote_fault BEFORE UPDATE OF pay_required ON interaction BEGIN SELECT RAISE(ABORT, 'disk I/O error (injected)'); END`)
	poll(t, req)
	if n := len(queuedAt(t, hub, req.AID())); n != 1 {
		t.Fatalf("the quote status was acknowledged although its quote was not stored (%d left)", n)
	}
	if rix := getIX(t, req, id); rix.State == interactions.StateInputRequired || rix.PayState != interactions.PayNone {
		t.Fatalf("requester: %s / %q; the status was stored without its quote", rix.State, rix.PayState)
	}
	recover()

	// The redelivery stores the status and the quote together, and the
	// automatic tier pays it.
	poll(t, req, req)
	if n := len(queuedAt(t, hub, req.AID())); n != 0 {
		t.Fatalf("%d envelopes left after storage recovered", n)
	}
	rix := getIX(t, req, id)
	if rix.PayState != interactions.PaySubmitted || len(rix.PayAuthIDs) != 1 {
		t.Fatalf("requester after the redelivery: %s / %q / %v, want the quote stored and paid", rix.State, rix.PayState, rix.PayAuthIDs)
	}
	poll(t, prov)
	if n := work.invoked.Load(); n != 1 {
		t.Fatalf("the paid work ran %d times", n)
	}
	poll(t, req, req)
	if rix := getIX(t, req, id); rix.State != interactions.StateCompleted || rix.PayState != interactions.PayCompleted {
		t.Fatalf("requester at the end: %s / %q", rix.State, rix.PayState)
	}
}

// [redteam:F28] The settlement a result reports goes in with the result: a
// storage error on the receipts rolls the result back (not acknowledged),
// and its redelivery records the result, the receipts, pay_state completed
// and one verified settlement. Written after the commit, the error left the
// payment short of completed with the result already acknowledged.
func TestRedteamSI10_ReceiptWriteFailureLeavesTheResultForRedelivery(t *testing.T) {
	work := &meteredWork{price: 120}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 200, AgentDailyMax: 500, DailyMax: u64(1000)}, prov.AID())
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov) // quote
	poll(t, req)  // paid automatically
	poll(t, prov) // settled, run, answered with the receipts
	if n := work.invoked.Load(); n != 1 {
		t.Fatalf("setup: the paid work ran %d times", n)
	}
	queued := len(queuedAt(t, hub, req.AID()))
	if queued == 0 {
		t.Fatal("setup: no answer for the requester")
	}

	recover := storeFault(t, req, "rt_receipt_fault",
		`CREATE TRIGGER rt_receipt_fault BEFORE UPDATE OF pay_receipts ON interaction BEGIN SELECT RAISE(ABORT, 'disk I/O error (injected)'); END`)
	poll(t, req)
	if n := len(queuedAt(t, hub, req.AID())); n == 0 {
		t.Fatal("the answer carrying the receipts was acknowledged although they were not stored")
	}
	if rix := getIX(t, req, id); rix.PayState == interactions.PayCompleted {
		t.Fatalf("requester: %s / %q; stored without its receipts", rix.State, rix.PayState)
	}
	recover()

	poll(t, req, req)
	if n := len(queuedAt(t, hub, req.AID())); n != 0 {
		t.Fatalf("%d envelopes left after storage recovered", n)
	}
	rix := getIX(t, req, id)
	if rix.State != interactions.StateCompleted || rix.PayState != interactions.PayCompleted || len(rix.PayReceipts) == 0 {
		t.Fatalf("requester at the end: %s / %q, %d receipt bytes", rix.State, rix.PayState, len(rix.PayReceipts))
	}
	verified := 0
	req.ledger.scan(EvPaymentSettled, 0, func(_ int64, p map[string]any) {
		if p["interaction_id"] == id && p["verified"] == true {
			verified++
		}
	})
	if verified != 1 {
		t.Fatalf("%d verified settlements recorded, want 1", verified)
	}
}

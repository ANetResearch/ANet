//go:build !no_x402

package daemon

// Red-team PoC for SI-10 on the requester's payment path; see
// si10_storefault_redteam_test.go for storeFault. A passing test means the
// defect is present.

import (
	"context"
	"errors"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The requester stores a provider's quote after the payment-required
// status and its replay row commit (ingestStatus → onProviderPayment →
// SetPayment). A storage error there is only logged: the status is acked,
// its redelivery is a duplicate, and the task sits in input-required with
// no quote: the auto tier never pays and `anet pay` answers ErrNoQuote.
// (The provider side has startup recovery for its twin window,
// untakenPayment; the requester side has none, and none for a store error
// without a restart.)
func TestRedteamSI10_QuoteWriteFailureIsAckedAndTheTaskCannotBePaid(t *testing.T) {
	work := &meteredWork{price: 120}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 200, AgentDailyMax: 500, DailyMax: u64(1000)}, prov.AID())
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov) // quote
	q := queuedAt(t, hub, req.AID())
	if len(q) != 1 {
		t.Fatalf("%d envelopes for the requester, want the quote", len(q))
	}
	env := q[0]

	recover := storeFault(t, req, "rt_quote_fault",
		`CREATE TRIGGER rt_quote_fault BEFORE UPDATE OF pay_required ON interaction BEGIN SELECT RAISE(ABORT, 'disk I/O error (injected)'); END`)
	poll(t, req)
	recover()

	if n := len(queuedAt(t, hub, req.AID())); n != 0 {
		t.Fatalf("the quote status was not acked (%d left); SI-10 held", n)
	}
	if r := receive(t, req, env); r.reason != dropDuplicate {
		t.Fatalf("redelivery: %+v", r)
	}
	rix := getIX(t, req, id)
	if rix.State != interactions.StateInputRequired || rix.PayState != interactions.PayNone || len(rix.PayAuthIDs) != 0 {
		t.Fatalf("requester: %s / %q / %v; attack failed", rix.State, rix.PayState, rix.PayAuthIDs)
	}
	if _, err := req.PayTask(ctx, PayRequest{TaskID: id, Decision: PayDecisionSubmit}); !errors.Is(err, ErrNoQuote) {
		t.Fatalf("PayTask = %v, want ErrNoQuote", err)
	}
	// A restart does not bring it back either.
	req2 := restartDaemon(t, req)
	if rix := getIX(t, req2, id); rix.PayState != interactions.PayNone {
		t.Fatalf("after restart pay_state = %q", rix.PayState)
	}
	poll(t, prov)
	if n := work.invoked.Load(); n != 0 {
		t.Fatalf("work ran %d times", n)
	}
}

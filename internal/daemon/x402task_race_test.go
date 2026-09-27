//go:build !no_x402

package daemon

// Orderings of the same-task payment flow that a single happy run does not
// reach (A2A-DESIGN §4.2, §8.3 [C13][C25][C34]): a payment and the end of
// its task, a refusal and a payment already taken, a quote repeated while
// a payment is outstanding, a stop between a payment's arrival and its
// being taken, and a revocation sweep over paid tasks.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// withSettleRetry sets the settlement retry pause for one test.
func withSettleRetry(t *testing.T, d time.Duration) {
	t.Helper()
	old := settleRetryBase.set(d)
	t.Cleanup(func() { settleRetryBase.set(old) })
}

// quotedTask delegates work.do from req to prov and delivers the quote.
func quotedTask(t *testing.T, req, prov *Daemon) (string, []byte) {
	t.Helper()
	id, err := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	rix := getIX(t, req, id)
	if rix.PayState != interactions.PayRequired {
		t.Fatalf("requester after the quote: %q", rix.PayState)
	}
	raw, err := req.payer().Authorize(storedQuote(rix).Accepts[0], id, x402a2a.PayBind(id, rix.TaskNonce),
		module.PurposeGateway)
	if err != nil {
		t.Fatal(err)
	}
	return id, raw
}

// A payment read against an open task, taken after the task ended: the
// end wins and nothing goes to the hub (§4.2 [C34]). The payment is taken
// under the write lock with the task's state, not on the strength of an
// earlier read.
func TestAPaymentForATaskThatEndedIsNotPresented(t *testing.T) {
	work := &meteredWork{price: 30}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{ExplicitMax: u64(100), DailyMax: u64(1000)}, prov.AID())
	ctx := context.Background()
	id, raw := quotedTask(t, req, prov)
	stale := getIX(t, prov, id)
	if _, err := prov.CancelTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if prov.takePayment(ctx, stale, "work.do", 30, raw) {
		t.Fatal("a payment for a canceled task was taken")
	}
	pix := getIX(t, prov, id)
	if pix.State != interactions.StateCanceled || pix.PayState == interactions.PaySubmitted {
		t.Fatalf("provider: %s / %q", pix.State, pix.PayState)
	}
	if n := len(settleBodiesOn(hub)); n != 0 || debitsOn(hub, req.AID()) != 0 || work.invoked.Load() != 0 {
		t.Errorf("%d settle calls, %d debits, ran %d", n, debitsOn(hub, req.AID()), work.invoked.Load())
	}
}

// A refusal of a payment that was never taken does not reach the payment
// that was: provider_busy or a lapsed quote on a task whose payment is
// submitted leaves it submitted, sends nothing, and the settlement goes on
// to completion with one debit. The provider does not cancel it either.
func TestARefusalDoesNotEndASubmittedPayment(t *testing.T) {
	withSettleRetry(t, time.Hour) // the loop does not retry on its own
	work := &meteredWork{price: 30}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	settleFaultsOn(hub, "pending")
	poll(t, prov)
	if pix := getIX(t, prov, id); pix.PayState != interactions.PaySubmitted {
		t.Fatalf("provider before: %q", pix.PayState)
	}
	statuses := func() int {
		msgs, _ := prov.ix.Messages(id)
		n := 0
		for _, m := range msgs {
			if m.Kind == interactions.MsgStatus {
				n++
			}
		}
		return n
	}
	before := statuses()
	prov.paymentFailed(ctx, id, payOpen, x402a2a.ReasonProviderBusy, "", "",
		failureReceipt(x402a2a.ReasonProviderBusy, ""))
	prov.paymentFailed(ctx, id, payOpen, x402a2a.ReasonQuoteExpired, "", "",
		failureReceipt(x402a2a.ReasonQuoteExpired, ""))
	pix := getIX(t, prov, id)
	if pix.PayState != interactions.PaySubmitted || pix.IsTerminal() || statuses() != before {
		t.Fatalf("after refusals meant for an open quote: %s / %q, %d statuses (was %d)",
			pix.State, pix.PayState, statuses(), before)
	}
	if _, err := prov.CancelTask(ctx, id); !errors.Is(err, ErrNotCancelable) {
		t.Fatalf("the provider canceled a task whose payment is submitted: %v", err)
	}
	// The retry the loop would make.
	if !prov.settleTaskPayment(ctx, id, false) {
		t.Fatal("the settlement did not complete")
	}
	prov.runPaidCall(id)
	if pix := getIX(t, prov, id); pix.State != interactions.StateCompleted || work.invoked.Load() != 1 ||
		debitsOn(hub, req.AID()) != 1 {
		t.Errorf("provider %s, ran %d, %d debits", pix.State, work.invoked.Load(), debitsOn(hub, req.AID()))
	}
}

// providerQuote has prov send req a payment-required for task id with pr.
func providerQuote(t *testing.T, prov, req *Daemon, id string, pr any) {
	t.Helper()
	mb, _ := json.Marshal(map[string]any{x402a2a.KeyStatus: x402a2a.StatusRequired, x402a2a.KeyRequired: pr})
	body, err := (&delegation.StatusMsg{State: string(interactions.StateInputRequired), Text: "pay",
		Metadata: mb, At: prov.nowMS()}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeStatus, id, body)); r.class != rxAccepted {
		t.Fatalf("quote: %+v", r)
	}
}

// authorizedCount is how many authorizations d signed for task id.
func authorizedCount(d *Daemon, id string) int {
	n := 0
	d.ledger.scan(EvPaymentAuthorized, 0, func(_ int64, p map[string]any) {
		if p["interaction_id"] == id {
			n++
		}
	})
	return n
}

// A quote repeated while a payment is outstanding is answered, in the auto
// tier, with the same authorization when the terms are the same, and with
// no new signature when they changed: until the payment sent has an
// outcome, a second one is an operator's decision (§8.3 [C13][C25]).
func TestARepeatedQuoteIsPaidWithTheSameAuthorizationOnly(t *testing.T) {
	work := &meteredWork{price: 30}
	_, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 500, DailyMax: u64(500)}, prov.AID())
	id, err := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req) // quoted, and paid automatically; the provider has not read it
	rix := getIX(t, req, id)
	if rix.PayState != interactions.PaySubmitted || len(rix.PayAuthIDs) != 1 || authorizedCount(req, id) != 1 {
		t.Fatalf("requester after the first quote: %q, %d ids, %d signed", rix.PayState, len(rix.PayAuthIDs),
			authorizedCount(req, id))
	}
	first := rix.PayAuthIDs[0]
	quote := storedQuote(rix)

	providerQuote(t, prov, req, id, quote)
	rix = getIX(t, req, id)
	if rix.PayState != interactions.PaySubmitted || len(rix.PayAuthIDs) != 1 || authorizedCount(req, id) != 1 {
		t.Fatalf("the same terms again: %q, ids %v, %d signed; want the first authorization sent again",
			rix.PayState, rix.PayAuthIDs, authorizedCount(req, id))
	}

	changed := *quote
	changed.Accepts = append(changed.Accepts[:0:0], quote.Accepts...)
	changed.Accepts[0].Amount = "31"
	providerQuote(t, prov, req, id, &changed)
	rix = getIX(t, req, id)
	if authorizedCount(req, id) != 1 || len(rix.PayAuthIDs) != 1 || rix.PayAuthIDs[0] != first {
		t.Fatalf("new terms while a payment is outstanding: ids %v, %d signed; want no new signature",
			rix.PayAuthIDs, authorizedCount(req, id))
	}
	if rix.PayState != interactions.PayRequired || PaymentReason(rix) != x402a2a.ReasonNeedsOperatorApproval {
		t.Errorf("the new terms wait for a decision: %q / %q", rix.PayState, PaymentReason(rix))
	}
}

// A payment that arrived and was stored, with the provider stopping before
// it was taken, is taken at the next start: the work runs once, the payer
// is charged once, and the task is not reported interrupted (§8.3, §17 C1).
func TestAPaymentNotTakenBeforeAStopIsTakenAtStart(t *testing.T) {
	withSettleRetry(t, 200*time.Millisecond)
	work := &meteredWork{price: 30}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{ExplicitMax: u64(100), DailyMax: u64(1000)}, prov.AID())
	id, raw := quotedTask(t, req, prov)
	// What the receive transaction commits for a payment-submitted: the
	// message and the task set working. The stop comes before the rest.
	meta, _ := json.Marshal(map[string]any{x402a2a.KeyStatus: x402a2a.StatusSubmitted,
		x402a2a.KeyPayload: json.RawMessage(raw)})
	if err := prov.ix.Update(func(tx *interactions.Tx) error {
		if _, _, err := tx.AddMessageRecord(interactions.MessageRecord{InteractionID: id, SenderAID: req.AID(),
			Kind: interactions.MsgPayment, MsgID: "msg_untaken", Metadata: meta}); err != nil {
			return err
		}
		_, err := tx.SetState(id, interactions.StateWorking)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	prov = reopen(t, prov)
	if err := prov.Providers().Register(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the payment to be taken and the work to run", func() bool {
		return getIX(t, prov, id).State == interactions.StateCompleted
	})
	if work.invoked.Load() != 1 || debitsOn(hub, req.AID()) != 1 {
		t.Errorf("ran %d, %d debits", work.invoked.Load(), debitsOn(hub, req.AID()))
	}
	if pix := getIX(t, prov, id); pix.PayState != interactions.PayCompleted {
		t.Errorf("provider pay_state %q", pix.PayState)
	}
}

// A revocation sweep does not cancel, or keep sending cancels for, a task
// whose payment was submitted: neither side cancels it (§4.2), and the
// sweep runs every minute.
func TestTheDenySweepLeavesAPaidTaskAlone(t *testing.T) {
	work := &meteredWork{price: 30}
	_, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	id, err := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	if rix := getIX(t, req, id); rix.PayState != interactions.PaySubmitted {
		t.Fatalf("requester: %q", rix.PayState)
	}
	denyPeers(t, req, prov.AID())
	req.revocationSweep()
	req.revocationSweep()
	msgs, _ := req.ix.Messages(id)
	for _, m := range msgs {
		if m.Kind == interactions.MsgCancel {
			t.Fatalf("the sweep sent a cancel for a paid task: %+v", m)
		}
	}
	if q, _ := req.ix.Outbox(id); len(q) > 1 {
		t.Errorf("%d messages queued for the paid task", len(q))
	}
}

// §17 C25, third case: the provider settles and then answers
// payment-failed (its answer and the settlement crossed). The requester
// signs no second authorization on its own; one signed by an operator is
// refused by the hub (one settlement per binding), the provider does not
// take it, and the requester is charged once for work done once.
func TestASecondAuthorizationAfterASettledFailureMovesNothing(t *testing.T) {
	withSettleRetry(t, time.Hour)
	work := &meteredWork{price: 30}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, ExplicitMax: u64(100), DailyMax: u64(500)},
		prov.AID())
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	settleFaultsOn(hub, "settle-then-drop")
	poll(t, prov) // settled at the hub; the provider does not know yet
	if debitsOn(hub, req.AID()) != 1 || getIX(t, prov, id).PayState != interactions.PaySubmitted {
		t.Fatalf("setup: %d debits, provider %q", debitsOn(hub, req.AID()), getIX(t, prov, id).PayState)
	}
	quote := storedQuote(getIX(t, req, id))
	mb, _ := json.Marshal(map[string]any{x402a2a.KeyStatus: x402a2a.StatusFailed,
		x402a2a.KeyError: x402a2a.CodeSettlementFailed, x402a2a.KeyRequired: quote,
		x402a2a.KeyReceipts: []any{}})
	body, _ := (&delegation.StatusMsg{State: string(interactions.StateInputRequired), Text: "payment failed",
		Metadata: mb, At: prov.nowMS()}).Marshal()
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeStatus, id, body)); r.class != rxAccepted {
		t.Fatalf("payment-failed: %+v", r)
	}
	if rix := getIX(t, req, id); rix.PayState != interactions.PayFailed || authorizedCount(req, id) != 1 {
		t.Fatalf("after payment-failed: %q, %d signed; want failed and no second signature",
			rix.PayState, authorizedCount(req, id))
	}
	if _, err := req.PayTask(ctx, PayRequest{TaskID: id, Decision: PayDecisionSubmit,
		Purpose: module.PurposeTaskManual}); err != nil {
		t.Fatal(err)
	}
	second := getIX(t, req, id).PayPayload
	st, err := prov.payer().Settle(ctx, second, quotedRequirementsOf(t, prov, id))
	if err != nil || st.Failed != "duplicate_binding" || st.Code != x402a2a.CodeDuplicateNonce {
		t.Fatalf("the hub on a second authorization for the binding: %+v %v", st, err)
	}
	// The provider's own retry learns the first settlement, and the work
	// runs; the second payment message finds the task done.
	if !prov.settleTaskPayment(ctx, id, false) {
		t.Fatal("the first settlement was not recovered")
	}
	prov.runPaidCall(id)
	poll(t, prov)
	if work.invoked.Load() != 1 || debitsOn(hub, req.AID()) != 1 {
		t.Errorf("ran %d, %d debits", work.invoked.Load(), debitsOn(hub, req.AID()))
	}
}

// quotedRequirementsOf is the requirements prov settles task id against.
func quotedRequirementsOf(t *testing.T, prov *Daemon, id string) payment.PaymentRequirements {
	t.Helper()
	req, ok := quotedRequirements(getIX(t, prov, id))
	if !ok {
		t.Fatal("no quoted requirements")
	}
	return req
}

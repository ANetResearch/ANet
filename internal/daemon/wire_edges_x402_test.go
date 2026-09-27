//go:build !no_x402

package daemon

// B3-02 on paid tasks: a short paid call cut off by a stop runs again at
// start (§3.6), the requester's cancel after a payment (0017 Q3), and a
// deny that meets paid work (0017 Q10).

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
)

// gatedWork is a priced long call that starts and waits for its gate.
func gatedWork() *meteredWork {
	return &meteredWork{price: 30, gate: make(chan struct{}), started: make(chan struct{})}
}

// paidAndRunning starts a paid long call from req on prov, paid within the
// automatic tier, and returns once the provider is running it.
func paidAndRunning(t *testing.T, work *meteredWork) (hub string, req, prov *Daemon, id string) {
	t.Helper()
	hub, req, prov = paidPair(t, work)
	req.stopRelayLoop()
	prov.stopRelayLoop()
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	id, err := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req, prov)
	<-work.started
	return hub, req, prov, id
}

// policyChanges counts the anet.policy.changed events on d's chain.
func policyChanges(d *Daemon) int {
	n := 0
	d.ledger.scan(EvPolicyChanged, 0, func(int64, map[string]any) { n++ })
	return n
}

// A short paid call left working with its payment taken — its delegation
// and its payment acknowledged, so nothing will come again — runs again at
// start, and is not charged again.
func TestAPaidShortCallLeftWorkingRunsAgainAtStart(t *testing.T) {
	work := &meteredWork{price: 30}
	hub, req, prov := paidPair(t, work)
	req.stopRelayLoop()
	prov.stopRelayLoop()
	const id = "ix_paid_cut_off"
	leftOpen(t, req, prov, id, "work.do")
	if _, err := prov.ix.SetState(id, interactions.StateWorking); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.SetPayment(id, interactions.PayUpdate{State: interactions.PayState(interactions.PayCompleted),
		AuthIDs: []string{"auth_taken"}}); err != nil {
		t.Fatal(err)
	}
	prov.recoverInterrupted()
	waitUntil(t, "the paid call to run again", func() bool {
		return getIX(t, prov, id).State == interactions.StateCompleted
	})
	if work.invoked.Load() != 1 || debitsOn(hub, req.AID()) != 0 {
		t.Fatalf("ran %d times, %d debits; want once, and no new charge", work.invoked.Load(), debitsOn(hub, req.AID()))
	}
}

// 0017 Q3: a requester's cancel after its payment settled leaves its task
// open, says anet.cancel_requested, and the provider completes and
// delivers; before any payment a cancel ends the task and says nothing of
// the kind.
func TestACancelAfterASettledPaymentLeavesTheTaskOpen(t *testing.T) {
	work := gatedWork()
	_, req, prov, id := paidAndRunning(t, work)
	ctx := context.Background()
	waitUntil(t, "the requester to learn the payment settled", func() bool {
		poll(t, req)
		return getIX(t, req, id).PayState == interactions.PayCompleted
	})
	ix, err := req.CancelTask(ctx, id)
	if err != nil || ix.State != interactions.StateWorking {
		t.Fatalf("cancel after settlement: %v, state %s (want unchanged)", err, ix.State)
	}
	if pm := req.PaymentStatusMeta(ix); pm[x402a2a.KeyCancelRequested] != true ||
		pm[x402a2a.KeyStatus] != x402a2a.StatusCompleted {
		t.Fatalf("status metadata after the cancel: %v", pm)
	}
	poll(t, prov) // the cancel reaches paid work: ignored
	close(work.gate)
	waitUntil(t, "the requester to complete", func() bool {
		poll(t, req)
		return getIX(t, req, id).State == interactions.StateCompleted
	})
	if pm := req.PaymentStatusMeta(getIX(t, req, id)); pm[x402a2a.KeyCancelRequested] != nil {
		t.Errorf("a completed task still says its cancel is pending: %v", pm)
	}

	// Quoted, not paid: the cancel ends the task.
	unpaid := &meteredWork{price: 30}
	_, req2, prov2 := paidPair(t, unpaid)
	id2, err := req2.DelegateCapability(ctx, prov2.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov2, req2)
	if rix := getIX(t, req2, id2); rix.PayState != interactions.PayRequired {
		t.Fatalf("setup: requester pay_state %q", rix.PayState)
	}
	ix2, err := req2.CancelTask(ctx, id2)
	if err != nil || ix2.State != interactions.StateCanceled {
		t.Fatalf("cancel of a quoted task: %v, state %s", err, ix2.State)
	}
	if pm := req2.PaymentStatusMeta(ix2); pm[x402a2a.KeyCancelRequested] != nil {
		t.Errorf("cancel of an unpaid task says cancel_requested: %v", pm)
	}
}

// 0017 Q10, the provider's side: denying a requester whose payment has
// settled leaves the paid task running, says so once in
// anet.policy.changed (skipped_paid) however often the sweep runs, and the
// work is completed and delivered.
func TestADenyLeavesPaidWorkRunningAndSaysSo(t *testing.T) {
	work := gatedWork()
	_, req, prov, id := paidAndRunning(t, work)
	ctx := context.Background()
	res, err := prov.DenyPeer(ctx, req.AID())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Canceled) != 0 || len(res.SkippedPaid) != 1 || res.SkippedPaid[0] != id {
		t.Fatalf("deny: canceled %v, skipped_paid %v; want only %s skipped", res.Canceled, res.SkippedPaid, id)
	}
	ev := lastLedgerPayload(t, prov, EvPolicyChanged)
	if sp, _ := ev["skipped_paid"].([]any); len(sp) != 1 || sp[0] != id {
		t.Fatalf("policy change evidence %v, want skipped_paid [%s]", ev, id)
	}
	before := policyChanges(prov)
	prov.revocationSweep()
	prov.revocationSweep()
	if n := policyChanges(prov); n != before {
		t.Fatalf("the sweep reported the paid task again: %d policy changes, want %d", n, before)
	}
	if st := getIX(t, prov, id).State; st.IsTerminal() {
		t.Fatalf("paid work ended by the deny: %s", st)
	}
	close(work.gate)
	waitUntil(t, "the paid work to be delivered", func() bool {
		poll(t, req)
		return getIX(t, req, id).State == interactions.StateCompleted
	})
	if rix := getIX(t, req, id); len(rix.Receipt) == 0 {
		t.Fatal("delivered without a receipt")
	}
}

// 0017 Q10, the requester's side: a requester that denies the provider it
// has paid still takes that task's status and result, so the paid work
// reaches it; the deny sweep does not cancel it.
func TestAPaidTaskIsDeliveredAfterItsProviderIsDenied(t *testing.T) {
	work := gatedWork()
	_, req, prov, id := paidAndRunning(t, work)
	res, err := req.DenyPeer(context.Background(), prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Canceled) != 0 || len(res.SkippedPaid) != 1 || res.SkippedPaid[0] != id {
		t.Fatalf("deny: canceled %v, skipped_paid %v", res.Canceled, res.SkippedPaid)
	}
	close(work.gate)
	waitUntil(t, "the paid work to reach the requester", func() bool {
		poll(t, req)
		return getIX(t, req, id).State == interactions.StateCompleted
	})
	if counter(req, dropDenied) != 0 {
		t.Fatalf("%d messages of the paid task dropped as denied", counter(req, dropDenied))
	}
}

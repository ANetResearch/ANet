//go:build !no_x402

package daemon

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
)

// drainEvents returns what the bus delivered to one subscriber so far.
func drainEvents(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

// A quote within the auto tier is paid by the node itself (A2A-DESIGN §8.3;
// §8.7: "auto 档内照常自动付款"), so nothing may announce the quote's
// input-required before that payment has been tried. Every client that waits
// on a task wakes on the bus: a blocking /tasks/send, /tasks/wait and MCP
// wait_task (waitTask stops at input-required), and the local A2A interface's
// blocking SendMessage and its streams (pumpTaskEvents re-reads the row on any
// event, a status message included). Woken there, each answered
// input-required, x402.payment.status payment-required, anet.reason
// needs_operator_approval (payment_extension_not_activated on A2A) with a
// status message telling the operator to pay — for a payment already on its
// way. The lab soak found every blocking paid call within auto_max answered
// that way (docs/notes/0036 F1).
func TestAnAutoPaidQuoteDoesNotWakeWaitersAsInputRequired(t *testing.T) {
	work := &meteredWork{price: 120}
	_, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 200, AgentDailyMax: 500, DailyMax: u64(1000)}, prov.AID())
	ctx := context.Background()

	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	_, events, stop, err := req.Watch(id)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	poll(t, prov) // the quote
	poll(t, req)  // lands, and is paid within auto_max
	evs := drainEvents(events)
	sawState := false
	for _, e := range evs {
		if e.Kind == EventState {
			if e.State == interactions.StateInputRequired {
				t.Errorf("an input-required was announced for a quote paid automatically: %+v", evs)
			}
			sawState = true
		}
		if !sawState && e.Kind == EventMessage && e.MsgKind == interactions.MsgStatus {
			// A stream reads the row on this event: it was still input-required.
			t.Errorf("the quote's status message was announced before the automatic payment: %+v", evs)
		}
	}
	if !sawState {
		t.Fatalf("no state was announced: %+v", evs)
	}
	if rix := getIX(t, req, id); rix.State != interactions.StateWorking || rix.PayState != interactions.PaySubmitted {
		t.Fatalf("requester after the quote: %s / %q", rix.State, rix.PayState)
	}
	poll(t, prov)
	poll(t, req, req)
	if rix := getIX(t, req, id); rix.State != interactions.StateCompleted || rix.PayState != interactions.PayCompleted {
		t.Fatalf("requester at the end: %s / %q", rix.State, rix.PayState)
	}
}

// Above the auto tier the quote is the operator's (or an agent's) to decide,
// and waiters must wake at its input-required, told why.
func TestAQuoteAboveTheAutoTierStillWakesWaiters(t *testing.T) {
	work := &meteredWork{price: 120}
	_, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 10, AgentMax: 500, AgentDailyMax: 500, DailyMax: u64(1000)}, prov.AID())
	ctx := context.Background()

	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	after := getIX(t, req, id).StateSeq
	type waited struct {
		ix  *interactions.Interaction
		err error
	}
	done := make(chan waited, 1)
	_, events, stop, err := req.Watch(id)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	poll(t, prov)
	poll(t, req)
	woke := false
	for _, e := range drainEvents(events) {
		woke = woke || (e.Kind == EventState && e.State == interactions.StateInputRequired)
	}
	if !woke {
		t.Fatal("the quote above the auto tier did not announce its input-required")
	}
	go func() {
		ix, _, err := req.waitTask(ctx, id, after, 0)
		done <- waited{ix, err}
	}()
	w := <-done
	if w.err != nil || w.ix.State != interactions.StateInputRequired || PaymentReason(w.ix) != x402a2a.ReasonNeedsOperatorApproval {
		t.Fatalf("a waiter got %v / %v", w.ix, w.err)
	}
	if n := work.invoked.Load(); n != 0 {
		t.Fatalf("the unpaid work ran %d times", n)
	}
}

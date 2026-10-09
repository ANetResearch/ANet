//go:build !no_x402

package daemon

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// CUX-D6: a payment message's messageId is held to its decision as any
// follow-up's is. The same decision again is that message: answered with the
// task, signed once, nothing sent again. Submit and reject under one id are
// two decisions: the second is refused (MESSAGE_ID_REUSED, .followup),
// nothing signed, sent or written — whichever came first.
func TestOnePaymentDecisionPerMessageID(t *testing.T) {
	work := &meteredWork{price: 5}
	_, req, prov := paidPair(t, work)
	ctx := context.Background()
	seam := req.TaskSeam()
	quoted := func() string {
		t.Helper()
		id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
		if err != nil {
			t.Fatal(err)
		}
		poll(t, prov, req)
		if ix := getIX(t, req, id); ix.State != interactions.StateInputRequired || ix.PayState != interactions.PayRequired {
			t.Fatalf("not quoted: %s / %s", ix.State, ix.PayState)
		}
		return id
	}
	decide := func(task, messageID, status string) (a2ashape.Task, error) {
		m := payMsg(task, map[string]any{x402a2a.KeyStatus: status})
		m.ID = messageID
		return seam.Send(ctx, prov.AID(), module.TaskSend{ReturnImmediately: true, Message: m})
	}
	authorized := func() int { return chainEvents(t, req, EvPaymentAuthorized) }

	// Submit, then reject under the same id.
	id := quoted()
	payPolicy(t, req, PaymentsConfig{AgentMax: 10, AgentDailyMax: 10}, prov.AID())
	if _, err := decide(id, "pay-1", x402a2a.StatusSubmitted); err != nil {
		t.Fatal(err)
	}
	n := countMsgs(t, req, id)
	if again, err := decide(id, "pay-1", x402a2a.StatusSubmitted); err != nil || again.ID != id || !deduplicated(again) {
		t.Fatalf("the same submit again: %v (deduplicated %v)", err, deduplicated(again))
	}
	_, err := decide(id, "pay-1", x402a2a.StatusRejected)
	reusedAs(t, err, "followup", id)
	if authorized() != 1 || countMsgs(t, req, id) != n {
		t.Fatalf("after one submit under pay-1: %d authorizations, %d → %d messages", authorized(), n, countMsgs(t, req, id))
	}
	if ix := getIX(t, req, id); ix.PayState == interactions.PayRejected || ix.State == interactions.StateCanceled {
		t.Fatalf("a refused reject moved the task: %s / %s", ix.State, ix.PayState)
	}

	// Reject, then reject again and submit under the same id.
	id2 := quoted()
	first, err := decide(id2, "pay-2", x402a2a.StatusRejected)
	if err != nil || first.Status.State != a2ashape.TaskStateCanceled {
		t.Fatalf("reject: %v %s", err, first.Status.State)
	}
	n2 := countMsgs(t, req, id2)
	if again, err := decide(id2, "pay-2", x402a2a.StatusRejected); err != nil || again.ID != id2 || !deduplicated(again) ||
		again.Status.State != a2ashape.TaskStateCanceled {
		t.Fatalf("the same reject again: %v (deduplicated %v)", err, deduplicated(again))
	}
	_, err = decide(id2, "pay-2", x402a2a.StatusSubmitted)
	reusedAs(t, err, "followup", id2)
	if authorized() != 1 || countMsgs(t, req, id2) != n2 {
		t.Fatalf("after one reject under pay-2: %d authorizations, %d → %d messages", authorized(), n2, countMsgs(t, req, id2))
	}
}

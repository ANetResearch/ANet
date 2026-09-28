//go:build !no_x402

package daemon

// Payment tests added for mutations that survived the suite (docs/notes/0026).

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// §8.4: a replayed settlement is accepted only when its receipt is for the
// authorization this task holds. A facilitator answering "already settled"
// with the receipt of some other payment has not settled this one: the
// provider refuses it and does not run the work. Mutation si9-12 (the
// auth_id comparison dropped) passed every payment test, because no fake
// answered that way; the fake hub's "foreign-receipt" fault does.
func TestAReplayedSettlementOfAnotherPaymentIsRefused(t *testing.T) {
	work := &meteredWork{price: 30}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	ctx := context.Background()
	first, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req, prov, req, req)
	if pix := getIX(t, prov, first); pix.PayState != interactions.PayCompleted || work.invoked.Load() != 1 {
		t.Fatalf("setup: first task pay_state %q, work ran %d times", pix.PayState, work.invoked.Load())
	}
	settleFaultsOn(hub, "foreign-receipt")
	second, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req, prov, req, req)
	if pix := getIX(t, prov, second); pix.PayState == interactions.PayCompleted || work.invoked.Load() != 1 {
		t.Fatalf("the provider took another payment's receipt as this task's: pay_state %q, work ran %d times",
			pix.PayState, work.invoked.Load())
	}
}

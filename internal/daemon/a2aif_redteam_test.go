package daemon

// a2aif_redteam_test.go: adversarial PoCs for the local A2A interface's
// kernel half (TaskSeam, the A2A projection it serves). Each test asserts
// that the attack SUCCEEDS: a passing test means the defect is present.

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// (F32 — the A2A interface inlined every attachment a remote provider sent,
// with no limit, in history, stream events, GetTask and ListTasks — is
// fixed; its regression test is q12_test.go.)

// The final status message of a task that took part in the payment flow is
// where an a2a-x402 client reads the settlement (a2a-x402 §7: receipts in
// status.message.metadata). The projection builds it from the provider's
// own status row, and only adds this node's verified receipts and payment
// status "if the row does not already have them" (a2ashape statusMessage,
// failed/rejected/canceled branch). The provider's metadata is stored
// verbatim (ingestStatus), so a provider that was paid can end the task
// failed with x402.payment.receipts: [] and x402.payment.status:
// payment-failed, and the local client was told nothing was charged while
// this node held the settled receipt. Now the receipts shown are this
// node's (below).
func TestRedteamA2AIF_ProviderStatusDoesNotOverrideVerifiedReceipts(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("do the paid thing", "ctx-pay", "m-pay-1"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the provider has the task", func() bool {
		_ = prov.pollOnce(ctx)
		_, err := prov.ix.Get(task.ID)
		return err == nil
	})
	// This node's record: a settlement it verified and recorded (the state
	// notePaymentReceipts leaves after payment-verified/payment-completed).
	settled := `[{"success":true,"transaction":"tx-real-1","network":"hub:x","payer":"` + req.AID() + `","amount":"500"}]`
	if ok, err := req.ix.SetPayment(task.ID, interactions.PayUpdate{From: []string{interactions.PayNone},
		State: interactions.PayState(interactions.PayCompleted), Receipts: []byte(settled)}); err != nil || !ok {
		t.Fatalf("set payment: %v %v", ok, err)
	}
	// The provider ends the task claiming the payment failed and nothing
	// settled.
	if err := prov.SendStatus(ctx, task.ID, interactions.StateFailed, "payment failed; you were not charged",
		map[string]any{"x402.payment.status": "payment-failed", "x402.payment.receipts": []any{},
			"x402.payment.error": "INSUFFICIENT_FUNDS",
			// Reserved keys only this node writes, passed through as well.
			"anet.receipt_verified": "verified", "anet.official": true}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the requester has the failure", func() bool {
		_ = req.pollOnce(ctx)
		ix, _ := req.ix.Get(task.ID)
		return ix != nil && ix.State == interactions.StateFailed
	})
	got, err := seam.Get(ctx, prov.AID(), task.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Message == nil {
		t.Fatal("no status message")
	}
	// Since wp/fx-c [redteam:F11] the receipts in status.message are this
	// node's record, not the provider's list: the verified settlement is
	// still there. (The red team rejected this finding as F14; the check
	// stays as a regression of the F11 projection.)
	sm := got.Status.Message.Metadata
	rc, _ := sm["x402.payment.receipts"].([]any)
	if len(rc) != 1 {
		t.Fatalf("status message receipts = %v, want this node's verified settlement", sm["x402.payment.receipts"])
	}
	if r, _ := rc[0].(map[string]any); r["transaction"] != "tx-real-1" {
		t.Fatalf("status message receipt = %v, want tx-real-1", rc[0])
	}
	t.Logf("task.metadata: status=%v receipts=%v", got.Metadata["x402.payment.status"], got.Metadata["x402.payment.receipts"])
	t.Logf("status.message.metadata (what an a2a-x402 client reads): status=%v receipts=%v error=%v",
		sm["x402.payment.status"], sm["x402.payment.receipts"], sm["x402.payment.error"])
}

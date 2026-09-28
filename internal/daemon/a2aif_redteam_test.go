package daemon

// a2aif_redteam_test.go: adversarial PoCs for the local A2A interface's
// kernel half (TaskSeam, the A2A projection it serves). Each test asserts
// that the attack SUCCEEDS: a passing test means the defect is present.

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// inlineBytes counts the attachment bytes a projected message carries
// inline (raw parts).
func inlineBytes(msgs ...*a2ashape.Message) int {
	n := 0
	for _, m := range msgs {
		if m == nil {
			continue
		}
		for _, p := range m.Parts {
			if p.Kind == a2ashape.PartRaw {
				n += len(p.Raw)
			}
		}
	}
	return n
}

func historyInline(t a2ashape.Task) int {
	n := 0
	for i := range t.History {
		n += inlineBytes(&t.History[i])
	}
	return n
}

// 0017 Q12 decided: "history 与流式事件只给附件元数据;GetTask/`/tasks/get`
// 内联总上限 8 MiB,超出部分给元数据占位". The A2A interface (scoped seam,
// inline=true) instead carries EVERY attachment of EVERY message inline, in
// the history, in the status message, in ListTasks pages and in stream
// events, with no cap. The attachment bytes are chosen by the remote
// provider: any agent the local client talks to can make each read of the
// task (a blocking SendMessage answer, GetTask, ListTasks, every stream
// event) load, base64 and JSON-round-trip (module/a2a convert) an unbounded
// amount of data in the requester daemon.
func TestRedteamA2AIF_ProviderAttachmentsInlinedWithoutQ12Cap(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("hello", "ctx-q12", "m-q12-1"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the provider has the task", func() bool {
		_ = prov.pollOnce(ctx)
		_, err := prov.ix.Get(task.ID)
		return err == nil
	})

	// The (malicious) provider answers with three 4 MiB files: 12 MiB, over
	// Q12's 8 MiB total, each message well under the relay's envelope limit.
	const each = 4 << 20
	const n = 3
	for i := 0; i < n; i++ {
		att, err := attachmentFromBytes(fmt.Sprintf("blob%d.bin", i), bytes.Repeat([]byte{byte('a' + i)}, each))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prov.sendMessage(ctx, task.ID, fmt.Sprintf("part %d", i), []delegation.Attachment{att}, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitUntil(t, "the requester stored the provider's files", func() bool {
		_ = req.pollOnce(ctx)
		atts, _ := req.ix.Attachments(task.ID)
		return len(atts) == n
	})

	// GetTask through the A2A seam: every byte inline.
	got, err := seam.Get(ctx, prov.AID(), task.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := historyInline(got); h < n*each || h <= 8<<20 {
		t.Fatalf("GetTask history inlines %d bytes; the Q12 cap (8 MiB) holds", h)
	}
	t.Logf("GetTask: history carries %d MiB inline (Q12 cap: 8 MiB, history: metadata only)", historyInline(got)>>20)

	// ListTasks without includeArtifacts: the history of every task on the
	// page, inline as well.
	page, err := seam.List(ctx, prov.AID(), module.TaskFilter{})
	if err != nil || len(page.Tasks) != 1 {
		t.Fatalf("list: %+v %v", page, err)
	}
	if h := historyInline(page.Tasks[0]); h < n*each {
		t.Fatalf("ListTasks inlines %d bytes", h)
	}

	// A stream event (Q12: stream events carry metadata only): the status
	// update for the provider's next message carries its file inline.
	_, events, err := seam.Watch(ctx, prov.AID(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	big, err := attachmentFromBytes("big.bin", bytes.Repeat([]byte{'z'}, 9<<20))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prov.sendMessage(ctx, task.ID, "one more", []delegation.Attachment{big}, nil); err != nil {
		t.Fatal(err)
	}
	go func() { _ = req.pollOnce(ctx) }()
	for ev := range events {
		if ev.StatusUpdate == nil {
			continue
		}
		if b := inlineBytes(ev.StatusUpdate.Status.Message); b >= 9<<20 {
			t.Logf("stream status-update event carries %d MiB inline; base64 in one SSE data line is > 10 MB, "+
				"a2a-go's client scanner limit (internal/sse MaxSSETokenSize)", b>>20)
			return
		}
	}
	t.Fatal("no stream event carried the file inline")
}

// The final status message of a task that took part in the payment flow is
// where an a2a-x402 client reads the settlement (a2a-x402 §7: receipts in
// status.message.metadata). The projection builds it from the provider's
// own status row, and only adds this node's verified receipts and payment
// status "if the row does not already have them" (a2ashape statusMessage,
// failed/rejected/canceled branch). The provider's metadata is stored
// verbatim (ingestStatus), so a provider that was paid can end the task
// failed with x402.payment.receipts: [] and x402.payment.status:
// payment-failed, and the local client is told nothing was charged while
// this node holds the settled receipt.
func TestRedteamA2AIF_ProviderStatusOverridesVerifiedReceipts(t *testing.T) {
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
	sm := got.Status.Message.Metadata
	rc, _ := sm["x402.payment.receipts"].([]any)
	if len(rc) != 0 || sm["x402.payment.status"] != "payment-failed" {
		t.Fatalf("status message carries this node's record (%v, %v): defect absent", sm["x402.payment.receipts"], sm["x402.payment.status"])
	}
	if sm["anet.official"] != true || sm["anet.receipt_verified"] != "verified" {
		t.Fatalf("reserved anet.* keys were filtered: %v", sm)
	}
	t.Logf("task.metadata: status=%v receipts=%v", got.Metadata["x402.payment.status"], got.Metadata["x402.payment.receipts"])
	t.Logf("status.message.metadata (what an a2a-x402 client reads): status=%v receipts=%v error=%v",
		sm["x402.payment.status"], sm["x402.payment.receipts"], sm["x402.payment.error"])
}

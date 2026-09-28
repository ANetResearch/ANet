package daemon

// A provider's claims of settlement on a task this node started are the
// provider's word, and the projection every door returns (control plane,
// MCP, module/a2a) states only what this node verified (A2A-DESIGN §8.2,
// §8.3; SI-6) [redteam:F11].
//
// The red team's PoCs (si6, 765a649) had a provider answer a free call —
// nothing quoted, nothing signed here — with made-up x402.payment.receipts
// in its result metadata, or a "paid" field in its deliverable, and the
// task read "Payment completed." with x402.payment.status=payment-completed
// and the forged transaction, while this node's own check had refused the
// receipt. They are kept below with their inputs, asserting the opposite.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// pvPair is a requester and a provider registered with one fake hub, and a
// free capability call from the first to the second, not yet answered.
func pvPair(t *testing.T) (req, prov *Daemon, id string) {
	t.Helper()
	srv := newFakeHub(t)
	ctx := context.Background()
	req = newTestDaemon(t, srv.URL, false)
	prov = newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil, ""); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "text.free", map[string]any{"q": 1})
	if err != nil {
		t.Fatal(err)
	}
	return req, prov, id
}

// pvResult is an anet.result/1 the provider signs and seals: a receipt
// that honestly covers deliverable, and meta as the result metadata.
func pvResult(t *testing.T, req, prov *Daemon, id string, deliverable []byte, meta map[string]any) []byte {
	t.Helper()
	ix, err := req.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := anetcid.Sum(deliverable)
	if err != nil {
		t.Fatal(err)
	}
	rc := &evidence.Receipt{InteractionID: id, RequesterAID: req.AID(), ProviderAID: prov.AID(),
		RequestCID: ix.RequestCID, ResultCID: cid, CompletedAt: uint64(nowMillis())}
	if err := rc.Sign(prov.self); err != nil {
		t.Fatal(err)
	}
	receipt, err := rc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	kel, err := identity.MarshalKEL(prov.self.KEL())
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := json.Marshal(meta)
	payload := mustMarshal(t, &delegation.ResultResp{Status: delegation.StatusDone, Deliverable: deliverable,
		Receipt: receipt, KEL: kel, Metadata: mb})
	return sealFrom(t, prov, req, seal.TypeResult, id, payload)
}

// pvView is the projection every door returns, as JSON-decoded maps.
func pvView(t *testing.T, d *Daemon, id string) map[string]any {
	t.Helper()
	ix, err := d.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	task, err := d.taskView(ix, viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func pvPath(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[k]
	}
	return v
}

// pvNothingPaid checks this node never signed a payment for the task.
func pvNothingPaid(t *testing.T, d *Daemon, id string) {
	t.Helper()
	d.ledger.scan(EvPaymentAuthorized, 0, func(_ int64, p map[string]any) {
		if p["interaction_id"] == id {
			t.Fatal("precondition: this node signed a payment for the task")
		}
	})
	if ix, _ := d.ix.Get(id); ix == nil || ix.PayState != interactions.PayNone {
		t.Fatal("precondition: the task must have no payment state")
	}
}

// pvNoCompletedPayment fails when any part of the view says this node's
// payment completed, or states a receipt carrying marker as a settlement.
func pvNoCompletedPayment(t *testing.T, v map[string]any, marker string) {
	t.Helper()
	for _, where := range [][]string{{"metadata"}, {"status", "message", "metadata"}} {
		md, _ := pvPath(v, where...).(map[string]any)
		if md[a2ashape.KeyX402Status] == a2ashape.PaymentCompleted {
			t.Errorf("%v says payment-completed: %v", where, md)
		}
		rc, _ := json.Marshal(md[a2ashape.KeyX402Receipts])
		if strings.Contains(string(rc), marker) || strings.Contains(string(rc), `"success":true`) {
			t.Errorf("%v states the provider's receipts as settled: %s", where, rc)
		}
	}
	text, _ := json.Marshal(pvPath(v, "status", "message", "parts"))
	if strings.Contains(string(text), "Payment completed.") {
		t.Errorf("status.message says %s", text)
	}
}

func TestRedteamSI6PeerReceiptsAreNotProjectedAsPaymentCompleted(t *testing.T) {
	req, prov, id := pvPair(t)
	deliverable := []byte(`{"capability":"text.free","status":"OK","verifiable":false}`)
	forged := []any{map[string]any{"success": true, "transaction": "tx-forged-by-provider",
		"network": "hub:" + prov.AID(), "amount": "1000", "payer": req.AID(),
		// A verdict the provider writes for itself is overwritten.
		"extensions": map[string]any{"anet.settlement_verified": "verified"}}}
	env := pvResult(t, req, prov, id, deliverable, map[string]any{
		"anet.state": "completed", "anet.effect_status": "OK",
		"x402.payment.status": "payment-completed", "x402.payment.receipts": forged,
	})
	if r := receive(t, req, env); r.class != rxAccepted {
		t.Fatalf("result not accepted: %+v", r)
	}
	pvNothingPaid(t, req, id)
	if ev := lastLedgerPayload(t, req, EvPaymentSettled); ev["verified"] != false {
		t.Fatalf("precondition: the node's own check must refuse the receipt, got %v", ev)
	}

	v := pvView(t, req, id)
	pvNoCompletedPayment(t, v, "tx-forged-by-provider")
	// Shown, and apart: what the provider claimed and this node could not
	// verify.
	un, _ := json.Marshal(pvPath(v, "metadata", a2ashape.KeyUnverifiedReceipts))
	if !strings.Contains(string(un), "tx-forged-by-provider") || !strings.Contains(string(un), `"unverified"`) {
		t.Errorf("anet.unverified_receipts = %s, want the forged claim marked unverified", un)
	}
	if ix, _ := req.ix.Get(id); ix.PayState != interactions.PayNone {
		t.Errorf("pay_state = %q after an unverified receipt", ix.PayState)
	}
}

// The same with nothing but the deliverable's "paid" field.
func TestRedteamSI6PeerPaidFieldIsNotProjectedAsPaymentCompleted(t *testing.T) {
	req, prov, id := pvPair(t)
	deliverable := []byte(`{"capability":"text.free","status":"OK","verifiable":false,` +
		`"paid":{"transaction":"tx-forged-in-deliverable","amount":"1000","network":"hub:x"}}`)
	env := pvResult(t, req, prov, id, deliverable, map[string]any{"anet.state": "completed", "anet.effect_status": "OK"})
	if r := receive(t, req, env); r.class != rxAccepted {
		t.Fatalf("result not accepted: %+v", r)
	}
	pvNothingPaid(t, req, id)
	v := pvView(t, req, id)
	pvNoCompletedPayment(t, v, "tx-forged-in-deliverable")
	if _, ok := pvPath(v, "metadata").(map[string]any)[a2ashape.KeyX402Status]; ok {
		t.Errorf("a task nothing was quoted for has x402.payment.status: %v", pvPath(v, "metadata"))
	}
	// The deliverable itself is still what the provider delivered.
	art, _ := json.Marshal(pvPath(v, "artifacts"))
	if !strings.Contains(string(art), "tx-forged-in-deliverable") {
		t.Errorf("the deliverable was altered: %s", art)
	}
}

// A provider status that ends the task and claims the payment completed,
// with its own receipts: status.message is the provider's row, and its
// settlement keys are this node's.
func TestAProviderStatusDoesNotStateThisNodesPayment(t *testing.T) {
	req, prov, id := pvPair(t)
	mb, _ := json.Marshal(map[string]any{
		"x402.payment.status": "payment-completed",
		"x402.payment.receipts": []any{map[string]any{"success": true, "transaction": "tx-forged-in-status",
			"network": "hub:x", "amount": "1000"}},
	})
	body := mustMarshal(t, &delegation.StatusMsg{State: delegation.StateFailed, Text: "paid and failed",
		Metadata: mb, At: uint64(time.Now().UnixMilli())})
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeStatus, id, body)); r.class != rxAccepted {
		t.Fatalf("status not accepted: %+v", r)
	}
	v := pvView(t, req, id)
	pvNoCompletedPayment(t, v, "tx-forged-in-status")
	text, _ := json.Marshal(pvPath(v, "status", "message", "parts"))
	if !strings.Contains(string(text), "paid and failed") {
		t.Errorf("status.message is not the provider's row: %s", text)
	}
}

// A provider that says it verified a payment on a call this node never
// paid: status.message carried x402.payment.status=payment-verified, the
// merchant's word that this node's payment checked out. With no payment
// of this node's out, the status is this node's (none) [redteam:F11].
func TestAProviderCannotSayItVerifiedAPaymentThisNodeNeverMade(t *testing.T) {
	req, prov, id := pvPair(t)
	mb, _ := json.Marshal(map[string]any{"x402.payment.status": "payment-verified"})
	body := mustMarshal(t, &delegation.StatusMsg{State: delegation.StateWorking, Text: "your payment checked out",
		Metadata: mb, At: uint64(time.Now().UnixMilli())})
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeStatus, id, body)); r.class != rxAccepted {
		t.Fatalf("status not accepted: %+v", r)
	}
	pvNothingPaid(t, req, id)
	v := pvView(t, req, id)
	for _, where := range [][]string{{"metadata"}, {"status", "message", "metadata"}} {
		md, _ := pvPath(v, where...).(map[string]any)
		if st, ok := md[a2ashape.KeyX402Status]; ok {
			t.Errorf("%v x402.payment.status = %v on a call this node never paid", where, st)
		}
	}
	text, _ := json.Marshal(pvPath(v, "status", "message", "parts"))
	if !strings.Contains(string(text), "your payment checked out") {
		t.Errorf("status.message is not the provider's row: %s", text)
	}
}

// anet.settlement_verified is this node's key. A provider that writes
// "verified" into it on a failure, or on an entry whose success is the
// string "true", does not have it stored or shown [redteam:F11].
func TestAProviderCannotWriteThisNodesVerdict(t *testing.T) {
	req, prov, id := pvPair(t)
	mb, _ := json.Marshal(map[string]any{"x402.payment.receipts": []any{
		map[string]any{"success": false, "errorReason": "insufficient_funds",
			"extensions": map[string]any{"anet.settlement_verified": "verified"}},
		map[string]any{"success": "true", "transaction": "tx-string-success",
			"extensions": map[string]any{"anet.settlement_verified": "verified"}},
	}})
	body := mustMarshal(t, &delegation.StatusMsg{State: delegation.StateWorking, Metadata: mb,
		At: uint64(time.Now().UnixMilli())})
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeStatus, id, body)); r.class != rxAccepted {
		t.Fatalf("status not accepted: %+v", r)
	}
	ix, _ := req.ix.Get(id)
	if strings.Contains(string(ix.PayReceipts), `"verified"`) {
		t.Errorf("a verdict of the provider's writing is stored: %s", ix.PayReceipts)
	}
	all, _ := json.Marshal(pvView(t, req, id))
	if strings.Contains(string(all), `"anet.settlement_verified":"verified"`) {
		t.Errorf("the view shows a verdict of the provider's writing: %s", all)
	}
}

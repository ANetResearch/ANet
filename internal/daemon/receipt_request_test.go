package daemon

// Red-team finding F15: the requester's receipt check bound the
// interaction, the parties and the result CID, and not the request CID. A
// provider could deliver a result under a receipt for a request never made
// (the CID of any bytes it likes); it was stored as verified, and a review
// this node then signed was anchored to it. The requester holds the CID of
// the request it sent, and a receipt naming another is refused like any
// receipt whose bindings do not hold.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// receiptFor seals an anet.result/1 from prov: deliverable under a receipt
// prov signs, which names requestCID as the request it answers.
func receiptFor(t *testing.T, req, prov *Daemon, id string, deliverable []byte, requestCID string) []byte {
	t.Helper()
	cid, err := anetcid.Sum(deliverable)
	if err != nil {
		t.Fatal(err)
	}
	rc := &evidence.Receipt{InteractionID: id, RequesterAID: req.AID(), ProviderAID: prov.AID(),
		RequestCID: requestCID, ResultCID: cid, CompletedAt: uint64(nowMillis())}
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
	meta, _ := json.Marshal(map[string]any{"anet.state": "completed"})
	payload := mustMarshal(t, &delegation.ResultResp{Status: delegation.StatusDone, Deliverable: deliverable,
		Receipt: receipt, KEL: kel, Metadata: meta})
	return sealFrom(t, prov, req, seal.TypeResult, id, payload)
}

func TestAReceiptForAnotherRequestIsRefused(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
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
	sent := mustIX(t, req, id).RequestCID
	if sent == "" {
		t.Fatal("precondition: the requester keeps the CID of the request it sent")
	}
	madeUp, err := anetcid.Sum([]byte("a request this node never made"))
	if err != nil {
		t.Fatal(err)
	}
	deliverable := []byte(`{"capability":"text.free","status":"OK","verifiable":false}`)

	// A receipt for a request this node never sent: refused, nothing
	// stored, nothing to review.
	if r := receive(t, req, receiptFor(t, req, prov, id, deliverable, madeUp)); r.class == rxAccepted ||
		r.reason != dropResultRefused {
		t.Fatalf("a receipt for another request: %+v, want %s", r, dropResultRefused)
	}
	ix := mustIX(t, req, id)
	if len(ix.Receipt) > 0 || ix.IsTerminal() {
		t.Fatalf("the refused result was stored: state %s, receipt %d bytes", ix.State, len(ix.Receipt))
	}
	if _, err := req.SubmitReview(id, 5, "great"); err == nil {
		t.Fatal("a review was signed without a receipt that holds")
	}

	// The same provider's receipt for the request that was sent verifies.
	if r := receive(t, req, receiptFor(t, req, prov, id, deliverable, sent)); r.class != rxAccepted {
		t.Fatalf("the genuine result: %+v", r)
	}
	ix = mustIX(t, req, id)
	if ix.ReceiptVerified != interactions.VerificationVerified {
		t.Fatalf("receipt_verified %q", ix.ReceiptVerified)
	}
	task, err := req.taskView(ix, viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := task.Metadata[a2ashape.KeyReceipt].(map[string]any)
	if task.Metadata[a2ashape.KeyReceiptVerified] != a2ashape.ReceiptVerified || rc["request_cid"] != sent ||
		task.Metadata[a2ashape.KeyRequestCID] != sent {
		t.Fatalf("verified %v, the receipt's request %v, this node's %v", task.Metadata[a2ashape.KeyReceiptVerified],
			rc["request_cid"], task.Metadata[a2ashape.KeyRequestCID])
	}
}

// The review is the one thing this node signs about a receipt, so it holds
// the receipt to the task too, whatever stored it: a row from before the
// request binding, with a provider's receipt for a request never made and
// marked verified then, is not reviewed (red-team F15, on review: the
// check at ingest does not reach rows already stored).
func TestAReviewIsNotAnchoredToAReceiptForAnotherRequest(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil, ""); err != nil {
		t.Fatal(err)
	}
	deliverable := []byte(`{"capability":"text.free","status":"OK","verifiable":false}`)
	resultCID, err := anetcid.Sum(deliverable)
	if err != nil {
		t.Fatal(err)
	}
	// store writes a result on a fresh task as a node before the fix would
	// have: under a receipt naming requestCID, marked verified.
	store := func(requestCID func(sent string) string) string {
		id, err := req.DelegateCapability(ctx, prov.AID(), "text.free", map[string]any{"q": 1})
		if err != nil {
			t.Fatal(err)
		}
		rc := &evidence.Receipt{InteractionID: id, RequesterAID: req.AID(), ProviderAID: prov.AID(),
			RequestCID: requestCID(mustIX(t, req, id).RequestCID), ResultCID: resultCID, CompletedAt: uint64(nowMillis())}
		if err := rc.Sign(prov.self); err != nil {
			t.Fatal(err)
		}
		receipt, err := rc.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := req.ix.Finish(id, interactions.Finish{State: interactions.StateCompleted, Result: deliverable,
			ResultCID: resultCID, Receipt: receipt, Verified: interactions.VerificationVerified}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	madeUp, err := anetcid.Sum([]byte("a request this node never made"))
	if err != nil {
		t.Fatal(err)
	}
	forged := store(func(string) string { return madeUp })
	if _, err := req.SubmitReview(forged, 5, "great"); err == nil {
		t.Fatal("a review was signed and anchored to a receipt for a request this node never made")
	}
	if ix := mustIX(t, req, forged); len(ix.Review) > 0 {
		t.Fatal("the review was stored")
	}
	genuine := store(func(sent string) string { return sent })
	if _, err := req.SubmitReview(genuine, 5, "great"); err != nil {
		t.Fatalf("the genuine receipt: %v", err)
	}
}

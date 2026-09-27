package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
)

// storeQuote plants a PAYMENT_REQUIRED answer for an interaction with a
// stated verification verdict, the way the inbound result path would.
func storeQuote(t *testing.T, d *Daemon, id string, seen interactions.Verification, amount string) {
	t.Helper()
	if err := d.ix.Put(id, interactions.RoleOutbound, d.AID(), "goal", "req_cid", []byte("request")); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(capabilityResult{
		Status: string(effect.PaymentRequired),
		Payment: &payment.PaymentRequired{
			Accepts: []payment.PaymentOption{{
				Scheme: payment.SchemeCredit, Network: "credit:hub", Amount: amount,
				PayTo: d.AID(), Asset: "credit",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rc := &evidence.Receipt{
		InteractionID: id, RequesterAID: d.AID(), ProviderAID: d.AID(),
		RequestCID: "req_cid", ResultCID: "res_cid", CompletedAt: 1,
	}
	if err := rc.Sign(d.self); err != nil {
		t.Fatal(err)
	}
	rcBytes, err := rc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ix.SetResult(id, body, "res_cid", rcBytes, seen); err != nil {
		t.Fatal(err)
	}
}

// A quote is acted on only when the task's provider stated it.
//
// It used to arrive as a result, and a result with no key history was
// accepted UNVERIFIED before any of its bindings were checked — so
// anything able to write to this node's mailbox with a known interaction
// id could state a price and be paid it. A quote is now a status on the
// task (A2A-DESIGN §8.3): it is stored only from an envelope the task's
// provider signed (SI-4), and a result that says PAYMENT_REQUIRED is not a
// quote at all.
func TestAnUnverifiedQuoteIsNotPaidAutomatically(t *testing.T) {
	srv := newFakeHub(t)
	d := newTestDaemon(t, srv.URL, false)

	// The old shape, planted as a result in every verification state: not
	// a price to pay.
	for _, seen := range []interactions.Verification{interactions.VerificationUnverified,
		interactions.VerificationUnknown, interactions.VerificationVerified} {
		id := "ix_result_" + string(seen)
		storeQuote(t, d, id, seen, "999999")
		q, err := d.awaitQuote(context.Background(), id)
		if err != nil || q != nil {
			t.Errorf("a PAYMENT_REQUIRED result (%q) was taken as a quote: %+v %v", seen, q, err)
		}
	}

	// A status from someone other than the task's provider is not taken.
	_, req, prov := registeredPair(t)
	stranger := registered(t, srv.URL, "stranger")
	id, err := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{x402a2a.KeyStatus: x402a2a.StatusRequired,
		x402a2a.KeyRequired: payment.PaymentRequired{X402Version: 2, Accepts: []payment.PaymentOption{{
			Scheme: payment.SchemeCredit, Network: "hub:x", Amount: "999999", PayTo: stranger.AID()}}}})
	body, _ := (&delegation.StatusMsg{State: delegation.StateInputRequired, Metadata: meta, At: 1}).Marshal()
	if r := receive(t, req, sealFrom(t, stranger, req, seal.TypeStatus, id, body)); r.class != rxDropped {
		t.Fatalf("a stranger's quote: %+v", r)
	}
	if ix, _ := req.ix.Get(id); ix.PayState != interactions.PayNone || len(ix.PayRequired) != 0 {
		t.Fatalf("a stranger's quote was stored: %q", ix.PayState)
	}

	// And the honest case still works: a quote stored on the task.
	raw, _ := json.Marshal(payment.PaymentRequired{X402Version: 2, Accepts: []payment.PaymentOption{{
		Scheme: payment.SchemeCredit, Network: "hub:x", Amount: "25", PayTo: prov.AID()}}})
	if _, err := req.ix.SetPayment(id, interactions.PayUpdate{State: interactions.PayState(interactions.PayRequired),
		Required: raw}); err != nil {
		t.Fatal(err)
	}
	q, err := req.awaitQuote(context.Background(), id)
	if err != nil {
		t.Fatalf("a stored quote was refused: %v", err)
	}
	if q == nil || len(q.Accepts) != 1 || q.Accepts[0].Amount != "25" {
		t.Fatalf("wrong quote returned: %+v", q)
	}
}

// The verdict has to be per result. ProviderKEL is a per-peer cache, so
// one verified interaction with a provider would otherwise make every
// later unverified result from it look checked.
func TestTheVerificationVerdictIsRecordedPerResult(t *testing.T) {
	srv := newFakeHub(t)
	d := newTestDaemon(t, srv.URL, false)

	storeQuote(t, d, "ix_a", interactions.VerificationVerified, "10")
	storeQuote(t, d, "ix_b", interactions.VerificationUnverified, "10")

	results, err := d.Results(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range results {
		got[r.InteractionID] = r.ReceiptVerified
	}
	if got["ix_a"] != string(interactions.VerificationVerified) {
		t.Errorf("ix_a reads %q, want verified", got["ix_a"])
	}
	if got["ix_b"] != string(interactions.VerificationUnverified) {
		t.Errorf("ix_b reads %q, want unverified", got["ix_b"])
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
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

// A quote is not work that was done. Accepting a completion this node
// could not verify is deliberate — the work happened, and dropping it
// would lose a real result. A quote is the opposite: nothing was
// delivered, and acting on it spends credit.
//
// The reachable path: delegation.VerifyResult returns ErrUnverifiable as
// soon as a result carries no key history, and does so BEFORE it checks
// the receipt signature, the provider binding, the interaction binding
// or the deliverable hash — so an unverified result has had none of
// those checked. /relay/send is unauthenticated by design, so anything
// that can write to this node's mailbox with a known interaction id can
// state a price. Before this, DelegateAndPay paid it.
func TestAnUnverifiedQuoteIsNotPaidAutomatically(t *testing.T) {
	srv := newFakeHub(t)
	d := newTestDaemon(t, srv.URL, false)

	storeQuote(t, d, "ix_injected", interactions.VerificationUnverified, "999999")
	_, err := d.awaitQuote(context.Background(), "ix_injected")
	if err == nil {
		t.Fatal("an unverified quote was accepted for automatic payment")
	}
	if !strings.Contains(err.Error(), "could not verify") {
		t.Errorf("refusal does not say why: %v", err)
	}

	// A row written before this node recorded the distinction is unknown,
	// not verified. Treating "we never wrote it down" as a pass would
	// leave every pre-upgrade interaction payable.
	storeQuote(t, d, "ix_legacy", interactions.VerificationUnknown, "10")
	if _, err := d.awaitQuote(context.Background(), "ix_legacy"); err == nil {
		t.Error("a quote with no recorded verdict was accepted for automatic payment")
	}

	// And the honest case still works, or the check would be a denial of
	// the whole paid path rather than a check.
	storeQuote(t, d, "ix_good", interactions.VerificationVerified, "25")
	q, err := d.awaitQuote(context.Background(), "ix_good")
	if err != nil {
		t.Fatalf("a verified quote was refused: %v", err)
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

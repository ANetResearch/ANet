package evidence_test

import (
	"testing"

	"github.com/ANetResearch/ANet/internal/protocol/evidence"
	"github.com/ANetResearch/ANet/internal/protocol/identity"
)

func TestReceiptSignVerifyRoundTrip(t *testing.T) {
	prov, _ := identity.Incept()
	req, _ := identity.Incept()
	rc := &evidence.Receipt{
		InteractionID: "ix_1", RequesterAID: req.AID(),
		RequestCID: "cid_req", ResultCID: "cid_res", CompletedAt: 1000,
	}
	if err := rc.Sign(prov); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if rc.ProviderAID != prov.AID() {
		t.Fatalf("Sign did not stamp provider AID")
	}
	if err := rc.Verify(prov.KEL(), 1000); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Round-trip through the wire form preserves the CID (sign binds CID).
	want, _ := rc.CID()
	b, err := rc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := evidence.UnmarshalReceipt(b)
	if err != nil {
		t.Fatal(err)
	}
	gotCID, _ := got.CID()
	if gotCID != want {
		t.Fatalf("CID changed across marshal: %s != %s", gotCID, want)
	}
	if err := got.Verify(prov.KEL(), 1000); err != nil {
		t.Fatalf("verify after round-trip: %v", err)
	}
}

func TestReceiptTamperRejected(t *testing.T) {
	prov, _ := identity.Incept()
	req, _ := identity.Incept()
	rc := &evidence.Receipt{InteractionID: "ix_1", RequesterAID: req.AID(), ResultCID: "cid_res", CompletedAt: 1000}
	_ = rc.Sign(prov)
	rc.ResultCID = "tampered" // mutate a signed field after signing
	if err := rc.Verify(prov.KEL(), 1000); err == nil {
		t.Fatal("tampered receipt must fail verification")
	}
}

func TestReceiptWrongSignerAIDRejected(t *testing.T) {
	prov, _ := identity.Incept()
	other, _ := identity.Incept()
	rc := &evidence.Receipt{InteractionID: "ix_1", RequesterAID: "did:x", ResultCID: "r", CompletedAt: 1}
	_ = rc.Sign(prov)
	// A verifier that presents the WRONG (other) KEL must reject: KEL AID != signer AID.
	if err := rc.Verify(other.KEL(), 1); err == nil {
		t.Fatal("receipt verified under the wrong KEL")
	}
}

func TestReviewSignVerifyAndRating(t *testing.T) {
	prov, _ := identity.Incept()
	req, _ := identity.Incept()
	rv := &evidence.Review{
		InteractionID: "ix_1", SubjectAID: prov.AID(),
		Rating: 5, Comment: "great", ReceiptCID: "cid_receipt", CreatedAt: 2000,
	}
	if err := rv.Sign(req); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if rv.ReviewerAID != req.AID() || !rv.ValidRating() {
		t.Fatalf("bad review state: %+v", rv)
	}
	if err := rv.Verify(req.KEL(), 2000); err != nil {
		t.Fatalf("verify: %v", err)
	}
	b, _ := rv.Marshal()
	got, err := evidence.UnmarshalReview(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rating != 5 || got.Comment != "great" || got.SubjectAID != prov.AID() {
		t.Fatalf("round-trip lost fields: %+v", got)
	}
	if err := got.Verify(req.KEL(), 2000); err != nil {
		t.Fatalf("verify after round-trip: %v", err)
	}
}

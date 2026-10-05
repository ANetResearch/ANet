package org_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/module/org"
)

// The golden vector MODOBJ-1, organization part (core §5.2), is written by ANetCore's
// golden/py/gen_v016.py, an implementation that shares nothing with this package; golden-xcheck
// regenerates it byte for byte. This test reproduces every value from the same inputs with the
// code under test, and checks that each negative is refused for the reason the file names.

type hexBytes []byte

func (h *hexBytes) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	out, err := hex.DecodeString(s)
	*h = out
	return err
}

type vecEnvelope struct {
	SignerAID   string `json:"signer_aid"`
	KeyStateSeq uint64 `json:"key_state_seq"`
	Alg         int    `json:"alg"`
}

type vecNegative struct {
	Name    string   `json:"name"`
	Note    string   `json:"note"`
	Wire    hexBytes `json:"wire"`
	Refused string   `json:"refused"`
}

type vecElsewhere struct {
	Name              string   `json:"name"`
	Note              string   `json:"note"`
	Preimage          hexBytes `json:"preimage"`
	TS                uint64   `json:"ts"`
	Sig               hexBytes `json:"sig"`
	ReceiptWire       hexBytes `json:"receipt_wire"`
	AuthorizationWire hexBytes `json:"authorization_wire"`
	VerifyAt          uint64   `json:"verify_at"`
}

type orgVector struct {
	ID          string            `json:"id"`
	Description string            `json:"description"`
	Labels      map[string]string `json:"labels"`
	Issuer      struct {
		AID string   `json:"aid"`
		KEL hexBytes `json:"kel"`
	} `json:"issuer"`
	Genesis struct {
		AsGiven []string `json:"governance_root_as_given"`
		M       uint64   `json:"m"`
		Nonce   string   `json:"nonce"`
		Bytes   hexBytes `json:"bytes"`
		OrgID   string   `json:"org_id"`
	} `json:"genesis"`
	Credential struct {
		OrgID    string      `json:"org_id"`
		Subject  string      `json:"subject"`
		Role     string      `json:"role"`
		IssuedAt int64       `json:"issued_at"`
		NotAfter int64       `json:"not_after"`
		Body     hexBytes    `json:"body"`
		BodyCID  string      `json:"body_cid"`
		Sig      hexBytes    `json:"sig"`
		Envelope vecEnvelope `json:"envelope"`
		Wire     hexBytes    `json:"wire"`
		VerifyAt int64       `json:"verify_at"`
	} `json:"credential"`
	Negatives []vecNegative  `json:"negatives"`
	Elsewhere []vecElsewhere `json:"signature_elsewhere"`
}

func loadOrgVector(t testing.TB) *orgVector {
	t.Helper()
	b, err := os.ReadFile("testdata/vec-org-1.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var v orgVector
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("vec-org-1.json: %v", err)
	}
	return &v
}

// wantSignatureFails asserts that err is a signature that does not verify, and nothing else: a
// refusal for another reason (a window, a key state) would say nothing about SI-81.
func wantSignatureFails(t *testing.T, err error) {
	t.Helper()
	var ve *identity.VErr
	if !errors.As(err, &ve) || ve.Reason != "INVALID_SIGNATURE" {
		t.Fatalf("want INVALID_SIGNATURE, got %v", err)
	}
}

func TestMODOBJ_1_Org(t *testing.T) {
	v := loadOrgVector(t)
	if v.ID != "MODOBJ-1" || v.Labels["genesis"] != org.GenesisType || v.Labels["credential"] != org.CredentialType {
		t.Fatalf("labels %v", v.Labels)
	}
	suite := identity.SuiteController()
	kel, err := identity.UnmarshalKEL(v.Issuer.KEL)
	if err != nil {
		t.Fatal(err)
	}
	if mine, _ := identity.MarshalKEL(suite.KEL()); !bytes.Equal(mine, v.Issuer.KEL) || v.Issuer.AID != suite.AID() {
		t.Fatal("the issuer is the conformance identity")
	}
	resolve := func(aid string) ([]identity.SignedEvent, bool) {
		if aid == suite.AID() {
			return kel, true
		}
		return nil, false
	}

	// L41: the genesis, written from the founders as given (unsorted, one twice).
	g := &org.Genesis{GovernanceRoot: v.Genesis.AsGiven, M: v.Genesis.M, Nonce: v.Genesis.Nonce}
	if b, err := g.Marshal(); err != nil || !bytes.Equal(b, v.Genesis.Bytes) {
		t.Fatalf("genesis\n got  %x\n want %x (%v)", b, []byte(v.Genesis.Bytes), err)
	}
	if id, err := g.OrgID(); err != nil || id != v.Genesis.OrgID {
		t.Fatalf("org_id %s, want %s (%v)", id, v.Genesis.OrgID, err)
	}
	pg, err := org.ParseGenesis(v.Genesis.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := pg.OrgID(); id != v.Genesis.OrgID || len(pg.GovernanceRoot) != 2 {
		t.Fatalf("parsed genesis %+v", pg)
	}

	// L42: the credential, signed with the real code path.
	cv := v.Credential
	if cv.OrgID != v.Genesis.OrgID {
		t.Fatal("the credential is not for the vector's organization")
	}
	c, err := g.Issue(suite, cv.Subject, cv.Role, cv.IssuedAt, cv.NotAfter)
	if err != nil {
		t.Fatal(err)
	}
	if pre, err := c.CanonicalPreimage(); err != nil || !bytes.Equal(pre, cv.Body) {
		t.Fatalf("credential body\n got  %x\n want %x (%v)", pre, []byte(cv.Body), err)
	}
	e := c.Envelope
	if !bytes.Equal(e.Sig, cv.Sig) || e.SignerAID != cv.Envelope.SignerAID || e.KeyStateSeq != cv.Envelope.KeyStateSeq || e.Alg != cv.Envelope.Alg {
		t.Fatalf("envelope %+v", e)
	}
	if raw, err := org.MarshalCredential(c); err != nil || !bytes.Equal(raw, cv.Wire) {
		t.Fatalf("credential wire\n got  %x\n want %x (%v)", raw, []byte(cv.Wire), err)
	}
	if cid, err := c.CID(); err != nil || cid != cv.BodyCID {
		t.Fatalf("credential CID %s, want %s (%v)", cid, cv.BodyCID, err)
	}
	back, err := org.UnmarshalCredential(cv.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := org.VerifyMembership(back, g, cv.VerifyAt, resolve, nil, nil); err != nil {
		t.Fatalf("the vector's credential must verify: %v", err)
	}

	if len(v.Negatives) == 0 || len(v.Elsewhere) != 3 {
		t.Fatal("negatives")
	}
	for _, n := range v.Negatives {
		t.Run(n.Name, func(t *testing.T) {
			got, err := org.UnmarshalCredential(n.Wire)
			if err == nil {
				err = org.VerifyMembership(got, g, cv.VerifyAt, resolve, nil, nil)
			}
			switch n.Refused {
			case "decode":
				if !errors.Is(err, org.ErrMalformed) {
					t.Fatalf("%s: want ErrMalformed, got %v", n.Note, err)
				}
				foreignVerifies(t, n.Name, n.Wire, kel)
			case "signature":
				wantSignatureFails(t, err)
			case "epoch":
				if !errors.Is(err, org.ErrEpoch) {
					t.Fatalf("%s: want ErrEpoch, got %v", n.Note, err)
				}
			default:
				t.Fatalf("unknown refusal %q", n.Refused)
			}
		})
	}
	for _, x := range v.Elsewhere {
		t.Run(x.Name, func(t *testing.T) { signatureElsewhereFails(t, x, kel, cv.Sig) })
	}
}

// foreignVerifies: a negative that presents a valid signed object of another kind as a credential
// is refused at decode although its signature is good. For the three foreign kinds the test checks
// the signature over the carried bytes, so the refusal is the type tag's and not an accident of
// the bytes.
func foreignVerifies(t *testing.T, name string, wire []byte, kel []identity.SignedEvent) {
	t.Helper()
	for _, kind := range []string{"inner_preimage_", "receipt_", "authorization_", "unit_as_"} {
		if !strings.HasPrefix(name, kind) {
			continue
		}
		var w struct {
			Body []byte        `cbor:"1,keyasint"`
			Env  aobj.Envelope `cbor:"2,keyasint"`
		}
		if err := coredet.Unmarshal(wire, &w); err != nil {
			t.Fatal(err)
		}
		if err := identity.VerifyObject(kel, w.Env.SignerAID, w.Env.KeyStateSeq, 0, w.Body, w.Env.Sig); err != nil {
			t.Fatalf("the %s's own signature does not verify: %v", strings.TrimSuffix(kind, "_"), err)
		}
	}
}

// signatureElsewhereFails: the credential's (or the unit's) signature, carried by another kind of
// object signed by the same key, fails that object's verifier on the signature.
func signatureElsewhereFails(t *testing.T, x vecElsewhere, kel []identity.SignedEvent, sig []byte) {
	t.Helper()
	switch {
	case len(x.Preimage) > 0:
		if !bytes.Equal(x.Sig, sig) {
			t.Fatal("the row does not carry the object's signature")
		}
		in := &seal.SealedInner{From: identity.SuiteController().AID(), KeyStateSeq: 0, TS: x.TS, Sig: x.Sig}
		if err := seal.VerifyInnerSig(in, x.Preimage, kel, x.TS); seal.ReasonOf(err) != seal.ReasonBadSig {
			t.Fatalf("as an inner signature: want %s, got %v", seal.ReasonBadSig, err)
		}
	case len(x.ReceiptWire) > 0:
		w, err := evidence.UnmarshalReceiptWire(x.ReceiptWire)
		if err != nil {
			t.Fatalf("the receipt decodes; it is its signature that must fail: %v", err)
		}
		if !bytes.Equal(w.Env.Sig, sig) {
			t.Fatal("the receipt does not carry the object's signature")
		}
		if _, err := w.Verify(kel, x.VerifyAt); !errors.Is(err, evidence.ErrSignature) {
			t.Fatalf("as a receipt signature: want ErrSignature, got %v", err)
		}
	case len(x.AuthorizationWire) > 0:
		a, err := payment.UnmarshalAuthorization(x.AuthorizationWire)
		if err != nil {
			t.Fatalf("the authorization decodes; it is its signature that must fail: %v", err)
		}
		if !bytes.Equal(a.Envelope.Sig, sig) {
			t.Fatal("the authorization does not carry the object's signature")
		}
		wantSignatureFails(t, a.Verify(kel, int64(x.VerifyAt)))
	default:
		t.Fatal("an empty row")
	}
}

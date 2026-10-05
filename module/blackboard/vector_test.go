package blackboard_test

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

	"github.com/ANetResearch/ANet/module/blackboard"
)

// The golden vector MODOBJ-1, blackboard part (core §5.2), is written by ANetCore's
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

type unitVector struct {
	ID          string            `json:"id"`
	Description string            `json:"description"`
	Labels      map[string]string `json:"labels"`
	Author      struct {
		AID string   `json:"aid"`
		KEL hexBytes `json:"kel"`
	} `json:"author"`
	Unit struct {
		Author string `json:"author"`
		TaskID string `json:"task_id"`
		Scope  string `json:"scope"`
		Type   string `json:"type"`
		Stamp  struct {
			Wall    int64  `json:"wall"`
			Logical uint16 `json:"logical"`
			NodeID  string `json:"node_id"`
		} `json:"stamp"`
		Payload  hexBytes `json:"payload"`
		BodyCID  string   `json:"body_cid"`
		Body     hexBytes `json:"body"`
		ID       string   `json:"id"`
		Sig      hexBytes `json:"sig"`
		Envelope struct {
			SignerAID   string `json:"signer_aid"`
			KeyStateSeq uint64 `json:"key_state_seq"`
			Alg         int    `json:"alg"`
		} `json:"envelope"`
		Wire     hexBytes `json:"wire"`
		VerifyAt int64    `json:"verify_at"`
	} `json:"unit"`
	Negatives []struct {
		Name    string   `json:"name"`
		Note    string   `json:"note"`
		Wire    hexBytes `json:"wire"`
		Refused string   `json:"refused"`
	} `json:"negatives"`
	Elsewhere []struct {
		Name              string   `json:"name"`
		Note              string   `json:"note"`
		Preimage          hexBytes `json:"preimage"`
		TS                uint64   `json:"ts"`
		Sig               hexBytes `json:"sig"`
		ReceiptWire       hexBytes `json:"receipt_wire"`
		AuthorizationWire hexBytes `json:"authorization_wire"`
		VerifyAt          uint64   `json:"verify_at"`
	} `json:"signature_elsewhere"`
}

func wantSignatureFails(t *testing.T, err error) {
	t.Helper()
	var ve *identity.VErr
	if !errors.As(err, &ve) || ve.Reason != "INVALID_SIGNATURE" {
		t.Fatalf("want INVALID_SIGNATURE, got %v", err)
	}
}

func TestMODOBJ_1_CogUnit(t *testing.T) {
	b, err := os.ReadFile("testdata/vec-cogunit-1.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var v unitVector
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("vec-cogunit-1.json: %v", err)
	}
	if v.ID != "MODOBJ-1" || v.Labels["unit"] != blackboard.CogUnitType {
		t.Fatalf("labels %v", v.Labels)
	}
	suite := identity.SuiteController()
	kel, err := identity.UnmarshalKEL(v.Author.KEL)
	if err != nil {
		t.Fatal(err)
	}
	if mine, _ := identity.MarshalKEL(suite.KEL()); !bytes.Equal(mine, v.Author.KEL) || v.Author.AID != suite.AID() {
		t.Fatal("the author is the conformance identity")
	}

	// L43: the unit, signed with the real code path.
	uv := v.Unit
	u := &blackboard.CogUnit{TaskID: uv.TaskID, Scope: uv.Scope, Type: uv.Type,
		Stamp: blackboard.HLC{Wall: uv.Stamp.Wall, Logical: uv.Stamp.Logical, NodeID: uv.Stamp.NodeID},
		Body:  uv.Payload, BodyCID: uv.BodyCID}
	if err := u.Sign(suite); err != nil {
		t.Fatal(err)
	}
	if u.Author != uv.Author {
		t.Fatalf("author %s, want %s", u.Author, uv.Author)
	}
	if pre, err := u.CanonicalPreimage(); err != nil || !bytes.Equal(pre, uv.Body) {
		t.Fatalf("unit body\n got  %x\n want %x (%v)", pre, []byte(uv.Body), err)
	}
	e := u.Envelope
	if !bytes.Equal(e.Sig, uv.Sig) || e.SignerAID != uv.Envelope.SignerAID || e.KeyStateSeq != uv.Envelope.KeyStateSeq || e.Alg != uv.Envelope.Alg {
		t.Fatalf("envelope %+v", e)
	}
	if raw, err := u.Marshal(); err != nil || !bytes.Equal(raw, uv.Wire) {
		t.Fatalf("unit wire\n got  %x\n want %x (%v)", raw, []byte(uv.Wire), err)
	}
	if id, err := u.ID(); err != nil || id != uv.ID {
		t.Fatalf("unit id %s, want %s (%v)", id, uv.ID, err)
	}
	back, err := blackboard.Unmarshal(uv.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := back.Verify(kel, uv.VerifyAt); err != nil {
		t.Fatalf("the vector's unit must verify: %v", err)
	}
	// It is what reaches the board.
	bb := blackboard.New("node-golden")
	resolve := func(aid string) ([]identity.SignedEvent, bool) { return kel, aid == suite.AID() }
	if added, err := bb.Add(back, resolve, uv.VerifyAt); err != nil || !added {
		t.Fatalf("the vector's unit is not added to the board: %v", err)
	}

	if len(v.Negatives) == 0 || len(v.Elsewhere) != 3 {
		t.Fatal("negatives")
	}
	for _, n := range v.Negatives {
		t.Run(n.Name, func(t *testing.T) {
			got, err := blackboard.Unmarshal(n.Wire)
			if err == nil {
				err = got.Verify(kel, uv.VerifyAt)
			}
			switch n.Refused {
			case "decode":
				if !errors.Is(err, blackboard.ErrUnitMalformed) {
					t.Fatalf("%s: want ErrUnitMalformed, got %v", n.Note, err)
				}
				for _, kind := range []string{"inner_preimage_", "receipt_", "authorization_", "credential_as_"} {
					if !strings.HasPrefix(n.Name, kind) {
						continue
					}
					var w struct {
						Body []byte        `cbor:"1,keyasint"`
						Env  aobj.Envelope `cbor:"2,keyasint"`
					}
					if err := coredet.Unmarshal(n.Wire, &w); err != nil {
						t.Fatal(err)
					}
					if err := identity.VerifyObject(kel, w.Env.SignerAID, w.Env.KeyStateSeq, 0, w.Body, w.Env.Sig); err != nil {
						t.Fatalf("the foreign object's own signature does not verify: %v", err)
					}
				}
			case "signature":
				wantSignatureFails(t, err)
			case "author":
				if !errors.Is(err, blackboard.ErrUnitAuthor) {
					t.Fatalf("%s: want ErrUnitAuthor, got %v", n.Note, err)
				}
			default:
				t.Fatalf("unknown refusal %q", n.Refused)
			}
			// Whatever the reason, the unit does not reach the board.
			if got != nil {
				if added, _ := blackboard.New("n").Add(got, resolve, uv.VerifyAt); added {
					t.Fatal("a refused unit was added to the board")
				}
			}
		})
	}
	for _, x := range v.Elsewhere {
		t.Run(x.Name, func(t *testing.T) {
			switch {
			case len(x.Preimage) > 0:
				if !bytes.Equal(x.Sig, uv.Sig) {
					t.Fatal("the row does not carry the unit's signature")
				}
				in := &seal.SealedInner{From: suite.AID(), KeyStateSeq: 0, TS: x.TS, Sig: x.Sig}
				if err := seal.VerifyInnerSig(in, x.Preimage, kel, x.TS); seal.ReasonOf(err) != seal.ReasonBadSig {
					t.Fatalf("as an inner signature: want %s, got %v", seal.ReasonBadSig, err)
				}
			case len(x.ReceiptWire) > 0:
				w, err := evidence.UnmarshalReceiptWire(x.ReceiptWire)
				if err != nil || !bytes.Equal(w.Env.Sig, uv.Sig) {
					t.Fatalf("the receipt decodes and carries the unit's signature: %v", err)
				}
				if _, err := w.Verify(kel, x.VerifyAt); !errors.Is(err, evidence.ErrSignature) {
					t.Fatalf("as a receipt signature: want ErrSignature, got %v", err)
				}
			case len(x.AuthorizationWire) > 0:
				a, err := payment.UnmarshalAuthorization(x.AuthorizationWire)
				if err != nil || !bytes.Equal(a.Envelope.Sig, uv.Sig) {
					t.Fatalf("the authorization decodes and carries the unit's signature: %v", err)
				}
				wantSignatureFails(t, a.Verify(kel, int64(x.VerifyAt)))
			default:
				t.Fatal("an empty row")
			}
		})
	}
}

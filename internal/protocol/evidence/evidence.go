// Package evidence defines the two v0.1 interaction-anchored trust objects — the Receipt and the
// Review — that let a centralized Hub display VERIFIABLE ratings without sitting in the P2P data path.
//
// The pair is deliberately minimal (see ROADMAP.md): there is NO interactive counter-sign. Each
// party signs exactly one object under its own KEL:
//
//	Receipt  — signed by the PROVIDER when it finishes a delegated task. It names the requester
//	           (RequesterAID), itself (ProviderAID), and content-anchors both the request and the
//	           result (RequestCID/ResultCID). It rides back to the requester with the deliverable.
//	Review   — signed by the REQUESTER. It rates the provider (SubjectAID) for a specific interaction
//	           and references the receipt (ReceiptCID).
//
// Binding without a handshake: the Hub accepts a review only if BOTH signatures verify AND
// receipt.RequesterAID == review's signer. The provider cannot forge the requester's review signature,
// and the requester cannot forge the provider's receipt signature — so a rating provably comes from a
// real counterparty of a real interaction. interaction_id (chosen by the requester at delegate time and
// shared on the wire) ties the two objects together and gives the Hub a uniqueness key.
//
// Every object uses the shared AObjEnvelope (design3/spec/_CONVENTIONS §5): a detached Ed25519 signature
// over the CoreDet-CBOR canonical preimage (the same bytes the CID is taken over — sign binds CID), so
// one verify routine (identity.VerifyObject) covers both.
package evidence

import (
	"errors"

	"github.com/ANetResearch/ANet/internal/protocol/anetcid"
	"github.com/ANetResearch/ANet/internal/protocol/aobj"
	"github.com/ANetResearch/ANet/internal/protocol/coredet"
	"github.com/ANetResearch/ANet/internal/protocol/identity"
)

// RatingMin/RatingMax bound a review score (inclusive).
const (
	RatingMin = 1
	RatingMax = 5
)

// Receipt is the provider-signed completion proof for one delegated interaction.
type Receipt struct {
	InteractionID string `cbor:"1,keyasint"`
	RequesterAID  string `cbor:"2,keyasint"`
	ProviderAID   string `cbor:"3,keyasint"`
	RequestCID    string `cbor:"4,keyasint"`
	ResultCID     string `cbor:"5,keyasint"`
	CompletedAt   uint64 `cbor:"6,keyasint"` // unix millis
	// Envelope is the detached provider signature. cbor:"-" keeps it out of the signed preimage; it
	// rides beside the object (like tsir.TaskDoc.Envelope).
	Envelope *aobj.Envelope `cbor:"-"`
}

type receiptPreimage struct {
	InteractionID string `cbor:"1,keyasint"`
	RequesterAID  string `cbor:"2,keyasint"`
	ProviderAID   string `cbor:"3,keyasint"`
	RequestCID    string `cbor:"4,keyasint"`
	ResultCID     string `cbor:"5,keyasint"`
	CompletedAt   uint64 `cbor:"6,keyasint"`
}

// CanonicalPreimage returns the CoreDet-CBOR signing/CID preimage (envelope excluded).
func (r *Receipt) CanonicalPreimage() ([]byte, error) {
	return coredet.Marshal(receiptPreimage{
		InteractionID: r.InteractionID, RequesterAID: r.RequesterAID, ProviderAID: r.ProviderAID,
		RequestCID: r.RequestCID, ResultCID: r.ResultCID, CompletedAt: r.CompletedAt,
	})
}

// CID is the content identifier over the canonical preimage (what a Review's ReceiptCID references).
func (r *Receipt) CID() (string, error) {
	pre, err := r.CanonicalPreimage()
	if err != nil {
		return "", err
	}
	return anetcid.Sum(pre)
}

// Sign attaches the provider's detached signature. The signer MUST be the ProviderAID.
func (r *Receipt) Sign(c *identity.Controller) error {
	if r.ProviderAID == "" {
		r.ProviderAID = c.AID()
	}
	pre, err := r.CanonicalPreimage()
	if err != nil {
		return err
	}
	sig, seq := c.Sign(pre)
	r.Envelope = &aobj.Envelope{SignerAID: c.AID(), KeyStateSeq: seq, Alg: aobj.AlgEdDSA, Sig: sig}
	return nil
}

// Verify checks the provider signature against the provider's KEL. It also binds the envelope signer to
// ProviderAID so a receipt cannot be attributed to a provider that did not sign it.
func (r *Receipt) Verify(kel []identity.SignedEvent, msgTime uint64) error {
	if r.Envelope == nil {
		return errors.New("evidence: unsigned receipt")
	}
	if err := r.Envelope.Validate(); err != nil {
		return err
	}
	if r.Envelope.SignerAID != r.ProviderAID {
		return errors.New("evidence: receipt signer is not the provider")
	}
	pre, err := r.CanonicalPreimage()
	if err != nil {
		return err
	}
	return identity.VerifyObject(kel, r.Envelope.SignerAID, r.Envelope.KeyStateSeq, msgTime, pre, r.Envelope.Sig)
}

// Marshal/Unmarshal serialize a receipt WITH its detached envelope for transport/storage (the deliverable
// carrier). The envelope is CBOR key 7 in the transport wrapper, kept out of the signed preimage.
type receiptWire struct {
	Body     receiptPreimage `cbor:"1,keyasint"`
	Envelope *aobj.Envelope  `cbor:"2,keyasint"`
}

// Marshal encodes the receipt + envelope for the wire.
func (r *Receipt) Marshal() ([]byte, error) {
	return coredet.Marshal(receiptWire{
		Body: receiptPreimage{
			InteractionID: r.InteractionID, RequesterAID: r.RequesterAID, ProviderAID: r.ProviderAID,
			RequestCID: r.RequestCID, ResultCID: r.ResultCID, CompletedAt: r.CompletedAt,
		},
		Envelope: r.Envelope,
	})
}

// UnmarshalReceipt decodes a wire receipt (body + detached envelope).
func UnmarshalReceipt(b []byte) (*Receipt, error) {
	var w receiptWire
	if err := coredet.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	return &Receipt{
		InteractionID: w.Body.InteractionID, RequesterAID: w.Body.RequesterAID, ProviderAID: w.Body.ProviderAID,
		RequestCID: w.Body.RequestCID, ResultCID: w.Body.ResultCID, CompletedAt: w.Body.CompletedAt,
		Envelope: w.Envelope,
	}, nil
}

// Review is the requester-signed rating of a provider for one interaction.
type Review struct {
	InteractionID string         `cbor:"1,keyasint"`
	SubjectAID    string         `cbor:"2,keyasint"` // the provider being rated
	ReviewerAID   string         `cbor:"3,keyasint"` // the requester
	Rating        int            `cbor:"4,keyasint"` // RatingMin..RatingMax
	Comment       string         `cbor:"5,keyasint,omitempty"`
	ReceiptCID    string         `cbor:"6,keyasint"` // CID of the receipt this review is anchored to
	CreatedAt     uint64         `cbor:"7,keyasint"` // unix millis
	Envelope      *aobj.Envelope `cbor:"-"`
}

type reviewPreimage struct {
	InteractionID string `cbor:"1,keyasint"`
	SubjectAID    string `cbor:"2,keyasint"`
	ReviewerAID   string `cbor:"3,keyasint"`
	Rating        int    `cbor:"4,keyasint"`
	Comment       string `cbor:"5,keyasint,omitempty"`
	ReceiptCID    string `cbor:"6,keyasint"`
	CreatedAt     uint64 `cbor:"7,keyasint"`
}

func (r *Review) canonicalStruct() reviewPreimage {
	return reviewPreimage{
		InteractionID: r.InteractionID, SubjectAID: r.SubjectAID, ReviewerAID: r.ReviewerAID,
		Rating: r.Rating, Comment: r.Comment, ReceiptCID: r.ReceiptCID, CreatedAt: r.CreatedAt,
	}
}

// CanonicalPreimage returns the CoreDet-CBOR signing preimage (envelope excluded).
func (r *Review) CanonicalPreimage() ([]byte, error) { return coredet.Marshal(r.canonicalStruct()) }

// ValidRating reports whether Rating is in [RatingMin, RatingMax].
func (r *Review) ValidRating() bool { return r.Rating >= RatingMin && r.Rating <= RatingMax }

// Sign attaches the requester's detached signature. The signer MUST be the ReviewerAID.
func (r *Review) Sign(c *identity.Controller) error {
	if r.ReviewerAID == "" {
		r.ReviewerAID = c.AID()
	}
	pre, err := r.CanonicalPreimage()
	if err != nil {
		return err
	}
	sig, seq := c.Sign(pre)
	r.Envelope = &aobj.Envelope{SignerAID: c.AID(), KeyStateSeq: seq, Alg: aobj.AlgEdDSA, Sig: sig}
	return nil
}

// Verify checks the requester signature against the reviewer's KEL and binds the signer to ReviewerAID.
func (r *Review) Verify(kel []identity.SignedEvent, msgTime uint64) error {
	if r.Envelope == nil {
		return errors.New("evidence: unsigned review")
	}
	if err := r.Envelope.Validate(); err != nil {
		return err
	}
	if r.Envelope.SignerAID != r.ReviewerAID {
		return errors.New("evidence: review signer is not the reviewer")
	}
	pre, err := r.CanonicalPreimage()
	if err != nil {
		return err
	}
	return identity.VerifyObject(kel, r.Envelope.SignerAID, r.Envelope.KeyStateSeq, msgTime, pre, r.Envelope.Sig)
}

type reviewWire struct {
	Body     reviewPreimage `cbor:"1,keyasint"`
	Envelope *aobj.Envelope `cbor:"2,keyasint"`
}

// Marshal encodes the review + envelope for the wire.
func (r *Review) Marshal() ([]byte, error) {
	return coredet.Marshal(reviewWire{Body: r.canonicalStruct(), Envelope: r.Envelope})
}

// UnmarshalReview decodes a wire review (body + detached envelope).
func UnmarshalReview(b []byte) (*Review, error) {
	var w reviewWire
	if err := coredet.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	return &Review{
		InteractionID: w.Body.InteractionID, SubjectAID: w.Body.SubjectAID, ReviewerAID: w.Body.ReviewerAID,
		Rating: w.Body.Rating, Comment: w.Body.Comment, ReceiptCID: w.Body.ReceiptCID, CreatedAt: w.Body.CreatedAt,
		Envelope: w.Envelope,
	}, nil
}

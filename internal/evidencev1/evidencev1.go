// Package evidencev1 is the 0.2 evidence pair — the provider's Receipt and the requester's Review
// — and the 0.2 check of a completion's receipt, kept in the daemon while its delegation code still
// issues and checks them. ANetCore 0.16 replaced them with evidence v2 (receipt and review
// commitments, the task binding); the daemon moves to those, and this package goes, with the
// review v2 work.
//
// v030-todo(PAY-14): the daemon issues and checks evidence v2 (ANetCore evidence, delegation
// VerifyResultForRequest) and this package is deleted.
package evidencev1

import (
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
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

// ErrUnverifiable is returned when the caller supplies no provider KEL, so
// the receipt cannot be checked at all. Distinct from a failed check.
var ErrUnverifiable = errors.New("delegation: completion carries no provider KEL")

// ErrRequestMismatch is returned when a receipt names a request other than the one the caller
// sent.
var ErrRequestMismatch = errors.New("delegation: receipt is for another request")

// VerifyResultForRequest checks a 0.2 receipt against the resolved provider KEL at msgTime and binds
// its signer, provider, requester, interaction, deliverable hash and RequestCID to
// requestCID, the CID of the request (TaskDoc) bytes the requester sent (anetcid.Sum over them).
// An empty requestCID skips that one binding, as for the other identifiers.
//
// Without it a provider can sign a receipt that names a request never made — the CID of any
// bytes it likes — for the result it delivered. The receipt verifies, the requester reviews the
// work, and a third party checking the pair with the 0.2 interlock against those bytes is
// shown the requester vouching for a request it never sent, while the request it did send no
// longer matches. A mismatch is ErrRequestMismatch.
func VerifyResultForRequest(r *delegation.ResultResp, kel []identity.SignedEvent, interactionID, requesterAID, providerAID, requestCID string, msgTime uint64) (*Receipt, error) {
	if r == nil || len(r.Receipt) == 0 {
		return nil, fmt.Errorf("delegation: completion carries no receipt")
	}
	if len(kel) == 0 {
		return nil, fmt.Errorf("%w (no resolved provider KEL supplied)", ErrUnverifiable)
	}
	rc, err := UnmarshalReceipt(r.Receipt)
	if err != nil {
		return nil, fmt.Errorf("delegation: undecodable receipt: %w", err)
	}
	if err := checkBodyKEL(r.KEL, kel); err != nil {
		return nil, err
	}
	return bindReceipt(rc, r.Deliverable, kel, interactionID, requesterAID, providerAID, requestCID, msgTime)
}

// bindReceipt verifies the receipt signature against kel at msgTime and binds each field the
// receipt asserts to what the caller knows. An empty interactionID, requesterAID, providerAID or
// requestCID skips that one binding.
func bindReceipt(rc *Receipt, deliverable []byte, kel []identity.SignedEvent, interactionID, requesterAID, providerAID, requestCID string, msgTime uint64) (*Receipt, error) {
	if err := rc.Verify(kel, msgTime); err != nil {
		return nil, fmt.Errorf("delegation: receipt signature invalid: %w", err)
	}
	if providerAID != "" && rc.ProviderAID != providerAID {
		return nil, fmt.Errorf("delegation: receipt signed by %s, but the task went to %s",
			rc.ProviderAID, providerAID)
	}
	if requesterAID != "" && rc.RequesterAID != requesterAID {
		return nil, fmt.Errorf("delegation: receipt names %s as requester, not us", rc.RequesterAID)
	}
	if interactionID != "" && rc.InteractionID != interactionID {
		return nil, fmt.Errorf("delegation: receipt is for interaction %s, not %s",
			rc.InteractionID, interactionID)
	}
	if requestCID != "" && rc.RequestCID != requestCID {
		return nil, fmt.Errorf("%w: it names %s, the request sent was %s", ErrRequestMismatch,
			rc.RequestCID, requestCID)
	}
	// The binding that makes the rest worth having: the signature must cover
	// the bytes that actually arrived.
	cid, err := anetcid.Sum(deliverable)
	if err != nil {
		return nil, err
	}
	if rc.ResultCID != cid {
		return nil, fmt.Errorf("delegation: receipt covers %s but the deliverable hashes to %s",
			rc.ResultCID, cid)
	}
	return rc, nil
}

// checkBodyKEL: a KEL carried in the completion body is accepted only when absent, equal to the
// resolved KEL or a prefix of it (the rule of ANetCore delegation).
func checkBodyKEL(body []byte, resolved []identity.SignedEvent) error {
	if len(body) == 0 {
		return nil
	}
	bodyKEL, err := identity.UnmarshalKEL(body)
	if err != nil {
		return fmt.Errorf("delegation: bad body KEL: %w", err)
	}
	if err := identity.ExtendsKEL(bodyKEL, resolved); err != nil {
		return fmt.Errorf("%w: %w", delegation.ErrBodyKELMismatch, err)
	}
	return nil
}

package blackboard

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module/inv1"
)

// OrgScopedObject marks a CogUnit as org-scoped (INV-1 runtime type tag): a blackboard contribution
// is org-internal and must never reach a commons publish path. Value receiver so both CogUnit and
// *CogUnit are caught by inv1.GuardCommonsPublish.
var _ inv1.OrgScoped = CogUnit{}

func (CogUnit) OrgScopedObject() {}

// CogUnitType is key 0 of a unit's body (core §2.19 CogUnitBody, L43): the type tag the author's
// identity key signs under, which keeps a unit's signature from passing for the signature of any
// other object that key signs (core §3.4).
const CogUnitType = "anet.cogunit/1"

// CogUnit is one signed, content-addressed contribution to the shared blackboard. Its ID is the
// CID of its body, coredet(CogUnitBody) with the type tag (cogUnitBody); the issuing Author signs
// the body (detached envelope, never in the signed bytes — acc/aet/tsir discipline). A large
// payload lives in CAS and is bound by BodyCID, which IS in the signed body (B5: the offloaded
// body's CID must be signed, so the body cannot be swapped) — small payloads may ride inline in
// Body.
type CogUnit struct {
	Author  string // the contributing AID
	TaskID  string // task this unit belongs to
	Scope   string // e.g. "org"
	Type    string // claim / evidence / conclusion / intent / retraction …
	Stamp   HLC    // causal stamp (orders + tags the OR-Set add)
	Body    []byte // inline small payload
	BodyCID string // CAS ref for a large payload (signed — B5)
	// Envelope travels beside the body (CogUnitWire) so a member that pulls a snapshot can
	// re-verify the unit, but it is not in the signed and CID bytes.
	Envelope *aobj.Envelope
}

// cogUnitBody is the signed and content-addressed encoding of a CogUnit.
//
//anet:wire CogUnitBody
type cogUnitBody struct {
	Tag     string `cbor:"0,keyasint"` // CogUnitType
	Author  string `cbor:"1,keyasint"`
	TaskID  string `cbor:"2,keyasint,omitempty"`
	Scope   string `cbor:"3,keyasint,omitempty"`
	Type    string `cbor:"4,keyasint"`
	Stamp   HLC    `cbor:"5,keyasint"`
	Body    []byte `cbor:"6,keyasint,omitempty"`
	BodyCID string `cbor:"7,keyasint,omitempty"`
}

// cogUnitWire is how a unit travels: its body as a byte string, the bytes the author signed,
// beside the envelope (core §2.19; CORE-E2: a verifier checks that the body bytes are exactly
// coredet of what they decode to).
//
//anet:wire CogUnitWire
type cogUnitWire struct {
	Body     []byte         `cbor:"1,keyasint"`
	Envelope *aobj.Envelope `cbor:"2,keyasint"`
}

// CogUnit errors.
var (
	ErrUnitUnsigned = errors.New("blackboard: unsigned cogunit")
	ErrUnitAuthor   = errors.New("blackboard: envelope signer != unit author")
	ErrUnitNoType   = errors.New("blackboard: cogunit missing type")
	ErrUnitBadStamp = errors.New("blackboard: cogunit missing causal stamp")
	// ErrUnitMalformed: the bytes are not a unit as 0.3.0 writes it — a body without the type tag
	// (a 0.2 unit), with another tag, a key it does not register or a non-canonical encoding, or a
	// wire form without its envelope. Such a unit never reaches the board.
	ErrUnitMalformed = errors.New("blackboard: not a well-formed cogunit")
)

// CanonicalPreimage is the CoreDet-CBOR signing/CID preimage: coredet(CogUnitBody), type tag
// included (L43); the envelope is not in it.
func (u *CogUnit) CanonicalPreimage() ([]byte, error) {
	return coredet.Marshal(cogUnitBody{
		Tag: CogUnitType, Author: u.Author, TaskID: u.TaskID, Scope: u.Scope, Type: u.Type,
		Stamp: u.Stamp, Body: u.Body, BodyCID: u.BodyCID,
	})
}

// Marshal encodes a signed unit for transport, its body beside its envelope (CogUnitWire): the
// form org-central serializes for bb.snapshot so the puller can re-verify. An unsigned unit is
// refused: nobody could verify it.
func (u *CogUnit) Marshal() ([]byte, error) {
	if u.Envelope == nil {
		return nil, ErrUnitUnsigned
	}
	body, err := u.CanonicalPreimage()
	if err != nil {
		return nil, err
	}
	return coredet.Marshal(cogUnitWire{Body: body, Envelope: u.Envelope})
}

// Unmarshal decodes a transported unit with its envelope. It decodes strictly (core CORE-K26):
// the wire form and the body each hold exactly their registered keys and are exactly coredet of
// what they decode to (CORE-E2), the envelope is present, and the body carries the type tag
// CogUnitType. A unit written by 0.2 (no tag, the envelope inside the map) and any other object's
// body are refused here, before any signature is checked. Every failure is ErrUnitMalformed.
func Unmarshal(b []byte) (*CogUnit, error) {
	var w cogUnitWire
	if err := coredet.UnmarshalStrict(b, &w); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnitMalformed, err)
	}
	if w.Envelope == nil {
		return nil, fmt.Errorf("%w: no envelope", ErrUnitMalformed)
	}
	if again, err := coredet.Marshal(w); err != nil || !bytes.Equal(again, b) {
		return nil, fmt.Errorf("%w: not in canonical form, or without its body", ErrUnitMalformed)
	}
	var body cogUnitBody
	if err := coredet.UnmarshalStrict(w.Body, &body); err != nil {
		return nil, fmt.Errorf("%w: the body does not decode: %v", ErrUnitMalformed, err)
	}
	if body.Tag != CogUnitType {
		return nil, fmt.Errorf("%w: the body's type tag is %q, want %q", ErrUnitMalformed, body.Tag, CogUnitType)
	}
	u := &CogUnit{Author: body.Author, TaskID: body.TaskID, Scope: body.Scope, Type: body.Type,
		Stamp: body.Stamp, Body: body.Body, BodyCID: body.BodyCID, Envelope: w.Envelope}
	if again, err := u.CanonicalPreimage(); err != nil || !bytes.Equal(again, w.Body) {
		return nil, fmt.Errorf("%w: the body is not the canonical encoding of a unit (a required key is missing, or an encoding is not CoreDet)", ErrUnitMalformed)
	}
	return u, nil
}

// ID is the content identifier over the canonical preimage.
func (u *CogUnit) ID() (string, error) {
	pre, err := u.CanonicalPreimage()
	if err != nil {
		return "", err
	}
	return anetcid.Sum(pre)
}

// Sign attaches the author's detached signature (the signer MUST be the unit's Author). signer
// signs and does nothing else: a module never holds an identity's controller or its key (KEL O-1).
func (u *CogUnit) Sign(signer identity.Signer) error {
	u.Author = signer.AID()
	pre, err := u.CanonicalPreimage()
	if err != nil {
		return err
	}
	sig, seq := signer.Sign(pre)
	u.Envelope = &aobj.Envelope{SignerAID: signer.AID(), KeyStateSeq: seq, Alg: aobj.AlgEdDSA, Sig: sig}
	return nil
}

// Verify checks structural validity + the author signature against the author KEL, with the
// revocation gate as-of msgTime. The envelope signer MUST equal Author (no author-spoofing).
func (u *CogUnit) Verify(kel []identity.SignedEvent, msgTime int64) error {
	if u.Type == "" {
		return ErrUnitNoType
	}
	if u.Stamp.IsZero() {
		return ErrUnitBadStamp
	}
	if u.Envelope == nil {
		return ErrUnitUnsigned
	}
	if err := u.Envelope.Validate(); err != nil {
		return err
	}
	if u.Envelope.SignerAID != u.Author {
		return ErrUnitAuthor
	}
	pre, err := u.CanonicalPreimage()
	if err != nil {
		return err
	}
	return identity.VerifyObject(kel, u.Author, u.Envelope.KeyStateSeq, uint64(msgTime), pre, u.Envelope.Sig)
}

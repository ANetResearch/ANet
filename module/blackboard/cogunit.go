package blackboard

import (
	"errors"

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

// CogUnit is one signed, content-addressed contribution to the shared blackboard. Its ID is the
// CID of its canonical preimage; the issuing Author signs it (detached envelope, cbor:"-", never
// in the signed map — acc/aet/tsir discipline). A large payload lives in CAS and is bound by
// BodyCID, which IS in the signed preimage (B5: the offloaded body's CID must be signed, so the
// body cannot be swapped) — small payloads may ride inline in Body.
type CogUnit struct {
	Author  string `cbor:"1,keyasint"`           // the contributing AID
	TaskID  string `cbor:"2,keyasint,omitempty"` // task this unit belongs to
	Scope   string `cbor:"3,keyasint,omitempty"` // e.g. "org"
	Type    string `cbor:"4,keyasint"`           // claim / evidence / conclusion / intent / retraction …
	Stamp   HLC    `cbor:"5,keyasint"`           // causal stamp (orders + tags the OR-Set add)
	Body    []byte `cbor:"6,keyasint,omitempty"` // inline small payload
	BodyCID string `cbor:"7,keyasint,omitempty"` // CAS ref for a large payload (signed — B5)
	// Envelope TRAVELS on the wire (key 8) so a member that pulls a snapshot can re-verify the
	// unit, but it is EXCLUDED from the signing/CID preimage (cogUnitPreimage, keys 1–7) — the
	// acc/aet/ascp.Vote detached-envelope discipline.
	Envelope *aobj.Envelope `cbor:"8,keyasint,omitempty"`
}

// cogUnitPreimage is the signed/CID-significant subset (keys 1–7); the envelope (key 8) carries
// the signature and is never part of what is signed, but DOES ride the wire form (Marshal).
type cogUnitPreimage struct {
	Author  string `cbor:"1,keyasint"`
	TaskID  string `cbor:"2,keyasint,omitempty"`
	Scope   string `cbor:"3,keyasint,omitempty"`
	Type    string `cbor:"4,keyasint"`
	Stamp   HLC    `cbor:"5,keyasint"`
	Body    []byte `cbor:"6,keyasint,omitempty"`
	BodyCID string `cbor:"7,keyasint,omitempty"`
}

// CogUnit errors.
var (
	ErrUnitUnsigned = errors.New("blackboard: unsigned cogunit")
	ErrUnitAuthor   = errors.New("blackboard: envelope signer != unit author")
	ErrUnitNoType   = errors.New("blackboard: cogunit missing type")
	ErrUnitBadStamp = errors.New("blackboard: cogunit missing causal stamp")
)

// CanonicalPreimage is the CoreDet-CBOR signing/CID preimage (keys 1–7; envelope excluded).
func (u *CogUnit) CanonicalPreimage() ([]byte, error) {
	return coredet.Marshal(cogUnitPreimage{
		Author: u.Author, TaskID: u.TaskID, Scope: u.Scope, Type: u.Type,
		Stamp: u.Stamp, Body: u.Body, BodyCID: u.BodyCID,
	})
}

// Marshal encodes the full unit INCLUDING the detached envelope (key 8) for transport — the form
// org-central serializes for bb.snapshot so the puller can re-verify.
func (u *CogUnit) Marshal() ([]byte, error) { return coredet.Marshal(u) }

// Unmarshal decodes a transported CogUnit (with its envelope).
func Unmarshal(b []byte) (*CogUnit, error) {
	var u CogUnit
	if err := coredet.Unmarshal(b, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// ID is the content identifier over the canonical preimage.
func (u *CogUnit) ID() (string, error) {
	pre, err := u.CanonicalPreimage()
	if err != nil {
		return "", err
	}
	return anetcid.Sum(pre)
}

// Sign attaches the author's detached signature (the signer MUST be the unit's Author).
func (u *CogUnit) Sign(ctrl *identity.Controller) error {
	u.Author = ctrl.AID()
	pre, err := u.CanonicalPreimage()
	if err != nil {
		return err
	}
	sig, seq := ctrl.Sign(pre)
	u.Envelope = &aobj.Envelope{SignerAID: ctrl.AID(), KeyStateSeq: seq, Alg: aobj.AlgEdDSA, Sig: sig}
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

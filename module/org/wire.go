package org

import (
	"bytes"
	"fmt"

	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/coredet"
)

// SignedCredential is how a credential travels: its body, the bytes the
// issuer signed (core §2.19, CORE-E2), beside the signature over them.
//
// The signature is detached — Credential.Envelope is deliberately outside
// the signed preimage — so the body alone is the fields it signed and *not*
// the signature over them. A credential sent that way arrives unsigned, and
// no amount of verification can recover what was never on the wire.
//
// That is easy to get wrong and silent when you do: the object round-trips,
// every field is present, and verification fails with "unsigned" for a
// credential that was signed perfectly well. anet3 carried the two parts
// side by side for exactly this reason, and so does this.
//
//anet:wire SignedCredential
type SignedCredential struct {
	// Cred is the credential's body, coredet(OrgCredBody): the signed bytes.
	Cred []byte `cbor:"1,keyasint"`
	// Envelope is the issuer's detached signature over those bytes.
	Envelope *aobj.Envelope `cbor:"2,keyasint"`
}

// MarshalCredential puts a signed credential on the wire, both halves.
func MarshalCredential(c *Credential) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("org: nil credential")
	}
	if c.Envelope == nil {
		// Refuse here rather than emit a credential nobody can verify: the
		// failure belongs at the point the mistake was made.
		return nil, fmt.Errorf("org: credential is unsigned — sign it before sending")
	}
	body, err := c.CanonicalPreimage()
	if err != nil {
		return nil, err
	}
	return coredet.Marshal(&SignedCredential{Cred: body, Envelope: c.Envelope})
}

// UnmarshalCredential reassembles a credential and its detached signature. It decodes strictly
// (core CORE-K26): the wire form and the body each hold exactly their registered keys and are
// exactly coredet of what they decode to (CORE-E2), the envelope is present, and the body carries
// the type tag CredentialType. A credential written by 0.2 (no tag) and any other object's body
// are refused here, before any signature is checked. Every failure is ErrMalformed.
func UnmarshalCredential(b []byte) (*Credential, error) {
	var sc SignedCredential
	if err := coredet.UnmarshalStrict(b, &sc); err != nil {
		return nil, fmt.Errorf("%w: not a signed credential: %v", ErrMalformed, err)
	}
	if sc.Envelope == nil {
		return nil, fmt.Errorf("%w: the signed credential carries no envelope", ErrMalformed)
	}
	if again, err := coredet.Marshal(&sc); err != nil || !bytes.Equal(again, b) {
		return nil, fmt.Errorf("%w: the signed credential is not in canonical form, or lacks its body", ErrMalformed)
	}
	c, err := decodeCredBody(sc.Cred)
	if err != nil {
		return nil, err
	}
	c.Envelope = sc.Envelope
	return c, nil
}

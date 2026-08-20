package org

import (
	"fmt"

	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/coredet"
)

// SignedCredential is how a credential travels.
//
// The signature is detached — Credential.Envelope is `cbor:"-"`, deliberately
// outside the signed preimage — so marshalling a Credential produces the
// fields it signed and *not* the signature over them. A credential sent that
// way arrives unsigned, and no amount of verification can recover what was
// never on the wire.
//
// That is easy to get wrong and silent when you do: the object round-trips,
// every field is present, and verification fails with "unsigned" for a
// credential that was signed perfectly well. anet3 carried the two parts
// side by side for exactly this reason, and so does this.
type SignedCredential struct {
	// Cred is coredet.Marshal(Credential) — the signed preimage fields.
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
	body, err := coredet.Marshal(c)
	if err != nil {
		return nil, err
	}
	return coredet.Marshal(&SignedCredential{Cred: body, Envelope: c.Envelope})
}

// UnmarshalCredential reassembles a credential and its detached signature.
func UnmarshalCredential(b []byte) (*Credential, error) {
	var sc SignedCredential
	if err := coredet.Unmarshal(b, &sc); err != nil {
		return nil, fmt.Errorf("org: not a signed credential: %w", err)
	}
	if sc.Envelope == nil {
		return nil, fmt.Errorf("org: signed credential carries no envelope")
	}
	var c Credential
	if err := coredet.Unmarshal(sc.Cred, &c); err != nil {
		return nil, fmt.Errorf("org: credential body malformed: %w", err)
	}
	c.Envelope = sc.Envelope
	return &c, nil
}

// Package aobj implements the AObjEnvelope, the uniform signature envelope every
// signed Agent Network object carries so one verification routine verifies all.
//
// Normative source: design3/spec/_CONVENTIONS §5 (P1 signature profile). The envelope
// is a CoreDet-CBOR int-key map; the signature is a detached Ed25519 signature over the
// object's CoreDet-CBOR preimage (the same bytes the CID is taken over — sign binds CID).
// Verify-before-use is a hard precondition at every boundary.
package aobj

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
)

// AlgEdDSA is the COSE algorithm id for EdDSA (Ed25519); it is the wire value of the
// envelope's alg field (design3/spec/_CONVENTIONS §5).
const AlgEdDSA = -8

// Envelope is the P1 AObj signature envelope. CBOR int keys (keyasint):
// 1 signer_aid, 2 key_state_seq, 3 alg, 4 sig, 5 canonical_preimage_ref (optional).
// It is carried beside the signed object, never inside the signed preimage.
type Envelope struct {
	SignerAID   string `cbor:"1,keyasint"`
	KeyStateSeq uint64 `cbor:"2,keyasint"`
	Alg         int    `cbor:"3,keyasint"`
	Sig         []byte `cbor:"4,keyasint"`
	PreimageRef string `cbor:"5,keyasint,omitempty"`
}

// ErrEnvelope is returned by Envelope.Validate for an envelope that violates the P1
// signature profile (wrong alg or wrong signature length).
var ErrEnvelope = errors.New("aobj: invalid envelope")

// Validate checks the envelope's static profile invariants (design3/spec/_CONVENTIONS §5):
// alg MUST be AlgEdDSA and the detached signature MUST be exactly 64 bytes. It does NOT
// verify the signature itself (see Verify); it is the structural gate before verification.
func (e Envelope) Validate() error {
	if e.Alg != AlgEdDSA {
		return ErrEnvelope
	}
	if len(e.Sig) != ed25519.SignatureSize {
		return ErrEnvelope
	}
	return nil
}

// Verify checks a detached Ed25519 signature over preimage against pub. The signature
// MUST be exactly 64 bytes (design3/spec/_CONVENTIONS §5).
func Verify(pub ed25519.PublicKey, preimage, sig []byte) error {
	if len(sig) != ed25519.SignatureSize {
		return errors.New("aobj: signature must be 64 bytes")
	}
	if !ed25519.Verify(pub, preimage, sig) {
		return errors.New("aobj: signature verification failed")
	}
	return nil
}

// ---- Suite test key (design3/spec/_CONVENTIONS §8) ----
// Deterministic key for reproducing signature golden vectors:
//   seed = SHA-256("anet-suite-test-key-v1")
// Ed25519 is deterministic (RFC 8032), so SuiteSign(preimage) is one fixed signature
// per preimage. This key is for conformance vectors only, never for production identity.

var (
	suiteSeed = sha256.Sum256([]byte("anet-suite-test-key-v1"))
	suiteKey  = ed25519.NewKeyFromSeed(suiteSeed[:])
	// SuitePub is the suite test public key (hex ae5ec9a1…071e802e).
	SuitePub = suiteKey.Public().(ed25519.PublicKey)
)

// SuiteSeed returns a copy of the 32-byte suite test seed.
func SuiteSeed() []byte { s := suiteSeed; return s[:] }

// SuiteSign signs preimage with the suite test key (for golden vectors only).
func SuiteSign(preimage []byte) []byte { return ed25519.Sign(suiteKey, preimage) }

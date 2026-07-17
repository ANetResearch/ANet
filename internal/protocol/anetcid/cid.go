// Package anetcid constructs Agent Network content identifiers.
//
// Normative source: design3/spec/_CONVENTIONS §3. Every content-addressed suite object
// uses one CID construction over its CoreDet-CBOR preimage:
//
//	digest    = sha2-256(preimage)
//	multihash = 0x12 0x20 ‖ digest
//	cid_v1    = 0x01 0x71 ‖ multihash      (0x01 CIDv1, 0x71 dag-cbor)
//	CID       = "b" ‖ base32-lower-no-pad(cid_v1)   (multibase 'b')
//
// The byte prefix 0x01 0x71 0x12 0x20 is FROZEN MUST suite-wide.
package anetcid

import (
	cid "github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// Sum returns the suite CID string for a CoreDet-CBOR preimage. The returned string
// is multibase base32-lower CIDv1 dag-cbor (sha2-256), e.g. "bafyrei…".
func Sum(preimage []byte) (string, error) {
	h, err := mh.Sum(preimage, mh.SHA2_256, -1)
	if err != nil {
		return "", err
	}
	return cid.NewCidV1(cid.DagCBOR, h).String(), nil
}

// MustSum is Sum that panics on error (sha2-256 over in-memory bytes does not fail
// in practice); for tests and known-good preimages.
func MustSum(preimage []byte) string {
	s, err := Sum(preimage)
	if err != nil {
		panic(err)
	}
	return s
}

// SumRaw returns a CID over ARBITRARY (non-CBOR) bytes — used to content-address binary attachments
// (images, media, archives). Same sha2-256 multihash as Sum, but the CIDv1 codec is 0x55 (raw) rather
// than dag-cbor, so the identifier correctly advertises that the preimage is opaque bytes, not a CBOR
// object. Integrity check on receipt is: SumRaw(data) == advertised CID.
func SumRaw(data []byte) (string, error) {
	h, err := mh.Sum(data, mh.SHA2_256, -1)
	if err != nil {
		return "", err
	}
	return cid.NewCidV1(cid.Raw, h).String(), nil
}

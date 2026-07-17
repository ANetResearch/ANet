// Package coredet implements CoreDet-CBOR, the Agent Network suite's deterministic
// CBOR profile. It is the single encoder for every CID preimage and signature
// preimage in the stack.
//
// Normative source: design3/spec/_CONVENTIONS §2 = RFC 8949 §4.2 Core Deterministic
// Encoding (shortest-form integers/lengths, definite-length items, map keys sorted by
// bytewise-lexicographic order of their encoded bytes, preferred float serialization)
// plus the suite restrictions C-R1 (no NaN/±Infinity in any value) and C-R2 (no CBOR
// tags). RFC 7049 "canonical CBOR" (length-first key sort) MUST NOT be substituted.
package coredet

import (
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

var (
	encMode cbor.EncMode
	decMode cbor.DecMode
)

func init() {
	opts := cbor.CoreDetEncOptions()        // RFC 8949 §4.2
	opts.NaNConvert = cbor.NaNConvertReject // C-R1
	opts.InfConvert = cbor.InfConvertReject // C-R1
	m, err := opts.EncMode()
	if err != nil {
		panic(fmt.Sprintf("coredet: invalid CoreDet enc options: %v", err))
	}
	encMode = m
	// C-R2: no CBOR tags. DupMapKeyEnforcedAPF: reject duplicate map keys — a canonical CoreDet
	// encoding (RFC 8949 §4.2) never has them, so a decoder that verifies the profile MUST reject
	// them rather than silently keep last-wins (which would let two implementations disagree).
	dm, err := cbor.DecOptions{TagsMd: cbor.TagsForbidden, DupMapKey: cbor.DupMapKeyEnforcedAPF}.DecMode()
	if err != nil {
		panic(fmt.Sprintf("coredet: invalid dec options: %v", err))
	}
	decMode = dm
}

// Marshal encodes v as CoreDet-CBOR. It returns an error if v contains NaN or
// ±Infinity (C-R1). The result is the canonical preimage for CID (see internal/protocol/anetcid)
// and for AObj signatures (see internal/protocol/aobj).
func Marshal(v any) ([]byte, error) {
	return encMode.Marshal(v)
}

// Unmarshal decodes CoreDet-CBOR bytes into v. Decoding determinism is not required
// (it is an encode property); re-encoding the result with Marshal reproduces canonical bytes.
func Unmarshal(b []byte, v any) error {
	return decMode.Unmarshal(b, v)
}

package aobj

import (
	"bytes"
	"testing"
)

// _CONVENTIONS §5: Envelope.Validate accepts alg == AlgEdDSA with a 64-byte sig, and
// rejects a wrong alg or a wrong-length signature.
func TestEnvelopeValidate(t *testing.T) {
	good := Envelope{Alg: AlgEdDSA, Sig: make([]byte, 64)}
	if err := good.Validate(); err != nil {
		t.Fatalf("good envelope: unexpected error %v", err)
	}

	wrongAlg := Envelope{Alg: -7, Sig: make([]byte, 64)} // ES256, not EdDSA
	if err := wrongAlg.Validate(); err != ErrEnvelope {
		t.Fatalf("wrong alg: want ErrEnvelope, got %v", err)
	}

	wrongLen := Envelope{Alg: AlgEdDSA, Sig: make([]byte, 63)}
	if err := wrongLen.Validate(); err != ErrEnvelope {
		t.Fatalf("wrong siglen: want ErrEnvelope, got %v", err)
	}
}

// Validate is the structural gate before Verify: a Validate-clean, suite-signed envelope
// also verifies against the suite key.
func TestEnvelopeValidateThenVerify(t *testing.T) {
	preimage := []byte("hello-anet")
	sig := SuiteSign(preimage)
	e := Envelope{Alg: AlgEdDSA, Sig: sig}
	if err := e.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !bytes.Equal(e.Sig, sig) {
		t.Fatal("sig mutated")
	}
	if err := Verify(SuitePub, preimage, e.Sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

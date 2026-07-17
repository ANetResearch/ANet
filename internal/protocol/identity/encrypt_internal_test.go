package identity

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"golang.org/x/crypto/curve25519"
)

// The derived X25519 private scalar and the Montgomery-u public must be a matching keypair:
// X25519(derivedPriv, basepoint) == X25519PublicFromEd25519(pub). This pins the birational map
// (the most security-load-bearing math) against a silent library/behavior change.
func TestX25519DerivationEquivalence(t *testing.T) {
	for i := 0; i < 50; i++ {
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		xpriv := x25519PrivateFromEd25519(priv)
		gotPub, err := curve25519.X25519(xpriv[:], curve25519.Basepoint)
		if err != nil {
			t.Fatalf("iter %d: X25519: %v", i, err)
		}
		want, err := X25519PublicFromEd25519(pub)
		if err != nil {
			t.Fatalf("iter %d: convert: %v", i, err)
		}
		if !bytes.Equal(gotPub, want[:]) {
			t.Fatalf("iter %d: derived-priv public != converted public", i)
		}
	}
}

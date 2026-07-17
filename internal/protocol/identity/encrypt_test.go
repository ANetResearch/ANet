package identity_test

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/ANetResearch/ANet/internal/protocol/identity"
)

func curEdPub(t *testing.T, c *identity.Controller) ed25519.PublicKey {
	t.Helper()
	st, err := identity.Replay(c.KEL())
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return ed25519.PublicKey(st[len(st)-1].CurrentKeys[0])
}

func TestSealOpenRoundTrip(t *testing.T) {
	a, _ := identity.Incept()
	msg := []byte("the org group key: 0123456789abcdef")
	ct, err := identity.SealTo(curEdPub(t, a), msg)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := a.Open(ct)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
	// ciphertext is not plaintext and carries the ephemeral prefix.
	if bytes.Contains(ct, msg) || len(ct) <= len(msg) {
		t.Fatal("ciphertext leaks plaintext / too short")
	}
}

func TestSealWrongRecipientFails(t *testing.T) {
	a, _ := identity.Incept()
	b, _ := identity.Incept()
	ct, err := identity.SealTo(curEdPub(t, a), []byte("for a only"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(ct); err == nil {
		t.Fatal("b must NOT open a box sealed to a")
	}
}

func TestSealTamperDetected(t *testing.T) {
	a, _ := identity.Incept()
	ct, err := identity.SealTo(curEdPub(t, a), []byte("authentic"))
	if err != nil {
		t.Fatal(err)
	}
	// flip a byte in the box body (after the 32-byte ephemeral prefix).
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := a.Open(tampered); err == nil {
		t.Fatal("tampered box must fail authentication")
	}
	// a too-short input is rejected, not panicked.
	if _, err := a.Open([]byte("short")); err == nil {
		t.Fatal("short ciphertext must error")
	}
}

// After the recipient rotates, a box sealed to its OLD key no longer opens (the keychain must
// re-seal to the new key) — documents the forward-rotation behavior.
func TestSealAfterRotation(t *testing.T) {
	a, _ := identity.Incept()
	oldPub := curEdPub(t, a)
	ctOld, _ := identity.SealTo(oldPub, []byte("sealed to old key"))
	if _, err := a.Open(ctOld); err != nil {
		t.Fatalf("open before rotation: %v", err)
	}
	if err := a.Rotate(1000); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Open(ctOld); err == nil {
		t.Fatal("box sealed to the pre-rotation key must NOT open after rotation")
	}
	// a fresh seal to the new current key opens.
	ctNew, _ := identity.SealTo(curEdPub(t, a), []byte("sealed to new key"))
	if got, err := a.Open(ctNew); err != nil || string(got) != "sealed to new key" {
		t.Fatalf("open after re-seal: got %q err %v", got, err)
	}
}

func TestX25519ConversionStable(t *testing.T) {
	a, _ := identity.Incept()
	pub := curEdPub(t, a)
	x1, err := identity.X25519PublicFromEd25519(pub)
	if err != nil {
		t.Fatal(err)
	}
	x2, _ := identity.X25519PublicFromEd25519(pub)
	if x1 != x2 {
		t.Fatal("conversion not deterministic")
	}
	if _, err := identity.X25519PublicFromEd25519([]byte("too short")); err == nil {
		t.Fatal("bad pubkey size must error")
	}
}

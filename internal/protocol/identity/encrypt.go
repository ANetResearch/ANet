package identity

// Encryption plane (C8d foundation): asymmetric encryption keyed off the SAME Ed25519 identity key
// the KEL already publishes, so there is no separate encryption key to publish, rotate, or resolve.
// A controller's X25519 keypair is DERIVED from its Ed25519 keypair (the age/Signal/libsodium
// convention): the X25519 private scalar = clamped SHA-512(seed)[:32] (exactly the scalar Ed25519
// signs with), and the X25519 public = the Montgomery-u form of the Ed25519 public point. A sender
// therefore encrypts to a recipient using only the recipient's KEL-published Ed25519 key.
//
// SealTo is an anonymous sealed box (libsodium crypto_box_seal): a fresh ephemeral X25519 key per
// message, deterministic nonce = blake2b(eph_pub ‖ recipient_pub); the wire form is eph_pub(32) ‖
// box. The sender is anonymous (no sender authentication — pair with a signature if you need it).
//
// CAVEAT (using one key for sign+DH): sharing the Ed25519 key material across Ed25519 signing and
// X25519 DH is the established convention but couples the two algorithms; a dedicated encryption key
// in the KEL is a possible future hardening. Open uses the controller's CURRENT key, so a group key
// sealed to a member must be re-sealed after that member rotates (the keychain handles refresh).

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"fmt"

	"filippo.io/edwards25519"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/nacl/box"
)

const sealEphemeralLen = 32 // X25519 public key prefixed on a sealed box

// x25519PrivateFromEd25519 derives the X25519 private scalar from an Ed25519 private key: the clamped
// first half of SHA-512(seed) — the same scalar Ed25519 derives for signing.
func x25519PrivateFromEd25519(priv ed25519.PrivateKey) [32]byte {
	h := sha512.Sum512(priv.Seed())
	var x [32]byte
	copy(x[:], h[:32])
	x[0] &= 248
	x[31] &= 127
	x[31] |= 64
	return x
}

// X25519PublicFromEd25519 converts an Ed25519 public key to its X25519 (Montgomery-u) form, the key
// a sender seals to. It is the public counterpart of x25519PrivateFromEd25519.
func X25519PublicFromEd25519(pub ed25519.PublicKey) ([32]byte, error) {
	var x [32]byte
	if len(pub) != ed25519.PublicKeySize {
		return x, fmt.Errorf("identity: bad ed25519 public key size %d", len(pub))
	}
	p, err := new(edwards25519.Point).SetBytes(pub)
	if err != nil {
		return x, fmt.Errorf("identity: ed25519->x25519 public: %w", err)
	}
	copy(x[:], p.BytesMontgomery())
	return x, nil
}

// sealNonce is the libsodium crypto_box_seal nonce: blake2b-24(eph_pub ‖ recipient_pub). Determinism
// is safe because the ephemeral key is fresh per message, so (key, nonce) never repeats.
func sealNonce(ephPub, recipientPub [32]byte) [24]byte {
	h, _ := blake2b.New(24, nil) // size 24 is valid; New only errors on out-of-range sizes
	h.Write(ephPub[:])
	h.Write(recipientPub[:])
	var n [24]byte
	copy(n[:], h.Sum(nil))
	return n
}

// SealTo encrypts plaintext to a recipient identified by its Ed25519 public key (anonymous sender).
// The output is eph_pub(32) ‖ box and can only be opened by the holder of the recipient's key.
func SealTo(recipientEdPub ed25519.PublicKey, plaintext []byte) ([]byte, error) {
	rpk, err := X25519PublicFromEd25519(recipientEdPub)
	if err != nil {
		return nil, err
	}
	ephPubP, ephPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	var ephPub [32]byte
	copy(ephPub[:], ephPubP[:])
	nonce := sealNonce(ephPub, rpk)
	out := make([]byte, 0, sealEphemeralLen+len(plaintext)+box.Overhead)
	out = append(out, ephPub[:]...)
	return box.Seal(out, plaintext, &nonce, &rpk, ephPriv), nil
}

// Open decrypts a SealTo ciphertext addressed to this controller's CURRENT key.
func (c *Controller) Open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < sealEphemeralLen+box.Overhead {
		return nil, errors.New("identity: sealed box too short")
	}
	var ephPub [32]byte
	copy(ephPub[:], ciphertext[:sealEphemeralLen])
	xpriv := x25519PrivateFromEd25519(c.cur)
	xpub, err := X25519PublicFromEd25519(c.cur.Public().(ed25519.PublicKey))
	if err != nil {
		return nil, err
	}
	nonce := sealNonce(ephPub, xpub)
	pt, ok := box.Open(nil, ciphertext[sealEphemeralLen:], &nonce, &ephPub, &xpriv)
	if !ok {
		return nil, errors.New("identity: sealed box open failed")
	}
	return pt, nil
}

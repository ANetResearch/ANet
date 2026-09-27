//go:build !no_a2a

package kelresolver

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
)

func controller(t *testing.T) *identity.Controller {
	t.Helper()
	c, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func kels(m map[string][]identity.SignedEvent) Resolver {
	return Resolver{KEL: func(_ context.Context, aid string) ([]identity.SignedEvent, error) {
		kel, ok := m[aid]
		if !ok {
			return nil, ErrUnknownAID
		}
		return kel, nil
	}}
}

func signedCard(t *testing.T, c *identity.Controller) ([]byte, a2a.AgentCardSignature) {
	t.Helper()
	card := []byte(`{"name":"n","description":"d","version":"1","supportedInterfaces":[{"url":"http://127.0.0.1:1/x","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],` +
		`"capabilities":{"streaming":true,"pushNotifications":false},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],` +
		`"skills":[{"id":"chat","name":"chat","description":"d","tags":["chat"]}]}`)
	signed, err := a2acard.SignWithController(card, c, "https://hub.example/agents/x/jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	var parsed a2a.AgentCard
	if err := json.Unmarshal(signed, &parsed); err != nil || len(parsed.Signatures) != 1 {
		t.Fatalf("signed card: %v", err)
	}
	return signed, parsed.Signatures[0]
}

// a2a-go verifies an anet-signed card with the key the KEL gives, and the
// jku the card names plays no part.
func TestVerifiesWithTheKELNotTheJKU(t *testing.T) {
	c := controller(t)
	card, sig := signedCard(t, c)
	v := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: kels(map[string][]identity.SignedEvent{c.AID(): c.KEL()})})
	if err := v.Verify(context.Background(), card, &sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
	key, err := kels(map[string][]identity.SignedEvent{c.AID(): c.KEL()}).ResolveKey(context.Background(), a2acard.KID(c.AID(), c.CurrentSeq()), "https://evil.example/jwks.json")
	if err != nil || !key.(ed25519.PublicKey).Equal(c.CurrentPrivateKey().Public()) {
		t.Fatalf("resolved %v %v", key, err)
	}
}

func TestRefusals(t *testing.T) {
	c, other := controller(t), controller(t)
	card, sig := signedCard(t, c)
	ctx := context.Background()

	// Unknown AID.
	v := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: kels(nil)})
	if err := v.Verify(ctx, card, &sig); err == nil {
		t.Fatal("verified with no key history")
	}
	// A key history that is somebody else's, offered for this AID.
	v = a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: kels(map[string][]identity.SignedEvent{c.AID(): other.KEL()})})
	if err := v.Verify(ctx, card, &sig); err == nil {
		t.Fatal("verified with another AID's key history")
	}
	// A rotated key is not current.
	if err := c.Rotate(1); err != nil {
		t.Fatal(err)
	}
	v = a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: kels(map[string][]identity.SignedEvent{c.AID(): c.KEL()})})
	if err := v.Verify(ctx, card, &sig); err == nil {
		t.Fatal("verified under a rotated key")
	}
	// Not an anet kid.
	if _, err := kels(nil).ResolveKey(ctx, "key-1", ""); err == nil {
		t.Fatal("resolved a kid that is not did:anet")
	}
	if _, err := (Resolver{}).ResolveKey(ctx, a2acard.KID(c.AID(), 0), ""); err == nil {
		t.Fatal("resolved without a source")
	}
	_, err := kels(nil).ResolveKey(ctx, a2acard.KID(c.AID(), 0), "")
	if !errors.Is(err, ErrUnknownAID) {
		t.Fatalf("unknown AID: %v", err)
	}
}

// kelHost answers ResolveKEL from a map; the rest of module.Host is not
// used by FromHost.
type kelHost struct {
	module.Host
	kels map[string][]identity.SignedEvent
}

func (h kelHost) ResolveKEL(aid string) ([]identity.SignedEvent, bool) {
	kel, ok := h.kels[aid]
	return kel, ok
}

// FromHost resolves through the key histories the daemon verified, and an
// AID it has none for is unknown rather than fetched from anywhere.
func TestFromHost(t *testing.T) {
	c := controller(t)
	card, sig := signedCard(t, c)
	ctx := context.Background()
	v := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: FromHost(kelHost{kels: map[string][]identity.SignedEvent{c.AID(): c.KEL()}})})
	if err := v.Verify(ctx, card, &sig); err != nil {
		t.Fatalf("verify through the host: %v", err)
	}
	_, err := FromHost(kelHost{}).ResolveKey(ctx, a2acard.KID(c.AID(), c.CurrentSeq()), "https://hub.example/jwks.json")
	if !errors.Is(err, ErrUnknownAID) {
		t.Fatalf("an AID the host has no history for: %v", err)
	}
}

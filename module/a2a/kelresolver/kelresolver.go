//go:build !no_a2a

// Package kelresolver resolves the key an anet card was signed with, for
// a2a-go's card verifier (a2acrypto.KeyResolver; A2A-DESIGN §10.3).
//
// An anet card names its key as did:anet:<AID>#<seq>. The key is not
// fetched from the card's jku: jku is the signer's own claim (the hub's
// JWKS, at best), and a2acrypto is explicit that a resolver must not take
// trust from it. The key comes from the AID's key event log instead —
// replayed, checked to replay to that AID, and read at its current key
// state — which is what the AID commits to and the only statement about
// the key that the key's holder cannot rewrite after the fact.
//
// Only module/a2a and the contract tests import this package: the daemon
// verifies cards with ANetCore a2acard, which carries no A2A SDK (SI-8).
package kelresolver

import (
	"context"
	"crypto"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
)

// ErrUnknownAID is returned when no key history is known for the kid's AID.
var ErrUnknownAID = errors.New("kelresolver: no key history known for this AID")

// Resolver is an a2acrypto.KeyResolver over key event logs.
type Resolver struct {
	// KEL returns the key history of aid. It is trusted to return what
	// this node holds for aid; the resolver replays it and checks that it
	// is aid's before using it.
	KEL func(ctx context.Context, aid string) ([]identity.SignedEvent, error)
}

var _ a2acrypto.KeyResolver = Resolver{}

// FromHost resolves against the key histories the daemon has verified
// itself (module.Host.ResolveKEL): its own and those of peers it has
// exchanged messages with.
func FromHost(h module.Host) Resolver {
	return Resolver{KEL: func(_ context.Context, aid string) ([]identity.SignedEvent, error) {
		kel, ok := h.ResolveKEL(aid)
		if !ok {
			return nil, ErrUnknownAID
		}
		return kel, nil
	}}
}

// ResolveKey returns the Ed25519 public key named by kid: the AID's
// current key, provided kid's key state is current. untrustedJKU is
// ignored.
func (r Resolver) ResolveKey(ctx context.Context, kid, _ string) (crypto.PublicKey, error) {
	if r.KEL == nil {
		return nil, errors.New("kelresolver: no key history source")
	}
	aid, _, err := a2acard.ParseKID(kid)
	if err != nil {
		return nil, err
	}
	kel, err := r.KEL(ctx, aid)
	if err != nil {
		return nil, fmt.Errorf("kelresolver: %s: %w", aid, err)
	}
	if len(kel) == 0 {
		return nil, fmt.Errorf("kelresolver: %s: %w", aid, ErrUnknownAID)
	}
	// CurrentKey replays the log, checks that it replays to aid, and
	// accepts only a key state no rotation has retired.
	pub, err := a2acard.CurrentKey(kid, kel)
	if err != nil {
		return nil, err
	}
	return pub, nil
}

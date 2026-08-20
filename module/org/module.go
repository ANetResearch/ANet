//go:build !no_org

package org

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// Organisation membership, as a module.
//
// This is the subsystem that taught the lesson the whole seam exists for.
// In anet3 it lived in 931 lines of its own package and 10,075 more spread
// across 27 daemon files — its own methods hung off *Daemon, reaching into
// the store 178 times, the p2p node 54, the HTTP mux 50. It was not
// tightly coupled; it had no boundary at all, and removing it took an axe.
//
// What came across is the part that had a boundary: the O2 credential chain
// from the v3 runtime — genesis, founder-as-CA, admin issuance, member
// credentials, expiry and revocation — 389 lines depending on the kernel
// and nothing else, with its fifteen tests unchanged.
//
// What it offers is one question: is this credential currently valid for
// this org. Membership is a fact the network needs to agree on, so it is a
// capability like any other; everything that made org unremovable last time
// — its own storage, its own routes, its own transport — is deliberately
// not here.
//
// Governance epochs did not come. They need ascpevo.GovernanceCert, a
// design3 protocol package that has not landed in ANetCore yet, and copying
// a protocol type into a module to avoid waiting is exactly the fork that
// produced two diverging kernels last time.
const name = "org"

// Capability ids this module offers.
const (
	CapVerify = "org.verify"
	CapInfo   = "org.info"
)

func init() {
	module.Register(name, func(raw []byte) (module.Module, error) {
		if len(raw) == 0 {
			return nil, nil // compiled in, not configured
		}
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		if cfg.Genesis == "" {
			return nil, fmt.Errorf("org: genesis is required (base64 of the signed genesis)")
		}
		return &Module{cfg: cfg}, nil
	})
}

// Config points the module at the org it serves.
type Config struct {
	// Genesis is the org's signed genesis object, base64 CBOR. A node that
	// cannot state which org it is verifying for has nothing to verify
	// against — there is no default org.
	Genesis string `json:"genesis"`
}

// Module answers membership questions for one org.
type Module struct {
	cfg  Config
	host module.Host

	mu      sync.RWMutex
	genesis *Genesis
	admins  map[string]*Credential // subject AID → admin credential
	revoked map[string]bool        // credential CID → revoked
}

func (m *Module) Name() string { return name }

func (m *Module) Start(ctx context.Context, h module.Host) error {
	b, err := base64.StdEncoding.DecodeString(m.cfg.Genesis)
	if err != nil {
		return fmt.Errorf("org: genesis not base64: %w", err)
	}
	var g Genesis
	if err := coredet.Unmarshal(b, &g); err != nil {
		return fmt.Errorf("org: genesis malformed: %w", err)
	}
	if err := g.Validate(); err != nil {
		return fmt.Errorf("org: genesis invalid: %w", err)
	}
	m.genesis = &g
	m.admins = map[string]*Credential{}
	m.revoked = map[string]bool{}
	m.host = h
	return h.Providers().Register(ctx, &orgProvider{m: m})
}

func (m *Module) Stop(context.Context) error { return nil }

// ForbiddenTokens declares the org id confidential — INV-2.
//
// Which org a node belongs to is not public information, and a node that
// publishes its org id has disclosed its membership to everyone whether or
// not it published a credential. The daemon screens what it publishes
// against whatever comes back here; it is not told what an org is, and
// this is the whole of what it learns.
func (m *Module) ForbiddenTokens() []string {
	id, err := m.OrgID()
	if err != nil || id == "" {
		return nil
	}
	return []string{id}
}

var _ module.Confidential = (*Module)(nil)

// OrgID is the org this module serves.
//
// It is derived from the genesis rather than stored: the id IS the hash of
// the founding set, so an org that claimed a different id than its founders
// produce would be claiming to be an org that does not exist.
func (m *Module) OrgID() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.genesis == nil {
		return "", fmt.Errorf("org: not started")
	}
	return m.genesis.OrgID()
}

// TrustAdmin records an admin credential this node has verified, so member
// credentials issued by that admin can be checked later.
//
// Admin credentials are learned, not configured: the founder issues them
// and they travel with the members they authorise. A node that has not seen
// an admin's credential cannot verify what that admin signed, and refusing
// is the correct answer — the alternative is trusting an issuer on the
// strength of a name.
func (m *Module) TrustAdmin(c *Credential) {
	if c == nil || c.Role != RoleAdmin {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.admins[c.Subject] = c
}

// Revoke marks a credential CID revoked from now on.
func (m *Module) Revoke(credCID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revoked[credCID] = true
}

func (m *Module) verifier() (*Verifier, error) {
	m.mu.RLock()
	g := m.genesis
	m.mu.RUnlock()
	if g == nil {
		return nil, fmt.Errorf("org: not started")
	}
	return NewVerifier(g, m.resolveKEL, m.lookupAdmin, m.isRevoked)
}

func (m *Module) resolveKEL(aid string) ([]identity.SignedEvent, bool) {
	if m.host == nil {
		return nil, false
	}
	return m.host.ResolveKEL(aid)
}

func (m *Module) lookupAdmin(aid string) (*Credential, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.admins[aid]
	return c, ok
}

func (m *Module) isRevoked(credCID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.revoked[credCID]
}

// orgProvider is the C1 face.
type orgProvider struct{ m *Module }

func (p *orgProvider) ID() string { return name }

func (p *orgProvider) Capabilities(context.Context) ([]string, error) {
	return []string{CapVerify, CapInfo}, nil
}

func (p *orgProvider) Describe(context.Context) (string, error) { return "", nil }

func (p *orgProvider) Health(context.Context) error {
	if p.m.genesis == nil {
		return fmt.Errorf("org: not started")
	}
	return nil
}

func (p *orgProvider) Invoke(_ context.Context, call provider.Call) (effect.Effect, error) {
	switch call.Capability {
	case CapVerify:
		return p.verify(call)
	case CapInfo:
		return p.info()
	}
	return effect.Effect{}, fmt.Errorf("org: unknown capability %q", call.Capability)
}

// verify answers whether a credential is valid for this org right now.
//
// "Right now" is the whole point: a credential carries an expiry and can be
// revoked, so a verdict without a time is not a verdict. The answer is an
// effect with evidence rather than a bare boolean, because a membership
// decision is exactly the kind of claim someone will later need to audit.
func (p *orgProvider) verify(call provider.Call) (effect.Effect, error) {
	raw, _ := call.Args["credential"].(string)
	if raw == "" {
		return effect.Effect{}, fmt.Errorf("org: verify needs a base64 credential in args.credential")
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return effect.Effect{}, fmt.Errorf("org: credential not base64: %w", err)
	}
	cred, err := UnmarshalCredential(b)
	if err != nil {
		return effect.Effect{}, err
	}
	v, err := p.m.verifier()
	if err != nil {
		return effect.Effect{}, err
	}
	now := time.Now().UnixMilli()
	if err := v.VerifyMembership(cred, now); err != nil {
		// A refusal is a result, not an error: the caller asked a question
		// and this is the answer, with the reason attached.
		return effect.Effect{
			Status:  effect.Failed,
			Message: err.Error(),
			Record:  &tsir.EffectRecord{Metrics: map[string]float64{"valid": 0}},
			Evidence: &effect.Evidence{
				Protocol: name, Requested: CapVerify,
				ObservedState: cred.Role, VerifyTrust: 3,
			},
		}, nil
	}
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"valid": 1}},
		Evidence: &effect.Evidence{
			Protocol: name, Requested: CapVerify, NativeAck: true,
			ObservedState: cred.Role, VerifyTrust: 3,
		},
	}, nil
}

// info states which org this node verifies for.
func (p *orgProvider) info() (effect.Effect, error) {
	id, err := p.m.OrgID()
	if err != nil {
		return effect.Effect{}, err
	}
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"admins": float64(len(p.m.admins))}},
		Evidence: &effect.Evidence{
			Protocol: name, Requested: CapInfo, NativeAck: true,
			ObservedState: id, VerifyTrust: 3,
		},
	}, nil
}

var _ provider.CapabilityProvider = (*orgProvider)(nil)

//go:build !no_org

package org

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

type orgHost struct {
	reg  *provider.Registry
	kels map[string][]identity.SignedEvent
}

func (h *orgHost) AID() string                      { return "node-1" }
func (h *orgHost) Providers() *provider.Registry    { return h.reg }
func (h *orgHost) RecordEvidence(string, any) error { return nil }
func (h *orgHost) ResolveKEL(aid string) ([]identity.SignedEvent, bool) {
	k, ok := h.kels[aid]
	return k, ok
}

var _ module.Host = (*orgHost)(nil)

// founded builds an org with one founder and starts the module for it.
func founded(t *testing.T) (*Module, *orgHost, *identity.Controller) {
	t.Helper()
	founder, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	g := &Genesis{GovernanceRoot: []string{founder.AID()}, M: 1, Nonce: "n1"}
	raw, err := coredet.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	h := &orgHost{reg: provider.NewRegistry(),
		kels: map[string][]identity.SignedEvent{founder.AID(): founder.KEL()}}
	m := &Module{cfg: Config{Genesis: base64.StdEncoding.EncodeToString(raw)}}
	if err := m.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop(context.Background()) })
	return m, h, founder
}

func memberCred(t *testing.T, issuer *identity.Controller, orgID, subject, role string) []byte {
	t.Helper()
	now := time.Now().UnixMilli()
	c := &Credential{
		OrgID: orgID, Subject: subject, Role: role,
		IssuedAt: now, NotAfter: now + int64(time.Hour/time.Millisecond),
	}
	if err := c.Sign(issuer); err != nil {
		t.Fatal(err)
	}
	// Both halves: the signature is detached, so marshalling the credential
	// alone would send the fields without the signature over them.
	raw, err := MarshalCredential(c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Membership reaches the network as a capability, like anything else.
func TestOrgIsOfferedThroughC1(t *testing.T) {
	_, h, _ := founded(t)
	for _, capID := range []string{CapVerify, CapInfo} {
		if _, ok := h.reg.Resolve(capID); !ok {
			t.Errorf("capability %q must be resolvable", capID)
		}
	}
}

// The org id is derived from the founding set, not configured: an org that
// could name itself anything would not be an org anyone could verify.
func TestOrgIDComesFromTheFoundingSet(t *testing.T) {
	m, h, _ := founded(t)
	id, err := m.OrgID()
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("an org must have a derived id")
	}
	p, _ := h.reg.Resolve(CapInfo)
	eff, err := p.Invoke(context.Background(), provider.Call{Capability: CapInfo})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Evidence.ObservedState != id {
		t.Fatalf("info reports %q, want the derived org id %q", eff.Evidence.ObservedState, id)
	}
}

// A credential from an issuer this node cannot resolve is refused — the
// same rule as the blackboard, for the same reason.
func TestCredentialFromUnknownIssuerIsRefused(t *testing.T) {
	m, h, _ := founded(t)
	stranger, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	id, _ := m.OrgID()
	raw := memberCred(t, stranger, id, "some-subject", RoleMember)

	p, _ := h.reg.Resolve(CapVerify)
	eff, err := p.Invoke(context.Background(), provider.Call{
		Capability: CapVerify,
		Args:       map[string]any{"credential": base64.StdEncoding.EncodeToString(raw)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Failed {
		t.Fatalf("status = %v, want FAILED for an unresolvable issuer", eff.Status)
	}
	if eff.Record.Metrics["valid"] != 0 {
		t.Error("a refused credential must report valid=0")
	}
}

// A verdict is an effect with evidence, not a bare boolean: a membership
// decision is exactly the kind of claim someone audits later.
func TestRefusalCarriesItsReason(t *testing.T) {
	m, h, founder := founded(t)
	id, _ := m.OrgID()

	// Expired on arrival.
	now := time.Now().UnixMilli()
	c := &Credential{OrgID: id, Subject: "s", Role: RoleMember,
		IssuedAt: now - 2000, NotAfter: now - 1000}
	if err := c.Sign(founder); err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalCredential(c)
	if err != nil {
		t.Fatal(err)
	}

	p, _ := h.reg.Resolve(CapVerify)
	eff, err := p.Invoke(context.Background(), provider.Call{
		Capability: CapVerify,
		Args:       map[string]any{"credential": base64.StdEncoding.EncodeToString(raw)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Failed {
		t.Fatalf("an expired credential must not verify, got %v", eff.Status)
	}
	if eff.Message == "" {
		t.Error("the refusal must say why")
	}
	if eff.Evidence == nil || eff.Evidence.Requested != CapVerify {
		t.Errorf("a membership verdict must carry evidence: %+v", eff.Evidence)
	}
}

// Revocation takes effect immediately: a credential valid a moment ago is
// refused once revoked, which is the property expiry alone cannot give.
func TestRevocationIsImmediate(t *testing.T) {
	m, h, founder := founded(t)
	id, _ := m.OrgID()
	now := time.Now().UnixMilli()
	c := &Credential{OrgID: id, Subject: "s", Role: RoleMember,
		IssuedAt: now, NotAfter: now + 3600_000}
	if err := c.Sign(founder); err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalCredential(c)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := c.CID()
	if err != nil {
		t.Fatal(err)
	}

	p, _ := h.reg.Resolve(CapVerify)
	args := map[string]any{"credential": base64.StdEncoding.EncodeToString(raw)}

	before, err := p.Invoke(context.Background(), provider.Call{Capability: CapVerify, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != effect.OK {
		t.Fatalf("a founder-issued member credential should verify: %s", before.Message)
	}

	m.Revoke(cid)
	after, err := p.Invoke(context.Background(), provider.Call{Capability: CapVerify, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != effect.Failed {
		t.Fatal("a revoked credential must stop verifying at once")
	}
}

// A node cannot start an org module without saying which org it serves.
func TestGenesisIsRequired(t *testing.T) {
	_, err := module.Build(map[string][]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &Module{cfg: Config{Genesis: "not base64!!"}}
	err = m.Start(context.Background(), &orgHost{reg: provider.NewRegistry()})
	if err == nil || !strings.Contains(err.Error(), "genesis") {
		t.Fatalf("a malformed genesis must be refused, got %v", err)
	}
}

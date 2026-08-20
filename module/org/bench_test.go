//go:build !no_org

package org

import (
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
)

// Membership is checked on every org-scoped action, so verification cost is
// paid constantly. It is dominated by signature checking — one for a
// founder-issued credential, two when an admin issued it and the admin's own
// credential has to be checked as well.

func benchVerifier(b *testing.B) (*Verifier, *Credential, *Credential) {
	b.Helper()
	founder, err := identity.Incept()
	if err != nil {
		b.Fatal(err)
	}
	admin, err := identity.Incept()
	if err != nil {
		b.Fatal(err)
	}
	g := &Genesis{GovernanceRoot: []string{founder.AID()}, M: 1, Nonce: "bench"}
	orgID, err := g.OrgID()
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().UnixMilli()
	mk := func(issuer *identity.Controller, subject, role string) *Credential {
		c := &Credential{OrgID: orgID, Subject: subject, Role: role,
			IssuedAt: now, NotAfter: now + 3600_000}
		if err := c.Sign(issuer); err != nil {
			b.Fatal(err)
		}
		return c
	}
	adminCred := mk(founder, admin.AID(), RoleAdmin)
	memberCred := mk(admin, "member-aid", RoleMember)
	direct := mk(founder, "direct-aid", RoleMember)

	kels := map[string][]identity.SignedEvent{
		founder.AID(): founder.KEL(), admin.AID(): admin.KEL(),
	}
	v, err := NewVerifier(g,
		func(aid string) ([]identity.SignedEvent, bool) { k, ok := kels[aid]; return k, ok },
		func(aid string) (*Credential, bool) {
			if aid == admin.AID() {
				return adminCred, true
			}
			return nil, false
		},
		func(string) bool { return false })
	if err != nil {
		b.Fatal(err)
	}
	return v, direct, memberCred
}

// A founder-issued credential: one signature to check.
func BenchmarkVerifyFounderIssued(b *testing.B) {
	v, direct, _ := benchVerifier(b)
	now := time.Now().UnixMilli()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := v.VerifyMembership(direct, now); err != nil {
			b.Fatal(err)
		}
	}
}

// An admin-issued credential: the member's signature and the admin's own
// credential, which is the ordinary case in an org of any size.
func BenchmarkVerifyAdminIssued(b *testing.B) {
	v, _, member := benchVerifier(b)
	now := time.Now().UnixMilli()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := v.VerifyMembership(member, now); err != nil {
			b.Fatal(err)
		}
	}
}

// Marshalling for the wire carries both halves; it is on the path of every
// credential handed to a peer.
func BenchmarkMarshalCredential(b *testing.B) {
	_, direct, _ := benchVerifier(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := MarshalCredential(direct); err != nil {
			b.Fatal(err)
		}
	}
}

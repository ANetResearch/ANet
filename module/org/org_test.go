package org_test

import (
	"errors"
	"testing"

	"github.com/ANetResearch/ANet/module/org"
	"github.com/ANetResearch/ANetCore/identity"
)

const (
	t0   = int64(1_000_000) // issued-at (unix-millis)
	tEnd = int64(2_000_000) // not-after
	tNow = int64(1_500_000) // within window
)

func resolver(ctrls ...*identity.Controller) org.KELResolver {
	m := map[string][]identity.SignedEvent{}
	for _, c := range ctrls {
		m[c.AID()] = c.KEL()
	}
	return func(aid string) ([]identity.SignedEvent, bool) {
		k, ok := m[aid]
		return k, ok
	}
}

func mkCred(t *testing.T, issuer *identity.Controller, orgID, subject, role string) *org.Credential {
	t.Helper()
	c := &org.Credential{OrgID: orgID, Subject: subject, Role: role, IssuedAt: t0, NotAfter: tEnd}
	if err := c.Sign(issuer); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return c
}

// org_id is deterministic and invariant to GovernanceRoot ordering (D5 normalization).
func TestOrgIDStableAndOrderInvariant(t *testing.T) {
	g1 := &org.Genesis{GovernanceRoot: []string{"f3", "f1", "f2"}, M: 2, Nonce: "n1"}
	g2 := &org.Genesis{GovernanceRoot: []string{"f1", "f2", "f3"}, M: 2, Nonce: "n1"}
	id1, err := g1.OrgID()
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := g2.OrgID()
	if id1 != id2 {
		t.Fatalf("org_id not order-invariant: %s vs %s", id1, id2)
	}
	// A different nonce → different org even with the same founders.
	g3 := &org.Genesis{GovernanceRoot: []string{"f1", "f2", "f3"}, M: 2, Nonce: "n2"}
	if id3, _ := g3.OrgID(); id3 == id1 {
		t.Fatal("distinct nonce must yield distinct org_id")
	}
}

func TestGenesisValidate(t *testing.T) {
	if err := (&org.Genesis{GovernanceRoot: nil, M: 1}).Validate(); !errors.Is(err, org.ErrBadGenesis) {
		t.Fatal("empty root must fail")
	}
	if err := (&org.Genesis{GovernanceRoot: []string{"a", "b"}, M: 3}).Validate(); !errors.Is(err, org.ErrBadGenesis) {
		t.Fatal("M>N must fail")
	}
	if err := (&org.Genesis{GovernanceRoot: []string{"a", "b"}, M: 0}).Validate(); !errors.Is(err, org.ErrBadGenesis) {
		t.Fatal("M<1 must fail")
	}
	if err := (&org.Genesis{GovernanceRoot: []string{"a", "b"}, M: 2}).Validate(); err != nil {
		t.Fatalf("valid genesis: %v", err)
	}
}

// Founder→admin→member is the happy chain; each tier verifies.
func TestChainHappyPath(t *testing.T) {
	f1, _ := identity.Incept()
	f2, _ := identity.Incept()
	admin, _ := identity.Incept()
	member, _ := identity.Incept()
	g := &org.Genesis{GovernanceRoot: []string{f1.AID(), f2.AID()}, M: 2, Nonce: "org"}
	orgID, _ := g.OrgID()
	res := resolver(f1, f2, admin, member)

	// Issuance is single-signature (spec §O2): one founder acting as CA issues the admin credential,
	// even in an M>1 org — the M-of-N threshold gates the governance epoch chain, not issuance.
	adminCred := mkCred(t, f1, orgID, admin.AID(), org.RoleAdmin)       // founder issues admin (single sig)
	memberCred := mkCred(t, admin, orgID, member.AID(), org.RoleMember) // admin issues member
	lookup := func(aid string) (*org.Credential, bool) {
		if aid == admin.AID() {
			return adminCred, true
		}
		return nil, false
	}
	if err := org.VerifyMembership(adminCred, g, tNow, res, lookup, nil); err != nil {
		t.Fatalf("admin cred: %v", err)
	}
	if err := org.VerifyMembership(memberCred, g, tNow, res, lookup, nil); err != nil {
		t.Fatalf("member cred: %v", err)
	}
	// A founder may also issue a member directly (no admin lookup needed).
	direct := mkCred(t, f2, orgID, member.AID(), org.RoleMember)
	if err := org.VerifyMembership(direct, g, tNow, res, nil, nil); err != nil {
		t.Fatalf("founder-issued member: %v", err)
	}
}

// A non-founder cannot mint an admin credential.
func TestNonFounderAdminRejected(t *testing.T) {
	f1, _ := identity.Incept()
	notFounder, _ := identity.Incept()
	target, _ := identity.Incept()
	g := &org.Genesis{GovernanceRoot: []string{f1.AID()}, M: 1, Nonce: "org"}
	orgID, _ := g.OrgID()
	bad := mkCred(t, notFounder, orgID, target.AID(), org.RoleAdmin)
	err := org.VerifyMembership(bad, g, tNow, resolver(f1, notFounder, target), nil, nil)
	if !errors.Is(err, org.ErrIssuerNotFounder) {
		t.Fatalf("want ErrIssuerNotFounder, got %v", err)
	}
}

// A member credential issued by a stranger (neither founder nor admin) is rejected.
func TestStrangerMemberRejected(t *testing.T) {
	f1, _ := identity.Incept()
	stranger, _ := identity.Incept()
	member, _ := identity.Incept()
	g := &org.Genesis{GovernanceRoot: []string{f1.AID()}, M: 1, Nonce: "org"}
	orgID, _ := g.OrgID()
	cred := mkCred(t, stranger, orgID, member.AID(), org.RoleMember)
	// lookupAdmin returns nothing for the stranger.
	err := org.VerifyMembership(cred, g, tNow, resolver(f1, stranger, member), func(string) (*org.Credential, bool) { return nil, false }, nil)
	if !errors.Is(err, org.ErrIssuerNotAuth) {
		t.Fatalf("want ErrIssuerNotAuth, got %v", err)
	}
}

// A member whose issuing "admin" was never actually granted admin (forged lookup) is rejected
// because the admin credential itself fails verification (not founder-issued).
func TestForgedAdminChainRejected(t *testing.T) {
	f1, _ := identity.Incept()
	fakeAdmin, _ := identity.Incept()
	member, _ := identity.Incept()
	g := &org.Genesis{GovernanceRoot: []string{f1.AID()}, M: 1, Nonce: "org"}
	orgID, _ := g.OrgID()
	// fakeAdmin self-issues its own "admin" credential (not founder-issued).
	selfAdmin := mkCred(t, fakeAdmin, orgID, fakeAdmin.AID(), org.RoleAdmin)
	memberCred := mkCred(t, fakeAdmin, orgID, member.AID(), org.RoleMember)
	lookup := func(aid string) (*org.Credential, bool) {
		if aid == fakeAdmin.AID() {
			return selfAdmin, true
		}
		return nil, false
	}
	err := org.VerifyMembership(memberCred, g, tNow, resolver(f1, fakeAdmin, member), lookup, nil)
	if !errors.Is(err, org.ErrIssuerNotAuth) {
		t.Fatalf("forged admin chain must fail with ErrIssuerNotAuth, got %v", err)
	}
}

func TestWrongOrg(t *testing.T) {
	f1, _ := identity.Incept()
	m, _ := identity.Incept()
	g := &org.Genesis{GovernanceRoot: []string{f1.AID()}, M: 1, Nonce: "org"}
	cred := mkCred(t, f1, "bafy-some-other-org", m.AID(), org.RoleMember)
	if err := org.VerifyMembership(cred, g, tNow, resolver(f1, m), nil, nil); !errors.Is(err, org.ErrWrongOrg) {
		t.Fatalf("want ErrWrongOrg, got %v", err)
	}
}

func TestExpiredAndRevoked(t *testing.T) {
	f1, _ := identity.Incept()
	m, _ := identity.Incept()
	g := &org.Genesis{GovernanceRoot: []string{f1.AID()}, M: 1, Nonce: "org"}
	orgID, _ := g.OrgID()
	cred := mkCred(t, f1, orgID, m.AID(), org.RoleMember)
	res := resolver(f1, m)

	if err := org.VerifyMembership(cred, g, tEnd+1, res, nil, nil); !errors.Is(err, org.ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	if err := org.VerifyMembership(cred, g, t0-1, res, nil, nil); !errors.Is(err, org.ErrExpired) {
		t.Fatalf("before window want ErrExpired, got %v", err)
	}
	cid, _ := cred.CID()
	revoked := func(c string) bool { return c == cid }
	if err := org.VerifyMembership(cred, g, tNow, res, nil, revoked); !errors.Is(err, org.ErrRevoked) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
}

// Mutating ANY signed field after signing must cause verify to reject (proves the signature
// covers the whole preimage, not just Role).
func TestTamperRejectsEverySignedField(t *testing.T) {
	f1, _ := identity.Incept()
	m, _ := identity.Incept()
	other, _ := identity.Incept()
	g := &org.Genesis{GovernanceRoot: []string{f1.AID()}, M: 1, Nonce: "org"}
	orgID, _ := g.OrgID()
	res := resolver(f1, m, other)

	muts := map[string]func(*org.Credential){
		"role":      func(c *org.Credential) { c.Role = org.RoleGuest },
		"subject":   func(c *org.Credential) { c.Subject = other.AID() },
		"org_id":    func(c *org.Credential) { c.OrgID = "bafy-other" },
		"not_after": func(c *org.Credential) { c.NotAfter = tEnd + 1_000_000 }, // extend expiry
		"issued_at": func(c *org.Credential) { c.IssuedAt = t0 - 100 },         // still >0, <now
	}
	for name, mut := range muts {
		cred := mkCred(t, f1, orgID, m.AID(), org.RoleMember)
		mut(cred)
		if err := org.VerifyMembership(cred, g, tNow, res, nil, nil); err == nil {
			t.Fatalf("tamper %q must be rejected", name)
		}
	}
	// Unsigned.
	un := &org.Credential{OrgID: orgID, Subject: m.AID(), Role: org.RoleMember, IssuedAt: t0, NotAfter: tEnd}
	if err := org.VerifyMembership(un, g, tNow, res, nil, nil); !errors.Is(err, org.ErrUnsigned) {
		t.Fatalf("want ErrUnsigned, got %v", err)
	}
}

// An admin cannot mint another admin (single-CA model: admins are founder-issued only).
func TestAdminIssuesAdminRejected(t *testing.T) {
	f1, _ := identity.Incept()
	admin, _ := identity.Incept()
	target, _ := identity.Incept()
	g := &org.Genesis{GovernanceRoot: []string{f1.AID()}, M: 1, Nonce: "org"}
	orgID, _ := g.OrgID()
	// admin tries to issue an admin credential.
	cred := mkCred(t, admin, orgID, target.AID(), org.RoleAdmin)
	if err := org.VerifyMembership(cred, g, tNow, resolver(f1, admin, target), nil, nil); !errors.Is(err, org.ErrIssuerNotFounder) {
		t.Fatalf("admin-issues-admin: want ErrIssuerNotFounder, got %v", err)
	}
}

// The cred.Epoch field is informational (a cred at a non-zero epoch still verifies), and issuance
// authority tracks the CURRENT governance root: SetCurrentRoot revokes a removed founder's authority and
// grants an added founder's — the governance-turnover semantic the epoch chain drives.
func TestEpochInformationalAndDynamicRoot(t *testing.T) {
	f1, _ := identity.Incept()
	fx, _ := identity.Incept() // a founder ADDED by a later epoch
	m, _ := identity.Incept()
	g := &org.Genesis{GovernanceRoot: []string{f1.AID()}, M: 1, Nonce: "org"}
	orgID, _ := g.OrgID()
	res := resolver(f1, fx, m)

	// a non-zero-epoch cred from the genesis founder verifies (epoch is informational, not fail-closed).
	c := &org.Credential{OrgID: orgID, Subject: m.AID(), Role: org.RoleMember, Epoch: 2, IssuedAt: t0, NotAfter: tEnd}
	if err := c.Sign(f1); err != nil {
		t.Fatal(err)
	}
	v, err := org.NewVerifier(g, res, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.VerifyMembership(c, tNow); err != nil {
		t.Fatalf("a non-zero-epoch cred from a current founder must verify: %v", err)
	}
	// advance the root to {fx} (f1 removed): f1's cred loses authority, fx's gains it.
	v.SetCurrentRoot([]string{fx.AID()})
	if err := v.VerifyMembership(c, tNow); err == nil {
		t.Fatal("a removed founder's cred must lose authority after the root advances")
	}
	cx := &org.Credential{OrgID: orgID, Subject: m.AID(), Role: org.RoleMember, Epoch: 2, IssuedAt: t0, NotAfter: tEnd}
	if err := cx.Sign(fx); err != nil {
		t.Fatal(err)
	}
	if err := v.VerifyMembership(cx, tNow); err != nil {
		t.Fatalf("an added founder must gain issuance authority: %v", err)
	}
}

// Unbounded / inverted validity windows are rejected (mandatory bounded expiry).
func TestBadWindowRejected(t *testing.T) {
	f1, _ := identity.Incept()
	m, _ := identity.Incept()
	g := &org.Genesis{GovernanceRoot: []string{f1.AID()}, M: 1, Nonce: "org"}
	orgID, _ := g.OrgID()
	res := resolver(f1, m)
	for name, c := range map[string]*org.Credential{
		"notafter zero":    {OrgID: orgID, Subject: m.AID(), Role: org.RoleMember, IssuedAt: t0, NotAfter: 0},
		"notafter<=issued": {OrgID: orgID, Subject: m.AID(), Role: org.RoleMember, IssuedAt: t0, NotAfter: t0},
		"issuedat zero":    {OrgID: orgID, Subject: m.AID(), Role: org.RoleMember, IssuedAt: 0, NotAfter: tEnd},
	} {
		if err := c.Sign(f1); err != nil {
			t.Fatal(err)
		}
		if err := org.VerifyMembership(c, g, tNow, res, nil, nil); !errors.Is(err, org.ErrBadWindow) {
			t.Fatalf("%s: want ErrBadWindow, got %v", name, err)
		}
	}
}

// The Issue helper mints valid credentials and rejects bad inputs.
func TestIssueHelper(t *testing.T) {
	f1, _ := identity.Incept()
	m, _ := identity.Incept()
	g := &org.Genesis{GovernanceRoot: []string{f1.AID()}, M: 1, Nonce: "org"}

	cred, err := g.Issue(f1, m.AID(), org.RoleMember, t0, tEnd)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := org.VerifyMembership(cred, g, tNow, resolver(f1, m), nil, nil); err != nil {
		t.Fatalf("issued cred must verify: %v", err)
	}
	if _, err := g.Issue(f1, m.AID(), "bogus", t0, tEnd); !errors.Is(err, org.ErrBadRole) {
		t.Fatalf("bad role: want ErrBadRole, got %v", err)
	}
	if _, err := g.Issue(f1, m.AID(), org.RoleMember, tEnd, t0); !errors.Is(err, org.ErrBadWindow) {
		t.Fatalf("inverted window: want ErrBadWindow, got %v", err)
	}
}

// A Verifier constructed once validates the genesis and verifies many creds; an invalid genesis
// is refused at construction.
func TestVerifierReuseAndGenesisGate(t *testing.T) {
	f1, _ := identity.Incept()
	a, _ := identity.Incept()
	b, _ := identity.Incept()
	g := &org.Genesis{GovernanceRoot: []string{f1.AID()}, M: 1, Nonce: "org"}
	v, err := org.NewVerifier(g, resolver(f1, a, b), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := g.Issue(f1, a.AID(), org.RoleMember, t0, tEnd)
	cb, _ := g.Issue(f1, b.AID(), org.RoleMember, t0, tEnd)
	if err := v.VerifyMembership(ca, tNow); err != nil {
		t.Fatalf("cred a: %v", err)
	}
	if err := v.VerifyMembership(cb, tNow); err != nil {
		t.Fatalf("cred b: %v", err)
	}
	// Invalid genesis (M>N) refused at construction.
	bad := &org.Genesis{GovernanceRoot: []string{f1.AID()}, M: 5}
	if _, err := org.NewVerifier(bad, resolver(f1), nil, nil); !errors.Is(err, org.ErrBadGenesis) {
		t.Fatalf("invalid genesis must be refused, got %v", err)
	}
}

// Issuance is single-signature even in an M>1 org (spec §O2): ONE founder acting as CA suffices to
// issue an admin credential. The M-of-N threshold governs the separate epoch'd governance chain
// (GovernanceCert, see govepoch_test.go), not routine credential issuance.
func TestAdminIssuanceSingleSigUnderHighM(t *testing.T) {
	f1, _ := identity.Incept()
	f2, _ := identity.Incept()
	f3, _ := identity.Incept()
	admin, _ := identity.Incept()
	g := &org.Genesis{GovernanceRoot: []string{f1.AID(), f2.AID(), f3.AID()}, M: 2, Nonce: "gov"}
	orgID, _ := g.OrgID()
	res := resolver(f1, f2, f3, admin)

	// a single founder's signature is sufficient to grant admin (delegated-CA model), regardless of M.
	c1 := mkCred(t, f1, orgID, admin.AID(), org.RoleAdmin)
	if err := org.VerifyMembership(c1, g, tNow, res, nil, nil); err != nil {
		t.Fatalf("single founder must be able to issue an admin cred under M=2: %v", err)
	}
	// a NON-founder still cannot mint an admin credential (the founder check is unchanged).
	notFounder, _ := identity.Incept()
	res2 := resolver(f1, f2, f3, admin, notFounder)
	bad := mkCred(t, notFounder, orgID, admin.AID(), org.RoleAdmin)
	if err := org.VerifyMembership(bad, g, tNow, res2, nil, nil); err == nil {
		t.Fatal("a non-founder must not be able to issue an admin credential")
	}
}

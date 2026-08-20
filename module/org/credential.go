package org

import (
	"errors"
	"fmt"

	"github.com/ANetResearch/ANet/module/inv1"
	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
)

// OrgScopedObject marks a Credential as org-scoped (INV-1 runtime type tag): a membership credential
// must never reach a commons publish path. Value receiver so both Credential and *Credential are
// caught by inv1.GuardCommonsPublish. Compile-time assertion that the marker is satisfied:
var _ inv1.OrgScoped = Credential{}

func (Credential) OrgScopedObject() {}

// Credential is a membership grant: a signed AObj asserting that Subject holds Role in the org
// identified by OrgID, issued (single-signature) by the envelope's signer. The envelope is
// detached (cbor:"-"), never inside the signed map (mirrors acc/aet/tsir discipline) — so the
// CanonicalPreimage marshals the Credential directly and every signed field is covered by
// construction (adding a field automatically signs it; only cbor:"-" fields are excluded).
type Credential struct {
	OrgID    string `cbor:"1,keyasint"`
	Subject  string `cbor:"2,keyasint"`           // the AID being granted membership
	Role     string `cbor:"3,keyasint"`           // RoleAdmin / RoleMember / RoleGuest
	Epoch    uint64 `cbor:"4,keyasint,omitempty"` // governance epoch (see VerifyMembership: only 0 accepted today)
	IssuedAt int64  `cbor:"5,keyasint"`           // unix-millis; also the envelope's binding time (revocation gate)
	NotAfter int64  `cbor:"6,keyasint"`           // unix-millis expiry (mandatory; must be > IssuedAt)

	// Envelope is the detached issuer signature; Envelope.SignerAID is the issuer (founder or admin).
	// Issuance is SINGLE-SIGNATURE throughout the chain root→admin→member (spec §O2: a founder acts as
	// CA to issue admin creds, an admin/founder issues member creds — "delegated admin without bothering
	// the governance layer"). The M-of-N threshold is NOT applied here; it governs the separate epoch'd
	// governance event chain (GovernanceCert), see internal/v3rt/org/govepoch.go.
	Envelope *aobj.Envelope `cbor:"-"`
}

// Credential / verification errors.
var (
	ErrUnsigned         = errors.New("org: unsigned credential")
	ErrWrongOrg         = errors.New("org: credential org_id does not match genesis")
	ErrBadRole          = errors.New("org: unknown role")
	ErrBadWindow        = errors.New("org: invalid validity window (need 0 < issued_at < not_after)")
	ErrExpired          = errors.New("org: credential outside its validity window")
	ErrRevoked          = errors.New("org: credential revoked")
	ErrUnknownIssuer    = errors.New("org: issuer KEL unresolvable")
	ErrIssuerNotFounder = errors.New("org: admin credential not issued by a GovernanceRoot founder")
	ErrIssuerNotAuth    = errors.New("org: member credential issuer is neither founder nor a valid admin")
	ErrNilCredential    = errors.New("org: nil credential")
)

// KELResolver resolves an AID to its key-event log for signature verification.
type KELResolver func(aid string) (kel []identity.SignedEvent, ok bool)

// AdminLookup returns the admin credential held by an AID (from the org-central store), used to
// verify a member credential whose issuer is an admin rather than a founder. ok=false if none.
type AdminLookup func(adminAID string) (*Credential, bool)

// CanonicalPreimage is the CoreDet-CBOR signing/CID preimage. The detached Envelope (cbor:"-")
// is excluded by construction; all other fields are signed.
func (c *Credential) CanonicalPreimage() ([]byte, error) {
	return coredet.Marshal(c)
}

// CID returns the credential content id over its canonical preimage.
func (c *Credential) CID() (string, error) {
	pre, err := c.CanonicalPreimage()
	if err != nil {
		return "", err
	}
	return anetcid.Sum(pre)
}

// Sign attaches the issuer's detached signature (sign-binds-CID). The issuer is ctrl.
func (c *Credential) Sign(ctrl *identity.Controller) error {
	pre, err := c.CanonicalPreimage()
	if err != nil {
		return err
	}
	sig, seq := ctrl.Sign(pre)
	c.Envelope = &aobj.Envelope{SignerAID: ctrl.AID(), KeyStateSeq: seq, Alg: aobj.AlgEdDSA, Sig: sig}
	return nil
}

// verifyEnvelope checks the issuer signature against the issuer KEL (revocation gate as-of
// IssuedAt, which checkWindow has already constrained to be > 0) and returns the issuer AID.
func (c *Credential) verifyEnvelope(resolve KELResolver) (string, error) {
	pre, err := c.CanonicalPreimage()
	if err != nil {
		return "", err
	}
	return verifyOneSig(c.Envelope, pre, c.IssuedAt, resolve)
}

// verifyOneSig validates one detached envelope over pre (as-of issuedAt) and returns its signer AID.
func verifyOneSig(env *aobj.Envelope, pre []byte, issuedAt int64, resolve KELResolver) (string, error) {
	if env == nil {
		return "", ErrUnsigned
	}
	if err := env.Validate(); err != nil {
		return "", err
	}
	kel, ok := resolve(env.SignerAID)
	if !ok {
		return "", ErrUnknownIssuer
	}
	if err := identity.VerifyObject(kel, env.SignerAID, env.KeyStateSeq, uint64(issuedAt), pre, env.Sig); err != nil {
		return "", err
	}
	return env.SignerAID, nil
}

// checkWindow enforces a mandatory bounded validity (0 < IssuedAt < NotAfter) and that now is
// within [IssuedAt, NotAfter]. Bounded validity caps the blast radius of a backdated/forged
// credential (see identity-rotation-gate-gap).
func (c *Credential) checkWindow(now int64) error {
	if c.IssuedAt <= 0 || c.NotAfter <= c.IssuedAt {
		return ErrBadWindow
	}
	if now < c.IssuedAt || now > c.NotAfter {
		return ErrExpired
	}
	return nil
}

// Verifier authorizes credentials against one org. Construct it ONCE (it validates the genesis
// and caches org_id) and call VerifyMembership per request — the store-backed resolve/lookupAdmin/
// revoked are held for the verifier's lifetime rather than threaded per call.
type Verifier struct {
	g           *Genesis
	orgID       string
	resolve     KELResolver
	lookupAdmin AdminLookup               // may be nil if only founder-issued creds are expected
	revoked     func(credCID string) bool // may be nil = nothing revoked (a footgun; supply it in production)
	// curRoot is the CURRENT GovernanceRoot — the inception's set by default, but UPDATED by the
	// governance epoch chain (SetCurrentRoot) so a founder added/removed by an advance gains/loses
	// issuance authority. nil ⇒ use the immutable genesis root.
	curRoot map[string]bool
}

// SetCurrentRoot updates the verifier's effective GovernanceRoot to the given epoch's set, so issuance
// authority tracks governance turnover (a removed founder can no longer issue; an added one can). Pass
// the genesis root to reset. Concurrency: call it from the same goroutine that drives epoch installs (the
// org-central serializes governance under its mu); VerifyMembership reads it without further locking.
func (v *Verifier) SetCurrentRoot(root []string) {
	if len(root) == 0 {
		v.curRoot = nil
		return
	}
	m := make(map[string]bool, len(root))
	for _, r := range root {
		m[r] = true
	}
	v.curRoot = m
}

// isFounder reports whether aid is in the CURRENT governance root (the epoch-updated set if present,
// else the immutable genesis root).
func (v *Verifier) isFounder(aid string) bool {
	if v.curRoot != nil {
		return v.curRoot[aid]
	}
	return v.g.IsFounder(aid)
}

// NewVerifier validates g, caches its org_id, and binds the callbacks. resolve is required.
func NewVerifier(g *Genesis, resolve KELResolver, lookupAdmin AdminLookup, revoked func(credCID string) bool) (*Verifier, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	if resolve == nil {
		return nil, errors.New("org: resolve (KELResolver) required")
	}
	id, err := g.OrgID()
	if err != nil {
		return nil, err
	}
	return &Verifier{g: g, orgID: id, resolve: resolve, lookupAdmin: lookupAdmin, revoked: revoked}, nil
}

// VerifyMembership checks that cred is a valid membership grant in this org at time now
// (unix-millis): correct org, known role, epoch 0 (governance epoch chain deferred), bounded
// window, not revoked, issuer signature valid, and the issuer authorized — an admin credential
// MUST be founder-issued; a member/guest credential MUST be issued by a founder or by a valid
// (founder-issued) admin. Re-verifying the issuing admin at `now` makes admin revocation/expiry
// cascade to every member it issued.
func (v *Verifier) VerifyMembership(cred *Credential, now int64) error {
	if cred == nil {
		return ErrNilCredential
	}
	if cred.OrgID != v.orgID {
		return ErrWrongOrg
	}
	if !validRole(cred.Role) {
		return ErrBadRole
	}
	// The cred.Epoch field references the governance epoch it was issued under; it is informational here.
	// Issuance AUTHORITY is enforced against the CURRENT governance root (isFounder, updated by the epoch
	// chain), so a founder removed by an advance loses authority and one added gains it — turnover is
	// effective immediately rather than resolved per historical epoch (a deliberate "removal takes effect
	// now" semantic; per-epoch B1-preserving resolution would need the full chain).
	if err := cred.checkWindow(now); err != nil {
		return err
	}
	if v.revoked != nil {
		cid, err := cred.CID()
		if err != nil {
			return err
		}
		if v.revoked(cid) {
			return ErrRevoked
		}
	}
	issuer, err := cred.verifyEnvelope(v.resolve)
	if err != nil {
		return err
	}

	// Authority chain. Issuance is single-signature (spec §O2): an admin credential is issued by ANY ONE
	// GovernanceRoot founder acting as CA — the M-of-N threshold gates the separate governance epoch chain
	// (GovernanceCert), not routine credential issuance.
	if cred.Role == RoleAdmin {
		if !v.isFounder(issuer) {
			return ErrIssuerNotFounder // admins are founder-issued (current governance root)
		}
		return nil
	}
	// member / guest: a founder may issue directly…
	if v.isFounder(issuer) {
		return nil
	}
	// …otherwise the issuer must be a valid admin (its admin credential founder-issued).
	if v.lookupAdmin == nil {
		return ErrIssuerNotAuth
	}
	adminCred, ok := v.lookupAdmin(issuer)
	if !ok || adminCred == nil || adminCred.Subject != issuer || adminCred.Role != RoleAdmin {
		return ErrIssuerNotAuth
	}
	// Recurse once: the admin credential lands in the Role==admin branch (founder check), which
	// does not recurse — so this terminates in one step.
	if err := v.VerifyMembership(adminCred, now); err != nil {
		return fmt.Errorf("%w: %w", ErrIssuerNotAuth, err)
	}
	return nil
}

// VerifyMembership is the one-shot free function: it constructs a Verifier and verifies once.
// For per-request use (a server gating every RPC), construct a Verifier once instead.
func VerifyMembership(cred *Credential, g *Genesis, now int64, resolve KELResolver, lookupAdmin AdminLookup, revoked func(credCID string) bool) error {
	v, err := NewVerifier(g, resolve, lookupAdmin, revoked)
	if err != nil {
		return err
	}
	return v.VerifyMembership(cred, now)
}

// Issue mints and signs a credential granting subject the given role in g, valid [issuedAt,
// notAfter]. It validates role + bounded window + non-empty subject and fills org_id from g,
// so callers cannot mis-set it. issuer is the founder (for admin) or founder/admin (for member).
func (g *Genesis) Issue(issuer *identity.Controller, subject, role string, issuedAt, notAfter int64) (*Credential, error) {
	if !validRole(role) {
		return nil, ErrBadRole
	}
	if subject == "" {
		return nil, errors.New("org: empty subject")
	}
	if issuedAt <= 0 || notAfter <= issuedAt {
		return nil, ErrBadWindow
	}
	orgID, err := g.OrgID()
	if err != nil {
		return nil, err
	}
	c := &Credential{OrgID: orgID, Subject: subject, Role: role, IssuedAt: issuedAt, NotAfter: notAfter}
	if err := c.Sign(issuer); err != nil {
		return nil, err
	}
	return c, nil
}

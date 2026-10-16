package org

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module/inv1"
)

// OrgScopedObject marks a Credential as org-scoped (INV-1 runtime type tag): a membership credential
// must never reach a commons publish path. Value receiver so both Credential and *Credential are
// caught by inv1.GuardCommonsPublish. Compile-time assertion that the marker is satisfied:
var _ inv1.OrgScoped = Credential{}

func (Credential) OrgScopedObject() {}

// CredentialType is key 0 of a credential body (core §2.19 OrgCredBody, L42): the type tag the
// issuer's identity key signs under, which keeps a credential's signature from passing for the
// signature of any other object that key signs (core §3.4).
const CredentialType = "anet.org-cred/1"

// Credential is a membership grant: a signed object asserting that Subject holds Role in the org
// identified by OrgID, issued (single-signature) by the envelope's signer. What is signed is its
// body, coredet(OrgCredBody) with the type tag (credBody); the envelope is detached, never inside
// the signed bytes (mirrors acc/aet/tsir discipline). Every field but the envelope is in the
// body, so every one is signed.
type Credential struct {
	OrgID    string
	Subject  string // the AID being granted membership
	Role     string // RoleAdmin / RoleMember / RoleGuest
	Epoch    uint64 // governance epoch; 0.3.0 accepts only 0, the default (VerifyMembership)
	IssuedAt int64  // unix-millis; also the envelope's binding time (revocation gate)
	NotAfter int64  // unix-millis expiry (mandatory; must be > IssuedAt)

	// Envelope is the detached issuer signature; Envelope.SignerAID is the issuer (founder or admin).
	// Issuance is SINGLE-SIGNATURE throughout the chain root→admin→member (spec §O2: a founder acts as
	// CA to issue admin creds, an admin/founder issues member creds — "delegated admin without bothering
	// the governance layer"). The M-of-N threshold is NOT applied here; it governs the separate epoch'd
	// governance event chain (GovernanceCert), see internal/v3rt/org/govepoch.go.
	Envelope *aobj.Envelope
}

// credBody is the signed encoding of a Credential.
//
//anet:wire OrgCredBody
type credBody struct {
	Type     string `cbor:"0,keyasint"` // CredentialType
	OrgID    string `cbor:"1,keyasint"`
	Subject  string `cbor:"2,keyasint"`
	Role     string `cbor:"3,keyasint"`
	Epoch    uint64 `cbor:"4,keyasint,omitempty"`
	IssuedAt int64  `cbor:"5,keyasint"`
	NotAfter int64  `cbor:"6,keyasint"`
}

func (c *Credential) body() credBody {
	return credBody{Type: CredentialType, OrgID: c.OrgID, Subject: c.Subject, Role: c.Role, Epoch: c.Epoch,
		IssuedAt: c.IssuedAt, NotAfter: c.NotAfter}
}

// decodeCredBody decodes a credential body strictly (core CORE-E2, CORE-E4, CORE-K26): every key
// registered and of its type, nothing else, the type tag CredentialType, and the bytes exactly
// coredet of what they decode to. Any failure is ErrMalformed.
func decodeCredBody(b []byte) (*Credential, error) {
	var body credBody
	if err := coredet.UnmarshalStrict(b, &body); err != nil {
		return nil, fmt.Errorf("%w: the body does not decode: %v", ErrMalformed, err)
	}
	if body.Type != CredentialType {
		return nil, fmt.Errorf("%w: the body's type is %q, want %q", ErrMalformed, body.Type, CredentialType)
	}
	c := &Credential{OrgID: body.OrgID, Subject: body.Subject, Role: body.Role, Epoch: body.Epoch,
		IssuedAt: body.IssuedAt, NotAfter: body.NotAfter}
	again, err := c.CanonicalPreimage()
	if err != nil || !bytes.Equal(again, b) {
		return nil, fmt.Errorf("%w: the body is not the canonical encoding of a credential (a required key is missing, or an encoding is not CoreDet)", ErrMalformed)
	}
	return c, nil
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
	ErrEpoch            = errors.New("org: the credential names a governance epoch; 0.3.0 accepts only epoch 0")
	// ErrMalformed: the bytes are not a credential as 0.3.0 writes it — a body without the type tag
	// (a 0.2 credential), with another tag, a key it does not register or a non-canonical encoding,
	// or a wire form without its envelope.
	ErrMalformed = errors.New("org: not a well-formed credential")
)

// KELResolver resolves an AID to its key-event log for signature verification.
type KELResolver func(aid string) (kel []identity.SignedEvent, ok bool)

// AdminLookup returns the admin credential held by an AID (from the org-central store), used to
// verify a member credential whose issuer is an admin rather than a founder. ok=false if none.
type AdminLookup func(adminAID string) (*Credential, bool)

// CanonicalPreimage is the CoreDet-CBOR signing/CID preimage: coredet(OrgCredBody), type tag
// included (L42). The detached Envelope is not in it; all other fields are.
func (c *Credential) CanonicalPreimage() ([]byte, error) {
	return coredet.Marshal(c.body())
}

// CID returns the credential content id over its canonical preimage.
func (c *Credential) CID() (string, error) {
	pre, err := c.CanonicalPreimage()
	if err != nil {
		return "", err
	}
	return anetcid.Sum(pre)
}

// Sign attaches the issuer's detached signature (sign-binds-CID). The issuer is signer, which
// signs and does nothing else: a module never holds an identity's controller or its key (KEL O-1).
func (c *Credential) Sign(signer identity.Signer) error {
	pre, err := c.CanonicalPreimage()
	if err != nil {
		return err
	}
	sig, seq := signer.Sign(pre)
	c.Envelope = &aobj.Envelope{SignerAID: signer.AID(), KeyStateSeq: seq, Alg: aobj.AlgEdDSA, Sig: sig}
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
// (unix-millis): correct org, known role, epoch 0 (ErrEpoch otherwise), bounded
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
	if cred.Epoch != 0 {
		return ErrEpoch
	}
	// Issuance AUTHORITY is enforced against the CURRENT governance root (isFounder, updated by the epoch
	// chain), so a founder removed by an advance loses authority and one added gains it — turnover is
	// effective immediately rather than resolved per historical epoch (a deliberate "removal takes effect
	// now" semantic; per-epoch B1-preserving resolution would need the full chain). A credential that
	// names an epoch is refused above (core §2.19: 0.3.0 accepts only the default).
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
func (g *Genesis) Issue(issuer identity.Signer, subject, role string, issuedAt, notAfter int64) (*Credential, error) {
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

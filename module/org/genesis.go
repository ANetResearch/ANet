// Package org is the v3 runtime org-membership layer (runtime policy, NOT protocol — design
// decision D1): the org-genesis object whose CID is the stable org_id, and the single-signature
// credential chain (founder → admin → member) that grants membership, both as signed AObjs
// verified against issuer KELs.
//
// Normative source: docs/superpowers/specs/2026-06-15-anet-p2p-and-org-functional-design.md §O1
// (org_id = CID of the IMMUTABLE inception + M-of-N GovernanceRoot, B1) and §O2 (credential
// chain, D7). Issuance model (decision 2026-06-15): a single GovernanceRoot founder acts as CA
// to issue admin credentials (1.2.0-style single-sig), and an admin (or founder) issues
// member/guest credentials — single-signature throughout. The M-of-N threshold is reserved for
// governance-level actions (epoch/root/policy change on the separate governance event chain),
// not routine issuance.
//
// B1 invariant: the inception holds ONLY immutable identity — founder set + threshold M (+ a
// uniqueness nonce). Mutable policy (join mode, custom roles) is NOT here; it rides the future
// epoch'd governance chain, so org_id never changes when policy changes.
package org

import (
	"errors"
	"fmt"
	"sort"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/coredet"
)

// Roles (the credential role enum, §O2).
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
	RoleGuest  = "guest"
)

func validRole(r string) bool { return r == RoleAdmin || r == RoleMember || r == RoleGuest }

// Genesis is the IMMUTABLE org inception. Its CID is the stable org_id (B1: governance/policy
// evolution rides a separate epoch'd event chain, never this object, so org_id never changes).
// It holds only immutable identity — no mutable policy fields.
type Genesis struct {
	GovernanceRoot []string `cbor:"1,keyasint"`           // founder AID set (D5 sorted+deduped on the wire)
	M              uint64   `cbor:"2,keyasint"`           // M-of-N threshold for GOVERNANCE actions (not issuance)
	Nonce          string   `cbor:"3,keyasint,omitempty"` // uniqueness: same founders → distinct org_id
}

// ErrBadGenesis is returned by Validate for a structurally invalid inception.
var ErrBadGenesis = errors.New("org: invalid genesis")

// sortedDedup returns a bytewise-sorted, deduped copy (D5) without mutating the input.
func sortedDedup(in []string) []string {
	cp := append([]string(nil), in...)
	sort.Strings(cp)
	out := cp[:0]
	for i, s := range cp {
		if i == 0 || s != cp[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// canonical returns a Genesis with the GovernanceRoot D5-normalized (sorted+deduped), for a
// deterministic preimage regardless of caller ordering.
func (g *Genesis) canonical() *Genesis {
	c := *g
	c.GovernanceRoot = sortedDedup(g.GovernanceRoot)
	return &c
}

// CanonicalPreimage is the CoreDet-CBOR preimage over the D5-normalized inception (_CONVENTIONS §3).
func (g *Genesis) CanonicalPreimage() ([]byte, error) {
	return coredet.Marshal(g.canonical())
}

// OrgID returns the stable org identifier = CID(canonical inception).
func (g *Genesis) OrgID() (string, error) {
	pre, err := g.CanonicalPreimage()
	if err != nil {
		return "", err
	}
	return anetcid.Sum(pre)
}

// Validate checks the inception's static invariants: a non-empty founder set, and a governance
// threshold M in [1, N] (N = deduped founder count).
func (g *Genesis) Validate() error {
	n := uint64(len(sortedDedup(g.GovernanceRoot)))
	if n == 0 {
		return fmt.Errorf("%w: empty GovernanceRoot", ErrBadGenesis)
	}
	if g.M < 1 || g.M > n {
		return fmt.Errorf("%w: M=%d out of [1,%d]", ErrBadGenesis, g.M, n)
	}
	return nil
}

// IsFounder reports whether aid is in the GovernanceRoot.
func (g *Genesis) IsFounder(aid string) bool {
	for _, f := range g.GovernanceRoot {
		if f == aid {
			return true
		}
	}
	return false
}

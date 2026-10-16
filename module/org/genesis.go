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
	"bytes"
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

// GenesisType is key 0 of an encoded genesis (core §2.19 OrgGenesis, L41). The genesis is not
// signed: its encoding is only hashed into the org_id, and the tag keeps that preimage apart from
// the preimage of every other object.
const GenesisType = "anet.org-genesis/1"

// Genesis is the IMMUTABLE org inception. Its CID is the stable org_id (B1: governance/policy
// evolution rides a separate epoch'd event chain, never this object, so org_id never changes).
// It holds only immutable identity — no mutable policy fields. Its encoding is genesisBody.
type Genesis struct {
	GovernanceRoot []string // founder AID set (sorted and deduplicated when encoded)
	M              uint64   // M-of-N threshold for GOVERNANCE actions (not issuance)
	Nonce          string   // uniqueness: same founders → distinct org_id
}

// genesisBody is the encoding of a Genesis: coredet of it is the preimage of the org_id.
//
//anet:wire OrgGenesis
type genesisBody struct {
	Type           string   `cbor:"0,keyasint"` // GenesisType
	GovernanceRoot []string `cbor:"1,keyasint"`
	M              uint64   `cbor:"2,keyasint"`
	Nonce          string   `cbor:"3,keyasint,omitempty"`
}

var (
	// ErrBadGenesis is returned by Validate for a structurally invalid inception, and by
	// ParseGenesis for bytes that are not the encoding of one.
	ErrBadGenesis = errors.New("org: invalid genesis")
	// ErrGenesisV02 is returned by ParseGenesis for a genesis written by anet 0.2 (no type tag).
	// 0.3.0 does not migrate it: the organization is created again.
	ErrGenesisV02 = errors.New("org: the genesis is in the 0.2 format, which 0.3.0 does not migrate; create the organization again with anet 0.3.0")
)

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

// CanonicalPreimage is the CoreDet-CBOR preimage over the D5-normalized inception (_CONVENTIONS §3):
// coredet(OrgGenesis), type tag included (L41). It is also the encoding Marshal writes.
func (g *Genesis) CanonicalPreimage() ([]byte, error) {
	c := g.canonical()
	return coredet.Marshal(genesisBody{Type: GenesisType, GovernanceRoot: c.GovernanceRoot, M: c.M, Nonce: c.Nonce})
}

// Marshal is the encoding of g that ParseGenesis reads, and the bytes a node is configured with.
func (g *Genesis) Marshal() ([]byte, error) { return g.CanonicalPreimage() }

// ParseGenesis decodes an encoded genesis strictly (core CORE-K26): every key registered and of
// its type, nothing else, the type tag GenesisType, and the bytes exactly the encoding Marshal
// writes (founders sorted and deduplicated), so one organization has one encoding and one org_id.
// A genesis without key 0 was written by 0.2: ErrGenesisV02. Any other failure is ErrBadGenesis.
// It does not judge the founders and the threshold; Validate does.
func ParseGenesis(b []byte) (*Genesis, error) {
	var keys map[uint64]any
	if err := coredet.Unmarshal(b, &keys); err != nil {
		return nil, fmt.Errorf("%w: not a CBOR map with integer keys: %v", ErrBadGenesis, err)
	}
	if _, ok := keys[0]; !ok {
		if _, founders := keys[1]; founders {
			return nil, ErrGenesisV02
		}
		return nil, fmt.Errorf("%w: no type tag (key 0)", ErrBadGenesis)
	}
	var body genesisBody
	if err := coredet.UnmarshalStrict(b, &body); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadGenesis, err)
	}
	if body.Type != GenesisType {
		return nil, fmt.Errorf("%w: type %q, want %q", ErrBadGenesis, body.Type, GenesisType)
	}
	g := &Genesis{GovernanceRoot: body.GovernanceRoot, M: body.M, Nonce: body.Nonce}
	again, err := g.Marshal()
	if err != nil || !bytes.Equal(again, b) {
		return nil, fmt.Errorf("%w: not the canonical encoding (founders sorted and deduplicated, CoreDet)", ErrBadGenesis)
	}
	return g, nil
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

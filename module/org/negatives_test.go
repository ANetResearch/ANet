package org_test

import (
	"errors"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/wire"

	"github.com/ANetResearch/ANet/module/org"
)

// CORE-S4: every key the org objects do not assign (the unassigned ones of 0-63, 64 and 65535),
// inserted with the value CBOR null into a valid encoding, is refused by that object's decoder:
// the genesis by ParseGenesis (ErrBadGenesis), the credential body and its wire form by
// UnmarshalCredential (ErrMalformed). The body is signed by its issuer, so it is refused for the
// key and not for its signature.
func TestOrgObjectsRefuseUnassignedKeys(t *testing.T) {
	r := wire.MustLoad()
	check := func(object string, valid []byte, want error, decode func([]byte) error) {
		t.Helper()
		if err := decode(valid); err != nil {
			t.Fatalf("%s: the valid encoding is refused: %v", object, err)
		}
		negs, err := wire.Negatives(r, object, valid)
		if err != nil {
			t.Fatal(err)
		}
		if len(negs) < 50 {
			t.Fatalf("%s: only %d negatives", object, len(negs))
		}
		for _, n := range negs {
			if err := decode(n.Bytes); !errors.Is(err, want) {
				t.Errorf("%s with key %d (%s): %v, want %v", object, n.Key, n.Why, err, want)
			}
		}
	}

	g := &org.Genesis{GovernanceRoot: []string{"bafyb", "bafya"}, M: 1, Nonce: "n"}
	gb, err := g.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	check("OrgGenesis", gb, org.ErrBadGenesis, func(b []byte) error { _, err := org.ParseGenesis(b); return err })

	c := &org.Credential{OrgID: "bafyorg", Subject: "bafysubject", Role: org.RoleMember, IssuedAt: 1767225600000, NotAfter: 1798761600000}
	if err := c.Sign(identity.SuiteController()); err != nil {
		t.Fatal(err)
	}
	body, err := c.CanonicalPreimage()
	if err != nil {
		t.Fatal(err)
	}
	check("OrgCredBody", body, org.ErrMalformed, func(b []byte) error { _, err := org.UnmarshalCredential(signedWire(t, b)); return err })
	w, err := org.MarshalCredential(c)
	if err != nil {
		t.Fatal(err)
	}
	check("SignedCredential", w, org.ErrMalformed, func(b []byte) error { _, err := org.UnmarshalCredential(b); return err })
}

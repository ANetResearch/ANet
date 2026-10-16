package org_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module/org"
)

// FuzzUnmarshalCredential: the strict decoder of a signed credential (core CORE-K26) never panics,
// and what it accepts is a credential as this package writes it: MarshalCredential gives back the
// same bytes (one credential, one encoding), the body carries the type tag, and verifying it does
// not panic either. Seeds: the MODOBJ-1 credential and its negatives.
func FuzzUnmarshalCredential(f *testing.F) {
	v := loadOrgVector(f)
	f.Add([]byte(v.Credential.Wire))
	for _, n := range v.Negatives {
		f.Add([]byte(n.Wire))
	}
	f.Add([]byte{0xa0})
	kel, err := identity.UnmarshalKEL(v.Issuer.KEL)
	if err != nil {
		f.Fatal(err)
	}
	g, err := org.ParseGenesis(v.Genesis.Bytes)
	if err != nil {
		f.Fatal(err)
	}
	resolve := func(string) ([]identity.SignedEvent, bool) { return kel, true }
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := org.UnmarshalCredential(b)
		if err != nil {
			if !errors.Is(err, org.ErrMalformed) {
				t.Fatalf("a refusal that is not ErrMalformed: %v", err)
			}
			return
		}
		again, err := org.MarshalCredential(c)
		if err != nil || !bytes.Equal(again, b) {
			t.Fatalf("accepted bytes that are not the credential's encoding (%v)\n in  %x\n out %x", err, b, again)
		}
		pre, err := c.CanonicalPreimage()
		if err != nil || !bytes.Contains(pre, []byte(org.CredentialType)) {
			t.Fatalf("an accepted body without its tag (%v)", err)
		}
		_ = org.VerifyMembership(c, g, v.Credential.VerifyAt, resolve, nil, nil)
	})
}

// FuzzParseGenesis: ParseGenesis never panics; what it accepts is the encoding Marshal writes,
// and ErrGenesisV02 is given only for a map without key 0.
func FuzzParseGenesis(f *testing.F) {
	v := loadOrgVector(f)
	f.Add([]byte(v.Genesis.Bytes))
	f.Add([]byte{0xa0})
	f.Add([]byte{0xa2, 0x01, 0x81, 0x61, 0x61, 0x02, 0x01}) // {1: ["a"], 2: 1}: a 0.2 genesis
	f.Fuzz(func(t *testing.T, b []byte) {
		g, err := org.ParseGenesis(b)
		if errors.Is(err, org.ErrGenesisV02) {
			if g != nil {
				t.Fatal("a 0.2 genesis was returned")
			}
			return
		}
		if err != nil {
			if !errors.Is(err, org.ErrBadGenesis) {
				t.Fatalf("a refusal that is neither ErrBadGenesis nor ErrGenesisV02: %v", err)
			}
			return
		}
		again, err := g.Marshal()
		if err != nil || !bytes.Equal(again, b) {
			t.Fatalf("accepted bytes that are not the genesis's encoding (%v)", err)
		}
		_ = g.Validate()
	})
}

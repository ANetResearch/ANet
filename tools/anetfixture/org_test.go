package main

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module/blackboard"
	"github.com/ANetResearch/ANet/module/org"
)

// The joint run founds an organization with org-genesis, hands the genesis
// back to org-credential and org-id, and gives the credential to org.verify
// and the unit to blackboard.add. Each object must be the one the modules
// decode: the genesis read back names the same organization, the credential
// is for that organization and verifies under it, and the unit decodes and
// verifies. A fixture that read the genesis as an empty one would issue
// credentials for an organization nobody founded.
func TestTheFixtureOrgObjectsAreTheModulesOwn(t *testing.T) {
	dir, c := withIdentity(t)
	gen := capture(t, func() error { return cmdOrgGenesis([]string{"--home", dir, "--nonce", "joint"}) })
	raw, err := base64.StdEncoding.DecodeString(gen)
	if err != nil {
		t.Fatalf("org-genesis output is not base64: %v", err)
	}
	g, err := org.ParseGenesis(raw)
	if err != nil {
		t.Fatalf("org-genesis output is not a genesis: %v", err)
	}
	want, err := g.OrgID()
	if err != nil {
		t.Fatal(err)
	}
	if id := capture(t, func() error { return cmdOrgID([]string{"--genesis", gen}) }); id != want {
		t.Fatalf("org-id %s, want %s", id, want)
	}

	cred := capture(t, func() error {
		return cmdOrgCredential([]string{"--home", dir, "--genesis", gen, "--subject", c.AID()})
	})
	cb, err := base64.StdEncoding.DecodeString(cred)
	if err != nil {
		t.Fatalf("org-credential output is not base64: %v", err)
	}
	cr, err := org.UnmarshalCredential(cb)
	if err != nil {
		t.Fatalf("org-credential output is not a credential: %v", err)
	}
	if cr.OrgID != want {
		t.Fatalf("the credential is for %s, the genesis names %s", cr.OrgID, want)
	}
	resolve := func(aid string) ([]identity.SignedEvent, bool) { return c.KEL(), aid == c.AID() }
	if err := org.VerifyMembership(cr, g, time.Now().UnixMilli(), resolve, nil, nil); err != nil {
		t.Fatalf("the fixture's credential does not verify: %v", err)
	}

	unit := capture(t, func() error { return cmdCogUnit([]string{"--home", dir, "--task", "t-1", "--body", "x"}) })
	ub, err := base64.StdEncoding.DecodeString(unit)
	if err != nil {
		t.Fatalf("cogunit output is not base64: %v", err)
	}
	u, err := blackboard.Unmarshal(ub)
	if err != nil {
		t.Fatalf("cogunit output is not a unit: %v", err)
	}
	if err := u.Verify(c.KEL(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("the fixture's unit does not verify: %v", err)
	}

	for _, bad := range []string{"not base64!", base64.StdEncoding.EncodeToString([]byte{0xa0})} {
		if err := cmdOrgID([]string{"--genesis", bad}); err == nil {
			t.Errorf("org-id took %q for a genesis", bad)
		}
		if err := cmdOrgCredential([]string{"--home", dir, "--genesis", bad}); err == nil {
			t.Errorf("org-credential took %q for a genesis", bad)
		}
	}
}

// Package golden pins the canonical id of fixed-content objects.
//
// These ids are taken over each object's deterministic preimage, never its
// signature, so they reproduce across implementations and runs with no key
// material. A change to one means a load-bearing object's canonical
// encoding moved — which breaks interop with every node running the other
// encoding, and for an org id invalidates every credential that references
// it.
//
// The org id is pinned to the value anet3 produces for the same inception.
// That is the point of having it here: the org layer was carried across
// from anet3 rather than rewritten, and a migration that quietly changed
// the wire would have been a migration that silently forked the family.
// The number below is anet3's, copied from its own golden test.
package golden_test

import (
	"testing"

	"github.com/ANetResearch/ANet/module/blackboard"
	"github.com/ANetResearch/ANet/module/cas"
	"github.com/ANetResearch/ANet/module/org"
)

const (
	// From anet3 internal/v3rt/golden. Identical inception, identical id.
	goldenOrgID = "bafyreicaw2uaernqv2rnhunxgkjwgsxrundhy4o2f2v423i27bk6yjh7ge"

	// A CogUnit's id is the CID of its unsigned preimage (keys 1-7); the
	// envelope rides the wire but is never signed over, so this id is
	// reproducible without a key. Also anet3's, for the same unit.
	goldenCogUnitID = "bafyreier4cem3uktuiq55mjtci45i5gblsog2gi4d6dk6jsowg737kworu"

	// A membership credential's CID, over its preimage. This is what a
	// revocation list names, so drift here means every revocation stops
	// matching the credential it was meant to revoke.
	goldenCredentialCID = "bafyreidll3nwldeihlehpfe2qttfltu4prttnzwf4uf2fqctm4ojmsokbq"

	// The CID content-addressed storage derives from fixed bytes. Drift
	// means two nodes store the same blob under different names and neither
	// can fetch the other's.
	goldenBlobCID = "bafyreihbs6iekk25duzvzv4cegoszfkuxav2zlovji44xgbv3ggui2y2n4"
)

func TestOrgIDMatchesAnet3(t *testing.T) {
	g := &org.Genesis{
		GovernanceRoot: []string{"did:anet:bgolden0founder0aid0000000000000000000000000000"},
		M:              1,
		Nonce:          "golden-org",
	}
	got, err := g.OrgID()
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenOrgID {
		t.Fatalf("org id drifted from anet3:\n got %s\nwant %s\n"+
			"An org id is the hash of its founding set. If this moved, every credential "+
			"referencing the old id is now for an org that does not exist.", got, goldenOrgID)
	}
}

// A CogUnit id must not depend on the envelope. Two nodes signing the same
// contribution produce different signatures and must produce the same id,
// or the board's OR-Set stops converging and every member accumulates
// duplicates of the same thought.
func TestCogUnitIDIgnoresTheEnvelope(t *testing.T) {
	mk := func() *blackboard.CogUnit {
		return &blackboard.CogUnit{
			Author: "did:anet:bgolden0author0aid000000000000000000000000000",
			TaskID: "golden-task", Scope: "org", Type: "claim",
			Stamp: blackboard.HLC{Wall: 1767225600000, Logical: 1, NodeID: "golden-node"},
			Body:  []byte("golden body"),
		}
	}
	bare, signed := mk(), mk()
	signed.Envelope = fakeEnvelope()

	a, err := bare.ID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := signed.ID()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("the envelope leaked into the id:\n unsigned %s\n   signed %s", a, b)
	}
	if a != goldenCogUnitID {
		t.Errorf("CogUnit id drifted:\n got %s\nwant %s", a, goldenCogUnitID)
	}
}

func TestCredentialCIDMatchesAnet3(t *testing.T) {
	cred := &org.Credential{
		OrgID:    goldenOrgID,
		Subject:  "did:anet:bgolden0subject0aid00000000000000000000000000",
		Role:     org.RoleMember,
		IssuedAt: 1767225600000,
		NotAfter: 1767229200000,
	}
	got, err := cred.CID()
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenCredentialCID {
		t.Fatalf("credential cid drifted from anet3:\n got %s\nwant %s", got, goldenCredentialCID)
	}
}

func TestBlobCIDMatchesAnet3(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Put([]byte("golden content-addressed bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenBlobCID {
		t.Fatalf("blob cid drifted from anet3:\n got %s\nwant %s", got, goldenBlobCID)
	}
}

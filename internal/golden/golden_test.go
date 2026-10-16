// Package golden pins the canonical id of fixed-content objects.
//
// These ids are taken over each object's deterministic preimage, never its
// signature, so they reproduce across implementations and runs with no key
// material. A change to one means a load-bearing object's canonical
// encoding moved — which breaks interop with every node running the other
// encoding, and for an org id invalidates every credential that references
// it.
//
// The org id, the unit id and the credential CID are pinned to the encoding
// of 0.3.0 (core §2.19): each object's preimage carries its type tag in
// key 0 (L41, L43, L42), which is why they differ from the ids anet3 and
// 0.2 produced for the same content; 0.3.0 does not migrate those (an org
// is created again). The values below were computed by ANetCore's
// golden/py/gen_v016.py (org_genesis_bytes, cogunit_body, org_cred_body),
// which shares no code with these packages. MODOBJ-1 covers the same
// objects with signatures and negatives.
package golden_test

import (
	"testing"

	"github.com/ANetResearch/ANetCore/anetcid"

	"github.com/ANetResearch/ANet/internal/transcript"
	"github.com/ANetResearch/ANet/module/blackboard"
	"github.com/ANetResearch/ANet/module/cas"
	"github.com/ANetResearch/ANet/module/org"
)

const (
	// The CID of coredet(OrgGenesis) for the inception below (L41).
	goldenOrgID = "bafyreihkfy6q5gnpyhyo47pgy4li4kylmtvxaime365wnnd5zakdw2ovge"

	// A CogUnit's id is the CID of its body, coredet(CogUnitBody) (keys 0-7);
	// the envelope rides the wire beside it but is never signed over, so this
	// id is reproducible without a key.
	goldenCogUnitID = "bafyreie7zzgeqk7mfxf6yi2czjysxrk6b4fe6wkos4pq26mbm5sneacc5i"

	// A membership credential's CID, over its body coredet(OrgCredBody). This
	// is what a revocation list names, so drift here means every revocation
	// stops matching the credential it was meant to revoke.
	goldenCredentialCID = "bafyreihj4a2td267pq74gry74qbfanqmq27q3ipiz7u2gwdk65zlfen5ym"

	// The CID content-addressed storage derives from fixed bytes. Drift
	// means two nodes store the same blob under different names and neither
	// can fetch the other's.
	goldenBlobCID = "bafyreihbs6iekk25duzvzv4cegoszfkuxav2zlovji44xgbv3ggui2y2n4"
)

func TestOrgIDIsPinned(t *testing.T) {
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
		t.Fatalf("org id drifted:\n got %s\nwant %s\n"+
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

func TestCredentialCIDIsPinned(t *testing.T) {
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
		t.Fatalf("credential cid drifted:\n got %s\nwant %s", got, goldenCredentialCID)
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

// The v2 transcript (A2A-DESIGN §2 X4) is the deliverable a provider signs
// when it completes a text task: its CID is the receipt's ResultCID. Both
// daemons and any third party re-deriving that CID must produce these exact
// bytes from these messages, so the encoding is pinned here, bytes and CID.
func TestTranscriptV2Vector(t *testing.T) {
	msgs := []transcript.Message{
		{From: "requester", Body: "summarise the attached report"},
		{From: "provider", Body: "done — see the attachment",
			Attachments: []transcript.Attachment{{Name: "summary.md", Mime: "text/markdown", Size: 42,
				CID: "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"}}},
	}
	b, err := transcript.EncodeV2("AAECAwQFBgcICQoLDA0ODw", msgs)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != goldenTranscriptV2 {
		t.Fatalf("v2 transcript bytes drifted:\n got %s\nwant %s", b, goldenTranscriptV2)
	}
	cid, err := anetcid.Sum(b)
	if err != nil {
		t.Fatal(err)
	}
	if cid != goldenTranscriptV2CID {
		t.Fatalf("v2 transcript CID drifted:\n got %s\nwant %s", cid, goldenTranscriptV2CID)
	}
	back, err := transcript.Parse(b)
	if err != nil || back.Version != 2 || back.Nonce != "AAECAwQFBgcICQoLDA0ODw" || len(back.Messages) != 2 {
		t.Fatalf("v2 vector does not read back: %+v %v", back, err)
	}
}

const (
	goldenTranscriptV2    = `{"v":2,"nonce":"AAECAwQFBgcICQoLDA0ODw","messages":[{"from":"requester","body":"summarise the attached report"},{"from":"provider","body":"done — see the attachment","attachments":[{"name":"summary.md","mime":"text/markdown","size":42,"cid":"bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"}]}]}`
	goldenTranscriptV2CID = "bafyreiaux65ezlwakald3ufckawk4i57we2tmcxzbd667h2ntqrzdrkbca"
)

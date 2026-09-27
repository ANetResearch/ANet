package daemon

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// A peer's key history is recorded when a message from it is accepted, and
// only then. That bound is the point: this node vouches for peers it has
// dealt with and for nobody else.
func TestAcceptedPeerKELIsRecorded(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Bob", nil, ""); err != nil {
		t.Fatal(err)
	}

	if _, ok := prov.peerKEL(req.AID()); ok {
		t.Fatal("a peer we have never heard from must not resolve")
	}

	if _, err := req.Delegate(ctx, prov.AID(), "do a thing", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	kel, ok := prov.peerKEL(req.AID())
	if !ok || len(kel) == 0 {
		t.Fatal("after accepting a delegation the peer's key history must be resolvable")
	}
	// And the requester, which initiated contact, holds the provider's
	// record pinned "outbound".
	row, err := req.ix.PeerIdentity(prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	if row.PinnedReason != interactions.PinOutbound || len(row.KeySet) == 0 {
		t.Fatalf("requester's record of the provider = pin %q, keyset %d bytes", row.PinnedReason, len(row.KeySet))
	}
}

// Writing a KEL that does not replay to the AID it is stored under is
// refused (C0): the record is keyed by AID, and a KEL for another AID
// under this key would make every signature by that other AID pass as this
// one's.
func TestPinPeerKELChecksTheDerivedAID(t *testing.T) {
	d := newTestDaemon(t, "", true)
	other, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	third, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pinPeerKEL(third.AID(), other.KEL(), interactions.PinHub); err == nil {
		t.Fatal("a KEL was stored under an AID it does not replay to")
	}
	if _, ok := d.peerKEL(third.AID()); ok {
		t.Fatal("the refused KEL is resolvable")
	}
	if _, err := d.pinPeerKEL(other.AID(), other.KEL(), interactions.PinHub); err != nil {
		t.Fatalf("a KEL under its own AID was refused: %v", err)
	}
}

// A stored KEL is only ever extended. A peer that rotated is followed; a
// shorter copy of its history does not move the record back, and a
// different history is refused.
func TestPeerKELIsExtendedNeverRolledBack(t *testing.T) {
	d := newTestDaemon(t, "", true)
	peer, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	short := append([]identity.SignedEvent(nil), peer.KEL()...)
	if _, err := d.pinPeerKEL(peer.AID(), short, interactions.PinOutbound); err != nil {
		t.Fatal(err)
	}
	if err := peer.Rotate(uint64(d.nowMS())); err != nil {
		t.Fatal(err)
	}
	long := peer.KEL()
	use, err := d.pinPeerKEL(peer.AID(), long, "")
	if err != nil || len(use) != len(long) {
		t.Fatalf("extension refused or not used: %v (len %d)", err, len(use))
	}
	use, err = d.pinPeerKEL(peer.AID(), short, "")
	if err != nil {
		t.Fatalf("a prefix of the stored KEL must be accepted and resolved to the stored one: %v", err)
	}
	if len(use) != len(long) {
		t.Fatalf("a shorter KEL rolled the record back to %d events", len(use))
	}
	if kel, _ := d.peerKEL(peer.AID()); len(kel) != len(long) {
		t.Fatalf("stored KEL has %d events, want %d", len(kel), len(long))
	}
}

// A key set verified against a candidate KEL is not stored when that KEL
// forks from the stored one at write time: the stored history wins, and so
// does the key set that goes with it.
func TestAKeySetUnderAForkedKELIsNotRecorded(t *testing.T) {
	d := newTestDaemon(t, "", true)
	peer, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	exported, err := peer.Export()
	if err != nil {
		t.Fatal(err)
	}
	fork, err := identity.Restore(exported)
	if err != nil {
		t.Fatal(err)
	}
	now := d.nowMS()
	if err := peer.Rotate(now - 1000); err != nil {
		t.Fatal(err)
	}
	if err := fork.Rotate(now - 2000); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pinPeerKEL(peer.AID(), peer.KEL(), ""); err != nil {
		t.Fatal(err)
	}
	raw := signedKeysFor(t, fork, 0)
	signed, err := seal.UnmarshalSignedEncKeySet(raw)
	if err != nil {
		t.Fatal(err)
	}
	set, err := seal.VerifyEncKeySet(signed, fork.AID(), fork.KEL(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.notePeer(peer.AID(), fork.KEL(), &peerKeySet{signed: raw, set: set}, "", false); err != nil {
		t.Fatal(err)
	}
	row, err := d.ix.PeerIdentity(peer.AID())
	if err != nil {
		t.Fatal(err)
	}
	if len(row.KeySet) != 0 {
		t.Fatal("a key set signed under a forked KEL was recorded")
	}
	if kel, _ := d.peerKEL(peer.AID()); len(kel) != len(peer.KEL()) {
		t.Fatal("the stored KEL changed")
	}
}

// PinPeer (for `anet peers allow`, §3.8): an operator's choice of peer is
// recorded pinned, fetched from the hub and verified against the AID named
// when this node holds no record yet, and never taken from a hub that
// answers with another identity's material.
func TestPinPeerRecordsAndPinsTheNamedPeer(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	if err := req.PinPeer(ctx, prov.AID(), "friend"); err == nil {
		t.Fatal("a pin reason outside allow/trust was accepted")
	}
	if err := req.PinPeer(ctx, prov.AID(), interactions.PinAllow); err != nil {
		t.Fatal(err)
	}
	row, err := req.ix.PeerIdentity(prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	if row.PinnedReason != interactions.PinAllow || len(row.KeySet) == 0 {
		t.Fatalf("pinned row = reason %q, %d key set bytes", row.PinnedReason, len(row.KeySet))
	}
	if kel, ok := req.peerKEL(prov.AID()); !ok || len(kel) != len(prov.self.KEL()) {
		t.Fatal("the pinned peer's KEL is not resolvable")
	}
	// An existing record changes only its reason.
	if err := req.PinPeer(ctx, prov.AID(), interactions.PinTrust); err != nil {
		t.Fatal(err)
	}
	if row, _ := req.ix.PeerIdentity(prov.AID()); row.PinnedReason != interactions.PinTrust {
		t.Fatalf("reason after re-pin = %q", row.PinnedReason)
	}
	// A hub answering for an AID with another identity's material records
	// nothing.
	victim := newStranger(t)
	mallory := newStranger(t)
	kel, _ := identity.MarshalKEL(mallory.kel)
	fake := fakeHubAt(t, srv.URL)
	fake.mu.Lock()
	fake.keysOverride[victim.aid] = hubapi.KeysResponse{AID: victim.aid,
		KeySet: base64.StdEncoding.EncodeToString(mallory.keys), KEL: base64.StdEncoding.EncodeToString(kel)}
	fake.mu.Unlock()
	if err := req.PinPeer(ctx, victim.aid, interactions.PinAllow); err == nil {
		t.Fatal("a pin was recorded from another identity's material")
	}
	if _, err := req.ix.PeerIdentity(victim.aid); err == nil {
		t.Fatal("a row exists for the refused pin")
	}
}

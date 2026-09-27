package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/seal"
)

const dayMS = 24 * 3600 * 1000

// A fresh node has a key ring on disk, readable only by its owner, whose
// published set verifies against the node's own KEL and names the node.
func TestAFreshNodeHasAKeyRing(t *testing.T) {
	d := newTestDaemon(t, "", true)
	fi, err := os.Stat(d.layout.EncKeysPath())
	if err != nil {
		t.Fatalf("no key ring file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key ring mode %v, want 0600", fi.Mode().Perm())
	}
	set := keySetOf(t, d) // verifies with expectAID = d.AID()
	if len(set.Keys) != 1 {
		t.Fatalf("fresh set has %d keys, want 1", len(set.Keys))
	}
	k := set.Keys[0]
	if k.NotAfter-k.NotBefore != seal.KeyLifetimeMS {
		t.Fatalf("key lifetime %d ms, want %d", k.NotAfter-k.NotBefore, seal.KeyLifetimeMS)
	}
	if _, ok := d.enc.Key(k.KID); !ok {
		t.Fatal("the ring does not hold the private half of the key it publishes")
	}
}

// A restart reloads the same ring and the same signed set: no new key, no
// new seq, so re-registration restates an identical set.
func TestARestartKeepsTheSameKeySet(t *testing.T) {
	d := newTestDaemon(t, "", true)
	before := append([]byte(nil), d.enc.SignedSet()...)
	layout := d.layout
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if !bytes.Equal(d2.enc.SignedSet(), before) {
		t.Fatal("a restart produced a different signed key set")
	}
}

// The rotation policy: a new key after seven days with a strictly higher
// seq, the old key still openable; private keys deleted fifteen days after
// they expire.
func TestTheKeyRingRotatesAndRetires(t *testing.T) {
	d := newTestDaemon(t, "", true)
	start := d.nowMS()
	first := keySetOf(t, d)
	firstKID := first.Keys[0].KID
	clock := start
	d.clock = func() uint64 { return clock }

	clock = start + 6*dayMS
	if changed, err := d.enc.maintain(d.self.Sign, d.self.CurrentSeq()); err != nil || changed {
		t.Fatalf("day 6: changed=%v err=%v, want no rotation yet", changed, err)
	}
	clock = start + 7*dayMS + 1
	changed, err := d.enc.maintain(d.self.Sign, d.self.CurrentSeq())
	if err != nil || !changed {
		t.Fatalf("day 7: changed=%v err=%v, want a rotation", changed, err)
	}
	second := keySetOf(t, d)
	if second.Seq <= first.Seq || len(second.Keys) != 2 {
		t.Fatalf("after rotation: seq %d (was %d), %d keys", second.Seq, first.Seq, len(second.Keys))
	}
	if second.Keys[1].NotBefore <= second.Keys[0].NotBefore {
		t.Fatal("keys are not in ascending not_before order")
	}
	// Senders choose the newest valid key.
	if k, err := seal.SelectKey(second, clock); err != nil || !bytes.Equal(k.KID, second.Keys[1].KID) {
		t.Fatalf("sender would choose %v (%v), want the new key", k, err)
	}

	// Past the first key's expiry plus retention: its private half is gone.
	clock = start + seal.KeyLifetimeMS + seal.KeyRetentionMS + 1
	if _, err := d.enc.maintain(d.self.Sign, d.self.CurrentSeq()); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.enc.Key(firstKID); ok {
		t.Fatal("a private key was kept past its retention")
	}
	// And it is gone from the file, not only hidden by the lookup.
	if err := d.enc.persist(); err != nil {
		t.Fatal(err)
	}
	onDisk, err := openKeyRing(d.layout.EncKeysPath(), d.AID(), d.nowMS)
	if err != nil {
		t.Fatal(err)
	}
	for _, kp := range onDisk.pairs {
		if bytes.Equal(kp.Public.KID, firstKID) {
			t.Fatal("the key ring file still holds a private key past its retention")
		}
	}
	// But not before: one hour inside retention it still opens.
	d2 := newTestDaemon(t, "", true)
	s2 := keySetOf(t, d2)
	c2 := d2.nowMS()
	d2.clock = func() uint64 { return c2 + seal.KeyLifetimeMS + seal.KeyRetentionMS - 3600*1000 }
	if _, ok := d2.enc.Key(s2.Keys[0].KID); !ok {
		t.Fatal("a private key was dropped inside its retention")
	}

	// The seq is persisted: a reload continues above it.
	if err := d.enc.persist(); err != nil {
		t.Fatal(err)
	}
	r, err := openKeyRing(d.layout.EncKeysPath(), d.AID(), d.nowMS)
	if err != nil {
		t.Fatal(err)
	}
	if r.Seq() != d.enc.Seq() {
		t.Fatalf("reloaded seq %d, want %d", r.Seq(), d.enc.Seq())
	}
}

// A data directory whose identity has no key ring — an identity restored
// without it — gets a new ring at start, and a ring left by another
// identity is not used.
func TestAMissingOrForeignKeyRingIsReplaced(t *testing.T) {
	d := newTestDaemon(t, "", true)
	layout := d.layout
	old := keySetOf(t, d).Keys[0].KID
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(layout.EncKeysPath()); err != nil {
		t.Fatal(err)
	}
	d2, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	fresh := keySetOf(t, d2).Keys[0].KID
	if bytes.Equal(fresh, old) {
		t.Fatal("the removed ring came back")
	}

	// A ring written for another AID is ignored.
	b, err := os.ReadFile(layout.EncKeysPath())
	if err != nil {
		t.Fatal(err)
	}
	var f encKeyFile
	if err := coredet.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	f.AID = "bafy-someone-else"
	b, _ = coredet.Marshal(f)
	if err := os.WriteFile(layout.EncKeysPath()+".x", b, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := openKeyRing(layout.EncKeysPath()+".x", d2.AID(), d2.nowMS)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.SignedSet()) != 0 || r.Seq() != 0 {
		t.Fatal("a ring for another AID was loaded")
	}
}

// A rotation while running is persisted and then published to the hub.
func TestARotationIsPublished(t *testing.T) {
	srv, _, prov := registeredPair(t)
	fake := fakeHubAt(t, srv.URL)
	start := prov.nowMS()
	prov.clock = func() uint64 { return start + 7*dayMS + 1 }
	prov.maintainKeyRing()
	fake.mu.Lock()
	stored := append([]byte(nil), fake.agents[prov.AID()].keyset...)
	fake.mu.Unlock()
	if !bytes.Equal(stored, prov.enc.SignedSet()) {
		t.Fatal("the rotated key set did not reach the hub")
	}
	disk, err := openKeyRing(prov.layout.EncKeysPath(), prov.AID(), prov.nowMS)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk.SignedSet(), prov.enc.SignedSet()) {
		t.Fatal("the published key set is not the one on disk")
	}
}

// A rotation whose key ring file cannot be written is not used. Every
// outgoing message carries the current set and a peer seals its next
// message to the newest key in it, so a set whose private keys exist only
// in memory would leave peers sealing to keys a crash destroys. The ring
// keeps its previous set, nothing new is published, and the next run with
// a writable file rotates, saves and publishes.
func TestAKeyRingThatCannotBeSavedIsNotUsed(t *testing.T) {
	srv, _, prov := registeredPair(t)
	fake := fakeHubAt(t, srv.URL)
	hubSet := func() []byte {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return append([]byte(nil), fake.agents[prov.AID()].keyset...)
	}
	before := append([]byte(nil), prov.enc.SignedSet()...)
	seq0 := prov.enc.Seq()

	// The file's directory is a regular file, so the write fails.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := prov.enc.path
	prov.enc.path = filepath.Join(blocker, "enc_keys.cbor")
	start := prov.nowMS()
	prov.clock = func() uint64 { return start + 7*dayMS + 1 }
	prov.maintainKeyRing()
	if !bytes.Equal(prov.enc.SignedSet(), before) || prov.enc.Seq() != seq0 {
		t.Fatal("a key set that could not be saved is attached to outgoing messages")
	}
	if !bytes.Equal(hubSet(), before) {
		t.Fatal("a key set that could not be saved was published")
	}

	prov.enc.path = path
	prov.maintainKeyRing()
	now := prov.enc.SignedSet()
	if bytes.Equal(now, before) || prov.enc.Seq() <= seq0 {
		t.Fatal("no rotation once the file was writable again")
	}
	disk, err := openKeyRing(path, prov.AID(), prov.nowMS)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk.SignedSet(), now) {
		t.Fatal("the key set in use is not the one on disk")
	}
	if !bytes.Equal(hubSet(), now) {
		t.Fatal("the saved rotation was not published")
	}
}

// After a KEL rotation the key set is signed again under the new key
// state. A key set is a present-tense statement and verifies only under the
// key at the head of the KEL (A2A-DESIGN §3.1 rule 2), so a set left signed
// by the retired key is refused by every sender that fetches it.
func TestTheKeySetIsResignedAfterAKELRotation(t *testing.T) {
	d := newTestDaemon(t, "", true)
	seq0 := d.enc.Seq()
	if err := d.self.Rotate(d.nowMS()); err != nil {
		t.Fatal(err)
	}
	changed, err := d.enc.maintain(d.self.Sign, d.self.CurrentSeq())
	if err != nil || !changed {
		t.Fatalf("maintain after a KEL rotation: changed=%v err=%v, want a re-signed set", changed, err)
	}
	set := keySetOf(t, d) // verifies against the rotated KEL
	if set.Seq <= seq0 {
		t.Fatalf("re-signed set has seq %d, not above %d", set.Seq, seq0)
	}
	signed, err := seal.UnmarshalSignedEncKeySet(d.enc.SignedSet())
	if err != nil {
		t.Fatal(err)
	}
	if signed.KeyStateSeq != d.self.CurrentSeq() {
		t.Fatalf("set signed under key state %d, identity is at %d", signed.KeyStateSeq, d.self.CurrentSeq())
	}
}

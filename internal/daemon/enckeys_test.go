package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

const dayMS = 24 * 3600 * 1000

// epochOf is s_e of the epoch t falls in, as the tests count it.
func epochOf(t uint64) uint64 { return t - t%seal.EpochMS }

// ringKids is the kids of the keys d's ring holds, by not_before.
func ringKids(d *Daemon) map[string]*ringKey {
	d.enc.mu.RLock()
	defer d.enc.mu.RUnlock()
	out := map[string]*ringKey{}
	for _, k := range d.enc.keys {
		c := *k
		out[string(k.Pair.Public.KID)] = &c
	}
	return out
}

// setKIDList is the kids a signed set lists, in order.
func setKIDList(t testing.TB, signed []byte) [][]byte {
	t.Helper()
	s, err := seal.UnmarshalSignedEncKeySet(signed)
	if err != nil {
		t.Fatal(err)
	}
	var set seal.EncKeySet
	if err := coredet.Unmarshal(s.Set, &set); err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, k := range set.Keys {
		out = append(out, k.KID)
	}
	return out
}

// decodedSet is a signed set's content.
func decodedSet(t testing.TB, signed []byte) seal.EncKeySet {
	t.Helper()
	s, err := seal.UnmarshalSignedEncKeySet(signed)
	if err != nil {
		t.Fatal(err)
	}
	var set seal.EncKeySet
	if err := coredet.Unmarshal(s.Set, &set); err != nil {
		t.Fatal(err)
	}
	return set
}

// atClock sets d's wire clock to a value a test moves, and returns it.
func atClock(d *Daemon, at uint64) *atomic.Uint64 {
	var c atomic.Uint64
	c.Store(at)
	d.setClock(c.Load)
	return &c
}

// FS-K1, FS-K2, FS-K3: a fresh node holds a key for the current epoch and
// each of the 14 after it, each spanning its UTC day exactly, and publishes
// all 15 with its mailbox key; the ring file is its owner's alone.
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
	if len(set.Keys) != 15 {
		t.Fatalf("fresh set has %d keys, want 15", len(set.Keys))
	}
	e := epochOf(d.nowMS())
	for i, k := range set.Keys {
		if k.NotBefore != e+uint64(i)*seal.EpochMS || k.NotAfter != k.NotBefore+seal.EpochMS {
			t.Fatalf("key %d spans [%d, %d), want epoch %d", i, k.NotBefore, k.NotAfter, e/seal.EpochMS+uint64(i))
		}
		if _, ok := d.enc.Key(k.KID); !ok {
			t.Fatalf("the ring does not hold the private half of key %d", i)
		}
	}
	if set.Mbox.Epoch != 1 || len(set.Mbox.Pub) != 32 {
		t.Fatalf("the set publishes no mailbox signing key: %+v", set.Mbox)
	}
	if set.IssuedAt != set.Seq || set.Seq > d.nowMS() {
		t.Fatalf("seq %d, issued_at %d", set.Seq, set.IssuedAt)
	}
}

// A restart reloads the same ring and the same signed set: no new key, no
// new seq, so re-registration restates an identical set.
func TestARestartKeepsTheSameKeySet(t *testing.T) {
	d := newTestDaemon(t, "", true)
	before := append([]byte(nil), d.enc.SignedSet()...)
	kids := ringKids(d)
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
	if len(ringKids(d2)) != len(kids) {
		t.Fatal("a restart changed the ring's keys")
	}
	if _, seq, prev := d2.enc.published(); prev != seq {
		t.Fatalf("after a restart prev_seq is %d, want last_seq %d", prev, seq)
	}
}

// FS-K1 to FS-K3 over the epochs: a day later the ring holds one more key,
// signs a new set with a higher seq that drops nothing still valid, and the
// previous epoch's key stays published until its not_after + D + M.
func TestTheRingFollowsTheEpochs(t *testing.T) {
	d := newTestDaemon(t, "", true)
	first := keySetOf(t, d)
	e := epochOf(first.IssuedAt)
	clock := atClock(d, e+seal.EpochMS+60_000) // a minute into the next epoch
	if _, err := d.maintainRing(); err != nil {
		t.Fatal(err)
	}
	second := keySetOf(t, d)
	if second.Seq <= first.Seq {
		t.Fatalf("seq %d after %d", second.Seq, first.Seq)
	}
	if len(second.Keys) != 16 || !bytes.Equal(second.Keys[0].KID, first.Keys[0].KID) {
		t.Fatalf("a minute into the next epoch: %d keys, the first %x (was %x)", len(second.Keys), second.Keys[0].KID, first.Keys[0].KID)
	}
	if last := second.Keys[15]; last.NotBefore != e+15*seal.EpochMS {
		t.Fatalf("the newest key starts at %d, want epoch e+15", last.NotBefore)
	}
	if second.Mbox.Epoch != first.Mbox.Epoch || !bytes.Equal(second.Mbox.Pub, first.Mbox.Pub) {
		t.Fatal("a new epoch changed the mailbox signing key")
	}
	// Senders choose the current epoch's key.
	if k, err := seal.SelectKey(second, clock.Load()); err != nil || k.NotBefore != e+seal.EpochMS {
		t.Fatalf("sender would choose %v (%v), want the key of the current epoch", k, err)
	}
	// Nothing changes until the previous key's deadline has passed.
	clock.Store(e + seal.EpochMS + 2*hourMS)
	if changed, err := d.enc.maintain(ringInputs{now: clock.Load(), sign: d.self.Sign, keySeq: d.self.CurrentSeq(), gate: clockFull}); err != nil || !changed {
		t.Fatalf("past the previous key's deadline: changed=%v err=%v", changed, err)
	}
	if third := keySetOf(t, d); len(third.Keys) != 15 || third.Keys[0].NotBefore != e+seal.EpochMS {
		t.Fatalf("past the deadline the set holds %d keys from %d", len(third.Keys), third.Keys[0].NotBefore)
	}
}

// TestPrevEpochKeyStaysPublishedUntilDeadline (mutation 21): the previous
// epoch's key is in the published set until t_e + D + M and not at it, so
// a hub that takes envelopes for it until t_e + D never refuses one because
// the set dropped the key first.
func TestPrevEpochKeyStaysPublishedUntilDeadline(t *testing.T) {
	d := newTestDaemon(t, "", true)
	e := epochOf(d.nowMS())
	prev := keySetOf(t, d).Keys[0].KID
	deadline := e + seal.EpochMS + seal.DeliveryDeadlineMS + drainMarginMS
	listed := func(at uint64) bool {
		t.Helper()
		atClock(d, at)
		if _, err := d.maintainRing(); err != nil {
			t.Fatal(err)
		}
		for _, k := range setKIDList(t, d.enc.SignedSet()) {
			if bytes.Equal(k, prev) {
				return true
			}
		}
		return false
	}
	for _, at := range []uint64{e + seal.EpochMS, e + seal.EpochMS + seal.DeliveryDeadlineMS, deadline - 1} {
		if !listed(at) {
			t.Fatalf("the previous epoch's key left the set at t_e%+d ms, before t_e + D + M", int64(at-(e+seal.EpochMS)))
		}
	}
	if listed(deadline) {
		t.Fatal("the previous epoch's key is still published at t_e + D + M")
	}
	if _, ok := d.enc.Key(prev); !ok {
		t.Fatal("leaving the set deleted the key")
	}
}

// P-3: at any time, after any sequence of regenerations and registrations,
// the published set is at most 32 keys, holds exactly one key of each epoch
// of [current, current + 14] that is not retired, the previous epoch's key
// only until t_e + D + M, and never a retired key or one whose removed_at
// is set.
func TestPublishedSetComposition(t *testing.T) {
	d := newTestDaemon(t, "", true)
	rng := rand.New(rand.NewPCG(3, 3))
	now := d.nowMS()
	clock := atClock(d, now)
	tag := uint64(0)
	for step := range 400 {
		switch r := rng.IntN(20); {
		case r == 0:
			tag++
			if _, err := d.regenerateKeys(tag); err != nil {
				t.Fatal(err)
			}
		case r < 3:
			// A registration of the current set (FS-K6).
			if err := d.enc.markRemoved(d.enc.SignedSet(), clock.Load()); err != nil {
				t.Fatal(err)
			}
		case r < 5 && step > 0:
			// The wall clock goes back up to three days now and then.
			clock.Store(clock.Load() - rng.Uint64N(3*dayMS))
		default:
			clock.Store(clock.Load() + rng.Uint64N(2*dayMS))
		}
		now := clock.Load()
		if _, err := d.enc.maintain(ringInputs{now: now, sign: d.self.Sign, keySeq: d.self.CurrentSeq(), gate: clockFull,
			drainMark: 0}); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		set := decodedSet(t, d.enc.SignedSet())
		if len(set.Keys) > seal.MaxKeysPerSet {
			t.Fatalf("step %d: %d keys", step, len(set.Keys))
		}
		ring := ringKids(d)
		e := epochOf(now)
		perEpoch := map[uint64]int{}
		for _, k := range set.Keys {
			rk := ring[string(k.KID)]
			if rk == nil || rk.Retired || rk.RemovedAt != 0 {
				t.Fatalf("step %d: the set lists %x: in ring %v, %+v", step, k.KID, rk != nil, rk)
			}
			perEpoch[k.NotBefore]++
			if k.NotBefore < e {
				if k.NotBefore != e-seal.EpochMS || now >= k.NotAfter+seal.DeliveryDeadlineMS+drainMarginMS {
					t.Fatalf("step %d: an old key [%d, %d) is published at %d", step, k.NotBefore, k.NotAfter, now)
				}
			}
		}
		for i := uint64(0); i <= ringWindowEpochs; i++ {
			if perEpoch[e+i*seal.EpochMS] != 1 {
				t.Fatalf("step %d: epoch e+%d has %d keys in the set", step, i, perEpoch[e+i*seal.EpochMS])
			}
		}
		if _, err := seal.VerifyEncKeySet(mustSigned(t, d.enc.SignedSet()), d.AID(), d.self.KEL(), now); err != nil {
			t.Fatalf("step %d: the set does not verify: %v", step, err)
		}
	}
}

func mustSigned(t testing.TB, b []byte) *seal.SignedEncKeySet {
	t.Helper()
	s, err := seal.UnmarshalSignedEncKeySet(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
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
	f, err := decodeEncKeyFile(b)
	if err != nil {
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
	if len(r.SignedSet()) != 0 || r.Seq() != 0 || len(r.keys) != 0 {
		t.Fatal("a ring for another AID was loaded")
	}
}

// A new epoch's set is persisted and then published to the hub.
func TestARotationIsPublished(t *testing.T) {
	srv, _, prov := registeredPair(t)
	fake := fakeHubAt(t, srv.URL)
	atClock(prov, epochOf(prov.nowMS())+seal.EpochMS+60_000)
	prov.maintainKeyRing()
	fake.mu.Lock()
	stored := append([]byte(nil), fake.agents[prov.AID()].keyset...)
	fake.mu.Unlock()
	if !bytes.Equal(stored, prov.enc.SignedSet()) {
		t.Fatal("the new set did not reach the hub")
	}
	disk, err := openKeyRing(prov.layout.EncKeysPath(), prov.AID(), prov.nowMS)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk.SignedSet(), prov.enc.SignedSet()) {
		t.Fatal("the published key set is not the one on disk")
	}
}

// A new set whose key ring file cannot be written is not used. Every
// outgoing message carries the current set and a peer seals its next
// message to the newest key in it, so a set whose private keys exist only
// in memory would leave peers sealing to keys a crash destroys. The ring
// keeps its previous set, nothing new is published, and the next run with
// a writable file saves and publishes.
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
	keys0 := len(ringKids(prov))

	// The file's directory is a regular file, so the write fails.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := prov.enc.path
	prov.enc.path = filepath.Join(blocker, "enc_keys.cbor")
	// A minute into the next epoch: the set still holds the key the hub's
	// clock finds valid.
	atClock(prov, epochOf(prov.nowMS())+seal.EpochMS+60_000)
	prov.maintainKeyRing()
	if !bytes.Equal(prov.enc.SignedSet(), before) || prov.enc.Seq() != seq0 || len(ringKids(prov)) != keys0 {
		t.Fatal("a key set that could not be saved is attached to outgoing messages")
	}
	if !bytes.Equal(hubSet(), before) {
		t.Fatal("a key set that could not be saved was published")
	}

	prov.enc.path = path
	prov.maintainKeyRing()
	now := prov.enc.SignedSet()
	if bytes.Equal(now, before) || prov.enc.Seq() <= seq0 {
		t.Fatal("no new set once the file was writable again")
	}
	disk, err := openKeyRing(path, prov.AID(), prov.nowMS)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk.SignedSet(), now) {
		t.Fatal("the key set in use is not the one on disk")
	}
	if !bytes.Equal(hubSet(), now) {
		t.Fatal("the saved set was not published")
	}
}

// TestPlannedRotationResignsOnly (FS-K17): after a planned KEL rotation the
// same keys and mailbox key are signed again under the new key state, with
// a higher seq; no key is made, retired or deleted. A key set is a
// present-tense statement and verifies only under the key at the head of
// the KEL (A2A-DESIGN §3.1 rule 2), so a set left signed by the retired key
// is refused by every sender that fetches it.
func TestPlannedRotationResignsOnly(t *testing.T) {
	d := newTestDaemon(t, "", true)
	before := keySetOf(t, d)
	kids := ringKids(d)
	if err := identitytest.Rotate(d.self, d.nowMS()); err != nil {
		t.Fatal(err)
	}
	d.maintainKeyRing()
	after := keySetOf(t, d) // verifies against the rotated KEL
	if after.Seq <= before.Seq {
		t.Fatalf("re-signed set has seq %d, not above %d", after.Seq, before.Seq)
	}
	if len(after.Keys) != len(before.Keys) || after.Mbox.Epoch != before.Mbox.Epoch || !bytes.Equal(after.Mbox.Pub, before.Mbox.Pub) {
		t.Fatal("a planned rotation changed the keys or the mailbox key")
	}
	for i := range after.Keys {
		if !bytes.Equal(after.Keys[i].KID, before.Keys[i].KID) {
			t.Fatalf("key %d changed", i)
		}
	}
	now := ringKids(d)
	if len(now) != len(kids) {
		t.Fatalf("the ring holds %d keys, held %d", len(now), len(kids))
	}
	for kid, k := range now {
		if kids[kid] == nil || k.Retired {
			t.Fatal("a planned rotation made or retired a key")
		}
	}
	signed := mustSigned(t, d.enc.SignedSet())
	if signed.KeyStateSeq != d.self.CurrentSeq() {
		t.Fatalf("set signed under key state %d, identity is at %d", signed.KeyStateSeq, d.self.CurrentSeq())
	}
}

// TestKeyRingRewriteZeroesOldInode (FS-K12, mutation 17): every rewrite of
// the key ring overwrites the replaced file's bytes. A hard link keeps the
// old inode reachable; after the rewrite it holds only zeros, at its old
// length, while the new file reads back.
func TestKeyRingRewriteZeroesOldInode(t *testing.T) {
	d := newTestDaemon(t, "", true)
	path := d.layout.EncKeysPath()
	link := path + ".old"
	if err := os.Link(path, link); err != nil {
		t.Skipf("hard links are not available in the test's temporary directory: %v", err)
	}
	oldBytes, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	priv := ringPrivates(t, oldBytes)
	atClock(d, epochOf(d.nowMS())+seal.EpochMS+1)
	if _, err := d.maintainRing(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(oldBytes) || bytes.Count(got, []byte{0}) != len(got) {
		t.Fatalf("the replaced key ring file was not overwritten: %d bytes, %d zero", len(got), bytes.Count(got, []byte{0}))
	}
	for _, p := range priv {
		if bytes.Contains(got, p) {
			t.Fatal("a private key is still in the replaced file")
		}
	}
	if b, err := os.ReadFile(path); err != nil || len(b) == 0 {
		t.Fatalf("the new key ring file: %v", err)
	} else if _, err := decodeEncKeyFile(b); err != nil {
		t.Fatalf("the new key ring file does not read back: %v", err)
	}
	if counter(d, counterOverwriteFailed) != 0 {
		t.Fatal("an overwrite failure was counted")
	}
}

// ringPrivates is the private keys an encoded ring file holds.
func ringPrivates(t testing.TB, b []byte) [][]byte {
	t.Helper()
	f, err := decodeEncKeyFile(b)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, k := range f.Keys {
		out = append(out, append([]byte(nil), k.Pair.Private...))
	}
	return out
}

// TestTmpLeftoverZeroed (FS-K13): a temporary file a crash left beside the
// ring may hold private keys; the start overwrites it with zeros before it
// removes it.
func TestTmpLeftoverZeroed(t *testing.T) {
	d := newTestDaemon(t, "", true)
	layout := d.layout
	cur, err := os.ReadFile(layout.EncKeysPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	tmp := layout.EncKeysPath() + ".tmp"
	if err := os.WriteFile(tmp, cur, 0o600); err != nil {
		t.Fatal(err)
	}
	link := tmp + ".seen"
	if err := os.Link(tmp, link); err != nil {
		t.Skipf("hard links are not available in the test's temporary directory: %v", err)
	}
	d2, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the leftover temporary file is still there: %v", err)
	}
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(cur) || bytes.Count(got, []byte{0}) != len(got) {
		t.Fatal("the leftover temporary file was removed without being overwritten")
	}
}

// TestRegenerateIsIdempotent (FS-K21, mutation 30; engine CR11): a
// regeneration retires every key, makes a new key for each epoch of the
// window and signs the set of the new keys, in one write; the same tag
// again — after a crash, in this process or the next — does nothing.
func TestRegenerateIsIdempotent(t *testing.T) {
	d := newTestDaemon(t, "", true)
	old := ringKids(d)
	done, err := d.regenerateKeys(7)
	if err != nil || !done {
		t.Fatalf("regenerate: %v %v", done, err)
	}
	after := ringKids(d)
	if len(after) != len(old)+15 {
		t.Fatalf("after regenerating the ring holds %d keys, want %d", len(after), len(old)+15)
	}
	for kid := range old {
		if !after[kid].Retired {
			t.Fatal("an old key was not retired")
		}
		if _, ok := d.enc.Key([]byte(kid)); !ok {
			t.Fatal("an old key is gone: mail already sealed to it would not open")
		}
	}
	listed := setKIDList(t, d.enc.SignedSet())
	if len(listed) != 15 {
		t.Fatalf("the new set lists %d keys", len(listed))
	}
	for _, k := range listed {
		if old[string(k)] != nil {
			t.Fatal("the new set lists an old key")
		}
	}
	file, _ := os.ReadFile(d.layout.EncKeysPath())
	signed := append([]byte(nil), d.enc.SignedSet()...)
	if done, err := d.regenerateKeys(7); err != nil || done {
		t.Fatalf("the same tag again: %v %v, want nothing done", done, err)
	}
	if again, _ := os.ReadFile(d.layout.EncKeysPath()); !bytes.Equal(again, file) || !bytes.Equal(d.enc.SignedSet(), signed) {
		t.Fatal("the same tag again changed the ring")
	}
	// The tag is on disk: the next process does not regenerate either.
	layout := d.layout
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if done, err := d2.regenerateKeys(7); err != nil || done {
		t.Fatalf("the same tag after a restart: %v %v", done, err)
	}
	if done, err := d2.regenerateKeys(8); err != nil || !done {
		t.Fatalf("a new tag: %v %v", done, err)
	}
	if _, err := d2.regenerateKeys(0); err == nil {
		t.Fatal("a regeneration without a tag was taken")
	}
}

// FS-K19: a deactivated identity's epoch private keys all go at once, and
// the file's old bytes with them.
func TestDeactivateDeletesAllKeys(t *testing.T) {
	d := newTestDaemon(t, "", true)
	kids := ringKids(d)
	var held [][]byte
	for kid := range kids {
		kp, _ := d.enc.Key([]byte(kid))
		held = append(held, kp.Private)
	}
	if err := d.enc.deleteAll(); err != nil {
		t.Fatal(err)
	}
	for kid := range kids {
		if _, ok := d.enc.Key([]byte(kid)); ok {
			t.Fatal("a key outlived the deactivation")
		}
	}
	for _, p := range held {
		if bytes.Count(p, []byte{0}) != len(p) {
			t.Fatal("a deleted private key was not cleared in memory")
		}
	}
	b, err := os.ReadFile(d.layout.EncKeysPath())
	if err != nil {
		t.Fatal(err)
	}
	if f, err := decodeEncKeyFile(b); err != nil || len(f.Keys) != 0 {
		t.Fatalf("the ring file after deactivation: %v, %d keys", err, len(f.Keys))
	}
}

// TestPublishCarriesPrevSeq (FS-K5; the daemon's half of mutation 20): a
// publication carries the set in base64url and prev_seq, the last_seq from
// before the set was signed, decoded strictly as the hub decodes it; the
// same set sent again names the same prev_seq; a registration carries the
// same two.
func TestPublishCarriesPrevSeq(t *testing.T) {
	srv, _, prov := registeredPair(t)
	h := fakeHubAt(t, srv.URL)
	seq0 := prov.enc.Seq()
	atClock(prov, epochOf(prov.nowMS())+seal.EpochMS+1)
	prov.maintainKeyRing()
	if prov.enc.Seq() <= seq0 {
		t.Fatal("no new set")
	}
	body := func(i int) hubapi.KeysPublishRequest {
		t.Helper()
		h.mu.Lock()
		defer h.mu.Unlock()
		if i < 0 {
			i += len(h.keyBodies)
		}
		var req hubapi.KeysPublishRequest
		dec := json.NewDecoder(bytes.NewReader(h.keyBodies[i]))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			t.Fatalf("publication %d: %v (%s)", i, err, h.keyBodies[i])
		}
		if !strings.Contains(string(h.keyBodies[i]), `"prev_seq":`) {
			t.Fatalf("publication %d has no prev_seq: %s", i, h.keyBodies[i])
		}
		return req
	}
	req := body(-1)
	keys, err := hubapi.DecodeB64(req.Keys, -1)
	if err != nil || !bytes.Equal(keys, prov.enc.SignedSet()) {
		t.Fatalf("keys is not the set in base64url: %v", err)
	}
	if req.PrevSeq != seq0 {
		t.Fatalf("prev_seq %d, want %d (last_seq before the set was signed)", req.PrevSeq, seq0)
	}
	// The same set again: the same prev_seq, and the hub says unchanged.
	prov.publishedKeySeq.Store(0)
	if err := prov.publishKeys(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if again := body(-1); again.PrevSeq != seq0 || again.Keys != req.Keys {
		t.Fatalf("sent again with prev_seq %d", again.PrevSeq)
	}
	// A registration carries the set and prev_seq too.
	n := len(h.requests)
	if err := prov.RegisterWithHub(context.Background(), srv.URL, "Prov", nil); err != nil {
		t.Fatal(err)
	}
	var reg hubapi.RegisterRequest
	found := false
	h.mu.Lock()
	for _, r := range h.requests[n:] {
		if r.path == "/register" {
			found = json.Unmarshal(r.body, &reg) == nil
		}
	}
	h.mu.Unlock()
	if !found || reg.PrevSeq == nil || *reg.PrevSeq != seq0 {
		t.Fatalf("the registration's prev_seq: %v", reg.PrevSeq)
	}
	if b, err := hubapi.DecodeB64(reg.EncKeys, -1); err != nil || !bytes.Equal(b, prov.enc.SignedSet()) {
		t.Fatalf("the registration's enc_keys is not the set in base64url: %v", err)
	}
}

// TestRemovedAtAfter200 (FS-K6): once the hub registered a set, the keys
// it does not list — the previous epoch's past its deadline, and retired
// ones — have their removed_at set to that moment; a key of an epoch still
// to come is never marked; a refused publication marks nothing.
func TestRemovedAtAfter200(t *testing.T) {
	srv, _, prov := registeredPair(t)
	h := fakeHubAt(t, srv.URL)
	e := epochOf(prov.nowMS())
	prevKID := keySetOf(t, prov).Keys[0].KID
	past := e + seal.EpochMS + seal.DeliveryDeadlineMS + drainMarginMS + 1
	clock := atClock(prov, past)
	h.mu.Lock()
	h.refuseKeySets = true
	h.keysAt = clock.Load
	h.mu.Unlock()
	prov.maintainKeyRing()
	if k := ringKids(prov)[string(prevKID)]; k == nil || k.RemovedAt != 0 {
		t.Fatalf("a refused publication marked the old key removed: %+v", k)
	}
	h.mu.Lock()
	h.refuseKeySets = false
	h.mu.Unlock()
	prov.publishedKeySeq.Store(0)
	prov.publishIfNeeded()
	for kid, k := range ringKids(prov) {
		switch {
		case kid == string(prevKID):
			if k.RemovedAt != past {
				t.Fatalf("the previous epoch's key: removed_at %d, want %d", k.RemovedAt, past)
			}
		case k.RemovedAt != 0:
			t.Fatalf("a key still published has removed_at %d", k.RemovedAt)
		}
	}
	// A regeneration: every old key is marked when the new set is
	// registered, the future ones too.
	clock.Store(past + 60_000)
	old := ringKids(prov)
	if _, err := prov.regenerateKeys(9); err != nil {
		t.Fatal(err)
	}
	if err := prov.publishKeys(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	for kid, k := range ringKids(prov) {
		if o := old[kid]; o != nil && k.RemovedAt == 0 {
			t.Fatalf("a retired key [%d, %d) was not marked removed", k.Pair.Public.NotBefore, k.Pair.Public.NotAfter)
		} else if o == nil && k.RemovedAt != 0 {
			t.Fatal("a new key was marked removed")
		}
	}
}

// TestKeysRollbackAdoptsStoredSeq (FS-B2's branch that ends nothing, until
// FS-10 adds the check): a hub that holds a newer set of this identity than
// the ring signed answers 409 keys_rollback with stored_seq; the ring signs
// its set again above it with prev_seq stored_seq, and the registration or
// the publication goes through. Counted, nothing else: no task ends.
func TestKeysRollbackAdoptsStoredSeq(t *testing.T) {
	srv, req, prov := registeredPair(t)
	h := fakeHubAt(t, srv.URL)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "a task in flight", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if ix, err := prov.ix.Get(id); err != nil || ix.IsTerminal() {
		t.Fatalf("the provider's task: %v %v", ix, err)
	}
	stored := prov.enc.Seq()
	layout := prov.layout
	// The provider loses its key ring: the new one signs from last_seq 0,
	// so its first publication names prev_seq 0 under the hub's set.
	if err := prov.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(layout.EncKeysPath()); err != nil {
		t.Fatal(err)
	}
	p2, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p2.Close() })
	p2.stopRelayLoop()
	if err := p2.RegisterWithHub(ctx, srv.URL, "Prov", nil); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	got := append([]byte(nil), h.agents[p2.AID()].keyset...)
	h.mu.Unlock()
	if !bytes.Equal(got, p2.enc.SignedSet()) {
		t.Fatal("the hub does not hold the new ring's set after the registration")
	}
	if _, seq, prev := p2.enc.published(); prev != stored || seq <= stored {
		t.Fatalf("after the hub's keys_rollback: seq %d, prev_seq %d; want above and equal to the hub's %d", seq, prev, stored)
	}
	if n := counter(p2, counterKeysRollbackUnverified); n != 1 {
		t.Fatalf("keys-rollback-unverified counted %d times", n)
	}
	// The same through POST /agents/{aid}/keys: the hub gets a newer set
	// this identity signed (another copy of it, say).
	newer := decodedSet(t, got)
	newer.Seq += 1000
	newer.IssuedAt = newer.Seq
	signed, err := seal.SignEncKeySet(&newer, p2.self.Sign)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := signed.Marshal()
	h.mu.Lock()
	h.agents[p2.AID()].keyset = b
	h.mu.Unlock()
	atClock(p2, epochOf(p2.nowMS())+seal.EpochMS+60_000)
	p2.maintainKeyRing()
	h.mu.Lock()
	got = append([]byte(nil), h.agents[p2.AID()].keyset...)
	h.mu.Unlock()
	if !bytes.Equal(got, p2.enc.SignedSet()) || p2.enc.Seq() <= newer.Seq {
		t.Fatalf("after the second keys_rollback the hub holds seq %d, the ring %d", decodedSet(t, got).Seq, p2.enc.Seq())
	}
	if n := counter(p2, counterKeysRollbackUnverified); n != 2 {
		t.Fatalf("keys-rollback-unverified counted %d times", n)
	}
	if ix, err := p2.ix.Get(id); err != nil || ix.IsTerminal() {
		t.Fatalf("a keys_rollback ended the task in flight: %v %v", ix, err)
	}
}

// TestEncKeyFileStrict (FS §2.11, CORE-E3): enc_keys.cbor is read strictly.
// Each damage is refused, and the start says the ring is damaged rather
// than making a new one over it.
func TestEncKeyFileStrict(t *testing.T) {
	d := newTestDaemon(t, "", true)
	good, err := os.ReadFile(d.layout.EncKeysPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeEncKeyFile(good); err != nil {
		t.Fatalf("this version's own file: %v", err)
	}
	edit := func(f func(m map[uint64]any)) []byte {
		var m map[uint64]any
		if err := coredet.Unmarshal(good, &m); err != nil {
			t.Fatal(err)
		}
		f(m)
		b, err := coredet.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	editKey := func(f func(k map[any]any)) []byte {
		return edit(func(m map[uint64]any) {
			keys := m[4].([]any)
			f(keys[0].(map[any]any))
		})
	}
	for name, b := range map[string][]byte{
		"an unregistered key 10": edit(func(m map[uint64]any) { m[10] = uint64(1) }),
		"no last_wall":           edit(func(m map[uint64]any) { delete(m, 6) }),
		"no unverified_run":      edit(func(m map[uint64]any) { delete(m, 9) }),
		"regen_tag 0 written":    edit(func(m map[uint64]any) { m[8] = uint64(0) }),
		"v 3":                    edit(func(m map[uint64]any) { m[1] = uint64(3) }),
		"a key twice": edit(func(m map[uint64]any) {
			keys := m[4].([]any)
			m[4] = append(keys, keys[0])
		}),
		"a kid that is not KID(pub)": editKey(func(k map[any]any) {
			pair := k[uint64(1)].(map[any]any)
			pub := pair[uint64(1)].(map[any]any)
			kid := append([]byte(nil), pub[uint64(1)].([]byte)...)
			kid[0] ^= 1
			pub[uint64(1)] = kid
		}),
		"a private key of another public key": editKey(func(k map[any]any) {
			pair := k[uint64(1)].(map[any]any)
			priv := append([]byte(nil), pair[uint64(2)].([]byte)...)
			priv[5] ^= 1
			pair[uint64(2)] = priv
		}),
		"a window over two days": editKey(func(k map[any]any) {
			pub := k[uint64(1)].(map[any]any)[uint64(1)].(map[any]any)
			pub[uint64(5)] = pub[uint64(4)].(uint64) + 3*dayMS
		}),
		"an unknown key in a ring key": editKey(func(k map[any]any) { k[uint64(4)] = true }),
		"trailing bytes":               append(append([]byte(nil), good...), 0),
	} {
		if _, err := decodeEncKeyFile(b); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	// The start refuses a damaged ring and says what to do.
	layout := d.layout
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	damaged := editKey(func(k map[any]any) { k[uint64(4)] = true })
	if err := os.WriteFile(layout.EncKeysPath(), damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	d2, err := New(layout)
	if d2 != nil {
		d2.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "damaged") || !strings.Contains(err.Error(), "move it out") {
		t.Fatalf("a damaged ring: %v", err)
	}
	if b, _ := os.ReadFile(layout.EncKeysPath()); !bytes.Equal(b, damaged) {
		t.Fatal("the start wrote over a damaged ring")
	}
}

// FuzzEncKeyFile: decodeEncKeyFile never panics, and what it takes encodes
// back to the same bytes.
func FuzzEncKeyFile(f *testing.F) {
	d := newTestDaemon(f, "", true)
	good, err := os.ReadFile(d.layout.EncKeysPath())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good)
	f.Add([]byte{0xa0})
	f.Add([]byte{0xa1, 0x01, 0x02})
	v1, _ := coredet.Marshal(v02KeyRing(1))
	f.Add(v1)
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := decodeEncKeyFile(b)
		if err != nil {
			return
		}
		re, err := coredet.Marshal(got)
		if err != nil || !bytes.Equal(re, b) {
			t.Fatalf("taken but not canonical: %v", err)
		}
		if got.V != encKeyFileVersion {
			t.Fatalf("took version %d", got.V)
		}
		kids := map[string]bool{}
		for _, k := range got.Keys {
			if checkRingPair(&k.Pair) != nil || kids[string(k.Pair.Public.KID)] {
				t.Fatal("took a key the rules refuse")
			}
			kids[string(k.Pair.Public.KID)] = true
		}
	})
}

package daemon

// enckeys.go is this node's epoch key ring (forward-secrecy §4.3; A2A-DESIGN
// §3.1): the X25519 key pairs other nodes seal envelopes to, the signed key
// set that publishes their public halves with the node's mailbox signing key
// (mbox.go), and when each private half is deleted.
//
// Keys (FS-K1, FS-K2): one key per UTC day (epoch e = floor(t / E), valid in
// [e·E, (e+1)·E)), held for the current epoch and the 14 after it. Every node
// changes keys at the same instant, so the change says nothing about the
// node, and a sender with a cached set can seal to it for two weeks without
// asking anyone.
//
// The published set (FS-K3) is the keys not retired and still in the hub's
// registered set until their not_after + D + M (so a hub accepting an
// envelope until not_after + D never refuses one for a key the set dropped
// first), and every key not retired of the coming epochs up to the 14th: at
// most 16 keys. Its seq is NextSeq(now, last_seq) and its issued_at now; a
// publication names prev_seq, the last_seq before the set was signed (FS-K5),
// and a hub that holds a newer set than that answers 409 keys_rollback.
// Once the hub registered a set (200), the keys it does not hold have their
// removed_at set (FS-K6): the hub accepts no more envelopes for them.
//
// A private key goes when epochKeyDeletable says so, under the clock gate of
// the maintenance period (epoch_delete.go): until FS-5 brings the early
// branch, at its hard bound not_after + L, at gate level full or local.
// Deleting one rewrites the file and overwrites the old file's bytes with
// zeros through a handle opened before the rename (FS-K12), so no copy of
// the key is left in the file system's freed blocks by this daemon; the key
// slice in memory is cleared. Nothing is promised about copies Go's runtime
// or crypto/ecdh made.
//
// The ring is written to <data>/enc_keys.cbor (0600, v2: EncKeyFile in the
// registry) and fsynced before any key it contains is used: before the
// signed set is attached to an outgoing message or published to the hub. A
// key a peer may already seal to can therefore never be lost to a crash
// between announcing and saving it. The file also keeps unverified_run, the
// running time with the hub gate closed (FS-K14a), so that restarting the
// daemon does not reset it.
//
// Rotations (FS-K17 to FS-K21): a planned KEL rotation only signs the same
// set again under the new key state. An emergency rotation and a local
// state rollback replace the ring (regenerate): every key is retired —
// published no more, kept until deletable so the mail already queued for
// them still opens — and a new key is made for each epoch of the window.
// Between the emergency rotation's E1 and E2 no set is signed or published
// (keysHeld): the mailbox key is already the new one, and a set signed then
// would publish the old epoch keys under it.
//
// A data directory that has an identity but no key ring (an identity
// restored or copied without it) gets a fresh ring at start. Envelopes that
// were sealed to the previous ring's keys cannot be opened and are dropped
// as sealed-to-unknown-key; the peer that sent them sees no delivery error.

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/seal"
)

// EncKeysPath is the key ring file.
func (l Layout) EncKeysPath() string { return filepath.Join(l.Root, "enc_keys.cbor") }

// encKeyFileVersion is v, key 1, of enc_keys.cbor (FS §2.11). A ring of
// another version stops the start (CheckDataDirForStart): 0.2 wrote 1.
const encKeyFileVersion = 2

// ringWindowEpochs is how many epochs after the current one the ring holds
// keys for: H / E (FS-K1).
const ringWindowEpochs = seal.PublishAheadMS / seal.EpochMS

// ringKey is one key of the epoch key ring as enc_keys.cbor keeps it
// (RingKey, FS §2.11): the pair, when the hub's registered set stopped
// holding it, and whether a regeneration retired it.
//
//anet:wire RingKey
type ringKey struct {
	Pair seal.KeyPair `cbor:"1,keyasint"`
	// RemovedAt is when a set without the key was registered at the
	// current hub (FS-K6), local unix ms; 0 while the registered set holds
	// it.
	RemovedAt uint64 `cbor:"2,keyasint"`
	// Retired: an emergency rotation or a local state rollback replaced the
	// ring (FS-K21); the key is published no more and kept until deletable.
	Retired bool `cbor:"3,keyasint"`
}

// rollbackMark is the last local state rollback this ring took note of
// (RollbackMark, FS §2.11, §4.7). The ring keeps it as it is given.
//
//anet:wire RollbackMark
type rollbackMark struct {
	At        uint64 `cbor:"1,keyasint"` // when it was detected
	StoredSeq uint64 `cbor:"2,keyasint"`
	Tasks     uint64 `cbor:"3,keyasint"` // in-flight interactions ended
	Logged    bool   `cbor:"4,keyasint"` // its evidence event is written
	PrevAt    uint64 `cbor:"5,keyasint"` // at of the mark before it; 0 for none
	Silent    bool   `cbor:"6,keyasint"` // a silent adoption: not shown, no evidence
}

// encKeyFile is enc_keys.cbor v2 (EncKeyFile). It holds private keys.
//
//anet:wire EncKeyFile
type encKeyFile struct {
	V       uint64    `cbor:"1,keyasint"` // encKeyFileVersion
	AID     string    `cbor:"2,keyasint"`
	LastSeq uint64    `cbor:"3,keyasint"` // the largest seq this ring signed
	Keys    []ringKey `cbor:"4,keyasint"`
	// Signed is the last SignedEncKeySet this ring produced. Kept so a
	// restart republishes the identical set (same seq, same bytes), which
	// the hub accepts as unchanged, instead of minting a new seq for
	// content that did not change.
	Signed []byte `cbor:"5,keyasint,omitempty"`
	// LastWall is the local wall clock (unix ms) of the maintenance period
	// that wrote the file; UnverifiedRun the running time with the hub gate
	// closed (FS-K14a).
	LastWall      uint64        `cbor:"6,keyasint"`
	Rollback      *rollbackMark `cbor:"7,keyasint,omitempty"`
	RegenTag      uint64        `cbor:"8,keyasint,omitempty"`
	UnverifiedRun uint64        `cbor:"9,keyasint"`
}

// errRingVersion is a key ring of another version than this one's.
var errRingVersion = errors.New("anet: the key ring is of another anet version")

// decodeEncKeyFile reads enc_keys.cbor strictly (CORE-E3): no key the
// registry does not list, the canonical encoding of what it holds — so a
// required key cannot be missing — version 2, and keys a sender could be
// given: suite 1, a kid that is KID(suite, pub), a 32-byte public key that
// the 32-byte private key derives, a window of at most MaxKeyValidityMS,
// each kid once.
func decodeEncKeyFile(b []byte) (*encKeyFile, error) {
	if v, ok := keyRingFileVersion(b); ok && v != encKeyFileVersion {
		return nil, fmt.Errorf("%w (v %d, this version reads %d)", errRingVersion, v, encKeyFileVersion)
	}
	var f encKeyFile
	if err := coredet.UnmarshalStrict(b, &f); err != nil {
		return nil, err
	}
	re, err := coredet.Marshal(&f)
	if err != nil {
		return nil, err
	}
	same := bytes.Equal(re, b)
	clear(re)
	if !same {
		return nil, errors.New("not the canonical encoding of the key ring")
	}
	seen := map[string]bool{}
	for i := range f.Keys {
		k := &f.Keys[i]
		if err := checkRingPair(&k.Pair); err != nil {
			return nil, fmt.Errorf("key %d: %w", i, err)
		}
		if seen[string(k.Pair.Public.KID)] {
			return nil, fmt.Errorf("key %d: kid appears twice", i)
		}
		seen[string(k.Pair.Public.KID)] = true
	}
	return &f, nil
}

// checkRingPair is what a pair of the ring must be to be published and to
// open what is sealed to it.
func checkRingPair(kp *seal.KeyPair) error {
	k := &kp.Public
	switch {
	case k.Suite != seal.SuiteX25519:
		return fmt.Errorf("suite %d", k.Suite)
	case len(k.Pub) != 32 || len(kp.Private) != 32:
		return errors.New("a key is not 32 bytes")
	case !bytes.Equal(k.KID, seal.KID(k.Suite, k.Pub)):
		return errors.New("kid is not KID(suite, pub)")
	case k.NotAfter <= k.NotBefore || k.NotAfter-k.NotBefore > seal.MaxKeyValidityMS:
		return fmt.Errorf("window [%d, %d)", k.NotBefore, k.NotAfter)
	}
	sk, err := ecdh.X25519().NewPrivateKey(kp.Private)
	if err != nil || !bytes.Equal(sk.PublicKey().Bytes(), k.Pub) {
		return errors.New("the private key is not the public key's")
	}
	return nil
}

// encKeyRing implements seal.KeyRing for Open and supplies the signed key
// set every outgoing inner message carries.
type encKeyRing struct {
	path string
	aid  string

	// maintMu serializes the writers (maintain, markRemoved, adopt,
	// regenerate, deleteAll), so two never compute from the same state and
	// write different files. mu guards the fields below; readers (Key,
	// SignedSet) take it only for reads and are not blocked while a writer
	// writes the file.
	maintMu sync.Mutex
	mu      sync.RWMutex
	keys    []*ringKey // ascending by not_before, then kid
	lastSeq uint64
	// prevSeq is the prev_seq of signed: last_seq before it was signed
	// (FS-K5). After a restart it is last_seq, which is not below the true
	// one: a hub holding a set this node never signed is still refused by
	// it unless that set's seq falls between the two.
	prevSeq  uint64
	signed   []byte    // current SignedEncKeySet encoding
	mbox     seal.Mbox // the mailbox signing key the signed set publishes; zero until known
	lastWall uint64
	run      uint64 // unverified_run (FS-K14a)
	rollback *rollbackMark
	regenTag uint64
	now      func() uint64
	// mboxOf is mboxSource: the node's current mailbox signing key
	// (mbox.Current(), mbox.go), which every set the ring signs publishes
	// (SSR-D3). false while the node has none yet; the ring then keeps
	// publishing the key of its last set, or signs nothing when it never
	// signed one.
	mboxOf func() (seal.Mbox, bool)
	// noteErr counts a failure the ring survives (the daemon's countErr).
	noteErr func(counter string, err error)
}

// counterOverwriteFailed counts a rewrite of the key ring that could not
// overwrite the old file's bytes (FS-K12). The new state stands.
const counterOverwriteFailed = "keyring-overwrite-failed"

// openKeyRing loads the ring at path for aid, or starts an empty one when
// the file is absent or belongs to another AID. A temporary file a crash
// left is overwritten and removed first (FS-K13): it may hold private keys.
// It does not create keys; maintain does.
func openKeyRing(path, aid string, now func() uint64) (*encKeyRing, error) {
	r := &encKeyRing{path: path, aid: aid, now: now}
	if err := zeroAndRemove(path + ".tmp"); err != nil {
		return nil, fmt.Errorf("anet: clear the key ring's leftover temporary file: %w", err)
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("anet: read the encryption key ring: %w", err)
	}
	defer clear(b)
	f, err := decodeEncKeyFile(b)
	if errors.Is(err, errRingVersion) {
		// CheckDataDirForStart says this before the start; a caller that
		// came here without it gets the same sentence.
		if v, _ := keyRingFileVersion(b); v > encKeyFileVersion {
			return nil, newerDataDirRefusal()
		}
		return nil, oldDataDirRefusal()
	}
	if err != nil {
		return nil, fmt.Errorf("anet: the encryption key ring %s is damaged (%v); move it out of the data directory "+
			"and start anet again, which makes a new one (mail already sealed to the old keys cannot be opened then)", path, err)
	}
	if f.AID != aid {
		// The data directory's identity changed under the ring. Its keys
		// were published for another AID and must not be used for this one;
		// the first write replaces the file and overwrites its bytes.
		for i := range f.Keys {
			clear(f.Keys[i].Pair.Private)
		}
		return r, nil
	}
	r.lastSeq, r.prevSeq, r.signed = f.LastSeq, f.LastSeq, f.Signed
	r.lastWall, r.run, r.rollback, r.regenTag = f.LastWall, f.UnverifiedRun, f.Rollback, f.RegenTag
	// The mailbox key stays what the last signed set published.
	if s, err := seal.UnmarshalSignedEncKeySet(f.Signed); err == nil {
		var set seal.EncKeySet
		if err := coredet.Unmarshal(s.Set, &set); err == nil {
			r.mbox = set.Mbox
		}
	}
	for i := range f.Keys {
		k := f.Keys[i]
		r.keys = append(r.keys, &k)
	}
	sortRingKeys(r.keys)
	return r, nil
}

func sortRingKeys(ks []*ringKey) {
	sort.SliceStable(ks, func(i, j int) bool {
		a, b := &ks[i].Pair.Public, &ks[j].Pair.Public
		if a.NotBefore != b.NotBefore {
			return a.NotBefore < b.NotBefore
		}
		return bytes.Compare(a.KID, b.KID) < 0
	})
}

// Key implements seal.KeyRing: the pair with this kid, while the ring holds
// its private half (FS-C16). Which keys the ring holds is the deletion
// rule's business (maintain), not the lookup's.
func (r *encKeyRing) Key(kid []byte) (*seal.KeyPair, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, k := range r.keys {
		if bytes.Equal(k.Pair.Public.KID, kid) {
			return &k.Pair, true
		}
	}
	return nil, false
}

// SignedSet returns the current SignedEncKeySet encoding, or nil when the
// ring has never signed one.
func (r *encKeyRing) SignedSet() []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.signed
}

// Seq returns the seq of the current signed set: last_seq.
func (r *encKeyRing) Seq() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastSeq
}

// published is the current signed set with the prev_seq a publication of
// it names (FS-K5).
func (r *encKeyRing) published() (signed []byte, seq, prevSeq uint64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.signed, r.lastSeq, r.prevSeq
}

// unverifiedRun is unverified_run as the ring holds it.
func (r *encKeyRing) unverifiedRun() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.run
}

// epochStart is s_e of the epoch t falls in.
func epochStart(t uint64) uint64 { return t - t%seal.EpochMS }

// publishable reports whether k may be in a published set at all: not
// retired, and never dropped from a set the hub registered (removed_at 0).
// A key once removed is not published again, even if a wall clock that
// went back makes it look current: the hub stopped taking mail for it, and
// its A(k) is fixed (FS-K7). The window gets a new key instead.
func publishable(k *ringKey) bool { return !k.Retired && k.RemovedAt == 0 }

// publishedKeys is FS-K3 at now: the publishable keys whose not_after + D +
// M is still ahead — the previous epoch's stays until then, so a hub taking
// envelopes for it until not_after + D never refuses one the set dropped
// first — and those of the coming epochs up to the window's last (a key past
// the window, left by a wall clock that went back, waits until the window
// reaches it); ascending by not_before, at most MaxKeysPerSet (the newest).
func publishedKeys(keys []*ringKey, now uint64) []seal.EncKey {
	windowEnd := satAdd(epochStart(now), (ringWindowEpochs+1)*seal.EpochMS)
	var pub []seal.EncKey
	for _, k := range keys {
		pk := &k.Pair.Public
		if !publishable(k) || pk.NotBefore >= windowEnd {
			continue
		}
		if now < satAdd(satAdd(pk.NotAfter, seal.DeliveryDeadlineMS), drainMarginMS) || pk.NotBefore > now {
			pub = append(pub, *pk)
		}
	}
	if len(pub) > seal.MaxKeysPerSet {
		pub = pub[len(pub)-seal.MaxKeysPerSet:]
	}
	return pub
}

// ringInputs is what one maintenance of the ring works from.
type ringInputs struct {
	now    uint64
	sign   seal.SignFunc
	keySeq uint64 // the identity's key_state_seq
	// gate, drainMark and heldKids decide deletions (epochKeyDeletable).
	gate      clockLevel
	drainMark uint64
	heldKids  kidSet
	// run is unverified_run after this period's update (FS-K14a); nil
	// keeps the ring's (a maintenance between periods). hold says no set
	// may be signed now (keysHeld).
	run  *uint64
	hold bool
}

// maintain applies the ring's rules at in.now: deletes the keys
// epochKeyDeletable lets go, makes the keys the window lacks (FS-K1), signs
// the published set again when its keys, its mailbox key or the signing key
// state changed (unless in.hold), and writes the result to the key ring
// file before the ring uses it. It reports whether the signed set changed.
//
// The new state is written and fsynced first and only then becomes the
// ring's state. Every outgoing inner message carries SignedSet, and a peer
// that receives a newer set seals its next message to the new key; a key
// set that reached a peer or the hub while its private keys existed only in
// memory would, after a crash, leave that peer sealing to a key nobody
// holds. When the write fails the ring keeps its previous state, nothing
// new is attached or published, and the next run tries again; keys it made
// for the failed state are cleared.
func (r *encKeyRing) maintain(in ringInputs) (bool, error) {
	r.maintMu.Lock()
	defer r.maintMu.Unlock()
	r.mu.RLock()
	current := append([]*ringKey(nil), r.keys...)
	lastSeq, prevSeq, signedNow, mbox, run := r.lastSeq, r.prevSeq, r.signed, r.mbox, r.run
	r.mu.RUnlock()
	mbox = r.mboxNow(mbox)
	changed := false
	if in.run != nil && *in.run != run {
		run, changed = *in.run, true
	}

	// Delete what may go (FS-K7, FS-K12).
	var keys, gone []*ringKey
	for _, k := range current {
		if ok, _ := epochKeyDeletable(k, in.now, in.gate, in.drainMark, in.heldKids); ok {
			gone = append(gone, k)
			changed = true
			continue
		}
		keys = append(keys, k)
	}

	// Make the keys the window lacks (FS-K1, FS-K2).
	var made []*ringKey
	have := map[uint64]bool{}
	for _, k := range keys {
		if publishable(k) && k.Pair.Public.NotAfter-k.Pair.Public.NotBefore == seal.EpochMS && k.Pair.Public.NotBefore%seal.EpochMS == 0 {
			have[k.Pair.Public.NotBefore] = true
		}
	}
	for i := uint64(0); i <= ringWindowEpochs; i++ {
		s := satAdd(epochStart(in.now), i*seal.EpochMS)
		if have[s] || s > math.MaxUint64-seal.EpochMS {
			continue // held already, or an epoch whose end no uint64 holds
		}
		kp, err := seal.GenerateKeyPair(seal.SuiteX25519, s, s+seal.EpochMS)
		if err != nil {
			clearRingKeys(made)
			return false, fmt.Errorf("anet: generate encryption key: %w", err)
		}
		made = append(made, &ringKey{Pair: *kp})
	}
	if len(made) > 0 {
		keys = append(keys, made...)
		sortRingKeys(keys)
		changed = true
	}

	// Sign the published set again when it changed (FS-K3, FS-K5, FS-K17).
	newSigned, newLast, newPrev := signedNow, lastSeq, prevSeq
	pub := publishedKeys(keys, in.now)
	if !in.hold && mbox.Epoch != 0 && len(pub) > 0 && !signedMatches(signedNow, r.aid, pub, mbox, in.keySeq) {
		b, seq, err := r.signSet(in, pub, mbox, lastSeq)
		if err != nil {
			clearRingKeys(made)
			return false, err
		}
		newSigned, newLast, newPrev = b, seq, lastSeq
		changed = true
	}
	if !changed {
		if len(signedNow) == 0 {
			return false, errMboxPending
		}
		return false, nil
	}
	err := r.writeState(r.stateWith(func(s *ringState) {
		s.keys, s.lastSeq, s.signed, s.lastWall, s.run = keys, newLast, newSigned, in.now, run
	}))
	if err != nil {
		clearRingKeys(made)
		return false, fmt.Errorf("anet: save key ring: %w", err)
	}
	r.mu.Lock()
	r.keys, r.lastSeq, r.prevSeq, r.signed, r.lastWall, r.run = keys, newLast, newPrev, newSigned, in.now, run
	if mbox.Epoch != 0 && !bytes.Equal(newSigned, signedNow) {
		r.mbox = mbox
	}
	r.mu.Unlock()
	clearRingKeys(gone)
	if len(newSigned) == 0 {
		// No mailbox key yet (mbox.go): the keys are saved, and the first
		// set is signed once the key exists.
		return false, errMboxPending
	}
	return !bytes.Equal(newSigned, signedNow), nil
}

// mboxNow is the mailbox signing key a set signed now publishes: the node's
// current one (mboxSource, SSR-D3), or last, the key of the ring's last
// set, while the node has none.
func (r *encKeyRing) mboxNow(last seal.Mbox) seal.Mbox {
	if r.mboxOf != nil {
		if m, ok := r.mboxOf(); ok {
			return m
		}
	}
	return last
}

// signSet signs pub with mbox under the identity's current key state: seq
// NextSeq(now, lastSeq), issued_at now (FS-K5).
func (r *encKeyRing) signSet(in ringInputs, pub []seal.EncKey, mbox seal.Mbox, lastSeq uint64) ([]byte, uint64, error) {
	set := &seal.EncKeySet{Type: seal.EncKeySetType, AID: r.aid, Seq: seal.NextSeq(in.now, lastSeq), Keys: pub,
		IssuedAt: in.now, Mbox: mbox}
	signed, err := seal.SignEncKeySet(set, in.sign)
	if err != nil {
		return nil, 0, fmt.Errorf("anet: sign encryption key set: %w", err)
	}
	if signed.KeyStateSeq != in.keySeq {
		return nil, 0, fmt.Errorf("anet: key set signed under key state %d, identity is at %d", signed.KeyStateSeq, in.keySeq)
	}
	b, err := signed.Marshal()
	if err != nil {
		return nil, 0, err
	}
	return b, set.Seq, nil
}

// signedMatches reports whether signed (a SignedEncKeySet encoding) lists
// exactly pub and mbox for aid and was signed under key state keySeq.
func signedMatches(signed []byte, aid string, pub []seal.EncKey, mbox seal.Mbox, keySeq uint64) bool {
	if len(signed) == 0 {
		return false
	}
	s, err := seal.UnmarshalSignedEncKeySet(signed)
	if err != nil || s.KeyStateSeq != keySeq {
		return false
	}
	var set seal.EncKeySet
	if err := coredet.Unmarshal(s.Set, &set); err != nil || set.AID != aid || len(set.Keys) != len(pub) ||
		set.Mbox.Epoch != mbox.Epoch || !bytes.Equal(set.Mbox.Pub, mbox.Pub) {
		return false
	}
	for i := range pub {
		if !bytes.Equal(set.Keys[i].KID, pub[i].KID) || set.Keys[i].NotBefore != pub[i].NotBefore ||
			set.Keys[i].NotAfter != pub[i].NotAfter {
			return false
		}
	}
	return true
}

// setKIDs returns the kids a SignedEncKeySet encoding lists.
func setKIDs(signed []byte) (kidSet, bool) {
	s, err := seal.UnmarshalSignedEncKeySet(signed)
	if err != nil {
		return nil, false
	}
	var set seal.EncKeySet
	if err := coredet.Unmarshal(s.Set, &set); err != nil {
		return nil, false
	}
	out := kidSet{}
	for _, k := range set.Keys {
		out[string(k.KID)] = struct{}{}
	}
	return out, true
}

// markRemoved is FS-K6: the hub registered the set published (200), so
// every key of the ring it does not list stopped being acceptable there now:
// its removed_at is set, once — the keys that left the set (the previous
// epoch's, past its not_after + D + M) and the retired ones (FS-K18), not a
// key of an epoch still to come that the set never held. Only the set the
// ring holds now counts: an older one that arrived late says nothing of keys
// made since.
func (r *encKeyRing) markRemoved(published []byte, now uint64) error {
	r.maintMu.Lock()
	defer r.maintMu.Unlock()
	r.mu.RLock()
	current, signed := r.keys, r.signed
	r.mu.RUnlock()
	if !bytes.Equal(published, signed) {
		return nil
	}
	listed, ok := setKIDs(published)
	if !ok {
		return nil
	}
	var keys []*ringKey
	changed := false
	for _, k := range current {
		_, in := listed[string(k.Pair.Public.KID)]
		if !in && k.RemovedAt == 0 && (k.Retired || k.Pair.Public.NotBefore <= now) {
			c := *k
			c.RemovedAt = max(now, 1)
			keys = append(keys, &c)
			changed = true
			continue
		}
		keys = append(keys, k)
	}
	if !changed {
		return nil
	}
	return r.commit(keys, nil)
}

// maxFollowedSeq bounds the stored_seq a keys_rollback can make the ring
// follow. A set's seq is the unix ms of its signing, or one above the last
// (seal.NextSeq), which a JSON number holds exactly (FS §2.9): every seq a
// hub honestly holds for this identity is below 2^53. One at or above it is
// a hub's error or forgery, and following it would leave last_seq close to
// 2^63 − 1, above which every hub refuses a set (ANetHub keys.go): a
// node led there could not publish its keys to any hub again. Below the
// bound, the room left under 2^63 is never used up.
const maxFollowedSeq = 1 << 53

// adoptStoredSeq is the hub's 409 keys_rollback with stored_seq (FS-B2, the
// unverified branch): the hub holds a set this ring's publication did not
// follow — newer than its prev_seq, or with a seq not below it. last_seq
// becomes max(last_seq, stored_seq), and the same keys and mailbox key are
// signed again above it, with prev_seq stored_seq. Nothing else changes. It
// reports whether a new set was signed. A stored_seq of maxFollowedSeq or
// more is not followed (an error, which the caller logs): no set of this
// identity has one, and following it would bring last_seq near the 2^63 − 1
// a hub stores at most, past which no set could be published to any hub.
//
// v030-todo(FS-10): FS-B2 first checks the hub's set against this node's
// KEL; when this identity did sign it, the data directory went back (a
// restored backup, or a copy running elsewhere) and FS-B3 ends the tasks in
// flight and regenerates the ring.
func (r *encKeyRing) adoptStoredSeq(storedSeq uint64, in ringInputs) (bool, error) {
	r.maintMu.Lock()
	defer r.maintMu.Unlock()
	r.mu.RLock()
	keys, lastSeq, mbox := r.keys, r.lastSeq, r.mbox
	r.mu.RUnlock()
	if in.hold {
		return false, nil
	}
	if storedSeq >= maxFollowedSeq {
		return false, fmt.Errorf("the hub names stored_seq %d, which no key set of this identity has (a seq is unix ms, below 2^53)", storedSeq)
	}
	mbox = r.mboxNow(mbox)
	pub := publishedKeys(keys, in.now)
	if mbox.Epoch == 0 || len(pub) == 0 {
		return false, errMboxPending
	}
	b, seq, err := r.signSet(in, pub, mbox, max(lastSeq, storedSeq))
	if err != nil {
		return false, err
	}
	if err := r.writeState(r.stateWith(func(s *ringState) { s.lastSeq, s.signed = seq, b })); err != nil {
		return false, fmt.Errorf("anet: save key ring: %w", err)
	}
	r.mu.Lock()
	r.lastSeq, r.prevSeq, r.signed, r.mbox = seq, storedSeq, b, mbox
	r.mu.Unlock()
	return true, nil
}

// regenerate is FS-K21: in one write of the ring file, every key is retired
// (published no more, kept until deletable: the mail already queued for it
// still opens), a new key is made for each epoch of the window, regen_tag
// becomes tag, and the set of the new keys is signed with the current
// mailbox key. The emergency rotation's E2 (tag: the emergency rot's seq,
// FS-K18) and a local state rollback (tag: stored_seq, FS-B3) call it. With
// regen_tag already tag it does nothing, so redoing it after a crash is
// harmless (engine CR11). It reports whether it regenerated.
//
// Every key is retired, the previous epoch's included, which FS-K21 does not
// name: the set published after it then holds only new keys (FS-K18), and a
// key the old data directory holds cannot be the target of new mail even in
// its last hour.
func (r *encKeyRing) regenerate(tag uint64, in ringInputs) (bool, error) {
	if tag == 0 {
		return false, errors.New("anet: a regeneration needs a tag")
	}
	r.maintMu.Lock()
	defer r.maintMu.Unlock()
	r.mu.RLock()
	current, lastSeq, regenTag, mbox := r.keys, r.lastSeq, r.regenTag, r.mbox
	r.mu.RUnlock()
	if regenTag == tag {
		return false, nil
	}
	mbox = r.mboxNow(mbox)
	if mbox.Epoch == 0 {
		return false, errMboxPending
	}
	var keys, made []*ringKey
	for _, k := range current {
		c := *k
		c.Retired = true
		keys = append(keys, &c)
	}
	for i := uint64(0); i <= ringWindowEpochs; i++ {
		s := epochStart(in.now) + i*seal.EpochMS
		kp, err := seal.GenerateKeyPair(seal.SuiteX25519, s, s+seal.EpochMS)
		if err != nil {
			clearRingKeys(made)
			return false, fmt.Errorf("anet: generate encryption key: %w", err)
		}
		made = append(made, &ringKey{Pair: *kp})
	}
	keys = append(keys, made...)
	sortRingKeys(keys)
	b, seq, err := r.signSet(in, publishedKeys(keys, in.now), mbox, lastSeq)
	if err != nil {
		clearRingKeys(made)
		return false, err
	}
	if err := r.writeState(r.stateWith(func(s *ringState) {
		s.keys, s.lastSeq, s.signed, s.regenTag = keys, seq, b, tag
	})); err != nil {
		clearRingKeys(made)
		return false, fmt.Errorf("anet: save key ring: %w", err)
	}
	r.mu.Lock()
	r.keys, r.lastSeq, r.prevSeq, r.signed, r.regenTag, r.mbox = keys, seq, lastSeq, b, tag, mbox
	r.mu.Unlock()
	return true, nil
}

// deleteAll is FS-K19: the identity is deactivated, and every epoch private
// key goes now, the file rewritten and its old bytes overwritten. The
// signed set stays as the last statement made; it is not published again.
func (r *encKeyRing) deleteAll() error {
	r.maintMu.Lock()
	defer r.maintMu.Unlock()
	r.mu.RLock()
	gone := r.keys
	r.mu.RUnlock()
	if err := r.commit([]*ringKey{}, gone); err != nil {
		return err
	}
	return nil
}

// commit writes keys as the ring's keys with the rest of its state, makes
// them the ring's, and clears the private keys of gone. The caller holds
// maintMu.
func (r *encKeyRing) commit(keys, gone []*ringKey) error {
	if err := r.writeState(r.stateWith(func(s *ringState) { s.keys = keys })); err != nil {
		return fmt.Errorf("anet: save key ring: %w", err)
	}
	r.mu.Lock()
	r.keys = keys
	r.mu.Unlock()
	clearRingKeys(gone)
	return nil
}

// clearRingKeys zeroes the private halves of keys (FS-K12).
func clearRingKeys(keys []*ringKey) {
	for _, k := range keys {
		clear(k.Pair.Private)
	}
}

// ringState is one state of the ring as the file holds it.
type ringState struct {
	keys     []*ringKey
	lastSeq  uint64
	signed   []byte
	lastWall uint64
	run      uint64
	rollback *rollbackMark
	regenTag uint64
}

// stateWith is the ring's current state with edit applied. The caller holds
// maintMu.
func (r *encKeyRing) stateWith(edit func(*ringState)) ringState {
	r.mu.RLock()
	s := ringState{keys: r.keys, lastSeq: r.lastSeq, signed: r.signed, lastWall: r.lastWall, run: r.run,
		rollback: r.rollback, regenTag: r.regenTag}
	r.mu.RUnlock()
	edit(&s)
	return s
}

// persist writes the ring's current state to its file durably.
func (r *encKeyRing) persist() error {
	r.maintMu.Lock()
	defer r.maintMu.Unlock()
	return r.writeState(r.stateWith(func(*ringState) {}))
}

// writeState stores one ring state in the key ring file (writeRingFile).
func (r *encKeyRing) writeState(s ringState) error {
	f := encKeyFile{V: encKeyFileVersion, AID: r.aid, LastSeq: s.lastSeq, Keys: []ringKey{}, Signed: s.signed,
		LastWall: s.lastWall, Rollback: s.rollback, RegenTag: s.regenTag, UnverifiedRun: s.run}
	for _, k := range s.keys {
		f.Keys = append(f.Keys, *k)
	}
	b, err := coredet.Marshal(&f)
	if err != nil {
		return err
	}
	defer clear(b)
	return r.writeRingFile(b)
}

// writeRingFile writes data as the key ring file (FS-K12): the old file is
// opened first and kept open; the new one is written to a temporary file,
// fsynced, renamed over the old one, and the directory fsynced, so once it
// returns a crash cannot lose a key the caller goes on to use or publish.
// Then the old inode's bytes are overwritten with zeros through the handle
// kept open and fsynced: the keys this write dropped are not left in the
// file system's freed blocks. That last step failing is counted
// (keyring-overwrite-failed) and logged; the new state stands.
func (r *encKeyRing) writeRingFile(data []byte) error {
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := zeroAndRemove(tmp); err != nil {
		return err
	}
	old, err := os.OpenFile(r.path, os.O_RDWR, 0)
	switch {
	case err == nil:
		defer old.Close()
	case errors.Is(err, os.ErrNotExist):
		old = nil
	default:
		return err
	}
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		fh.Close()
		_ = zeroAndRemove(tmp)
		return err
	}
	if _, err := fh.Write(data); err != nil {
		return fail(err)
	}
	if err := fh.Sync(); err != nil {
		return fail(err)
	}
	if err := fh.Close(); err != nil {
		_ = zeroAndRemove(tmp)
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil {
		_ = zeroAndRemove(tmp)
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	if old != nil {
		if err := zeroFile(old); err != nil {
			err = fmt.Errorf("overwrite the replaced key ring file: %w", err)
			if r.noteErr != nil {
				r.noteErr(counterOverwriteFailed, err)
			} else {
				log.Printf("anet: %s: %v", counterOverwriteFailed, err)
			}
		}
	}
	return nil
}

// zeroFile overwrites every byte of the open file f with zeros and fsyncs
// it.
func zeroFile(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	zeros := make([]byte, 32<<10)
	for off := int64(0); off < fi.Size(); {
		n := min(int64(len(zeros)), fi.Size()-off)
		if _, err := f.WriteAt(zeros[:n], off); err != nil {
			return err
		}
		off += n
	}
	return f.Sync()
}

// zeroAndRemove overwrites the file at path with zeros and removes it
// (FS-K13): a temporary file a write left may hold private keys. A missing
// file is no error.
func zeroAndRemove(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	zerr := zeroFile(f)
	f.Close()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if zerr != nil && !errors.Is(zerr, io.EOF) {
		log.Printf("anet: %s: overwrite %s before removing it: %v", counterOverwriteFailed, path, zerr)
	}
	return nil
}

// syncDir fsyncs the directory dir, so a rename in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeFileSync is writeFileAtomic with fsync of the file and of its
// directory.
func writeFileSync(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := fh.Write(data); err != nil {
		fh.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

// setupKeyRing loads the ring and the mailbox signing key the ring
// publishes (setupMbox), and runs the first maintenance period of the
// process without publishing. Called once from New, before anything can
// publish. A node whose mailbox key waits for its hub (mbox.go) starts
// without a new set; the mailbox loop signs and publishes one once the key
// exists.
func (d *Daemon) setupKeyRing() error {
	r, err := openKeyRing(d.layout.EncKeysPath(), d.AID(), d.nowMS)
	if err != nil {
		return err
	}
	r.noteErr = d.countErr
	if err := d.setupMbox(r.mbox.Epoch); err != nil {
		return err
	}
	r.mboxOf = d.mbox.Current
	d.enc = r
	level, err := d.maintainRing()
	if err != nil && !errors.Is(err, errMboxPending) {
		d.enc = nil
		return err
	}
	d.startLevel = level
	return nil
}

// keyMaintainEvery is the maintenance period S as the loop's timer takes it
// (keyMaintainEveryMS). A knob so a test can watch the loop run periods.
var keyMaintainEvery = newKnob(time.Duration(keyMaintainEveryMS) * time.Millisecond)

// keysHeld reports whether no key set may be signed or published now: an
// emergency rotation is between its E1 and E2 (core §4.7). E1 has put the
// new mailbox key in place, and a set signed then would publish the old
// epoch keys under it; E2 regenerates the ring (regenerate) and publishes
// the set of the new keys.
//
// v030-todo(KEL-9): the emergency rotation's state answers this (KEL-9
// runs E1 and E2); until then only a test holds key sets.
func (d *Daemon) keysHeld() bool {
	if f := d.testKeyHold(); f != nil {
		return f()
	}
	return false
}

// maintainRing is one maintenance period of the ring (FS-K4, FS-K14a): the
// clock gate's level from the latest check against the hub, unverified_run
// brought up to date, then the ring's deletions, new keys and signature
// (maintain). It returns the level the period ran at.
func (d *Daemon) maintainRing() (clockLevel, error) {
	wall, mono := d.nowMS(), d.monoMS()
	obs := d.clockObservation()
	d.clk.mu.Lock()
	prev := d.clk.prev
	d.clk.prev = &cycleMark{wall: wall, mono: mono}
	d.clk.mu.Unlock()
	run := nextUnverifiedRun(d.enc.unverifiedRun(), hubGateOpen(obs, wall, mono), prev, wall, mono)
	level, _ := clockGate(obs, run, wall, mono)
	in := d.ringInputsAt(wall)
	in.gate, in.run, in.hold = level, &run, d.keysHeld()
	in.drainMark, in.heldKids = d.drainInputs()
	_, err := d.enc.maintain(in)
	if err != nil {
		// The ring kept its state, but unverified_run is this process's to
		// count: it goes to the file with the next write.
		d.enc.mu.Lock()
		d.enc.run = run
		d.enc.mu.Unlock()
	}
	return level, err
}

// maintainKeyRing brings the key ring's signature up to date and publishes
// the set when the hub does not hold it yet: the mailbox key's retries and
// a new mailbox key call it between maintenance periods (mbox.go). It
// deletes nothing and counts no running time: that is the period's
// (keyMaintenance).
func (d *Daemon) maintainKeyRing() {
	if err := d.resignKeyRing(); err != nil {
		// The ring kept its previous state (see maintain), so nothing that
		// is not on disk is attached to messages or published.
		log.Printf("anet: key ring maintenance: %v (new keys not used or published)", err)
		return
	}
	d.publishIfNeeded()
}

// resignKeyRing brings the key ring's keys and signature up to date —
// a new mailbox key, a new key state, a new epoch — without deleting or
// publishing anything.
func (d *Daemon) resignKeyRing() error {
	in := d.ringInputsAt(d.nowMS())
	in.gate, in.hold = clockClosed, d.keysHeld()
	_, err := d.enc.maintain(in)
	return err
}

// publishIfNeeded publishes the ring's set to the configured hub when the
// hub has not confirmed holding it. A publish failure is retried by the
// next period or the mailbox loop, because the hub keeps serving the
// previous set and its keys stay valid for two weeks.
func (d *Daemon) publishIfNeeded() {
	hub := d.config().HubURL
	if hub == "" || d.keysHeld() || d.publishedKeySeq.Load() == d.enc.Seq() {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
	defer cancel()
	if err := d.publishKeys(ctx, hub); err != nil {
		log.Printf("anet: publish encryption keys to %s: %v", hub, err)
	}
}

// keyMaintenance is the maintenance period S (FS-K4): the clock gate and
// unverified_run, the key ring (deletions, new keys, signature), the
// interaction keys' close_after sweep (FS-X9), the retention sweep (FS-L6)
// and the store's housekeeping after it (sweepHardBound), all under the
// period's one gate level, and then the ring's publication. The deletions run with the mailbox poll's lock, after a poll
// round, so they are serial with the receive path of the hub mailbox; the
// publication, a hub call, runs after the lock is released, so a hub slow
// to answer it does not hold up the next poll.
func (d *Daemon) keyMaintenance() {
	if err := d.keyPeriod(); err != nil {
		if !errors.Is(err, errMboxPending) {
			log.Printf("anet: key ring maintenance: %v (new keys not used or published)", err)
		}
		return
	}
	d.publishIfNeeded()
}

// keyPeriod is the part of a period that runs with the poll lock: the
// ring's maintenance and the hard-bound sweeps at its level.
func (d *Daemon) keyPeriod() error {
	d.pollMu.Lock()
	defer d.pollMu.Unlock()
	level, err := d.maintainRing()
	d.sweepHardBound(level)
	return err
}

// sweepHardBound runs the deletions with a hard bound other than the epoch
// keys' at level: the interaction keys whose close_after has come, and the
// tasks past their retention. Each sweep takes the level and deletes
// nothing at closed (FS-K15). Then, whatever the level, the exec work
// directories of ended tasks go (FS-L11) and the store's log is truncated,
// so no key deleted until now stays readable in it (FS-L8).
func (d *Daemon) sweepHardBound(level clockLevel) {
	if d.ix == nil {
		return
	}
	now := int64(d.nowMS())
	if _, err := d.ix.SweepCloseAfter(now, level); err != nil {
		log.Printf("anet: interaction keys past close_after: %v", err)
	}
	d.pruneRetentionAt(now, level)
	d.pruneEndedWorkDirs()
	d.checkpointStore()
}

// regenerateKeys is the key ring's part of the emergency rotation's E2
// (FS-K18; tag: the emergency rot's seq) and of a local state rollback
// (FS-B3; tag: stored_seq): the ring is regenerated and the set of its new
// keys signed with the current mailbox key (regenerate). The caller
// publishes it.
func (d *Daemon) regenerateKeys(tag uint64) (bool, error) {
	return d.enc.regenerate(tag, d.ringInputsAt(d.nowMS()))
}

// ringInputsAt is what the ring signs a set with at now: the identity's
// signer and its key_state_seq, the planned rotation's new key once it is
// in force (FS-K17). Every set the ring signs is signed through it
// (registry signers: EncKeySet).
func (d *Daemon) ringInputsAt(now uint64) ringInputs {
	return ringInputs{now: now, sign: d.self.Sign, keySeq: d.self.CurrentSeq()}
}

// clockState is what the clock gate reads besides the ring's
// unverified_run (FS-K14, FS-K14a): the latest check of this node's clock
// against its hub, and the two clocks at the previous maintenance period of
// this process. In memory only: a restart starts with no check, and its
// first period adds nothing to unverified_run.
type clockState struct {
	mu sync.Mutex
	// hub is the hub the check was made with; a check made with another
	// hub says nothing about this one.
	hub string
	obs clockObs
	// prev is the clocks at the previous maintenance period; nil before the
	// first.
	prev *cycleMark
}

// noteHubDate records a check of this node's clock against the hub at
// hubURL, from the Date header of one of its answers (FS-K14). Only the
// configured hub's answers count, and an answer without a Date that parses
// counts as none. Every answer of the hub is a check: the signed polls,
// registrations and join attempts, key set publications, GET /hub/identity
// and the anonymous side's requests alike (hubTransmit).
func (d *Daemon) noteHubDate(hubURL, date string) {
	if date == "" || !sameHub(hubURL, d.config().HubURL) {
		return
	}
	at, err := http.ParseTime(date)
	if err != nil || at.UnixMilli() < 0 {
		return
	}
	wall, mono := d.nowMS(), d.monoMS()
	d.clk.mu.Lock()
	defer d.clk.mu.Unlock()
	d.clk.hub = strings.TrimRight(hubURL, "/")
	d.clk.obs = clockObs{hub: true, checked: true, wall: wall, mono: mono, offset: int64(wall) - at.UnixMilli()}
}

// clockObservation is the latest check that applies to the configured hub,
// as the gate reads it: no hub configured, or the hub and what its last
// answer said (nothing yet when it has not answered in this process).
func (d *Daemon) clockObservation() clockObs {
	hub := d.config().HubURL
	if hub == "" {
		return clockObs{}
	}
	d.clk.mu.Lock()
	defer d.clk.mu.Unlock()
	if !sameHub(d.clk.hub, hub) {
		return clockObs{hub: true}
	}
	return d.clk.obs
}

// forgetCycle makes the next maintenance period count as the first of the
// process (FS-K14a step 2): it adds nothing to unverified_run.
func (d *Daemon) forgetCycle() {
	d.clk.mu.Lock()
	d.clk.prev = nil
	d.clk.mu.Unlock()
}

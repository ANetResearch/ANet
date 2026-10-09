package daemon

// enckeys.go is this node's encryption key ring (A2A-DESIGN §3.1): the
// X25519 key pairs other nodes seal envelopes to, the signed key set that
// publishes their public halves with the node's mailbox signing key
// (mbox.go), and the policy that rotates them.
//
// Policy (interim, inside the limits of key set v2; see encKeyRotateEvery):
//   - a new key pair every day, each valid for two days;
//   - a private key is kept until seal.MaxMessageLifetimeMS (15 days) after
//     its not_after, so an envelope that waited in a hub mailbox for up to
//     the hub's 14-day TTL can still be opened, then deleted;
//   - the published EncKeySet lists the keys that are valid now or later and
//     the node's mailbox signing key, and its seq is max(now_ms, last_seq +
//     1), persisted with the ring.
//
// The ring is written to <data>/enc_keys.cbor (0600) and fsynced before any
// key it contains is used: before the signed set is attached to an outgoing
// message or published to the hub. A key a peer may already seal to can
// therefore never be lost to a crash between announcing and saving it.
//
// A data directory that has an identity but no key ring (an identity
// restored or copied without it) gets a fresh ring at start. Envelopes that
// were sealed to the previous ring's keys cannot be opened and are dropped
// as sealed-to-unknown-key; the peer that sent them sees no delivery error.

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/seal"
)

// EncKeysPath is the key ring file.
func (l Layout) EncKeysPath() string { return filepath.Join(l.Root, "enc_keys.cbor") }

// encKeyFileVersion is the version of the key ring file format.
const encKeyFileVersion = 1

// v030-todo(FS-4): the epoch key ring replaces this interim policy (one key
// per UTC epoch, published seal.PublishAheadMS ahead, deleted by the drain
// and clock rules). Until then the v0.2 rotation is kept, shortened to fit
// key set v2: a new key every seal.EpochMS, each valid for
// seal.MaxKeyValidityMS, its private half kept seal.MaxMessageLifetimeMS
// past not_after (the hard deletion bound).
const (
	encKeyRotateEvery = seal.EpochMS
	encKeyValidFor    = seal.MaxKeyValidityMS
	encKeyKeptAfter   = seal.MaxMessageLifetimeMS
)

// keptAt reports whether the ring still holds k's private half at now.
func keptAt(k *seal.EncKey, now uint64) bool {
	if k.NotAfter > math.MaxUint64-encKeyKeptAfter {
		return true
	}
	return now < k.NotAfter+encKeyKeptAfter
}

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

// encKeyFile is the persisted form of the ring. It holds private keys.
type encKeyFile struct {
	V       uint64         `cbor:"1,keyasint"`
	AID     string         `cbor:"2,keyasint"`
	LastSeq uint64         `cbor:"3,keyasint"`
	Pairs   []seal.KeyPair `cbor:"4,keyasint"`
	// Signed is the last SignedEncKeySet this ring produced. Kept so a
	// restart republishes the identical set (same seq, same bytes), which
	// the hub accepts as unchanged, instead of minting a new seq for
	// content that did not change.
	Signed []byte `cbor:"5,keyasint,omitempty"`
}

// encKeyRing implements seal.KeyRing for Open and supplies the signed key
// set every outgoing inner message carries.
type encKeyRing struct {
	path string
	aid  string

	// maintMu serializes maintain, so two runs never compute from the same
	// state and write different files. mu guards the fields below; readers
	// (Key, SignedSet) take it only for reads and are not blocked while a
	// maintenance run writes the file.
	maintMu sync.Mutex
	mu      sync.RWMutex
	lastSeq uint64
	pairs   []*seal.KeyPair // ascending by NotBefore
	signed  []byte          // current SignedEncKeySet encoding
	mbox    seal.Mbox       // the mailbox signing key the signed set publishes; zero until known
	now     func() uint64
	// mboxOf is mboxSource: the node's current mailbox signing key
	// (mbox.Current(), mbox.go), which every set the ring signs publishes
	// (SSR-D3). false while the node has none yet; the ring then keeps
	// publishing the key of its last set, or signs nothing when it never
	// signed one.
	mboxOf func() (seal.Mbox, bool)
}

// openKeyRing loads the ring at path for aid, or starts an empty one when
// the file is absent or belongs to another AID. It does not create keys;
// maintain does.
func openKeyRing(path, aid string, now func() uint64) (*encKeyRing, error) {
	r := &encKeyRing{path: path, aid: aid, now: now}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("anet: read key ring: %w", err)
	}
	var f encKeyFile
	if err := coredet.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("anet: key ring %s is unreadable: %w", path, err)
	}
	if f.V != encKeyFileVersion {
		return nil, fmt.Errorf("anet: key ring %s has version %d, this build reads %d", path, f.V, encKeyFileVersion)
	}
	if f.AID != aid {
		// The data directory's identity changed under the ring. Its keys
		// were published for another AID and must not be used for this one.
		return r, nil
	}
	r.lastSeq = f.LastSeq
	r.signed = f.Signed
	// The mailbox key stays what the last signed set published.
	if s, err := seal.UnmarshalSignedEncKeySet(f.Signed); err == nil {
		var set seal.EncKeySet
		if err := coredet.Unmarshal(s.Set, &set); err == nil {
			r.mbox = set.Mbox
		}
	}
	for i := range f.Pairs {
		kp := f.Pairs[i]
		r.pairs = append(r.pairs, &kp)
	}
	sortPairs(r.pairs)
	return r, nil
}

func sortPairs(ps []*seal.KeyPair) {
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Public.NotBefore < ps[j].Public.NotBefore })
}

// Key implements seal.KeyRing: the pair with this kid, while the ring keeps
// its private half (keptAt).
func (r *encKeyRing) Key(kid []byte) (*seal.KeyPair, bool) {
	now := r.now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, kp := range r.pairs {
		if bytes.Equal(kp.Public.KID, kid) && keptAt(&kp.Public, now) {
			return kp, true
		}
	}
	return nil, false
}

// SignedSet returns the current SignedEncKeySet encoding, or nil when the
// ring has never been maintained.
func (r *encKeyRing) SignedSet() []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.signed
}

// Seq returns the seq of the current signed set.
func (r *encKeyRing) Seq() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastSeq
}

// maintain applies the rotation policy at the ring's clock, re-signs the
// published set when its content or the signing key state changed, and
// writes the result to the key ring file before the ring uses it. It
// reports whether anything changed. sign and keySeq are the node's current
// identity signer and key_state_seq.
//
// The new state is written and fsynced first and only then becomes the
// ring's state. Every outgoing inner message carries SignedSet, and a peer
// that receives a newer set seals its next message to the new key; a key
// set that reached a peer or the hub while its private keys existed only in
// memory would, after a crash, leave that peer sealing to a key nobody
// holds. When the write fails the ring keeps its previous state, nothing
// new is attached or published, and the next run tries again.
func (r *encKeyRing) maintain(sign seal.SignFunc, keySeq uint64) (bool, error) {
	r.maintMu.Lock()
	defer r.maintMu.Unlock()
	now := r.now()
	r.mu.RLock()
	current := append([]*seal.KeyPair(nil), r.pairs...)
	lastSeq, signedNow, mbox := r.lastSeq, r.signed, r.mbox
	r.mu.RUnlock()
	changed := false
	if r.mboxOf != nil {
		if m, ok := r.mboxOf(); ok {
			mbox = m
		}
	}
	if mbox.Epoch == 0 {
		return false, errMboxPending
	}

	// Drop private keys past retention.
	var pairs []*seal.KeyPair
	for _, kp := range current {
		if keptAt(&kp.Public, now) {
			pairs = append(pairs, kp)
		} else {
			changed = true
		}
	}

	// Rotate: a new key when none is valid now, or when the newest one is
	// at least one rotation interval old.
	var newest *seal.KeyPair
	valid := false
	for _, kp := range pairs {
		if kp.Public.ValidAt(now) {
			valid = true
		}
		if newest == nil || kp.Public.NotBefore > newest.Public.NotBefore {
			newest = kp
		}
	}
	if !valid || newest == nil || now >= newest.Public.NotBefore+encKeyRotateEvery {
		kp, err := seal.GenerateKeyPair(seal.SuiteX25519, now, now+encKeyValidFor)
		if err != nil {
			return false, fmt.Errorf("anet: generate encryption key: %w", err)
		}
		pairs = append(pairs, kp)
		sortPairs(pairs)
		changed = true
	}

	// The published set: keys not yet expired, newest last, at most
	// seal.MaxKeysPerSet.
	var pub []seal.EncKey
	for _, kp := range pairs {
		if kp.Public.NotAfter > now {
			pub = append(pub, kp.Public)
		}
	}
	if len(pub) > seal.MaxKeysPerSet {
		pub = pub[len(pub)-seal.MaxKeysPerSet:]
	}

	if !changed && signedMatches(signedNow, r.aid, pub, mbox, keySeq) {
		return false, nil
	}
	set := &seal.EncKeySet{Type: seal.EncKeySetType, AID: r.aid, Seq: seal.NextSeq(now, lastSeq), Keys: pub, IssuedAt: now, Mbox: mbox}
	signed, err := seal.SignEncKeySet(set, sign)
	if err != nil {
		return false, fmt.Errorf("anet: sign encryption key set: %w", err)
	}
	if signed.KeyStateSeq != keySeq {
		return false, fmt.Errorf("anet: key set signed under key state %d, identity is at %d", signed.KeyStateSeq, keySeq)
	}
	b, err := signed.Marshal()
	if err != nil {
		return false, err
	}
	if err := r.write(pairs, set.Seq, b); err != nil {
		return false, fmt.Errorf("anet: save key ring: %w", err)
	}
	r.mu.Lock()
	r.pairs, r.lastSeq, r.signed, r.mbox = pairs, set.Seq, b, mbox
	r.mu.Unlock()
	return true, nil
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

// persist writes the ring's current state to its file durably.
func (r *encKeyRing) persist() error {
	r.mu.RLock()
	pairs, lastSeq, signed := append([]*seal.KeyPair(nil), r.pairs...), r.lastSeq, r.signed
	r.mu.RUnlock()
	return r.write(pairs, lastSeq, signed)
}

// write stores one ring state in the key ring file: the temporary file is
// fsynced before the rename and the directory after it, so once write
// returns a crash cannot lose a key the caller goes on to use or publish.
func (r *encKeyRing) write(pairs []*seal.KeyPair, lastSeq uint64, signed []byte) error {
	f := encKeyFile{V: encKeyFileVersion, AID: r.aid, LastSeq: lastSeq, Signed: signed}
	for _, kp := range pairs {
		f.Pairs = append(f.Pairs, *kp)
	}
	b, err := coredet.Marshal(f)
	if err != nil {
		return err
	}
	return writeFileSync(r.path, b, 0o600)
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
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// setupKeyRing loads the ring and the mailbox signing key the ring
// publishes (setupMbox), applies the rotation policy and persists a changed
// ring. Called once from New, before anything can publish. A node whose
// mailbox key waits for its hub (mbox.go) starts without a new set; the
// mailbox loop signs and publishes one once the key exists.
func (d *Daemon) setupKeyRing() error {
	r, err := openKeyRing(d.layout.EncKeysPath(), d.AID(), d.nowMS)
	if err != nil {
		return err
	}
	if err := d.setupMbox(r.mbox.Epoch); err != nil {
		return err
	}
	r.mboxOf = d.mbox.Current
	if _, err := r.maintain(d.self.Sign, d.self.CurrentSeq()); err != nil && !errors.Is(err, errMboxPending) {
		return err
	}
	d.enc = r
	return nil
}

// maintainKeyRing is the periodic half of the policy: rotate and persist
// (maintain), then publish the new set to the hub. A publish failure is
// retried on the next run, because the hub keeps serving the previous set
// and the previous keys stay valid for a week after the rotation.
func (d *Daemon) maintainKeyRing() {
	if _, err := d.enc.maintain(d.self.Sign, d.self.CurrentSeq()); err != nil {
		// The ring kept its previous state (see maintain), so nothing that
		// is not on disk is attached to messages or published.
		log.Printf("anet: key ring maintenance: %v (new keys not used or published)", err)
		return
	}
	hub := d.config().HubURL
	if hub == "" || d.publishedKeySeq.Load() == d.enc.Seq() {
		return
	}
	if err := d.publishKeys(d.ctx, hub); err != nil {
		log.Printf("anet: publish encryption keys to %s: %v", hub, err)
	}
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

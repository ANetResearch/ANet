package daemon

// mbox.go is this node's mailbox signing key (SS §3.2, §4.3.1; core §2.16):
// the Ed25519 key that signs the delivery capabilities this node issues
// (seal.SignCap), published as key 5 of its encryption key set. The key
// ring reads it through mbox.Current() (enckeys.go); the capability policy
// signs with mbox.SignCap.
//
// One key per AID. Its epoch rises by one each time the key is replaced (an
// emergency rotation, core §4.7 E1; a hub whose revocation list for this
// node is full, SS §4.3.8 item 4), and the hub and every holder accept only
// capabilities under the current epoch (SSR-W11): replacing the key voids
// every capability the old one signed without revoking them one by one.
//
// The key lives in <data>/mbox.key, 0600, as {0: "anet.mbox/1", 1: epoch,
// 2: seed}. It is written to a temporary file, fsynced, renamed over the old
// one and the directory fsynced, so a crash leaves either the old key or
// the new one, never neither.
//
// The epoch of a new key (SSR-W10): one above the key it replaces. A node
// that has no mbox.key — a fresh identity, or an identity restored without
// its data directory — takes one above what it published before: the epoch
// its own last key set carried, and, for an identity that has been at a hub
// (its KEL names a witness hub, or an event is pending), what its hub
// stores for it (GET /agents/{self}/keys). An agent joins a hub by
// designating it as its witness, so an identity whose KEL never named one
// has published nothing anywhere and is not asked about. While a configured
// hub cannot be asked, the node does not guess: a key numbered too low would
// be refused by the hub for good (SSR-W9). It keeps asking, and publishes no
// new key set meanwhile.
//
// SSR-D4: after every registration and key set publication the daemon
// checks that the hub took the set (keys_status ok or unchanged) and that
// the set carries the current mailbox key. When not, it publishes again, 5
// seconds after the first failure and doubling to 10 minutes, and reports
// sender_privacy.mailbox_registered = false until it succeeds.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/fsperm"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// MboxKeyPath is the mailbox signing key file.
func (l Layout) MboxKeyPath() string { return filepath.Join(l.Root, "mbox.key") }

// mboxFileType is key 0 of mbox.key (core §3.2 L21).
const mboxFileType = "anet.mbox/1"

// mboxEpochLimit bounds the epoch of a mailbox signing key: [1, 2^53)
// (SSR-W10). An honest hub refuses a key set at 2^53 or above (SSR-W9), so
// it never stores one, and staying below it keeps the epoch exact in every
// JSON reader.
const mboxEpochLimit uint64 = 1 << 53

// Republishing after the hub did not take the current mailbox key (SSR-D4),
// and asking the hub for the epoch of a missing key: the first retry after
// mboxRetryBase, doubling up to mboxRetryMax.
const (
	mboxRetryBase = 5 * time.Second
	mboxRetryMax  = 10 * time.Minute
)

// mboxTickEvery is how often the mailbox loop looks for a retry that is
// due. A knob so a test that drives mboxTick itself can keep the loop out.
var mboxTickEvery = newKnob(5 * time.Second)

// capsPruneEvery is how often the capability tables are pruned (SSR-D2).
// A knob so a test can see the loop prune without waiting five minutes.
var capsPruneEvery = newKnob(5 * time.Minute)

// mboxRetryAfter is the wait before the next retry after failures
// consecutive failures (failures >= 1).
func mboxRetryAfter(failures int) time.Duration {
	d := mboxRetryBase
	for i := 1; i < failures && d < mboxRetryMax; i++ {
		d *= 2
	}
	return min(d, mboxRetryMax)
}

// errMboxPending reports that the node has no mailbox signing key yet.
var errMboxPending = errors.New("anet: this node's mailbox signing key is not ready yet: " +
	"anet is asking its hub which number the new key takes and will publish its keys once it knows")

// mboxKeyFile is mbox.key.
//
//anet:wire MboxKeyFile
type mboxKeyFile struct {
	Type  string `cbor:"0,keyasint"` // mboxFileType
	Epoch uint64 `cbor:"1,keyasint"` // in [1, mboxEpochLimit)
	Seed  []byte `cbor:"2,keyasint"` // ed25519.SeedSize bytes
}

// encodeMboxFile is the content of mbox.key for a key.
func encodeMboxFile(epoch uint64, seed []byte) ([]byte, error) {
	if epoch == 0 || epoch >= mboxEpochLimit || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("anet: no mailbox signing key with epoch %d and a %d-byte seed", epoch, len(seed))
	}
	return coredet.Marshal(mboxKeyFile{Type: mboxFileType, Epoch: epoch, Seed: seed})
}

// decodeMboxFile reads mbox.key strictly: the three keys and no other, the
// type tag, an epoch in [1, 2^53), a 32-byte seed, and the canonical
// encoding of all that.
func decodeMboxFile(b []byte) (epoch uint64, seed []byte, err error) {
	var f mboxKeyFile
	if err := coredet.UnmarshalStrict(b, &f); err != nil {
		return 0, nil, err
	}
	switch {
	case f.Type != mboxFileType:
		return 0, nil, fmt.Errorf("type %q, want %q", f.Type, mboxFileType)
	case f.Epoch == 0 || f.Epoch >= mboxEpochLimit:
		return 0, nil, fmt.Errorf("epoch %d is outside [1, 2^53)", f.Epoch)
	case len(f.Seed) != ed25519.SeedSize:
		return 0, nil, fmt.Errorf("seed is %d bytes, want %d", len(f.Seed), ed25519.SeedSize)
	}
	re, err := coredet.Marshal(f)
	if err != nil {
		return 0, nil, err
	}
	if !bytes.Equal(re, b) {
		return 0, nil, errors.New("not the canonical encoding of the key file")
	}
	return f.Epoch, f.Seed, nil
}

// nextMboxEpoch is the epoch of a new mailbox signing key that replaces
// one at prev (0: there was none), SSR-W10. anomaly is a prev that leaves
// no room below 2^53: the new key restarts at 1 rather than add, since
// only a hub could have put this node there (an honest one never stores
// 2^53 or more, SSR-W9) and 2^64 − 1 + 1 would wrap to the epoch 0 that
// every key set check refuses (CORE-K8).
func nextMboxEpoch(prev uint64) (epoch uint64, anomaly bool) {
	if prev >= mboxEpochLimit-1 {
		return 1, true
	}
	return prev + 1, false
}

// writeMboxFile writes mbox.key durably: a temporary file, fsync, rename,
// directory fsync. step, when not nil, is called before each of "write",
// "fsync", "rename" and "dirsync", and an error from it stops the write
// there, as a crash would (tests).
func writeMboxFile(path string, data []byte, step func(string) error) error {
	at := func(s string) error {
		if step == nil {
			return nil
		}
		return step(s)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	// A temporary file left by a crash is not reused: its mode might not be
	// 0600, and O_EXCL below creates the file with exactly that.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		fh.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := at("write"); err != nil {
		return fail(err)
	}
	if _, err := fh.Write(data); err != nil {
		return fail(err)
	}
	if err := at("fsync"); err != nil {
		return fail(err)
	}
	if err := fh.Sync(); err != nil {
		return fail(err)
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := at("rename"); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := protectTemp(tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := fsperm.ReplaceFile(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := at("dirsync"); err != nil {
		return err
	}
	return syncDir(dir)
}

// mboxKey is this node's mailbox signing key in memory: what mbox.key
// holds, or nothing yet.
type mboxKey struct {
	mu    sync.RWMutex
	path  string
	epoch uint64             // 0 while the node has no key
	priv  ed25519.PrivateKey // nil while the node has no key
}

// Current is mbox.Current(): the public half and epoch of the mailbox
// signing key the node publishes, and false while it has none.
func (m *mboxKey) Current() (seal.Mbox, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.epoch == 0 {
		return seal.Mbox{}, false
	}
	pub := m.priv.Public().(ed25519.PublicKey)
	return seal.Mbox{Epoch: m.epoch, Pub: bytes.Clone(pub)}, true
}

// SignCap signs a capability body with the current mailbox signing key.
// body.Epoch must be the current epoch: a capability signed under a key
// that has just been replaced would verify nowhere.
func (m *mboxKey) SignCap(body *seal.CapBody) (*seal.Cap, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.epoch == 0 {
		return nil, errMboxPending
	}
	if body == nil || body.Epoch != m.epoch {
		var got uint64
		if body != nil {
			got = body.Epoch
		}
		return nil, fmt.Errorf("anet: capability for mailbox epoch %d, the key is at %d", got, m.epoch)
	}
	return seal.SignCap(m.priv, body)
}

// load reads mbox.key. It reports false when there is no file. A file that
// cannot be read or decoded stops the start: replacing it silently would
// void every capability this node issued, and the operator should see why.
func (m *mboxKey) load() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("anet: cannot read the mailbox signing key %s (%w); make it readable by this user and start anet again", m.path, err)
	}
	epoch, seed, err := decodeMboxFile(b)
	if err != nil {
		return false, fmt.Errorf("anet: the mailbox signing key %s is damaged (%v); move it out of the data directory and start anet again, which makes a new one", m.path, err)
	}
	m.epoch, m.priv = epoch, ed25519.NewKeyFromSeed(seed)
	clear(seed)
	return true, nil
}

// install makes a new key at epoch, writes it durably and only then makes
// it the current one. The old private key is cleared from memory. step is
// writeMboxFile's (nil outside tests).
func (m *mboxKey) install(epoch uint64, step func(string) error) (seal.Mbox, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return seal.Mbox{}, err
	}
	defer clear(seed)
	data, err := encodeMboxFile(epoch, seed)
	if err != nil {
		return seal.Mbox{}, err
	}
	defer clear(data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := writeMboxFile(m.path, data, step); err != nil {
		return seal.Mbox{}, fmt.Errorf("anet: save the mailbox signing key: %w", err)
	}
	old := m.priv
	m.epoch, m.priv = epoch, ed25519.NewKeyFromSeed(seed)
	clear(old)
	return seal.Mbox{Epoch: epoch, Pub: bytes.Clone(m.priv.Public().(ed25519.PublicKey))}, nil
}

// mboxState is what the daemon knows about getting its mailbox key to the
// hub: whether the hub holds the current one (SSR-D4) and when to try
// again, and, while the node has no key, when to ask the hub again.
type mboxState struct {
	mu         sync.Mutex
	registered bool
	failures   int
	retryAt    time.Time // zero: no republish due
	notes      uint64    // answers recorded by noteKeysAtHub
	// pending: the node has no mailbox key yet and waits for its hub to
	// say which epoch the key takes. ringEpoch is the epoch of the node's
	// last key set, kept for that decision.
	pending     bool
	pendingFail int
	pendingAt   time.Time
	ringEpoch   uint64
	// epochInvalid is /status.sender_privacy.hub_epoch_invalid: an epoch
	// with no room below 2^53 was seen in this run (SSR-W10).
	epochInvalid atomic.Bool
	// healedFrom is the epoch of the hub's mailbox key the last SSR-W10a
	// heal went above (0: none in this run). The same hub key again is not
	// healed from twice: the new key it made did not satisfy the hub, and
	// making another would void every capability for nothing.
	healedFrom uint64
}

// mailboxStatus is what /status.sender_privacy reports about the mailbox
// key (SS §4.8 SSR-U2): whether the hub holds the current key, and whether
// an impossible epoch was seen in this run.
func (d *Daemon) mailboxStatus() (registered, epochInvalid bool) {
	d.mboxSt.mu.Lock()
	registered = d.mboxSt.registered
	d.mboxSt.mu.Unlock()
	return registered, d.mboxSt.epochInvalid.Load()
}

// setupMbox loads mbox.key, or makes the first key (SSR-D3). Called once
// from setupKeyRing, before the key ring signs a set; ringEpoch is the
// mailbox epoch of the ring's last signed set (0: none).
func (d *Daemon) setupMbox(ringEpoch uint64) error {
	d.mbox = &mboxKey{path: d.layout.MboxKeyPath()}
	ok, err := d.mbox.load()
	if err != nil || ok {
		return err
	}
	ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeoutShort)
	defer cancel()
	if err := d.makeFirstMbox(ctx, ringEpoch); err != nil {
		d.mboxSt.mu.Lock()
		d.mboxSt.pending, d.mboxSt.ringEpoch = true, ringEpoch
		d.mboxSt.pendingFail = 1
		d.mboxSt.pendingAt = time.Now().Add(mboxRetryAfter(1))
		d.mboxSt.mu.Unlock()
		log.Printf("%v (%v)", errMboxPending, err)
	}
	return nil
}

// makeFirstMbox makes the key of a node that has none, numbered by
// SSR-W10. It fails, and makes nothing, when a configured hub cannot say
// what it stores.
func (d *Daemon) makeFirstMbox(ctx context.Context, ringEpoch uint64) error {
	prev := ringEpoch
	if hub := d.config().HubURL; hub != "" && beenAtAHub(d.self) {
		stored, found, err := d.hubStoredMboxEpoch(ctx, hub)
		if err != nil {
			return err
		}
		if found {
			prev = max(prev, stored)
		}
	}
	epoch, anomaly := nextMboxEpoch(prev)
	if anomaly {
		d.noteMboxEpochAnomaly(prev)
	}
	m, err := d.mbox.install(epoch, nil)
	if err != nil {
		return err
	}
	log.Printf("anet: made the mailbox signing key, number %d", m.Epoch)
	return nil
}

// beenAtAHub reports whether an identity may have published a key set at a
// hub: its KEL names a witness hub (an agent joins a hub by designating it,
// and every later rotation of an agent names its witness), or an event is
// pending (a designation not yet accepted, or the rotation of an identity
// being recovered on a new machine).
func beenAtAHub(c *identity.Controller) bool {
	if _, ok := c.Pending(); ok {
		return true
	}
	for _, e := range c.KEL() {
		if len(e.Event.Witnesses) > 0 {
			return true
		}
	}
	return false
}

// hubStoredMboxEpoch asks this node's hub which mailbox epoch it stores for
// this node (GET /agents/{self}/keys, SSR-W10). found is false when the hub
// has no key set for it (404).
//
// The answer is not verified against this node's KEL: the only party that
// could forge it is the hub being asked, which can refuse this node's keys
// anyway, and a set signed under an older key state (an identity restored
// after a rotation) must still be read. An epoch too large to add to is
// caught by nextMboxEpoch.
func (d *Daemon) hubStoredMboxEpoch(ctx context.Context, hub string) (epoch uint64, found bool, err error) {
	m, found, err := d.hubStoredMbox(ctx, hub)
	return m.Epoch, found, err
}

// hubStoredMbox is the mailbox signing key of the key set this node's hub
// stores for it (GET /agents/{self}/keys, on the signed side): its epoch
// and public key. found is false when the hub has none (404). The set is
// decoded, not verified, as hubStoredMboxEpoch says.
func (d *Daemon) hubStoredMbox(ctx context.Context, hub string) (seal.Mbox, bool, error) {
	var out hubapi.KeysResponse
	err := d.hubGet(ctx, hub, "/agents/"+url.PathEscape(d.AID())+"/keys", nil, &out)
	if hubStatus(err) == 404 {
		return seal.Mbox{}, false, nil
	}
	if err != nil {
		return seal.Mbox{}, false, err
	}
	raw, err := decodeHubB64(out.KeySet)
	if err != nil {
		return seal.Mbox{}, false, fmt.Errorf("anet: the hub's key set for this node is not base64: %w", err)
	}
	signed, err := seal.UnmarshalSignedEncKeySet(raw)
	if err != nil {
		return seal.Mbox{}, false, fmt.Errorf("anet: the hub's key set for this node: %w", err)
	}
	var set seal.EncKeySet
	if err := coredet.UnmarshalStrict(signed.Set, &set); err != nil {
		return seal.Mbox{}, false, fmt.Errorf("anet: the hub's key set for this node: %w", err)
	}
	if set.AID != d.AID() || (out.AID != "" && out.AID != d.AID()) {
		return seal.Mbox{}, false, fmt.Errorf("anet: the hub answered with the key set of %s", set.AID)
	}
	if set.Mbox.Epoch == 0 {
		return seal.Mbox{}, false, errors.New("anet: the hub's key set for this node has no mailbox signing key")
	}
	return set.Mbox, true, nil
}

// healMboxEpoch is SSR-W10a: the hub refused this node's key set as invalid
// — at /register keys_status invalid, on POST /agents/{aid}/keys 400
// bad_request — and the cause may be the mailbox key rule (SSR-W9): the
// hub holds a mailbox key this node's does not follow, because mbox.key
// came back from a backup, or was lost with the key ring by an identity
// that had not been at a hub. The hub's stored key is read (on the signed
// side); when its epoch is above this node's, or equal with another key, a
// new mailbox key is made one above the hub's (rotateMboxAboveTx: the
// capabilities the old key signed go with it), and the caller publishes
// again. It reports whether it made one.
//
// At most once per refusal: the caller publishes once more and stops, and
// the hub key healed from is remembered, so a hub that refuses again (one
// whose stored epoch is 2^53 or more, which no honest hub stores, gives a
// new key numbered 1 that it refuses too) is not healed from again; the
// retries back off (SSR-D4). A refusal for any other cause — the hub's key
// is this node's, or older — makes nothing.
func (d *Daemon) healMboxEpoch(ctx context.Context, hub string) bool {
	cur, ok := d.mbox.Current()
	if !ok {
		return false
	}
	stored, found, err := d.hubStoredMbox(ctx, hub)
	if err != nil || !found {
		if err != nil {
			log.Printf("anet: read the mailbox signing key %s stores for this node: %v", hub, err)
		}
		return false
	}
	if stored.Epoch < cur.Epoch || stored.Epoch == cur.Epoch && bytes.Equal(stored.Pub, cur.Pub) {
		return false
	}
	d.mboxSt.mu.Lock()
	again := d.mboxSt.healedFrom != 0 && d.mboxSt.healedFrom == stored.Epoch
	if !again {
		d.mboxSt.healedFrom = stored.Epoch
	}
	d.mboxSt.mu.Unlock()
	if again {
		return false
	}
	var m seal.Mbox
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		m, err = d.rotateMboxAboveTx(tx, stored.Epoch)
		return err
	}); err != nil {
		log.Printf("anet: replace the mailbox signing key %s does not take: %v", hub, err)
		return false
	}
	log.Printf("anet: %s holds mailbox signing key number %d, which this node's (number %d) does not follow; "+
		"anet made number %d and publishes its keys again", hub, stored.Epoch, cur.Epoch, m.Epoch)
	return true
}

// decodeHubB64 decodes a binary value of a hub answer: base64url without
// padding, in its one spelling (wire 3, CORE-E5).
func decodeHubB64(s string) ([]byte, error) { return hubapi.DecodeB64(s, -1) }

// noteMboxEpochAnomaly records an epoch with no room below 2^53 (SSR-W10):
// counter hub-epoch-anomaly and hub_epoch_invalid for the rest of the run.
func (d *Daemon) noteMboxEpochAnomaly(prev uint64) {
	d.mboxSt.epochInvalid.Store(true)
	d.countErr("hub-epoch-anomaly", fmt.Errorf("mailbox epoch %d leaves no room below 2^53; the new key starts again at 1", prev))
}

// rotateMboxTx replaces the mailbox signing key inside tx, the
// transaction that also clears caps_issued (SS §4.3.8 item 4; core §4.7
// E1 through capsEmergencyTx). The new key, one epoch up (SSR-W10), is in
// mbox.key and fsynced before rotateMboxTx returns, so before tx commits:
// a crash after the commit cannot bring the old key back, and a crash
// before it leaves a newer key and the old rows, which the next rotation
// clears. The caller publishes a key set with the new key once tx has
// committed; until then the hub holds the old one (SSR-D4 reports so).
func (d *Daemon) rotateMboxTx(tx *interactions.Tx) (seal.Mbox, error) {
	cur, ok := d.mbox.Current()
	if !ok {
		return seal.Mbox{}, errMboxPending
	}
	return d.rotateMboxAboveTx(tx, cur.Epoch)
}

// rotateMboxAboveTx is rotateMboxTx with the new key numbered one above
// prev rather than above this node's key (SSR-W10a: above the hub's).
func (d *Daemon) rotateMboxAboveTx(tx *interactions.Tx, prev uint64) (seal.Mbox, error) {
	epoch, anomaly := nextMboxEpoch(prev)
	if anomaly {
		d.noteMboxEpochAnomaly(prev)
	}
	m, err := d.mbox.install(epoch, nil)
	if err != nil {
		return seal.Mbox{}, err
	}
	d.mboxSt.mu.Lock()
	d.mboxSt.registered = false
	d.mboxSt.mu.Unlock()
	if _, err := tx.ClearCapsIssued(); err != nil {
		return seal.Mbox{}, err
	}
	return m, nil
}

// capsEmergencyTx is SSR-D22, part (c) of core §4.7 E1: in the emergency
// rotation's transaction, a new mailbox signing key (written and fsynced
// first, rotateMboxTx) and both capability tables cleared — every
// capability this node issued stops verifying once E2 publishes the new
// key set, and every capability and direct address its peers granted it is
// dropped, since its old data directory may be in someone else's hands.
// The first-contact sessions go too, their keys zeroed (SSR-D23): they were
// opened under the retired key, and the hub drops them as well when it
// takes the rotation (SSR-H9). They are only in memory, so this happens
// whether or not the transaction commits; a session dropped too early only
// costs one opening.
func (d *Daemon) capsEmergencyTx(tx *interactions.Tx) error {
	d.dropSessions()
	if _, err := d.rotateMboxTx(tx); err != nil {
		return err
	}
	_, err := tx.ClearCapsHeld()
	return err
}

// keySetCarries reports whether signed (a SignedEncKeySet encoding)
// publishes mbox.
func keySetCarries(signed []byte, mbox seal.Mbox) bool {
	s, err := seal.UnmarshalSignedEncKeySet(signed)
	if err != nil {
		return false
	}
	var set seal.EncKeySet
	if err := coredet.Unmarshal(s.Set, &set); err != nil {
		return false
	}
	return set.Mbox.Epoch == mbox.Epoch && bytes.Equal(set.Mbox.Pub, mbox.Pub)
}

// noteKeysAtHub records the hub's answer to a registration or key set
// publication (SSR-D4): accepted is keys_status ok or unchanged, published
// the set that was sent (nil: none). Anything short of "the hub took a set
// with the current mailbox key" schedules a republish.
func (d *Daemon) noteKeysAtHub(published []byte, accepted bool) {
	cur, ok := d.mbox.Current()
	reg := accepted && ok && keySetCarries(published, cur)
	d.mboxSt.mu.Lock()
	defer d.mboxSt.mu.Unlock()
	d.mboxSt.notes++
	if reg {
		d.mboxSt.registered, d.mboxSt.failures, d.mboxSt.retryAt = true, 0, time.Time{}
		return
	}
	d.mboxSt.registered = false
	d.mboxSt.failures++
	d.mboxSt.retryAt = time.Now().Add(mboxRetryAfter(d.mboxSt.failures))
}

// republishKeys brings the key set up to the current mailbox key and
// publishes it (SSR-D4); publishKeys records the answer. The key ring's
// maintenance re-signs a set that predates the key and publishes a set the
// hub has not confirmed; only when it sent nothing is the set sent here, so
// one retry is one request and one recorded answer.
func (d *Daemon) republishKeys(ctx context.Context, hub string) error {
	notes := func() uint64 {
		d.mboxSt.mu.Lock()
		defer d.mboxSt.mu.Unlock()
		return d.mboxSt.notes
	}
	before := notes()
	d.maintainKeyRing()
	if notes() != before {
		return nil
	}
	return d.publishKeys(ctx, hub)
}

// mboxTick does what is due at now: ask the hub again for the epoch of a
// missing key, or publish the key set again after the hub did not take the
// current mailbox key.
func (d *Daemon) mboxTick(ctx context.Context, now time.Time) {
	st := &d.mboxSt
	st.mu.Lock()
	pending, pendingAt, ringEpoch := st.pending, st.pendingAt, st.ringEpoch
	retryAt := st.retryAt
	st.mu.Unlock()
	if pending {
		if now.Before(pendingAt) {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, hubCallTimeoutShort)
		err := d.makeFirstMbox(cctx, ringEpoch)
		cancel()
		st.mu.Lock()
		if err != nil {
			st.pendingFail++
			st.pendingAt = now.Add(mboxRetryAfter(st.pendingFail))
			st.mu.Unlock()
			log.Printf("%v (%v)", errMboxPending, err)
			return
		}
		st.pending = false
		st.mu.Unlock()
		// The first key set with the key; published when there is a hub.
		d.maintainKeyRing()
		return
	}
	if retryAt.IsZero() || now.Before(retryAt) {
		return
	}
	hub := d.config().HubURL
	if hub == "" {
		st.mu.Lock()
		st.retryAt = time.Time{}
		st.mu.Unlock()
		return
	}
	cctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	if err := d.republishKeys(cctx, hub); err != nil {
		log.Printf("anet: this node's mailbox signing key is not registered at %s yet: %v (anet tries again on its own)", hub, err)
	}
}

// mboxLoop runs the mailbox key retries and prunes the capability tables
// every capsPruneEvery (SSR-D2).
func (d *Daemon) mboxLoop(ctx context.Context) {
	tick := time.NewTicker(mboxTickEvery.get())
	defer tick.Stop()
	prune := time.NewTicker(capsPruneEvery.get())
	defer prune.Stop()
	d.pruneCaps()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			d.mboxTick(ctx, now)
		case <-prune.C:
			d.pruneCaps()
		}
	}
}

// pruneCaps applies SSR-D2 at the wire clock.
func (d *Daemon) pruneCaps() {
	if d.ix == nil {
		return
	}
	p, err := d.ix.PruneCaps(d.nowMS())
	if err != nil {
		log.Printf("anet: prune delivery credentials: %v", err)
		return
	}
	if p.HeldEvicted > 0 {
		log.Printf("anet: held delivery credentials reached %d; dropped %d that expire first", interactions.MaxHeldCaps, p.HeldEvicted)
	}
}

package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// startAt starts a daemon on an existing data directory, stops its relay
// loop as buildTestDaemon does, and closes it with the test.
func startAt(t *testing.T, root string) *Daemon {
	t.Helper()
	d, err := New(NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	if d.relayStop != nil {
		d.relayStop()
		d.relayStop = nil
	}
	d.mu.Unlock()
	t.Cleanup(func() { d.Close() })
	return d
}

// rootWithIdentityOf is a new data directory holding only from's identity
// and a config naming hubURL: the identity restored on a new machine,
// without mbox.key and without the key ring.
func rootWithIdentityOf(t *testing.T, from *Daemon, hubURL string) string {
	t.Helper()
	root := t.TempDir()
	b, err := os.ReadFile(from.layout.IdentityPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "identity.kel"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "hub_url": hubURL})
	return root
}

// keepMboxLoopOut stops the mailbox loop of the daemons the test starts
// from running a retry on its own, so the test's own mboxTick calls are the
// only ones.
func keepMboxLoopOut(t *testing.T) {
	old := mboxTickEvery.set(time.Hour)
	t.Cleanup(func() { mboxTickEvery.set(old) })
}

// joinedRoot is a data directory whose identity has designated a witness
// hub (KEL D-13), so it may have published a key set at a hub, with hubURL
// in its config.
func joinedRoot(t *testing.T, hubURL string) string {
	t.Helper()
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "hub_url": hubURL})
	c, err := identity.InceptAgent(uint64(time.Now().Add(-24 * time.Hour).UnixMilli()))
	if err != nil {
		t.Fatal(err)
	}
	if err := identitytest.SwitchWitness(c, identitytest.Witness, uint64(time.Now().Add(-time.Hour).UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if err := saveIdentity(NewLayout(root), c); err != nil {
		t.Fatal(err)
	}
	return root
}

// mboxOnDisk reads mbox.key of d.
func mboxOnDisk(t *testing.T, d *Daemon) (uint64, []byte) {
	t.Helper()
	b, err := os.ReadFile(d.layout.MboxKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	epoch, seed, err := decodeMboxFile(b)
	if err != nil {
		t.Fatal(err)
	}
	return epoch, seed
}

func currentMbox(t *testing.T, d *Daemon) seal.Mbox {
	t.Helper()
	m, ok := d.mbox.Current()
	if !ok {
		t.Fatal("the node has no mailbox signing key")
	}
	return m
}

// TestMboxKeyFile (SS §3.2, core §2.16): mbox.key is 0600, holds exactly
// {0: "anet.mbox/1", 1: epoch, 2: 32-byte seed}, is the key the node's key
// set publishes, survives a restart, is written to a temporary file that
// is fsynced and renamed over the old one, and is read strictly.
func TestMboxKeyFile(t *testing.T) {
	d := newTestDaemon(t, "", true)
	fi, err := os.Stat(d.layout.MboxKeyPath())
	if err != nil {
		t.Fatalf("no mbox.key after start: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mbox.key mode %v, want 0600", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(d.layout.MboxKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	var m map[uint64]any
	if err := coredet.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 3 || m[0] != "anet.mbox/1" || m[1] != uint64(1) {
		t.Fatalf("mbox.key holds %v, want {0: anet.mbox/1, 1: 1, 2: seed}", m)
	}
	seed, _ := m[2].([]byte)
	if len(seed) != ed25519.SeedSize {
		t.Fatalf("seed is %d bytes", len(seed))
	}
	cur := currentMbox(t, d)
	if !bytes.Equal(cur.Pub, ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)) || cur.Epoch != 1 {
		t.Fatalf("Current() = %+v is not the key in mbox.key", cur)
	}
	// The key set publishes it (SSR-D3).
	if set := keySetOf(t, d); set.Mbox.Epoch != cur.Epoch || !bytes.Equal(set.Mbox.Pub, cur.Pub) {
		t.Fatalf("the key set publishes %+v, the node's key is %+v", set.Mbox, cur)
	}
	// It signs capabilities that verify under it, at its epoch only.
	_, holder, _ := ed25519.GenerateKey(nil)
	body := &seal.CapBody{Owner: d.AID(), Epoch: cur.Epoch, CapID: bytes.Repeat([]byte{1}, seal.CapIDLen),
		Class: seal.CapClassContact, CapExp: d.nowMS() + 3600_000, Holder: holder.Public().(ed25519.PublicKey)}
	c, err := d.mbox.SignCap(body)
	if err != nil || !ed25519.Verify(cur.Pub, c.Body, c.Sig) {
		t.Fatalf("SignCap: %v", err)
	}
	body.Epoch = 2
	if _, err := d.mbox.SignCap(body); err == nil {
		t.Fatal("signed a capability for an epoch the key is not at")
	}

	// A restart keeps the key.
	layout := d.layout
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2 := startAt(t, layout.Root)
	if got := currentMbox(t, d2); got.Epoch != cur.Epoch || !bytes.Equal(got.Pub, cur.Pub) {
		t.Fatalf("after a restart the key is %+v, was %+v", got, cur)
	}

	// The write: temporary file, fsync, rename, directory fsync, in that
	// order; a crash before the rename leaves the old key whole.
	dir := t.TempDir()
	path := filepath.Join(dir, "mbox.key")
	old, _ := encodeMboxFile(3, bytes.Repeat([]byte{3}, 32))
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(path) // the old file's inode
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	next, _ := encodeMboxFile(4, bytes.Repeat([]byte{4}, 32))
	var steps []string
	if err := writeMboxFile(path, next, func(s string) error { steps = append(steps, s); return nil }); err != nil {
		t.Fatal(err)
	}
	if want := []string{"write", "fsync", "rename", "dirsync"}; !reflect.DeepEqual(steps, want) {
		t.Fatalf("write steps %v, want %v", steps, want)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, next) {
		t.Fatal("mbox.key does not hold the new key")
	}
	inode := make([]byte, len(old)+8)
	n, _ := held.ReadAt(inode, 0)
	if !bytes.Equal(inode[:n], old) {
		t.Fatal("the old key file was written in place, not replaced by a rename")
	}
	crash := errors.New("crash")
	for _, at := range []string{"write", "fsync", "rename"} {
		if err := writeMboxFile(path, old, func(s string) error {
			if s == at {
				return crash
			}
			return nil
		}); !errors.Is(err, crash) {
			t.Fatalf("crash at %s: %v", at, err)
		}
		if b, _ := os.ReadFile(path); !bytes.Equal(b, next) {
			t.Fatalf("a crash at %s changed the key file", at)
		}
		if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a crash at %s left the temporary file", at)
		}
	}
	// A temporary file left by an earlier crash, readable by others, is
	// not reused.
	if err := os.WriteFile(path+".tmp", []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeMboxFile(path, old, nil); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v after a leftover temporary file", fi.Mode().Perm())
	}

	// Strict reading.
	seed32 := bytes.Repeat([]byte{9}, 32)
	enc := func(v any) []byte {
		b, err := coredet.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	bad := map[string][]byte{
		"type":            enc(map[uint64]any{0: "anet.mbox/2", 1: uint64(1), 2: seed32}),
		"epoch 0":         enc(map[uint64]any{0: "anet.mbox/1", 1: uint64(0), 2: seed32}),
		"epoch 2^53":      enc(map[uint64]any{0: "anet.mbox/1", 1: uint64(1) << 53, 2: seed32}),
		"short seed":      enc(map[uint64]any{0: "anet.mbox/1", 1: uint64(1), 2: seed32[:31]}),
		"no seed":         enc(map[uint64]any{0: "anet.mbox/1", 1: uint64(1)}),
		"extra key":       enc(map[uint64]any{0: "anet.mbox/1", 1: uint64(1), 2: seed32, 3: uint64(0)}),
		"long-form epoch": append(append([]byte{0xa3, 0x00, 0x6b}, "anet.mbox/1"...), append([]byte{0x01, 0x18, 0x01, 0x02, 0x58, 0x20}, seed32...)...),
		"not cbor":        []byte("not a key"),
	}
	if _, _, err := decodeMboxFile(enc(map[uint64]any{0: "anet.mbox/1", 1: uint64(1), 2: seed32})); err != nil {
		t.Fatalf("a good file is refused: %v", err)
	}
	for name, b := range bad {
		if _, _, err := decodeMboxFile(b); err == nil {
			t.Errorf("%s: read as a key", name)
		}
	}
	// A damaged file stops the start with what to do.
	if err := d2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.MboxKeyPath(), bad["short seed"], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(layout); err == nil || !strings.Contains(err.Error(), "damaged") ||
		!strings.Contains(err.Error(), "move it out of the data directory") {
		t.Fatalf("start with a damaged mbox.key: %v", err)
	}
}

// TestMboxEpochRules (SSR-W10): a fresh identity starts at 1 without asking
// anyone; a replaced key is one above the old; an identity that has been at
// a hub and has no mbox.key is one above what its hub stores, 1 when the hub
// has nothing (404); an
// epoch with no room below 2^53 restarts at 1, counts hub-epoch-anomaly and
// sets hub_epoch_invalid; a hub that cannot be asked leaves the key pending
// rather than guessed.
func TestMboxEpochRules(t *testing.T) {
	for _, c := range []struct {
		prev, want uint64
		anomaly    bool
	}{
		{0, 1, false}, {1, 2, false}, {41, 42, false},
		{1<<53 - 2, 1<<53 - 1, false},
		{1<<53 - 1, 1, true}, {1 << 53, 1, true}, {1<<64 - 1, 1, true},
	} {
		if got, an := nextMboxEpoch(c.prev); got != c.want || an != c.anomaly {
			t.Errorf("nextMboxEpoch(%d) = %d, %v; want %d, %v", c.prev, got, an, c.want, c.anomaly)
		}
	}
	for f, want := range map[int]time.Duration{1: 5 * time.Second, 2: 10 * time.Second, 3: 20 * time.Second,
		7: 320 * time.Second, 8: 10 * time.Minute, 50: 10 * time.Minute} {
		if got := mboxRetryAfter(f); got != want {
			t.Errorf("mboxRetryAfter(%d) = %v, want %v", f, got, want)
		}
	}

	srv := newFakeHub(t)
	fake := fakeHubAt(t, srv.URL)
	gets := func() int {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.keysGets
	}

	// A fresh identity, which never named a witness hub: 1, and the hub is
	// not asked, not even from a new machine.
	fresh := newTestDaemon(t, srv.URL, true)
	if got := currentMbox(t, fresh); got.Epoch != 1 {
		t.Fatalf("a fresh identity's key is at %d", got.Epoch)
	}
	again := startAt(t, rootWithIdentityOf(t, fresh, srv.URL))
	if got := currentMbox(t, again); got.Epoch != 1 {
		t.Fatalf("a fresh identity on a new machine: key at %d", got.Epoch)
	}
	if gets() != 0 {
		t.Fatalf("the hub was asked %d times about an identity that never joined a hub", gets())
	}

	// An identity that designated a witness hub is asked about; this hub
	// has never seen it: 404, so 1.
	a := startAt(t, joinedRoot(t, srv.URL))
	if got := currentMbox(t, a); got.Epoch != 1 {
		t.Fatalf("identity unknown to the hub: key at %d, want 1", got.Epoch)
	}
	if gets() != 1 {
		t.Fatalf("the hub was asked %d times about a joined identity", gets())
	}
	if err := a.RegisterWithHub(context.Background(), srv.URL, "A", nil); err != nil {
		t.Fatal(err)
	}

	// The node's own key replaced: one up (and the hub takes it).
	for want := uint64(2); want <= 3; want++ {
		if err := a.ix.Update(func(tx *interactions.Tx) error { _, err := a.rotateMboxTx(tx); return err }); err != nil {
			t.Fatal(err)
		}
		if got := currentMbox(t, a); got.Epoch != want {
			t.Fatalf("rotated to %d, want %d", got.Epoch, want)
		}
		if e, _ := mboxOnDisk(t, a); e != want {
			t.Fatalf("mbox.key holds %d, want %d", e, want)
		}
		if err := a.republishKeys(context.Background(), srv.URL); err != nil {
			t.Fatal(err)
		}
	}

	// The identity on a new machine: one above what the hub stores.
	n0 := gets()
	b := startAt(t, rootWithIdentityOf(t, a, srv.URL))
	if got := currentMbox(t, b); got.Epoch != 4 {
		t.Fatalf("restored identity's key is at %d, want 4 (the hub stores 3)", got.Epoch)
	}
	if gets() != n0+1 {
		t.Fatalf("the hub was asked %d times", gets()-n0)
	}
	if err := b.publishKeys(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if reg, _ := b.mailboxStatus(); !reg {
		t.Fatal("the hub did not take the restored identity's new key")
	}

	// The hub reports an epoch with no room below 2^53.
	for _, huge := range []uint64{1 << 53, 1<<64 - 1} {
		e := startAt(t, joinedRoot(t, ""))
		signed := keySetWithMboxEpoch(t, e, huge)
		setFake(t, srv.URL, func(h *fakeHub) {
			h.keysOverride[e.AID()] = hubapi.KeysResponse{AID: e.AID(), KeySet: base64.StdEncoding.EncodeToString(signed)}
		})
		f := startAt(t, rootWithIdentityOf(t, e, srv.URL))
		if got := currentMbox(t, f); got.Epoch != 1 {
			t.Fatalf("hub reports %d: key at %d, want 1", huge, got.Epoch)
		}
		if _, invalid := f.mailboxStatus(); !invalid {
			t.Fatalf("hub reports %d: hub_epoch_invalid not set", huge)
		}
		if f.ReceiveStats()["hub-epoch-anomaly"] != 1 {
			t.Fatalf("hub reports %d: hub-epoch-anomaly = %d", huge, f.ReceiveStats()["hub-epoch-anomaly"])
		}
		if _, invalid := e.mailboxStatus(); invalid {
			t.Fatal("hub_epoch_invalid set on a node that saw nothing wrong")
		}
	}

	// The hub answers with another identity's key set: that says nothing
	// about this node's epoch, so the key waits rather than take it.
	{
		e := startAt(t, joinedRoot(t, ""))
		other := startAt(t, joinedRoot(t, ""))
		signed := keySetWithMboxEpoch(t, other, 7)
		setFake(t, srv.URL, func(h *fakeHub) {
			h.keysOverride[e.AID()] = hubapi.KeysResponse{AID: e.AID(), KeySet: base64.StdEncoding.EncodeToString(signed)}
		})
		f := startAt(t, rootWithIdentityOf(t, e, srv.URL))
		if m, ok := f.mbox.Current(); ok {
			t.Fatalf("a key was made from another identity's key set: epoch %d", m.Epoch)
		}
	}

	// The node's own epoch at 2^53 − 1: the next key restarts at 1 too.
	g := newTestDaemon(t, "", true)
	data, _ := encodeMboxFile(1<<53-1, bytes.Repeat([]byte{5}, 32))
	if err := writeMboxFile(g.layout.MboxKeyPath(), data, nil); err != nil {
		t.Fatal(err)
	}
	root := g.layout.Root
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g2 := startAt(t, root)
	if err := g2.ix.Update(func(tx *interactions.Tx) error { _, err := g2.rotateMboxTx(tx); return err }); err != nil {
		t.Fatal(err)
	}
	if got := currentMbox(t, g2); got.Epoch != 1 {
		t.Fatalf("rotation from 2^53 - 1 gave %d, want 1", got.Epoch)
	}
	if _, invalid := g2.mailboxStatus(); !invalid || g2.ReceiveStats()["hub-epoch-anomaly"] != 1 {
		t.Fatal("rotation from 2^53 - 1 was not reported")
	}

	// Without a hub, a node that lost mbox.key goes one above the epoch of
	// its own last key set.
	h := newTestDaemon(t, "", true)
	if err := h.ix.Update(func(tx *interactions.Tx) error { _, err := h.rotateMboxTx(tx); return err }); err != nil {
		t.Fatal(err)
	}
	h.maintainKeyRing()
	if set := keySetOf(t, h); set.Mbox.Epoch != 2 {
		t.Fatalf("the key set publishes epoch %d, want 2", set.Mbox.Epoch)
	}
	root = h.layout.Root
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "mbox.key")); err != nil {
		t.Fatal(err)
	}
	h2 := startAt(t, root)
	if got := currentMbox(t, h2); got.Epoch != 3 {
		t.Fatalf("lost mbox.key, last set at 2: key at %d, want 3", got.Epoch)
	}
}

// keySetWithMboxEpoch is a key set signed by d whose mailbox key has epoch:
// what a hub might claim to store.
func keySetWithMboxEpoch(t *testing.T, d *Daemon, epoch uint64) []byte {
	t.Helper()
	now := d.nowMS()
	kp, err := seal.GenerateKeyPair(seal.SuiteX25519, now, now+seal.MaxKeyValidityMS)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	signed, err := seal.SignEncKeySet(&seal.EncKeySet{Type: seal.EncKeySetType, AID: d.AID(), Seq: now,
		Keys: []seal.EncKey{kp.Public}, IssuedAt: now, Mbox: seal.Mbox{Epoch: epoch, Pub: pub}}, d.self.Sign)
	if err != nil {
		t.Fatal(err)
	}
	b, err := signed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A configured hub that cannot be asked leaves the key pending: the node
// starts, makes no key and signs no key set, and the mailbox loop makes the
// key once the hub answers.
func TestMboxWaitsForAHubThatCannotBeAsked(t *testing.T) {
	keepMboxLoopOut(t)
	srv := newFakeHub(t)
	var down atomic.Bool
	down.Store(true)
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Scheme, r2.URL.Host, r2.RequestURI = "http", strings.TrimPrefix(srv.URL, "http://"), ""
		resp, err := http.DefaultTransport.RoundTrip(r2)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		w.Write(buf.Bytes())
	}))
	t.Cleanup(gate.Close)

	a := startAt(t, joinedRoot(t, srv.URL))
	if err := a.RegisterWithHub(context.Background(), srv.URL, "A", nil); err != nil {
		t.Fatal(err)
	}
	b := startAt(t, rootWithIdentityOf(t, a, gate.URL))
	if _, ok := b.mbox.Current(); ok {
		t.Fatal("a key was made without the hub's answer")
	}
	if _, err := os.Stat(b.layout.MboxKeyPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("mbox.key was written without the hub's answer")
	}
	if len(b.enc.SignedSet()) != 0 {
		t.Fatal("a key set was signed without a mailbox key")
	}
	b.mboxSt.mu.Lock()
	pendingAt := b.mboxSt.pendingAt
	b.mboxSt.mu.Unlock()
	// Still down: still pending, asked again later.
	b.mboxTick(context.Background(), pendingAt)
	b.mboxSt.mu.Lock()
	later := b.mboxSt.pendingAt
	b.mboxSt.mu.Unlock()
	if _, ok := b.mbox.Current(); ok || !later.After(pendingAt) {
		t.Fatal("a failed retry made a key or did not back off")
	}
	down.Store(false)
	b.mboxTick(context.Background(), later)
	if got := currentMbox(t, b); got.Epoch != 2 {
		t.Fatalf("after the hub answered: key at %d, want 2 (the hub stores 1)", got.Epoch)
	}
	if set := keySetOf(t, b); set.Mbox.Epoch != 2 {
		t.Fatalf("the key set publishes epoch %d", set.Mbox.Epoch)
	}
}

// TestCapsEmergencyTx (SSR-D22, core §4.7 E1 part c): the emergency
// transaction writes and fsyncs the new mailbox key first, then clears both
// capability tables; when the key cannot be written nothing is cleared.
func TestCapsEmergencyTx(t *testing.T) {
	d := newTestDaemon(t, "", true)
	fill := func() {
		t.Helper()
		if err := d.ix.Update(func(tx *interactions.Tx) error {
			if err := tx.PutIssuedCap(interactions.IssuedCap{CapID: bytes.Repeat([]byte{1}, 16), Grantee: "did:anet:g", Class: 1,
				Scope: interactions.CapScopePeer, HolderSeed: bytes.Repeat([]byte{1}, 32), HolderPub: bytes.Repeat([]byte{2}, 32),
				Epoch: 1, CapExp: d.nowMS() + 3600_000, IssuedAt: d.nowMS()}); err != nil {
				return err
			}
			_, err := tx.PutHeldGrant(interactions.HeldCap{Issuer: "did:anet:i", Scope: interactions.CapScopePeer,
				CapID: bytes.Repeat([]byte{3}, 16), Cap: []byte{1}, HolderSeed: bytes.Repeat([]byte{3}, 32), Class: 1,
				Epoch: 1, CapExp: d.nowMS() + 3600_000, Home: "did:anet:hub", P2P: "192.0.2.1:7000", ReceivedAt: d.nowMS()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	counts := func() (int, int) {
		t.Helper()
		var issued, held int
		if err := d.ix.Update(func(tx *interactions.Tx) error {
			hs, err := tx.HeldCaps("did:anet:i")
			if err != nil {
				return err
			}
			is, err := tx.IssuedCaps("did:anet:g")
			issued, held = len(is), len(hs)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return issued, held
	}
	fill()
	before := currentMbox(t, d)

	// The key cannot be written: nothing changes.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.mbox.mu.Lock()
	path := d.mbox.path
	d.mbox.path = filepath.Join(blocker, "mbox.key")
	d.mbox.mu.Unlock()
	if err := d.ix.Update(d.capsEmergencyTx); err == nil {
		t.Fatal("the emergency transaction went through without its new key on disk")
	}
	if i, h := counts(); i != 1 || h != 1 {
		t.Fatalf("a failed emergency transaction cleared the tables: %d issued, %d held", i, h)
	}
	if got := currentMbox(t, d); got.Epoch != before.Epoch || !bytes.Equal(got.Pub, before.Pub) {
		t.Fatal("a key that could not be saved became the current one")
	}
	d.mbox.mu.Lock()
	d.mbox.path = path
	d.mbox.mu.Unlock()

	// The new key is on disk before the transaction commits.
	var onDiskInTx uint64
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		if err := d.capsEmergencyTx(tx); err != nil {
			return err
		}
		onDiskInTx, _ = mboxOnDisk(t, d)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if onDiskInTx != before.Epoch+1 {
		t.Fatalf("inside the transaction mbox.key holds %d, want %d", onDiskInTx, before.Epoch+1)
	}
	if i, h := counts(); i != 0 || h != 0 {
		t.Fatalf("after the emergency transaction: %d issued, %d held", i, h)
	}
	after := currentMbox(t, d)
	if after.Epoch != before.Epoch+1 || bytes.Equal(after.Pub, before.Pub) {
		t.Fatalf("the key after the emergency: %+v (was %+v)", after, before)
	}
	if reg, _ := d.mailboxStatus(); reg {
		t.Fatal("the hub is reported to hold a key it has not been sent")
	}
	// The next key set publishes the new key.
	d.maintainKeyRing()
	if set := keySetOf(t, d); set.Mbox.Epoch != after.Epoch || !bytes.Equal(set.Mbox.Pub, after.Pub) {
		t.Fatal("the key set does not publish the new mailbox key")
	}

	// Rolled back after the key was written (a crash before the commit):
	// the rows stay, the newer key stays, and running it again moves on.
	fill()
	boom := errors.New("crash before the commit")
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		if err := d.capsEmergencyTx(tx); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if i, h := counts(); i != 1 || h != 1 {
		t.Fatalf("a rolled-back emergency cleared rows: %d issued, %d held", i, h)
	}
	if got := currentMbox(t, d); got.Epoch != after.Epoch+1 {
		t.Fatalf("after the rolled-back emergency the key is at %d, want %d", got.Epoch, after.Epoch+1)
	}
	if err := d.ix.Update(d.capsEmergencyTx); err != nil {
		t.Fatal(err)
	}
	if i, h := counts(); i != 0 || h != 0 || currentMbox(t, d).Epoch != after.Epoch+2 {
		t.Fatal("the emergency run again did not finish the job")
	}
}

// TestMboxRepublishWhenHubLacksIt (SSR-D4): after a registration or a key
// set publication that leaves the hub without the current mailbox key —
// the hub refused the set, or the set predates the key — the node reports
// mailbox_registered = false and publishes again, 5 seconds after the first
// failure and doubling, until the hub holds a set with the current key.
func TestMboxRepublishWhenHubLacksIt(t *testing.T) {
	keepMboxLoopOut(t)
	srv, _, prov := registeredPair(t)
	fake := fakeHubAt(t, srv.URL)
	ctx := context.Background()
	hubMbox := func() seal.Mbox {
		t.Helper()
		fake.mu.Lock()
		raw := append([]byte(nil), fake.agents[prov.AID()].keyset...)
		fake.mu.Unlock()
		s, err := seal.UnmarshalSignedEncKeySet(raw)
		if err != nil {
			t.Fatal(err)
		}
		var set seal.EncKeySet
		if err := coredet.Unmarshal(s.Set, &set); err != nil {
			t.Fatal(err)
		}
		return set.Mbox
	}
	state := func() (bool, int, time.Time) {
		prov.mboxSt.mu.Lock()
		defer prov.mboxSt.mu.Unlock()
		return prov.mboxSt.registered, prov.mboxSt.failures, prov.mboxSt.retryAt
	}
	if reg, _, _ := state(); !reg {
		t.Fatal("a registered node does not report its mailbox key registered")
	}

	// The hub refuses the key set.
	setFake(t, srv.URL, func(h *fakeHub) { h.refuseKeySets = true })
	start := time.Now()
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil); err != nil {
		t.Fatal(err)
	}
	reg, fails, at := state()
	if reg || fails != 1 || at.Before(start.Add(mboxRetryBase)) || at.After(time.Now().Add(mboxRetryBase)) {
		t.Fatalf("after a refused set: registered=%v failures=%d retry in %v", reg, fails, at.Sub(start))
	}
	// Not yet due: nothing is sent.
	prov.mboxTick(ctx, at.Add(-time.Second))
	if _, f, _ := state(); f != 1 {
		t.Fatal("a republish ran before it was due")
	}
	// Due, still refused: backs off.
	prov.mboxTick(ctx, at)
	reg, fails, at2 := state()
	if wait := time.Until(at2); reg || fails != 2 || wait < mboxRetryAfter(2)-time.Second || wait > mboxRetryAfter(2) {
		t.Fatalf("second refusal: registered=%v failures=%d next in %v, want %v", reg, fails, wait, mboxRetryAfter(2))
	}
	// The hub takes it now.
	setFake(t, srv.URL, func(h *fakeHub) { h.refuseKeySets = false })
	prov.mboxTick(ctx, at2)
	if reg, fails, at := state(); !reg || fails != 0 || !at.IsZero() {
		t.Fatalf("after the hub took the set: registered=%v failures=%d retry=%v", reg, fails, at)
	}
	if got, cur := hubMbox(), currentMbox(t, prov); got.Epoch != cur.Epoch || !bytes.Equal(got.Pub, cur.Pub) {
		t.Fatal("the hub does not hold the current mailbox key")
	}

	// The mailbox key was replaced after the set was signed: the hub takes
	// the registration's (old) set, which is not enough.
	if err := prov.ix.Update(func(tx *interactions.Tx) error { _, err := prov.rotateMboxTx(tx); return err }); err != nil {
		t.Fatal(err)
	}
	if reg, _, _ := state(); reg {
		t.Fatal("right after the mailbox key was replaced, the hub is reported to hold it")
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil); err != nil {
		t.Fatal(err)
	}
	reg, _, at = state()
	if reg || at.IsZero() {
		t.Fatal("a hub holding the old mailbox key is reported as holding the current one")
	}
	if hubMbox().Epoch == currentMbox(t, prov).Epoch {
		t.Fatal("set-up: the registration carried the new key already")
	}
	posts := func() int {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.keyPosts
	}
	p0 := posts()
	prov.mboxTick(ctx, at)
	if reg, _, _ := state(); !reg {
		t.Fatal("the republish did not bring the hub up to the current key")
	}
	if n := posts() - p0; n != 1 {
		t.Fatalf("one republish sent %d key sets, want 1", n)
	}
	if got, cur := hubMbox(), currentMbox(t, prov); got.Epoch != cur.Epoch || !bytes.Equal(got.Pub, cur.Pub) {
		t.Fatalf("the hub holds mailbox epoch %d, the node is at %d", got.Epoch, cur.Epoch)
	}

	// The real hub's rule (SSR-W9) refuses a changed key at the same epoch:
	// the node keeps reporting it and keeps retrying, backing off to 10
	// minutes.
	setFake(t, srv.URL, func(h *fakeHub) { h.refuseKeySets = true })
	if err := prov.publishKeys(ctx, srv.URL); err == nil {
		t.Fatal("a refused publication reported success")
	}
	// A new set the hub refuses: each retry is one request and one failure.
	if err := prov.ix.Update(func(tx *interactions.Tx) error { _, err := prov.rotateMboxTx(tx); return err }); err != nil {
		t.Fatal(err)
	}
	_, f0, at := state()
	p0 = posts()
	prov.mboxTick(ctx, at)
	if _, f1, _ := state(); f1 != f0+1 || posts()-p0 != 1 {
		t.Fatalf("a refused retry counted %d failures and sent %d key sets, want 1 and 1", f1-f0, posts()-p0)
	}
	for i := 0; i < 12; i++ {
		_, _, at := state()
		prov.mboxTick(ctx, at)
	}
	reg, fails, at = state()
	if wait := time.Until(at); reg || fails < 12 || wait > mboxRetryMax || wait < mboxRetryMax-time.Minute {
		t.Fatalf("after many refusals: registered=%v failures=%d next in %v, want about %v", reg, fails, wait, mboxRetryMax)
	}
}

// TestMboxLoopPrunesCaps (SSR-D2): the daemon prunes the capability tables
// on its own, every five minutes, at the wire clock.
func TestMboxLoopPrunesCaps(t *testing.T) {
	old := capsPruneEvery.set(20 * time.Millisecond)
	t.Cleanup(func() { capsPruneEvery.set(old) })
	if old != 5*time.Minute {
		t.Fatalf("the capability tables are pruned every %v, want 5m", old)
	}
	d := newTestDaemon(t, "", true)
	now := d.nowMS()
	issuedID, heldID := bytes.Repeat([]byte{7}, 16), bytes.Repeat([]byte{9}, 16)
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		if err := tx.PutIssuedCap(interactions.IssuedCap{CapID: issuedID, Grantee: "did:anet:g", Class: 1,
			Scope: interactions.CapScopePeer, HolderSeed: bytes.Repeat([]byte{7}, 32), HolderPub: bytes.Repeat([]byte{8}, 32),
			Epoch: 1, CapExp: now - 1, IssuedAt: now - 10}); err != nil {
			return err
		}
		_, err := tx.PutHeldGrant(interactions.HeldCap{Issuer: "did:anet:i", Scope: interactions.CapScopePeer,
			CapID: heldID, Cap: []byte{1}, HolderSeed: bytes.Repeat([]byte{9}, 32), Class: 1,
			Epoch: 1, CapExp: now - 1, Home: "did:anet:hub", ReceivedAt: now - 10})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, errI := d.ix.IssuedCap(issuedID)
		_, errH := d.ix.HeldCap("did:anet:i", interactions.CapScopePeer, "")
		if errors.Is(errI, interactions.ErrNotFound) && errors.Is(errH, interactions.ErrNotFound) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expired capabilities are still stored: issued %v, held %v", errI, errH)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// FuzzMboxKeyFile: whatever bytes mbox.key holds, decodeMboxFile either
// refuses them or returns a key it would write back byte for byte, with an
// epoch in [1, 2^53) and a 32-byte seed.
func FuzzMboxKeyFile(f *testing.F) {
	for _, e := range []uint64{1, 2, 1<<53 - 1} {
		b, err := encodeMboxFile(e, bytes.Repeat([]byte{byte(e)}, 32))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte{0xa3, 0x00, 0x6b})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		epoch, seed, err := decodeMboxFile(b)
		if err != nil {
			return
		}
		if epoch == 0 || epoch >= mboxEpochLimit || len(seed) != ed25519.SeedSize {
			t.Fatalf("accepted epoch %d with a %d-byte seed", epoch, len(seed))
		}
		re, err := encodeMboxFile(epoch, seed)
		if err != nil || !bytes.Equal(re, b) {
			t.Fatalf("accepted bytes that are not the key's own encoding (%v)", err)
		}
	})
}

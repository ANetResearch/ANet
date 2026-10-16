package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"
)

// KEL D-50: this node's own witness notice — checked before the gate sees
// it, replaced only by a newer one about the same tip or by one about
// another tip, kept in key_state_notice.cbor and read back strictly at
// start.

// witnessJoinHooks are the join hooks of the identity area (KEL D-13, KEL-9)
// as far as these tests need them: Prepare builds the witness designation
// committing the join record's witness_rot (or reuses the one built), and
// Adopt makes it the accepted tip; both are written to identity.kel.
type witnessJoinHooks struct{ d *Daemon }

func (h witnessJoinHooks) Prepare(_ context.Context, hubAID string) ([]byte, error) {
	c := h.d.self
	if _, ok := c.Pending(); !ok {
		next := identitytest.NewNext()
		if r := h.d.joinSnapshot(); r != nil && r.WitnessRot != nil {
			next = r.WitnessRot.NextDigest
		}
		if _, err := c.BuildWitnessDesignation(hubAID, next, h.d.nowMS()); err != nil {
			return nil, err
		}
		if err := saveIdentity(h.d.layout, c); err != nil {
			return nil, err
		}
	}
	return identity.MarshalKEL(c.KEL())
}

func (h witnessJoinHooks) Adopt(context.Context, RegisterResult) error {
	c := h.d.self
	p, ok := c.Pending()
	if !ok || p.Event == nil {
		return nil
	}
	var ev identity.SignedEvent
	if err := coredet.Unmarshal(p.Event, &ev); err != nil {
		return err
	}
	if err := c.Adopt(ev); err != nil {
		return err
	}
	return saveIdentity(h.d.layout, c)
}

// witnessedNode is a node that joined a fake hub which witnesses it: the
// witness designation is accepted, and the hub signs notices when notices
// is set. The relay loop is stopped; the test polls.
func witnessedNode(t *testing.T, notices bool) (*Daemon, *httptest.Server, *fakeHub) {
	t.Helper()
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	fh.notices = notices
	fh.mu.Unlock()
	d, _ := joinDaemon(t)
	d.setJoinHooks(witnessJoinHooks{d})
	next := base64.StdEncoding.EncodeToString(identitytest.NewNext())
	if code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+next+`"`)); code != 200 ||
		out["state"] != JoinRegistered {
		t.Fatalf("join: %d %v", code, out)
	}
	d.stopRelayLoop()
	if w := identityFactsOf(d.self).Witness; w != fh.self.AID() {
		t.Fatalf("the witness is %q, want the hub %s", w, fh.self.AID())
	}
	return d, srv, fh
}

// hubNotice is the notice hub signs for c's accepted tip, observed at at,
// as a hub answer carries it (base64url).
func hubNotice(t *testing.T, hub identity.Signer, c *identity.Controller, at uint64) string {
	t.Helper()
	kel := c.Accepted()
	tip := kel[len(kel)-1]
	b, err := identity.SignNotice(hub, identity.NoticeBody{AID: c.AID(), Seq: tip.Event.Seq, Head: tip.EventID, ObservedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// forgedNotice is a notice about c's tip that names witness and is signed
// by forger: what a party without the witness hub's key can make.
func forgedNotice(t *testing.T, witness string, forger identity.Signer, c *identity.Controller, at uint64) []byte {
	t.Helper()
	kel := c.Accepted()
	tip := kel[len(kel)-1]
	body, err := coredet.Marshal(identity.NoticeBody{Type: identity.NoticeType, AID: c.AID(), Seq: tip.Event.Seq,
		Head: tip.EventID, ObservedAt: at, Witness: witness})
	if err != nil {
		t.Fatal(err)
	}
	real, err := base64.RawURLEncoding.DecodeString(hubNotice(t, forger, c, at))
	if err != nil {
		t.Fatal(err)
	}
	var w identity.NoticeWire
	if err := coredet.Unmarshal(real, &w); err != nil {
		t.Fatal(err)
	}
	w.Body = body
	w.Env.SignerAID = witness
	w.Env.Sig, _ = forger.Sign(body)
	b, err := coredet.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The register answer of the hub that witnesses the node brings the first
// notice, and the gate opens; every poll answer brings a newer one.
func TestTheNoticeComesWithRegisterAndPoll(t *testing.T) {
	d, _, fh := witnessedNode(t, true)
	if s := d.identityState(); s.State != IdentityOK || s.Reason != reasonIdentityOK {
		t.Fatalf("after the join: %+v", s)
	}
	first, _, _ := d.idGate.read()
	now := uint64(time.Now().UnixMilli()) + 4*hourMS
	d.setClock(func() uint64 { return now })
	if s := d.identityState(); s.Reason != reasonConfirmAging {
		t.Fatalf("four hours on: %+v", s)
	}
	fh.mu.Lock()
	fh.noticeAt = func() uint64 { return now }
	fh.mu.Unlock()
	if err := d.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := d.identityState(); s.Reason != reasonIdentityOK {
		t.Fatalf("after a poll: %+v", s)
	}
	if n, _, _ := d.idGate.read(); n == first || n.Body.ObservedAt != now {
		t.Fatalf("the poll's notice was not taken: %+v", n)
	}
	// A hub in quarantine answers without a notice: the one held stays,
	// until it is too old, and then the state says why.
	fh.mu.Lock()
	fh.noticeErr = noticeErrQuarantine
	fh.mu.Unlock()
	if err := d.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := d.identityState(); s.Reason != reasonIdentityOK {
		t.Fatalf("an answer without a notice dropped the one held: %+v", s)
	}
	now += NoticeStopLine + 1
	if s := d.identityState(); s.State != IdentityNoticeStale || s.Reason != reasonQuarantine {
		t.Fatalf("past the stop line in quarantine: %+v", s)
	}
}

// What comes in is checked before the gate sees it (KEL D-50,
// identity.VerifyNotice): a notice the witness did not sign, about another
// key state, too old, from the future, or not base64url never opens the
// gate; the real one does.
func TestOnlyAVerifiedNoticeReachesTheGate(t *testing.T) {
	d, _, fh := witnessedNode(t, false)
	stale := func(step string) {
		t.Helper()
		if s := d.identityState(); s.State != IdentityNoticeStale || s.Reason != reasonNoticeStale {
			t.Fatalf("%s: %+v, want notice_stale", step, s)
		}
		if b, err := os.ReadFile(d.layout.keyStateNoticePath()); err == nil {
			t.Fatalf("%s: key_state_notice.cbor was written (%d bytes)", step, len(b))
		}
	}
	stale("no notice yet")
	now := uint64(time.Now().UnixMilli())
	forger, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	// Signed by another hub, naming the witness: the signer is not the
	// witness.
	d.noteHubNotice(hubNotice(t, forger, d.self, now), "")
	stale("a notice another hub signed")
	// Signed by another hub under the witness's AID in the envelope: the
	// signature does not verify under the witness's KEL.
	d.noteHubNotice(base64.RawURLEncoding.EncodeToString(forgedNotice(t, fh.self.AID(), forger, d.self, now)), "")
	stale("a forged signature")
	kel := d.self.Accepted()
	real, err := base64.RawURLEncoding.DecodeString(hubNotice(t, fh.self, d.self, now))
	if err != nil {
		t.Fatal(err)
	}
	// About another key state: the icp.
	icp, err := identity.SignNotice(fh.self, identity.NoticeBody{AID: d.AID(), Seq: 0, Head: kel[0].EventID, ObservedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	d.noteHubNotice(base64.RawURLEncoding.EncodeToString(icp), "")
	stale("a notice about another key state")
	// Too old, and from the future.
	d.noteHubNotice(hubNotice(t, fh.self, d.self, now-identity.NoticeMaxAge-minuteMS), "")
	stale("a notice older than six hours")
	d.noteHubNotice(hubNotice(t, fh.self, d.self, now+10*minuteMS), "")
	stale("a notice from the future")
	// Not base64url, and not a notice at all.
	d.noteHubNotice(base64.StdEncoding.EncodeToString(real)+"==", "")
	d.noteHubNotice(base64.RawURLEncoding.EncodeToString([]byte("not a notice")), "")
	d.noteHubNotice(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, seal.MaxNoticeBytes+1)), "")
	stale("bytes that are not a notice")
	// An error this node does not know changes nothing either: not the
	// state, and not the reason a known error gave it.
	d.noteHubNotice("", "bogus")
	stale("an unknown key_state_notice_error")
	d.noteHubNotice("", noticeErrQuarantine)
	d.noteHubNotice("", "bogus")
	if s := d.identityState(); s.State != IdentityNoticeStale || s.Reason != reasonQuarantine {
		t.Fatalf("an unknown key_state_notice_error after quarantine: %+v, want identity.quarantine", s)
	}
	// The witness's own notice opens the gate and is kept.
	d.noteHubNotice(hubNotice(t, fh.self, d.self, now), "")
	if s := d.identityState(); s.State != IdentityOK {
		t.Fatalf("the witness's notice: %+v", s)
	}
	if _, err := os.Stat(d.layout.keyStateNoticePath()); err != nil {
		t.Fatalf("key_state_notice.cbor: %v", err)
	}
}

// A notice the node cannot check — its witness's KEL is not held — is not
// kept either (KEL C-51 step 5).
func TestANoticeOfAnUnknownWitnessIsNotKept(t *testing.T) {
	d := newTestDaemon(t, "", false)
	if err := identitytest.SwitchWitness(d.self, identitytest.Witness, d.nowMS()); err != nil {
		t.Fatal(err)
	}
	d.noteHubNotice(hubNotice(t, identity.SuiteController(), d.self, d.nowMS()), "")
	if n, _, _ := d.idGate.read(); n != nil {
		t.Fatalf("a notice of a witness whose KEL is not held was kept: %+v", n.Body)
	}
}

// The notice held only moves forward (KEL D-50): about the same tip, a
// newer one replaces it and an older one does not; about another tip — a
// rotation, a hub switch whose new hub's clock runs behind — the newest
// statement of the key state replaces it whatever its time.
func TestTheNoticeHeldOnlyMovesForward(t *testing.T) {
	d, _, fh := witnessedNode(t, false)
	now := uint64(time.Now().UnixMilli())
	d.setClock(func() uint64 { return now })
	observed := func(step string, want uint64) {
		t.Helper()
		n, _, _ := d.idGate.read()
		if n == nil || n.Body.ObservedAt != want {
			t.Fatalf("%s: holds %+v, want one observed at %d", step, n, want)
		}
		got, err := readKeyStateNoticeFile(d.layout)
		if err != nil || got == nil || got.Body.ObservedAt != want || !bytes.Equal(got.Wire, n.Wire) {
			t.Fatalf("%s: key_state_notice.cbor holds %+v (%v), want the notice held", step, got, err)
		}
	}
	d.noteHubNotice(hubNotice(t, fh.self, d.self, now-2*hourMS), "")
	observed("the first", now-2*hourMS)
	d.noteHubNotice(hubNotice(t, fh.self, d.self, now-hourMS), "")
	observed("a newer one about the same tip", now-hourMS)
	d.noteHubNotice(hubNotice(t, fh.self, d.self, now-3*hourMS), "")
	observed("an older one about the same tip", now-hourMS)
	d.noteHubNotice(hubNotice(t, fh.self, d.self, now-hourMS), "")
	observed("the same one again", now-hourMS)
	// A rotation: the tip moves. The hub's clock is behind this time, and
	// its notice about the new tip still replaces the one about the old.
	if err := identitytest.Rotate(d.self, now+1); err != nil {
		t.Fatal(err)
	}
	if s := d.identityState(); s.State != IdentityNoticeStale {
		t.Fatalf("after the rotation, before its notice: %+v", s)
	}
	d.noteHubNotice(hubNotice(t, fh.self, d.self, now-90*minuteMS), "")
	observed("a notice about the new tip, observed before the one held", now-90*minuteMS)
	if s := d.identityState(); s.State != IdentityOK {
		t.Fatalf("the new tip confirmed: %+v", s)
	}
	if cur := d.keyCursor(); cur.NoticeObservedAt != now-90*minuteMS || cur.KeyStateSeq != d.self.CurrentSeq() {
		t.Fatalf("keyCursor = %+v", cur)
	}
}

// key_state_notice.cbor is read at start, strictly and checked again, so a
// restarted node does not wait for its hub's next answer to send; a file
// that does not pass is not used.
func TestTheNoticeIsReadBackAtStart(t *testing.T) {
	d, _, fh := witnessedNode(t, true)
	if s := d.identityState(); s.State != IdentityOK {
		t.Fatalf("before the restart: %+v", s)
	}
	fh.mu.Lock()
	fh.notices = false // the hub's answers say nothing from here on
	fh.mu.Unlock()
	layout := d.layout
	good, err := os.ReadFile(layout.keyStateNoticePath())
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(layout.keyStateNoticePath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key_state_notice.cbor: %v %v, want mode 0600", fi.Mode(), err)
	}
	restart := func(step string, file []byte, want string) {
		t.Helper()
		d.Close()
		if err := os.WriteFile(layout.keyStateNoticePath(), file, 0o600); err != nil {
			t.Fatal(err)
		}
		var err error
		if d, err = New(layout); err != nil {
			t.Fatal(err)
		}
		d.setJoinHooks(witnessJoinHooks{d})
		t.Cleanup(func() { d.Close() })
		if s := d.identityState(); s.State != want {
			t.Fatalf("%s: %+v, want %s", step, s, want)
		}
	}
	restart("the file as written", good, IdentityOK)
	f, _, err := decodeKeyStateNoticeFile(good)
	if err != nil {
		t.Fatal(err)
	}
	var bad keyStateNoticeFile
	if err := coredet.Unmarshal(good, &bad); err != nil {
		t.Fatal(err)
	}
	extra, err := coredet.Marshal(map[uint64]any{1: bad.Notice, 2: bad.SavedAt, 3: 1})
	if err != nil {
		t.Fatal(err)
	}
	restart("a key the file does not have", extra, IdentityNoticeStale)
	restart("garbage", []byte("not cbor"), IdentityNoticeStale)
	forger, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	forgedFile, err := encodeKeyStateNoticeFile(forgedNotice(t, fh.self.AID(), forger, d.self, f.Body.ObservedAt), f.Body.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	restart("a notice the witness did not sign", forgedFile, IdentityNoticeStale)
	// A notice the witness signed, observed after now: within the clock
	// skew that is the hub's clock, and it is used; beyond it, the notice
	// would pass for fresh longer than it is, and it is not.
	observedAt := func(at uint64) []byte {
		t.Helper()
		wire, err := base64.RawURLEncoding.DecodeString(hubNotice(t, fh.self, d.self, at))
		if err != nil {
			t.Fatal(err)
		}
		b, err := encodeKeyStateNoticeFile(wire, at)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	ahead := func(by uint64) []byte { return observedAt(uint64(time.Now().UnixMilli()) + by) }
	restart("a notice observed ten minutes from now", ahead(10*minuteMS), IdentityNoticeStale)
	restart("a notice observed two minutes from now", ahead(2*minuteMS), IdentityOK)
	// A notice the witness signed seven hours ago is read back all the
	// same: checked as of when the hub observed it, its age is the gate's
	// to judge. The gate stays closed, and the identity says how long the
	// hub has not confirmed the key, from that notice.
	restart("a notice observed seven hours ago", observedAt(uint64(time.Now().UnixMilli())-7*hourMS), IdentityNoticeStale)
	if s := d.IdentityStatus(); s.Reason != reasonNoticeStale || s.Notice == nil || s.Notice.AgeS < 7*hourMS/1000 {
		t.Fatalf("a notice observed seven hours ago: %+v", s)
	}
	restart("the file as written, again", good, IdentityOK)
}

// StoredNotice (anetfixture seal --kel self) gives the file's notice only
// for the KEL whose tip it states: not after a rotation, not for another
// identity, not for no KEL.
func TestStoredNoticeIsTheTipsOnly(t *testing.T) {
	d, _, _ := witnessedNode(t, true)
	held, _, _ := d.idGate.read()
	if held == nil {
		t.Fatal("no notice held")
	}
	if got := StoredNotice(d.layout, d.self.KEL()); !bytes.Equal(got, held.Wire) {
		t.Fatalf("the tip's notice: %x, want %x", got, held.Wire)
	}
	other, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	if got := StoredNotice(d.layout, other.KEL()); got != nil {
		t.Fatalf("another identity's KEL: %x", got)
	}
	if got := StoredNotice(d.layout, nil); got != nil {
		t.Fatalf("no KEL: %x", got)
	}
	if err := identitytest.Rotate(d.self, d.nowMS()+1); err != nil {
		t.Fatal(err)
	}
	if got := StoredNotice(d.layout, d.self.KEL()); got != nil {
		t.Fatalf("after a rotation, the notice of the tip before: %x", got)
	}
}

// The file's decoder (core CORE-E3): exactly keys 1 and 2, a notice in
// NoticeWire form of 1 to seal.MaxNoticeBytes bytes, a time, and the
// canonical encoding.
func TestKeyStateNoticeFileIsReadStrictly(t *testing.T) {
	c, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	if err := identitytest.SwitchWitness(c, identitytest.Witness, 2); err != nil {
		t.Fatal(err)
	}
	wire, err := base64.RawURLEncoding.DecodeString(hubNotice(t, identity.SuiteController(), c, 3))
	if err != nil {
		t.Fatal(err)
	}
	good, err := encodeKeyStateNoticeFile(wire, 4)
	if err != nil {
		t.Fatal(err)
	}
	n, at, err := decodeKeyStateNoticeFile(good)
	if err != nil || at != 4 || !bytes.Equal(n.Wire, wire) || n.Body.AID != c.AID() || n.Body.ObservedAt != 3 {
		t.Fatalf("the file as written: %+v %d %v", n, at, err)
	}
	mk := func(v any) []byte {
		b, err := coredet.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// the notice's length in four bytes where two or one would do
	nonCanonical := append([]byte{0xa2, 0x01, 0x5a, 0, 0, byte(len(wire) >> 8), byte(len(wire))}, wire...)
	nonCanonical = append(nonCanonical, 0x02, 0x04)
	for name, b := range map[string][]byte{
		"empty":                    nil,
		"not cbor":                 []byte("{}"),
		"an array":                 mk([]any{wire, 4}),
		"no saved_at":              mk(map[uint64]any{1: wire}),
		"saved_at 0":               mk(map[uint64]any{1: wire, 2: 0}),
		"no notice":                mk(map[uint64]any{2: 4}),
		"an empty notice":          mk(map[uint64]any{1: []byte{}, 2: 4}),
		"an unknown key":           mk(map[uint64]any{1: wire, 2: 4, 3: 1}),
		"a text key":               mk(map[string]any{"notice": wire, "saved_at": 4}),
		"a notice that is not one": mk(map[uint64]any{1: []byte("x"), 2: 4}),
		"a notice too long":        mk(map[uint64]any{1: bytes.Repeat([]byte{1}, seal.MaxNoticeBytes+1), 2: 4}),
		"trailing bytes":           append(append([]byte(nil), good...), 0),
		"non-canonical length":     nonCanonical,
	} {
		if _, _, err := decodeKeyStateNoticeFile(b); !errors.Is(err, errKeyStateNoticeFile) {
			t.Errorf("%s: %v, want errKeyStateNoticeFile", name, err)
		}
	}
	if _, err := encodeKeyStateNoticeFile(nil, 4); err == nil {
		t.Error("an empty notice was encoded")
	}
	if _, err := encodeKeyStateNoticeFile(wire, 0); err == nil {
		t.Error("a file without a time was encoded")
	}
}

// FuzzKeyStateNoticeFile: the decoder of key_state_notice.cbor never
// panics, and what it accepts is the canonical encoding of a notice in its
// own form, which it reads back to the same notice.
func FuzzKeyStateNoticeFile(f *testing.F) {
	c, err := identitytest.Agent()
	if err != nil {
		f.Fatal(err)
	}
	if err := identitytest.SwitchWitness(c, identitytest.Witness, 2); err != nil {
		f.Fatal(err)
	}
	kel := c.Accepted()
	tip := kel[len(kel)-1]
	wire, err := identity.SignNotice(identity.SuiteController(), identity.NoticeBody{AID: c.AID(), Seq: tip.Event.Seq,
		Head: tip.EventID, ObservedAt: 3})
	if err != nil {
		f.Fatal(err)
	}
	good, err := encodeKeyStateNoticeFile(wire, 4)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good)
	f.Add([]byte{})
	f.Add([]byte{0xa2, 0x01, 0x40, 0x02, 0x01})
	f.Add(append(append([]byte(nil), good...), 0))
	f.Fuzz(func(t *testing.T, b []byte) {
		n, at, err := decodeKeyStateNoticeFile(b)
		if err != nil {
			if !errors.Is(err, errKeyStateNoticeFile) {
				t.Fatalf("an error that is not errKeyStateNoticeFile: %v", err)
			}
			return
		}
		if n == nil || at == 0 || len(n.Wire) == 0 || len(n.Wire) > seal.MaxNoticeBytes || n.Body.Type != identity.NoticeType {
			t.Fatalf("accepted %x as %+v at %d", b, n, at)
		}
		back, err := encodeKeyStateNoticeFile(n.Wire, at)
		if err != nil || !bytes.Equal(back, b) {
			t.Fatalf("accepted %x, which is not the encoding of what it read (%x, %v)", b, back, err)
		}
		if body, _, err := identity.DecodeNotice(n.Wire); err != nil || body != n.Body {
			t.Fatalf("the body read %+v is not the notice's %+v (%v)", n.Body, body, err)
		}
	})
}

// Every envelope the node seals carries the notice that confirms the KEL it
// carries (inner key 11, KEL D-50, C-52), signed under the key state that
// KEL ends at; a node without one seals nothing (engine ENG-K13, ENG-R3
// S3): its gate is closed, and with the gate held open no notice confirms
// the key state it would sign under.
func TestSealedEnvelopesCarryTheNotice(t *testing.T) {
	d, _, _ := witnessedNode(t, true)
	peer := newTestDaemon(t, "", false)
	held, _, _ := d.idGate.read()
	if held == nil {
		t.Fatal("no notice held")
	}
	op, err := seal.Open(sealFrom(t, d, peer, seal.TypeMessage, "ix_1", []byte("hi")), peer.AID(), peer.enc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(op.Inner.Notices) != 1 || !bytes.Equal(op.Inner.Notices[0], held.Wire) {
		t.Fatalf("inner key 11 is %x, want the notice held %x", op.Inner.Notices, held.Wire)
	}
	if err := identity.VerifyNotice(op.Inner.Notices[0], kelOf(t, op.Inner.KEL), d.hubKELs, d.nowMS()); err != nil {
		t.Fatalf("the notice does not confirm the KEL the envelope carries: %v", err)
	}
	if op.Inner.KeyStateSeq != held.Body.Seq {
		t.Fatalf("signed under key state %d, the notice confirms %d", op.Inner.KeyStateSeq, held.Body.Seq)
	}
	bare := newTestDaemon(t, "", false)
	if env, err := bare.sealWith(peer.AID(), seal.TypeMessage, "ix_2", []byte("hi"), keySetOf(t, peer)); !errors.Is(err, errGateClosed) {
		t.Fatalf("a node without a notice sealed %d bytes (%v)", len(env), err)
	}
	holdGateOpen(bare)
	if env, err := bare.sealWith(peer.AID(), seal.TypeMessage, "ix_2", []byte("hi"), keySetOf(t, peer)); !errors.Is(err, errNoTipNotice) {
		t.Fatalf("a node without a notice, its gate held open, sealed %d bytes (%v)", len(env), err)
	}
}

func kelOf(t *testing.T, b []byte) []identity.SignedEvent {
	t.Helper()
	kel, err := identity.UnmarshalKEL(b)
	if err != nil {
		t.Fatal(err)
	}
	return kel
}

// An answer that carries a notice also says the hub has no
// key_state_notice_error any more, whether its notice replaces the one held
// (newer, or another tip) or not (the same, or older): a hub out of
// quarantine must not read as one in it once the notice ages.
func TestANoticeClearsTheHubsError(t *testing.T) {
	var g identityGateInputs
	steps := []struct {
		name     string
		n        *keyStateNotice
		replaced bool
	}{
		{"the first notice", noticeAt(tsNow), true},
		{"the same notice again", noticeAt(tsNow), false},
		{"an older one about the same tip", noticeAt(tsNow - minuteMS), false},
		{"a newer one", noticeAt(tsNow + minuteMS), true},
	}
	for _, s := range steps {
		g.noteNotice(nil, noticeErrQuarantine)
		if got := g.noteNotice(s.n, ""); got != s.replaced {
			t.Fatalf("%s: replaced %v, want %v", s.name, got, s.replaced)
		}
		if _, e, _ := g.read(); e != "" {
			t.Fatalf("%s: the hub's error %q stays", s.name, e)
		}
		in := identityInputs{File: witnessedFacts()}
		in.Notice, in.NoticeErr, in.PublishErr = g.read()
		in.Now = in.Notice.Body.ObservedAt + NoticeStopLine + 1
		if st := identityStateOf(in); st.Reason != reasonNoticeStale {
			t.Fatalf("%s, then past the stop line: %+v, want notice_stale", s.name, st)
		}
	}
}

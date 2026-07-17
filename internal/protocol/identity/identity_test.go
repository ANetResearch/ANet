package identity

import (
	"crypto/ed25519"
	"testing"

	"github.com/ANetResearch/ANet/internal/protocol/anetcid"
	"github.com/ANetResearch/ANet/internal/protocol/coredet"
)

// A delegation (drt) must round-trip through Export/Restore AND still allow a later Rotate: the drt
// carries the pre-rotation commitment forward, so the KEL head stays restorable and the next Rotate's
// pre-rotation gate (which reads the IMMEDIATELY-prior event's next_digest) still matches. Regression
// for the drt-missing-NextDigest bug (anrp VAL-11 mode (b) self-delegation bricked restore + rotation).
func TestDelegateExportRestoreRotate(t *testing.T) {
	c, _ := Incept()
	hostPub, _, _ := ed25519.GenerateKey(nil)
	if err := c.Delegate(hostPub, 1000); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	// delegate → rotate directly (exercises priorNextDigest reading the drt event).
	if err := c.Rotate(2000); err != nil {
		t.Fatalf("rotate immediately after delegate: %v", err)
	}
	// a second delegation after the rotation, then export/restore (the bug failed at Restore).
	hostPub2, _, _ := ed25519.GenerateKey(nil)
	if err := c.Delegate(hostPub2, 3000); err != nil {
		t.Fatalf("second delegate: %v", err)
	}
	blob, err := c.Export()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	r, err := Restore(blob)
	if err != nil {
		t.Fatalf("restore after delegate: %v", err)
	}
	// both delegated keys survive in the restored key state.
	states, err := Replay(r.KEL())
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, dk := range states[len(states)-1].DelegatedKeys {
		have[string(dk)] = true
	}
	if !have[string(hostPub)] || !have[string(hostPub2)] {
		t.Fatal("delegated keys did not survive delegate/rotate/delegate/restore")
	}
	// the restored controller can still rotate again.
	if err := r.Rotate(4000); err != nil {
		t.Fatalf("rotate after restore: %v", err)
	}
}

func TestInceptionAndReplay(t *testing.T) {
	c, err := Incept()
	if err != nil {
		t.Fatal(err)
	}
	if c.AID() == "" || c.DID() != "did:anet:"+c.AID() {
		t.Fatalf("bad AID/DID: %q / %q", c.AID(), c.DID())
	}
	states, err := Replay(c.KEL())
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(states) != 1 || states[0].KeyStateSeq != 0 || states[0].Status != StatusActive {
		t.Fatalf("bad icp state: %+v", states[0])
	}
	if states[0].AID != c.AID() {
		t.Fatalf("AID mismatch: %s vs %s", states[0].AID, c.AID())
	}
}

// AID is stable across rotation; key_state_seq increments; current key changes.
func TestRotationStableAID(t *testing.T) {
	c, _ := Incept()
	aid0 := c.AID()
	firstKey := append([]byte(nil), c.cur.Public().(ed25519.PublicKey)...)
	if err := c.Rotate(1000); err != nil {
		t.Fatal(err)
	}
	if err := c.Rotate(2000); err != nil {
		t.Fatal(err)
	}
	if c.AID() != aid0 {
		t.Fatalf("AID changed across rotation: %s -> %s", aid0, c.AID())
	}
	states, err := Replay(c.KEL())
	if err != nil {
		t.Fatalf("replay after rotation: %v", err)
	}
	if len(states) != 3 {
		t.Fatalf("want 3 states, got %d", len(states))
	}
	for i, s := range states {
		if uint64(i) != s.KeyStateSeq {
			t.Fatalf("state %d has seq %d", i, s.KeyStateSeq)
		}
		if s.AID != aid0 {
			t.Fatalf("state %d AID drift", i)
		}
	}
	if bytesEqual(states[2].CurrentKeys[0], firstKey) {
		t.Fatal("current key did not change after rotation")
	}
}

// Object signed under current key verifies at its key_state_seq, before and after rotation.
// A KEL survives Marshal→Unmarshal byte round-trips: the rebuilt KEL re-derives the same AID +
// key-states and still verifies objects signed across a rotation (the basis for persisting/wiring
// member KELs, e.g. org-central restart resume).
func TestKELMarshalRoundTrip(t *testing.T) {
	c, _ := Incept()
	pre0 := []byte("object-at-seq-0")
	sig0, seq0 := c.Sign(pre0)
	if err := c.Rotate(5000); err != nil {
		t.Fatal(err)
	}
	pre1 := []byte("object-at-seq-1")
	sig1, seq1 := c.Sign(pre1)

	raw, err := MarshalKEL(c.KEL())
	if err != nil {
		t.Fatalf("marshal kel: %v", err)
	}
	kel2, err := UnmarshalKEL(raw)
	if err != nil {
		t.Fatalf("unmarshal kel: %v", err)
	}
	if len(kel2) != len(c.KEL()) {
		t.Fatalf("round-trip kel len %d, want %d", len(kel2), len(c.KEL()))
	}
	// the rebuilt KEL replays to the same AID + key-state count.
	states, err := Replay(kel2)
	if err != nil {
		t.Fatalf("replay round-tripped kel: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("round-tripped kel states = %d, want 2", len(states))
	}
	// and it still verifies objects signed by the controller, before and after rotation.
	if err := VerifyObject(kel2, c.AID(), seq0, 4999, pre0, sig0); err != nil {
		t.Fatalf("verify seq0 via round-tripped kel: %v", err)
	}
	if err := VerifyObject(kel2, c.AID(), seq1, 6000, pre1, sig1); err != nil {
		t.Fatalf("verify seq1 via round-tripped kel: %v", err)
	}
}

// A Controller survives Export→Restore (durable daemon identity): the restored controller has the
// same AID, can still Sign verifiably, and can Rotate — even after a prior rotation.
func TestControllerExportRestore(t *testing.T) {
	c, _ := Incept()
	if err := c.Rotate(1000); err != nil { // export a controller that has rotated once
		t.Fatal(err)
	}
	blob, err := c.Export()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	r, err := Restore(blob)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if r.AID() != c.AID() {
		t.Fatalf("restored AID %s != %s", r.AID(), c.AID())
	}
	// restored controller signs verifiably at the current key state.
	pre := []byte("after restore")
	sig, seq := r.Sign(pre)
	if err := VerifyObject(r.KEL(), r.AID(), seq, 2000, pre, sig); err != nil {
		t.Fatalf("restored controller sign/verify: %v", err)
	}
	// and it can still rotate (the pre-committed next key was restored).
	if err := r.Rotate(3000); err != nil {
		t.Fatalf("restored controller rotate: %v", err)
	}
	if r.CurrentSeq() != 2 {
		t.Fatalf("after restore+rotate seq = %d, want 2", r.CurrentSeq())
	}

	// corruption is rejected (not silently accepted).
	if _, err := Restore([]byte("garbage")); err == nil {
		t.Fatal("Restore must reject garbage")
	}
}

// Restore rejects a tampered NEXT seed (which would silently brick rotation) and a deactivated
// identity (which must not be reconstituted as a signer).
func TestRestoreRejectsCorruptNxtAndDeactivated(t *testing.T) {
	c, _ := Incept()
	blob, err := c.Export()
	if err != nil {
		t.Fatal(err)
	}
	var e controllerExport
	if err := coredet.Unmarshal(blob, &e); err != nil {
		t.Fatal(err)
	}
	e.NxtSeed[0] ^= 0xff // corrupt the pre-committed next key
	bad, err := coredet.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(bad); err == nil {
		t.Fatal("Restore must reject a corrupt NxtSeed (mismatched pre-rotation commitment)")
	}

	// a deactivated identity must not be restorable.
	c2, _ := Incept()
	if err := c2.Deactivate(1000); err != nil {
		t.Fatal(err)
	}
	db, err := c2.Export()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(db); err == nil {
		t.Fatal("Restore must reject a deactivated identity")
	}
}

func TestSignVerifyAcrossRotation(t *testing.T) {
	c, _ := Incept()
	pre0 := []byte("object-at-seq-0")
	sig0, seq0 := c.Sign(pre0)
	if err := VerifyObject(c.KEL(), c.AID(), seq0, 0, pre0, sig0); err != nil {
		t.Fatalf("verify at seq0: %v", err)
	}
	// rotate at t=5000.
	if err := c.Rotate(5000); err != nil {
		t.Fatal(err)
	}
	pre1 := []byte("object-at-seq-1")
	sig1, seq1 := c.Sign(pre1)
	if seq1 != 1 {
		t.Fatalf("want seq1=1, got %d", seq1)
	}
	if err := VerifyObject(c.KEL(), c.AID(), seq1, 6000, pre1, sig1); err != nil {
		t.Fatalf("verify at seq1: %v", err)
	}
	// grace (unknown time): the seq0 object still verifies against the seq0 key after rotation.
	if err := VerifyObject(c.KEL(), c.AID(), seq0, 0, pre0, sig0); err != nil {
		t.Fatalf("grace verify at seq0 (msgTime=0) after rotation: %v", err)
	}
	// time-aware grace: an object time-stamped BEFORE the rotation (t=4999 < 5000) is honored.
	if err := VerifyObject(c.KEL(), c.AID(), seq0, 4999, pre0, sig0); err != nil {
		t.Fatalf("time-aware grace verify at seq0 (msgTime<rot) : %v", err)
	}
	// time-aware revocation (M5.1.3): an object claiming the OLD seq0 key but time-stamped
	// AT/AFTER the rotation (t>=5000) is REVOKED_KEY — the key was already retired.
	err := VerifyObject(c.KEL(), c.AID(), seq0, 5000, pre0, sig0)
	if err == nil {
		t.Fatal("object at retired key after rotation time must be REVOKED_KEY")
	}
	if ve, ok := err.(*VErr); !ok || ve.Reason != "REVOKED_KEY" {
		t.Fatalf("want REVOKED_KEY, got %v", err)
	}
}

// Pre-rotation: a stolen CURRENT key cannot rotate to an attacker-chosen key, because the
// next key was pre-committed only as a digest (arch-03 §5.1 M5.1.1).
func TestPreRotationTheftPrevented(t *testing.T) {
	c, _ := Incept()
	last := c.kel[len(c.kel)-1]

	// Gate 1 — pre-rotation digest gate: attacker holds the current key (c.cur) but NOT the
	// pre-committed next key; they mint their own key and try to forge a rot signed by the
	// stolen current key. The revealed key fails the pre-committed-digest hash-match.
	_, attacker, _ := ed25519.GenerateKey(nil)
	forged := KeyEvent{
		AID:        c.aid,
		Seq:        1,
		Prev:       last.EventID,
		Type:       Rotation,
		Keys:       [][]byte{attacker.Public().(ed25519.PublicKey)},
		NextDigest: nextDigest(attacker.Public().(ed25519.PublicKey)),
		Threshold:  1,
	}
	pre, _ := preimage(forged)
	id, _ := anetcid.Sum(pre)
	sig := ed25519.Sign(c.cur, pre) // signed by the STOLEN current key
	tampered := append(append([]SignedEvent(nil), c.KEL()...), SignedEvent{Event: forged, Sig: sig, EventID: id})
	_, err := Replay(tampered)
	if err == nil {
		t.Fatal("pre-rotation theft NOT prevented: forged rotation accepted")
	}
	// The SPECIFIC rejection reason must be the pre-rotation digest mismatch (M5.1.1),
	// not merely "some error".
	if !contains(err.Error(), "pre-committed digest") {
		t.Fatalf("want pre-committed-digest rejection, got: %v", err)
	}

	// Gate 2 (positive control) — prior-key signing gate: this rot reveals the CORRECT
	// pre-committed next key (so gate 1 passes) but is signed by a NON-prior key. It MUST
	// still be rejected, proving the two gates are independent. We recover the correct next
	// key from the controller's own pre-commitment (c.nxt = the legitimately pre-committed K1).
	correctNext := c.nxt.Public().(ed25519.PublicKey)
	_, freshNextNxt, _ := ed25519.GenerateKey(nil)
	_, wrongSigner, _ := ed25519.GenerateKey(nil) // NOT the prior current key
	goodKeyRot := KeyEvent{
		AID:        c.aid,
		Seq:        1,
		Prev:       last.EventID,
		Type:       Rotation,
		Keys:       [][]byte{correctNext}, // matches prior next_digest → passes gate 1
		NextDigest: nextDigest(freshNextNxt.Public().(ed25519.PublicKey)),
		Threshold:  1,
	}
	pre2, _ := preimage(goodKeyRot)
	id2, _ := anetcid.Sum(pre2)
	sig2 := ed25519.Sign(wrongSigner, pre2) // signed by a key that is NOT the prior current key
	tampered2 := append(append([]SignedEvent(nil), c.KEL()...), SignedEvent{Event: goodKeyRot, Sig: sig2, EventID: id2})
	err2 := Replay2Err(tampered2)
	if err2 == nil {
		t.Fatal("rot revealing correct pre-committed key but signed by non-prior key was accepted")
	}
	if !contains(err2.Error(), "prior current key") {
		t.Fatalf("want prior-current-key rejection, got: %v", err2)
	}
}

// Replay2Err is a tiny helper returning only the error from Replay (test readability).
func Replay2Err(kel []SignedEvent) error { _, err := Replay(kel); return err }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Deactivation terminates the AID; an object at the terminal seq is REVOKED_KEY.
func TestDeactivation(t *testing.T) {
	c, _ := Incept()
	pre := []byte("final-object")
	sig, seq := c.Sign(pre)
	if err := c.Deactivate(8000); err != nil { // dip at t=8000
		t.Fatal(err)
	}
	states, err := Replay(c.KEL())
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if states[len(states)-1].Status != StatusDeactivated {
		t.Fatalf("want deactivated, got %s", states[len(states)-1].Status)
	}
	if err := c.Rotate(9000); err == nil {
		t.Fatal("rotate after deactivation should fail")
	}
	// object signed at seq0 still verifies (grace, unknown time); a fresh object claiming the
	// terminal (deactivation) seq is rejected by the revocation gate.
	if err := VerifyObject(c.KEL(), c.AID(), seq, 0, pre, sig); err != nil {
		t.Fatalf("grace verify of pre-deactivation object: %v", err)
	}
	// time-aware grace: the seq0 object time-stamped before the dip (t<8000) is honored.
	if err := VerifyObject(c.KEL(), c.AID(), seq, 7999, pre, sig); err != nil {
		t.Fatalf("time-aware grace of pre-deactivation object: %v", err)
	}
	// time-aware revocation: the seq0 key time-stamped at/after the dip is REVOKED_KEY.
	if err := VerifyObject(c.KEL(), c.AID(), seq, 8000, pre, sig); err == nil {
		t.Fatal("seq0 object at/after deactivation time should be REVOKED_KEY")
	}
	dipSeq := states[len(states)-1].KeyStateSeq
	dipPre := []byte("post-mortem")
	dipSig := ed25519.Sign(c.cur, dipPre)
	// object claiming the terminal (deactivation) seq is REVOKED_KEY, both unknown-time and timed.
	if err := VerifyObject(c.KEL(), c.AID(), dipSeq, 0, dipPre, dipSig); err == nil {
		t.Fatal("object at deactivated seq (msgTime=0) should be REVOKED_KEY")
	}
	if err := VerifyObject(c.KEL(), c.AID(), dipSeq, 9000, dipPre, dipSig); err == nil {
		t.Fatal("object at deactivated seq (timed) should be REVOKED_KEY")
	}
}

func TestTamperAndWrongAID(t *testing.T) {
	c, _ := Incept()
	pre := []byte("x")
	sig, seq := c.Sign(pre)
	if err := VerifyObject(c.KEL(), "did-not-this", seq, 0, pre, sig); err == nil {
		t.Fatal("wrong AID should fail")
	}
	if err := VerifyObject(c.KEL(), c.AID(), seq, 0, []byte("tampered"), sig); err == nil {
		t.Fatal("tampered preimage should fail")
	}
}

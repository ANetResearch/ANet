package daemon

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// rotatedElsewhere is d's identity rotated by someone holding kelSeed(2):
// d's identity file copied, a planned rot built and adopted there.
func rotatedElsewhere(t *testing.T, d *Daemon) []identity.SignedEvent {
	t.Helper()
	b, err := d.self.Export()
	if err != nil {
		t.Fatal(err)
	}
	thief, err := identity.Restore(b)
	if err != nil {
		t.Fatal(err)
	}
	kel := thief.Accepted()
	ts := max(uint64(time.Now().UnixMilli()), kel[len(kel)-1].Event.Timestamp+1)
	ev, err := thief.BuildRotation(ed25519.NewKeyFromSeed(kelSeed(2)), kelDigest(99),
		identityFactsOf(d.self).Witness, false, ts)
	if err != nil {
		t.Fatal(err)
	}
	if err := thief.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	return thief.Accepted()
}

// setHubRecord makes the fake hub hold kel for aid.
func setHubRecord(t *testing.T, fh *fakeHub, aid string, kel []identity.SignedEvent) {
	t.Helper()
	b, err := identity.MarshalKEL(kel)
	if err != nil {
		t.Fatal(err)
	}
	fh.mu.Lock()
	fh.agents[aid].kel = b
	fh.mu.Unlock()
}

// noCheckLimit lifts HubKELCheck for a test.
func noCheckLimit(t *testing.T) {
	t.Helper()
	prev := hubKELCheckEvery.set(0)
	t.Cleanup(func() { hubKELCheckEvery.set(prev) })
}

// M32 (KEL D-27 row e, D-29): the witness holds a rotation this node did not
// make; the poll it refuses makes the node look, and it stops: every task
// in flight fails with identity_stopped and nothing is sent about it, every
// interaction key goes, the epoch keys stay, and nothing more is sent or
// polled.
func TestStoppedClosesInteractions(t *testing.T) {
	noCheckLimit(t)
	_, req, prov := opsPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "in flight", nil)
	if err != nil {
		t.Fatal(err)
	}
	k, err := seal.GenerateIXKey(seal.SuiteX25519, id, prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	if err := req.ix.Update(func(tx *interactions.Tx) error {
		return tx.PutIXKey(interactions.IXKey{IX: id, PeerAID: prov.AID(), Suite: k.Suite, Pub: k.Pub, Priv: k.Private})
	}); err != nil && !errors.Is(err, interactions.ErrIXKeyExists) { // FS-7: Delegate already made one
		t.Fatal(err)
	}
	epochKeys := len(req.enc.keys)
	fh := fakeHubAt(t, req.config().HubURL)
	setHubRecord(t, fh, req.AID(), rotatedElsewhere(t, req))
	time.Sleep(5 * time.Millisecond) // the poll is signed after the rot
	if err := req.pollOnce(ctx); err == nil {
		t.Fatal("the hub took a poll signed with a key its record retired")
	}
	waitIdentity(t, req, IdentityStopped)
	waitUntil(t, "the tasks of the stopped identity are closed", func() bool { return interactionsCountOpen(t, req) == 0 })
	if st, _, reason := taskViewMap(t, req, id); st != string(a2ashape.TaskStateFailed) || reason != string(closeStopped) {
		t.Fatalf("the task after the stop: %s %s, want failed identity_stopped", st, reason)
	}
	if rows := allOutboxRows(t, req); len(rows) != 0 {
		t.Fatalf("%d rows queued after the stop: nothing is sent for a stopped identity", len(rows))
	}
	if rk, err := req.ix.OwnRK(id); err != nil || rk != nil {
		t.Fatalf("the interaction key survived the stop (%v)", err)
	}
	if len(req.enc.keys) != epochKeys || epochKeys == 0 {
		t.Fatalf("epoch keys: %d after the stop, %d before; they stay", len(req.enc.keys), epochKeys)
	}
	if f, err := identity.ParseIdentityFile(mustRead(t, req.layout.IdentityPath())); err != nil || f.State != identity.StateStopped {
		t.Fatalf("identity.kel after the stop: %+v (%v)", f, err)
	}
	if ref := req.sendGate(); ref == nil || ref.Key != reasonStopped {
		t.Fatalf("a send after the stop: %+v", ref)
	}
	if req.relayRunning() {
		t.Fatal("the relay loop runs for a stopped identity")
	}
}

// M39 (KEL D-27 row a, §1.6 item 1): a witness that returns a longer record
// with a broken signature, another identity's valid record, or bytes that
// are no record, stops nothing: the identity stays as it is, and its
// state says the hub's record does not verify.
func TestForgedLongerHubKELDoesNotStop(t *testing.T) {
	noCheckLimit(t)
	d, srv, fh := opsNode(t)
	waitIdentity(t, d, IdentityOK)
	longer := rotatedElsewhere(t, d)
	other, _, _ := opsNode(t)
	otherKEL, err := identity.MarshalKEL(other.self.Accepted())
	if err != nil {
		t.Fatal(err)
	}
	forgedKEL, err := identity.MarshalKEL(forged(longer))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		kel  []byte
	}{
		{"a longer record with a broken signature", forgedKEL},
		{"another identity's record", otherKEL},
		{"bytes that are no record", []byte("not a key history at all")},
	} {
		fh.mu.Lock()
		fh.agentKELOverride = map[string][]byte{d.AID(): c.kel}
		calls := fh.agentKELCalls
		fh.mu.Unlock()
		d.triggerHubKELCheck(srv.URL, triggerPoll401)
		waitUntil(t, c.name+": the record was read", func() bool {
			fh.mu.Lock()
			defer fh.mu.Unlock()
			return fh.agentKELCalls > calls
		})
		waitUntil(t, c.name+": the check ended", func() bool {
			d.idOps.mu.Lock()
			defer d.idOps.mu.Unlock()
			return !d.idOps.checking
		})
		if d.self.State() != identity.StateOK {
			t.Fatalf("%s: the identity is %s", c.name, d.self.State())
		}
		if s := d.IdentityStatus(); s.State != IdentityOK || s.Reason != reasonHubRecordInvalid {
			t.Fatalf("%s: /identity/status %s %s, want ok %s", c.name, s.State, s.Reason, reasonHubRecordInvalid)
		}
	}
	// A record that verifies again clears it.
	fh.mu.Lock()
	fh.agentKELOverride = nil
	fh.mu.Unlock()
	d.triggerHubKELCheck(srv.URL, triggerPoll401)
	waitUntil(t, "a good record clears the warning", func() bool { return !d.idOps.hubRecordInvalid.Load() })
}

// KEL D-27 (2026-10-10), H-22: a hub restored from a backup answers a poll
// with key_state_notice_error quarantine and holds a strict prefix of this
// node's KEL; the node pushes it the longer one at once.
func TestAQuarantineAnswerPushesTheLongerKEL(t *testing.T) {
	noCheckLimit(t)
	d, _, fh := opsNode(t)
	waitIdentity(t, d, IdentityOK)
	if code, out := controlCall(t, d.hIdentityRotate, "/identity/rotate", rotateBody(rotateScheduled, 2, 3, "")); code != http.StatusOK {
		t.Fatalf("rotation: %d %v", code, out)
	}
	waitIdentity(t, d, IdentityOK)
	// The rotation reaches the witness in the background; a push of it that lands after the backup's
	// record is set below would replace that record with the full KEL, and every check would then
	// find the hub up to date. So the record is set only once the witness holds the rotation.
	waitUntil(t, "the witness holds the rotation", func() bool {
		return len(d.self.WitnessesToPush()) == 0
	})
	full := d.self.Accepted()
	if len(full) < 3 {
		t.Fatalf("the KEL after the rotation has %d events, want at least 3", len(full))
	}
	setHubRecord(t, fh, d.AID(), full[:2]) // the backup's
	fh.mu.Lock()
	delete(fh.kelPushes, d.AID())
	fh.noticeErr = noticeErrQuarantine
	fh.mu.Unlock()
	// The quarantined hub still answers the poll it can verify (its old
	// key's grace), with the quarantine.
	// A check that the rotation set going drops a trigger that arrives while it runs (the next answer
	// brings another), so let it end first.
	waitUntil(t, "no check is running", func() bool {
		d.idOps.mu.Lock()
		defer d.idOps.mu.Unlock()
		return !d.idOps.checking
	})
	// A check that starts between the wait above and the notice (a trigger the rotation left behind)
	// drops the notice in the same way, so the notice is given again while no push has arrived, as
	// the hub's next answer would give it. The wait allows 30 s for a loaded CI runner.
	var noted time.Time
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		fh.mu.Lock()
		pushed := fh.kelPushes[d.AID()] > 0
		fh.mu.Unlock()
		if pushed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the longer KEL to be pushed")
		}
		d.idOps.mu.Lock()
		checking := d.idOps.checking
		d.idOps.mu.Unlock()
		if !checking && time.Since(noted) > 500*time.Millisecond {
			// A re-registration that the rotation set going also carries the full KEL and replaces
			// the backup's record (which ends a real quarantine as well); the record is put back so
			// that the check finds the witness behind.
			noted = time.Now()
			setHubRecord(t, fh, d.AID(), full[:2])
			d.noteHubNotice("", noticeErrQuarantine)
		}
	}
	fh.mu.Lock()
	held, _ := identity.UnmarshalKEL(fh.agents[d.AID()].kel)
	fh.mu.Unlock()
	if len(held) != len(full) {
		t.Fatalf("the hub holds %d events after the push, want %d", len(held), len(full))
	}
	if d.self.State() != identity.StateOK {
		t.Fatal("a hub behind the node stopped it")
	}
}

// KEL D-27 row d: a poll refused by a hub that keeps the identity only as a
// tombstone (registered false: the operator revoked its admission) sends
// the join back to its start, so the hub row says what to do.
func TestATombstoneSendsTheJoinBack(t *testing.T) {
	noCheckLimit(t)
	d, srv, fh := opsNode(t)
	waitIdentity(t, d, IdentityOK)
	fh.mu.Lock()
	fh.departedKEL[d.AID()] = fh.agents[d.AID()].kel
	delete(fh.agents, d.AID())
	fh.admission = fakeAdmitClosed
	fh.mu.Unlock()
	if err := d.pollOnce(context.Background()); err == nil {
		t.Fatal("the hub took a poll of an AID it removed")
	}
	waitJoinState(t, d, JoinPendingAdmission)
	if d.self.State() != identity.StateOK {
		t.Fatal("a tombstone stopped the identity")
	}
	_ = srv
}

// M26 (KEL D-36): a rotation the witness took but whose answer was lost is
// never signed again — the same bytes go until the witness answers; only
// the witness refusing those bytes for their time signs it at a new one.
func TestRestampOnlyAfterTheTimeIsRefused(t *testing.T) {
	d, _, fh := opsNode(t)
	waitIdentity(t, d, IdentityOK)
	prev := restampEvery.set(0)
	t.Cleanup(func() { restampEvery.set(prev) })
	fh.mu.Lock()
	fh.registerScript = []fakeRegisterAnswer{{drop: true}}
	fh.mu.Unlock()
	code, out := controlCall(t, d.hIdentityRotate, "/identity/rotate", rotateBody(rotateScheduled, 2, 3, ""))
	if code != http.StatusOK || out["state"] != IdentityPublishing {
		t.Fatalf("a rotation whose answer is lost: %d %v", code, out)
	}
	p, _ := d.self.Pending()
	first, _ := pendingEvent(p)
	retry := func() {
		d.idOps.mu.Lock()
		d.idOps.pubNextAt = time.Time{}
		d.idOps.mu.Unlock()
		d.publishNow(context.Background())
	}
	fh.mu.Lock()
	fh.registerScript = []fakeRegisterAnswer{{status: 503, body: map[string]any{"reason": "internal", "error": "x"}}}
	fh.mu.Unlock()
	retry()
	p, _ = d.self.Pending()
	if again, _ := pendingEvent(p); again == nil || again.EventID != first.EventID {
		t.Fatal("M26: the rotation was signed again after a lost answer (a hub may hold the first bytes)")
	}
	fh.mu.Lock()
	fh.registerScript = []fakeRegisterAnswer{{status: 400, body: map[string]any{"reason": hubKELTSOutOfWindow, "error": "x"}}}
	fh.mu.Unlock()
	retry()
	if st := d.identityState(); st.Reason != reasonClockSkew && st.State != IdentityOK {
		t.Fatalf("after the time was refused: %+v", st)
	}
	p, ok := d.self.Pending()
	if ok {
		if again, _ := pendingEvent(p); again == nil || again.EventID == first.EventID {
			t.Fatal("the rotation was not signed again after the witness refused its time")
		}
	}
	retry()
	waitIdentity(t, d, IdentityOK)
	if kel := d.self.Accepted(); kel[len(kel)-1].EventID == first.EventID {
		t.Fatal("the witness took the first bytes after refusing their time")
	}
}

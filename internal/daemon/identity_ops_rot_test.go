package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// opsPair is a provider that accepts everyone, joined like every test
// node, and a requester joined with the daemon's own hooks and the
// commitment of kelSeed(2), on one fake hub. Their relay loops are
// stopped; the tests poll.
func opsPair(t *testing.T) (*httptest.Server, *Daemon, *Daemon) {
	t.Helper()
	srv := newFakeHub(t)
	prov := newTestDaemon(t, srv.URL, true)
	if err := prov.RegisterWithHub(context.Background(), srv.URL, "Prov", nil); err != nil {
		t.Fatal(err)
	}
	req := opsDaemon(t)
	joinTestGroup(t, req, false)
	if out := opsJoin(t, req, srv.URL); out["state"] != JoinRegistered {
		t.Fatalf("join: %v", out)
	}
	req.stopRelayLoop()
	waitIdentity(t, req, IdentityOK)
	return srv, req, prov
}

// publishedKIDs are the kids of the key set d's ring publishes.
func publishedKIDs(t *testing.T, d *Daemon) kidSet {
	t.Helper()
	signed, _, _ := d.enc.published()
	kids, ok := setKIDs(signed)
	if !ok {
		t.Fatal("the ring's set does not decode")
	}
	return kids
}

// E-T6 with the real orchestration, and M24a–g (KEL D-41): an emergency
// rotation replaces every secret the data directory held — the rot carries
// key 12 and commits the new recovery seed; every epoch key is new and only
// the new ones are published; the mailbox key is one epoch up and both
// capability tables are empty (with them the direct addresses learned);
// every task in flight failed with key_compromise, its interaction key
// gone, a cancel queued to the peer of a delegation that left and nothing
// for one that never did; the control token and the local A2A token are
// new. The notices go out after the gate opens, sealed under the new key.
// A second emergency rotation right after (KEL D-16, D-31a) ends no task
// again and loses no notice.
func TestEmergencyRotationReplacesEverySecret(t *testing.T) {
	_, req, prov := opsPair(t)
	ctx := context.Background()
	handed, err := req.Delegate(ctx, prov.AID(), "handed over", nil)
	if err != nil {
		t.Fatal(err)
	}
	hub := req.config().HubURL
	setHubURL(req, deadHubURL)
	neverSent, err := req.Delegate(ctx, prov.AID(), "never sent", nil)
	if err != nil {
		t.Fatal(err)
	}
	setHubURL(req, hub)
	if rows := outboxRows(t, req, neverSent); len(rows) != 1 || rows[0].HandedAt != 0 {
		t.Fatalf("setup: %+v, want a delegation never handed over", rows)
	}
	// Each task holds an interaction key (FS-X1).
	for _, id := range []string{handed, neverSent} {
		k, err := seal.GenerateIXKey(seal.SuiteX25519, id, prov.AID())
		if err != nil {
			t.Fatal(err)
		}
		if err := req.ix.Update(func(tx *interactions.Tx) error {
			return tx.PutIXKey(interactions.IXKey{IX: id, PeerAID: prov.AID(), Suite: k.Suite, Pub: k.Pub, Priv: k.Private})
		}); err != nil && !errors.Is(err, interactions.ErrIXKeyExists) { // FS-7: sending already made one
			t.Fatal(err)
		}
	}
	// Capabilities both ways: one req holds (with the direct address a
	// grant may carry) and one it issued.
	heldCapFrom(t, prov, req)
	if g, _ := issueNow(t, req, prov.AID(), decContactPeer(req.nowMS()), req.nowMS()); g == nil {
		t.Fatal("setup: req issued nothing")
	}
	if held, issued, err := req.ix.CapsCounts(req.nowMS()); err != nil || held == 0 || issued == 0 {
		t.Fatalf("setup: %d held, %d issued capabilities (%v)", held, issued, err)
	}
	oldKIDs := publishedKIDs(t, req)
	oldMbox, _ := req.mbox.Current()
	if _, err := newControlToken(req.layout); err != nil {
		t.Fatal(err)
	}
	oldToken := mustRead(t, req.layout.ControlTokenPath())

	code, out := controlCall(t, req.hIdentityRotate, "/identity/rotate", rotateBody(rotateEmergency, 2, 3, ""))
	if code != http.StatusOK {
		t.Fatalf("emergency rotation: %d %v", code, out)
	}
	waitIdentity(t, req, IdentityOK)
	kel := req.self.Accepted()
	tip := kel[len(kel)-1].Event
	// (a) key 12, (b) the new commitment.
	if !tip.Emergency || tip.Seq != 2 {
		t.Fatalf("M24a: the rot %+v is not an emergency one", tip)
	}
	if string(tip.NextDigest) != string(kelDigest(3)) {
		t.Fatal("M24b: the rot does not commit the new recovery seed's key")
	}
	// (c) every epoch key new, only the new ones published.
	if !req.enc.regeneratedFor(2) {
		t.Fatal("M24c: the ring was not regenerated for the rot")
	}
	for kid := range publishedKIDs(t, req) {
		if _, old := oldKIDs[kid]; old {
			t.Fatal("M24c: an old epoch key is still published")
		}
	}
	if _, seq, _ := req.enc.published(); req.publishedKeySeq.Load() != seq {
		t.Fatal("M24c: the new key set was not published")
	}
	// (d), (f) the mailbox key and both capability tables.
	if m, _ := req.mbox.Current(); m.Epoch != oldMbox.Epoch+1 {
		t.Fatalf("M24d: mailbox key epoch %d, want %d", m.Epoch, oldMbox.Epoch+1)
	}
	if held, issued, err := req.ix.CapsCounts(req.nowMS()); err != nil || held != 0 || issued != 0 {
		t.Fatalf("M24d/f: %d held and %d issued capabilities after the rotation (%v)", held, issued, err)
	}
	// (e) the tasks.
	for _, id := range []string{handed, neverSent} {
		if st, _, reason := taskViewMap(t, req, id); st != string(a2ashape.TaskStateFailed) || reason != string(closeKeyCompromise) {
			t.Fatalf("M24e: task %s is %s %s, want failed key_compromise", id, st, reason)
		}
		if rk, err := req.ix.OwnRK(id); err != nil || rk != nil {
			t.Fatalf("M24e: task %s still has its interaction key (%v)", id, err)
		}
	}
	notices := outboxRows(t, req, handed)
	if len(notices) != 1 || notices[0].Type != seal.TypeMessage || notices[0].Kind != rowKindCancel {
		t.Fatalf("E-T6: queued for the handed task: %+v, want one cancel", notices)
	}
	if rows := outboxRows(t, req, neverSent); len(rows) != 0 {
		t.Fatalf("E-T6: queued for the task never sent: %+v, want nothing", rows)
	}

	// (g) the tokens.
	if string(mustRead(t, req.layout.ControlTokenPath())) == string(oldToken) {
		t.Fatal("M24g: the control token is the old one")
	}
	if got := rotations(t, req); len(got) != 2 || got[1] != rotationEmergency {
		t.Fatalf("anet.identity.rotated: %v", got)
	}
	// The notice goes out under the new key once the gate is open.
	if err := retryNow(t, req, notices[0].ID); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(t, req, handed); len(rows) != 1 || rows[0].HandedAt == 0 || rows[0].SealedKSS != 2 {
		t.Fatalf("E-T6: the cancel after the gate opened: %+v, want it handed over sealed under key seq 2", rows)
	}

	// A second emergency rotation right after: no task ends twice, no
	// notice goes.
	before := len(allOutboxRows(t, req))
	code, out = controlCall(t, req.hIdentityRotate, "/identity/rotate", rotateBody(rotateEmergency, 3, 4, ""))
	if code != http.StatusOK {
		t.Fatalf("the second emergency rotation: %d %v", code, out)
	}
	waitIdentity(t, req, IdentityOK)
	if n := len(allOutboxRows(t, req)); n < before-1 {
		t.Fatalf("the second rotation dropped notices: %d rows, had %d", n, before)
	}
	if got := rotations(t, req); len(got) != 3 || got[2] != rotationEmergency {
		t.Fatalf("anet.identity.rotated after two: %v", got)
	}
	if !req.enc.regeneratedFor(3) {
		t.Fatal("the second rotation did not regenerate the ring")
	}
}

// KEL D-40: an emergency rotation is built and closes the gate at once, its
// witness up or not; with the witness down it stays publishing, the key
// sets held, and is published when the hub is back.
func TestEmergencyRotationWaitsForItsWitness(t *testing.T) {
	d, srv, _ := opsNode(t)
	waitIdentity(t, d, IdentityOK)
	// The dry run says the witness is down, so the CLI can warn (L-3).
	url := srv.URL
	setHubURL(d, deadHubURL)
	d.join.mu.Lock()
	d.join.rec.Hub = deadHubURL
	d.join.mu.Unlock()
	code, out := controlCall(t, d.hIdentityRotate, "/identity/rotate", `{"mode":"emergency","dry_run":true}`)
	if code != http.StatusServiceUnavailable || out["reason"] != reasonWitnessUnreachable {
		t.Fatalf("dry run with the witness down: %d %v", code, out)
	}
	code, out = controlCall(t, d.hIdentityRotate, "/identity/rotate", rotateBody(rotateEmergency, 2, 3, ""))
	if code != http.StatusOK || out["state"] != IdentityPublishing {
		t.Fatalf("emergency rotation with the witness down: %d %v", code, out)
	}
	if g := d.identityGate(); g.Open {
		t.Fatal("the gate is open while the emergency rotation is unpublished")
	}
	if !d.keysHeld() {
		t.Fatal("key sets are not held while the emergency rotation is unpublished")
	}
	setHubURL(d, url)
	d.join.mu.Lock()
	d.join.rec.Hub = url
	d.join.mu.Unlock()
	if err := d.self.SetPendingHubURL(url); err != nil {
		t.Fatal(err)
	}
	d.idOps.mu.Lock()
	d.idOps.pubNextAt = time.Time{}
	d.idOps.mu.Unlock()
	d.kickIdentity()
	waitIdentity(t, d, IdentityOK)
	if d.keysHeld() {
		t.Fatal("key sets are still held after E5")
	}
}

// KEL D-32, M27: a hub switch registers the rot at the new hub (which checks
// its time) and pushes the key history to the old one, never registers it
// there; the node then lives at the new hub: config.json, hub_join.json and
// the witness name it, and the old hub, confirmed, leaves the list of
// unconfirmed witnesses.
func TestHubSwitch(t *testing.T) {
	d, a, fa := opsNode(t)
	waitIdentity(t, d, IdentityOK)
	b := newFakeHub(t)
	fb := fakeHubAt(t, b.URL)
	fa.mu.Lock()
	regsAtA := len(fa.registerKELs)
	fa.mu.Unlock()
	code, out := controlCall(t, d.hIdentityRotate, "/identity/rotate", rotateBody(rotateScheduled, 2, 3, `,"hub_url":"`+b.URL+`"`))
	if code != http.StatusOK {
		t.Fatalf("switch: %d %v", code, out)
	}
	d.stopRelayLoop()
	if w := identityFactsOf(d.self).Witness; w != fb.self.AID() {
		t.Fatalf("the witness after the switch is %q, want the new hub", w)
	}
	if d.config().HubURL != b.URL {
		t.Fatalf("config.json names %q after the switch", d.config().HubURL)
	}
	if r := d.joinSnapshot(); r == nil || r.Hub != b.URL || r.State != JoinRegistered || r.HubAID != fb.self.AID() {
		t.Fatalf("hub_join.json after the switch: %+v", r)
	}
	fa.mu.Lock()
	if len(fa.registerKELs) != regsAtA {
		t.Error("M27: the switch rot was registered at the old hub")
	}
	fa.mu.Unlock()
	fb.mu.Lock()
	gotAtB := len(fb.registerKELs) > 0
	fb.mu.Unlock()
	if !gotAtB {
		t.Fatal("M27: the new hub got no registration")
	}
	waitUntil(t, "the old hub is pushed the key history", func() bool {
		fa.mu.Lock()
		defer fa.mu.Unlock()
		return fa.kelPushes[d.AID()] > 0
	})
	waitUntil(t, "the old hub confirmed", func() bool { return len(d.oldWitnessUnacked()) == 0 })
	waitIdentity(t, d, IdentityOK)
	if got := rotations(t, d); len(got) != 2 || got[1] != rotationScheduled {
		t.Fatalf("anet.identity.rotated: %v", got)
	}
	push := d.self.WitnessesToPush()
	if len(push) != 1 || push[0].HubAID != fa.self.AID() || push[0].AckedSeq != 2 {
		t.Fatalf("witnesses_to_push after the switch: %+v", push)
	}
	_ = a
}

// KEL D-33, §4.3.1: a former witness that is down is not confirmed and is
// listed as such (info, nothing to do), and is pushed again on its own once
// it is back.
func TestOldHubUnackedUntilItIsBack(t *testing.T) {
	d, _, fa := opsNode(t)
	waitIdentity(t, d, IdentityOK)
	b := newFakeHub(t)
	fa.mu.Lock()
	fa.kelPushDown = true
	fa.mu.Unlock()
	if code, out := controlCall(t, d.hIdentityRotate, "/identity/rotate",
		rotateBody(rotateScheduled, 2, 3, `,"hub_url":"`+b.URL+`"`)); code != http.StatusOK {
		t.Fatalf("switch: %d %v", code, out)
	}
	d.stopRelayLoop()
	waitUntil(t, "a push to the old hub was tried", func() bool {
		return len(d.self.WitnessesToPush()) == 1 && d.self.WitnessesToPush()[0].NextTry > 0
	})
	if un := d.oldWitnessUnacked(); len(un) != 1 {
		t.Fatalf("old_witness_unacked with the old hub down: %+v", un)
	}
	if st := d.IdentityStatus(); len(st.OldWitnessUnacked) != 1 || st.State != IdentityOK {
		t.Fatalf("/identity/status: %+v", st)
	}
	fa.mu.Lock()
	fa.kelPushDown = false
	fa.mu.Unlock()
	push := d.self.WitnessesToPush()
	push[0].NextTry = 0
	if err := d.self.SetWitnessesToPush(push); err != nil {
		t.Fatal(err)
	}
	d.kickIdentity()
	waitUntil(t, "the old hub confirmed", func() bool { return len(d.oldWitnessUnacked()) == 0 })
}

// M37 (KEL D-32 step 5): OldHubDrain after the switch, the node leaves the
// old hub, once, and its join — the new hub's — stays as it was.
func TestOldHubLeaveKeepsCurrentJoin(t *testing.T) {
	d, _, fa := opsNode(t)
	waitIdentity(t, d, IdentityOK)
	b := newFakeHub(t)
	if code, out := controlCall(t, d.hIdentityRotate, "/identity/rotate",
		rotateBody(rotateScheduled, 2, 3, `,"hub_url":"`+b.URL+`"`)); code != http.StatusOK {
		t.Fatalf("switch: %d %v", code, out)
	}
	d.stopRelayLoop()
	waitUntil(t, "the old hub confirmed", func() bool { return len(d.oldWitnessUnacked()) == 0 })
	joinBefore := mustRead(t, d.layout.hubJoinPath())
	prevPoll := oldHubPollEvery.set(10 * time.Millisecond)
	t.Cleanup(func() { oldHubPollEvery.set(prevPoll) })
	later := d.nowMS() + OldHubDrain + 60_000
	d.setClock(func() uint64 { return later })
	d.idOps.mu.Lock()
	for _, dr := range d.idOps.drains {
		dr.nextPoll = time.Time{}
	}
	d.idOps.mu.Unlock()
	d.kickIdentity()
	waitUntil(t, "the old hub was left", func() bool {
		fa.mu.Lock()
		defer fa.mu.Unlock()
		_, still := fa.agents[d.AID()]
		return !still
	})
	if r := d.joinSnapshot(); r == nil || r.Hub != b.URL || r.State != JoinRegistered {
		t.Fatalf("M37: the join after leaving the old hub: %+v", r)
	}
	if after, err := os.ReadFile(d.layout.hubJoinPath()); err != nil || string(after) != string(joinBefore) {
		t.Fatalf("M37: hub_join.json changed when the old hub was left (%v)", err)
	}
	if d.config().HubURL != b.URL {
		t.Fatalf("M37: config.json names %q", d.config().HubURL)
	}
}

// KEL D-46: the deactivation ends every task (identity_deactivated,
// nothing sent), deletes every epoch private key, makes the dip the tip
// and pushes it to the witness until it confirms; the identity says
// deactivated, sends nothing and records nothing more. Its dry run counts
// the tasks it would end and changes nothing.
func TestDeactivation(t *testing.T) {
	_, req, prov := opsPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "in flight", nil)
	if err != nil {
		t.Fatal(err)
	}
	code, out := controlCall(t, req.hIdentityDeactivate, "/identity/deactivate", `{"dry_run":true}`)
	if code != http.StatusOK || out["closed_tasks"] != float64(1) || out["state"] != IdentityOK {
		t.Fatalf("dry run: %d %v", code, out)
	}
	code, out = controlCall(t, req.hIdentityDeactivate, "/identity/deactivate", `{"revealed_seed":"`+b64std(kelSeed(3))+`"}`)
	if code != http.StatusConflict || out["reason"] != reasonRecoveryMismatch {
		t.Fatalf("a deactivation with the wrong key: %d %v", code, out)
	}
	code, out = controlCall(t, req.hIdentityDeactivate, "/identity/deactivate", `{"revealed_seed":"`+b64std(kelSeed(2))+`"}`)
	if code != http.StatusOK || out["state"] != IdentityDeactivated || out["closed_tasks"] != float64(1) {
		t.Fatalf("deactivation: %d %v", code, out)
	}
	if st, _, reason := taskViewMap(t, req, id); st != string(a2ashape.TaskStateFailed) || reason != string(closeDeactivated) {
		t.Fatalf("the task after the deactivation: %s %s", st, reason)
	}
	if rows := allOutboxRows(t, req); len(rows) != 0 {
		t.Fatalf("%d rows queued after the deactivation, want none", len(rows))
	}
	if len(req.enc.keys) != 0 {
		t.Fatal("epoch private keys survived the deactivation")
	}
	kel := req.self.Accepted()
	if kel[len(kel)-1].Event.Type != identity.Deactivation {
		t.Fatal("the dip is not the tip")
	}
	fh := fakeHubAt(t, req.config().HubURL)
	waitUntil(t, "the witness confirmed the dip", func() bool {
		fh.mu.Lock()
		defer fh.mu.Unlock()
		return fh.kelPushes[req.AID()] > 0 && len(req.self.WitnessesToPush()) == 0
	})
	if code, out := controlCall(t, req.hIdentityRotate, "/identity/rotate", rotateBody(rotateScheduled, 3, 4, "")); code != http.StatusGone ||
		out["reason"] != reasonDeactivated {
		t.Fatalf("a rotation of a deactivated identity: %d %v", code, out)
	}
	if _, err := req.ledger.Append(EvDelegationSent, map[string]any{"n": 1}); err == nil {
		t.Fatal("a deactivated identity recorded evidence")
	}
}

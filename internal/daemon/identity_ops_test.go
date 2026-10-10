package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// b64std is standard base64, as the control plane writes binary values.
func b64std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// opsDaemon is a test node with the daemon's own join hooks (no test
// stand-in) and its real outbound gate, joined to nothing yet.
func opsDaemon(t *testing.T) *Daemon {
	t.Helper()
	return buildTestDaemonAs(t, "", nil, true)
}

// opsJoin joins d to the fake hub at hubURL through /hub-register with the
// commitment of kelSeed(2), as `anet hub-register` does (KEL D-10).
func opsJoin(t *testing.T, d *Daemon, hubURL string) map[string]any {
	t.Helper()
	code, out := controlCall(t, d.hHubRegister, "/hub-register",
		hubRegisterBody(hubURL, `,"next_digest":"`+b64std(kelDigest(2))+`"`))
	if code != http.StatusOK {
		t.Fatalf("hub-register %s: %d %v", hubURL, code, out)
	}
	return out
}

// opsNode is an opsDaemon joined to a fresh fake hub, which witnesses it.
func opsNode(t *testing.T) (*Daemon, *httptest.Server, *fakeHub) {
	t.Helper()
	srv := newFakeHub(t)
	d := opsDaemon(t)
	if out := opsJoin(t, d, srv.URL); out["state"] != JoinRegistered {
		t.Fatalf("join: %v", out)
	}
	d.stopRelayLoop()
	return d, srv, fakeHubAt(t, srv.URL)
}

// rotateBody is an /identity/rotate body revealing kelSeed(revealed) and
// committing kelDigest(next).
func rotateBody(mode string, revealed, next int, extra string) string {
	return `{"mode":"` + mode + `","revealed_seed":"` + b64std(kelSeed(revealed)) + `","next_digest":"` +
		b64std(kelDigest(next)) + `"` + extra + `}`
}

// waitIdentity waits until d's identity state is want.
func waitIdentity(t *testing.T, d *Daemon, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for d.identityState().State != want {
		if time.Now().After(deadline) {
			t.Fatalf("identity state %+v, want %s", d.identityState(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// rotations are the anet.identity.rotated records on d's chain: seq and
// mode, in order.
func rotations(t *testing.T, d *Daemon) []string {
	t.Helper()
	_, recs := d.ledger.Evidence(EvidenceQuery{EventType: evtypes.IdentityRotated, Limit: 100})
	var out []string
	for _, r := range recs {
		if p, ok := evtypes.PayloadOf(evtypes.IdentityRotated); ok {
			if why := p.Check(r.Payload); why != "" {
				t.Fatalf("anet.identity.rotated %v: %s", r.Payload, why)
			}
		}
		out = append(out, r.Payload["mode"].(string))
	}
	return out
}

// KEL D-10–D-13: a first join with a commitment designates the hub the
// identity's witness in the rot the join builds, signed with the hot key
// the icp committed, registered with the key set signed under it; the hub
// accepting it makes it the tip, witness_rot goes, the event is on the
// chain, and the gate opens on the hub's notice.
func TestFirstJoinDesignatesTheWitness(t *testing.T) {
	d, srv, fh := opsNode(t)
	kel := d.self.Accepted()
	if len(kel) != 2 {
		t.Fatalf("the KEL after the join has %d events, want the icp and the designation", len(kel))
	}
	rot := kel[1].Event
	if rot.Type != identity.Rotation || len(rot.Witnesses) != 1 || rot.Witnesses[0] != fh.self.AID() ||
		string(rot.NextDigest) != string(kelDigest(2)) || rot.Emergency {
		t.Fatalf("the designation: %+v", rot)
	}
	if _, pending := d.self.Pending(); pending {
		t.Fatal("the designation is still pending after the hub accepted it")
	}
	if r := d.joinSnapshot(); r == nil || r.WitnessRot != nil || r.State != JoinRegistered {
		t.Fatalf("the join record after the join: %+v", r)
	}
	if got := rotations(t, d); len(got) != 1 || got[0] != rotationWitness {
		t.Fatalf("anet.identity.rotated after the join: %v", got)
	}
	if fh.registerKeys[d.AID()] != hubapi.KeysStatusOK {
		t.Fatalf("the hub took the key set as %q: it was not signed under the designation's key", fh.registerKeys[d.AID()])
	}
	waitIdentity(t, d, IdentityOK)
	if s := d.IdentityStatus(); s.LastRotation == nil || s.LastRotation.Mode != rotationWitness || s.Witness == nil ||
		s.Witness.URL != srv.URL {
		t.Fatalf("/identity/status after the join: %+v", s)
	}
}

// M43 (KEL D-11), the daemon's part of TestNoTTYFirstJoinCompletes:
// a join without a terminal (unprotected) at a hub that admits by AID waits
// in pending_admission without building a rot, finishes on its own once the
// operator admits the AID, and the hub gets exactly one rot, the
// designation; the identity says it has no recovery passphrase.
func TestNoTTYFirstJoinCompletes(t *testing.T) {
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	fh.admission = fakeAdmitClosed
	fh.mu.Unlock()
	d := opsDaemon(t)
	code, out := controlCall(t, d.hHubRegister, "/hub-register",
		hubRegisterBody(srv.URL, `,"next_digest":"`+b64std(kelDigest(2))+`","unprotected":true`))
	if code != http.StatusOK || out["state"] != JoinPendingAdmission || out["reason"] != reasonPending {
		t.Fatalf("join before admission: %d %v", code, out)
	}
	if _, pending := d.self.Pending(); pending || len(d.self.KEL()) != 1 {
		t.Fatal("a rot was built before the hub admitted the AID (KEL D-12)")
	}
	if rec, _ := out["recovery"].(map[string]any); rec == nil || rec["state"] != RecoveryUnprotected {
		t.Fatalf("the answer does not say the identity is unprotected: %v", out)
	}
	fh.mu.Lock()
	fh.admitted[d.AID()] = true
	fh.mu.Unlock()
	d.kickJoin()
	waitJoinState(t, d, JoinRegistered)
	waitIdentity(t, d, IdentityOK)
	fh.mu.Lock()
	kels := append([][]byte(nil), fh.registerKELs...)
	fh.mu.Unlock()
	rots := map[string]bool{}
	for _, b := range kels {
		k, err := identity.UnmarshalKEL(b)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range k[1:] {
			rots[e.EventID] = true
		}
	}
	if len(rots) != 1 {
		t.Fatalf("the hub got %d distinct rots, want the one designation", len(rots))
	}
	if st := d.recoveryState(); st.State != RecoveryUnprotected {
		t.Fatalf("recovery after the join: %+v", st)
	}
}

// M35 (KEL D-14): once the designation is built, hub-register of another
// hub, or with another commitment, is rotation_pending and changes
// nothing; the same hub and commitment again are fine. Before it is built,
// a new commitment replaces the one in witness_rot.
func TestPendingWitnessEventBlocksRetarget(t *testing.T) {
	srv := newFakeHub(t)
	other := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	d := opsDaemon(t)
	// Not built yet: the hub has not admitted the AID.
	fh.mu.Lock()
	fh.admission = fakeAdmitClosed
	fh.mu.Unlock()
	opsJoin(t, d, srv.URL)
	code, out := controlCall(t, d.hHubRegister, "/hub-register",
		hubRegisterBody(srv.URL, `,"next_digest":"`+b64std(kelDigest(3))+`"`))
	if code != http.StatusOK || string(d.joinSnapshot().WitnessRot.NextDigest) != string(kelDigest(3)) {
		t.Fatalf("a new commitment before the designation is built: %d %v", code, out)
	}
	// Built: admitted, but the registration does not get through.
	fh.mu.Lock()
	fh.admission = ""
	fh.registerScript = []fakeRegisterAnswer{{status: 200, body: map[string]any{"status": "admitted"}},
		{status: 503, body: map[string]any{"reason": "internal", "error": "down"}}}
	fh.mu.Unlock()
	opsJoinWith(t, d, srv.URL, 3)
	p, ok := d.self.Pending()
	if !ok || p.Kind != identity.PendingWitness {
		t.Fatalf("no designation pending after an admitted join whose registration failed")
	}
	before := mustRead(t, d.layout.IdentityPath())
	for _, c := range []struct{ name, body string }{
		{"another hub", hubRegisterBody(other.URL, `,"next_digest":"`+b64std(kelDigest(3))+`"`)},
		{"another commitment", hubRegisterBody(srv.URL, `,"next_digest":"`+b64std(kelDigest(4))+`"`)},
		{"another commitment, unprotected", hubRegisterBody(srv.URL, `,"next_digest":"`+b64std(kelDigest(4))+`","unprotected":true`)},
	} {
		code, out := controlCall(t, d.hHubRegister, "/hub-register", c.body)
		if code != http.StatusConflict || out["reason"] != reasonRotationPending {
			t.Fatalf("%s while the designation is pending: %d %v", c.name, code, out)
		}
		if after := mustRead(t, d.layout.IdentityPath()); string(after) != string(before) {
			t.Fatalf("%s changed identity.kel", c.name)
		}
		if r := d.joinSnapshot(); r.Hub != srv.URL || string(r.WitnessRot.NextDigest) != string(kelDigest(3)) {
			t.Fatalf("%s changed the join record: %+v", c.name, r)
		}
	}
	// The same hub and commitment: the designation built is registered.
	opsJoinWith(t, d, srv.URL, 3)
	waitJoinState(t, d, JoinRegistered)
	if kel := d.self.Accepted(); len(kel) != 2 || string(kel[1].Event.NextDigest) != string(kelDigest(3)) {
		t.Fatal("the designation registered is not the one built")
	}
}

// opsJoinWith is opsJoin committing kelDigest(n).
func opsJoinWith(t *testing.T, d *Daemon, hubURL string, n int) (int, map[string]any) {
	t.Helper()
	return controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(hubURL, `,"next_digest":"`+b64std(kelDigest(n))+`"`))
}

// mustRead is a file's bytes.
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// KEL D-14a: a hub that refuses the designation for good never took it, so
// it is dropped — the join can go to another hub — while a registration
// that got no answer keeps it: the hub may hold it.
func TestRefusedWitnessEventIsDiscarded(t *testing.T) {
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	d := opsDaemon(t)
	fh.mu.Lock()
	fh.registerScript = []fakeRegisterAnswer{{status: 200, body: map[string]any{"status": "admitted"}},
		{status: 400, body: map[string]any{"reason": "witness_not_this_hub", "error": "no"}}}
	fh.mu.Unlock()
	code, out := opsJoinWith(t, d, srv.URL, 2)
	if code != http.StatusOK || out["state"] != JoinRefused || out["reason"] != reasonSwitch {
		t.Fatalf("a refused designation: %d %v", code, out)
	}
	if _, pending := d.self.Pending(); pending || len(d.self.KEL()) != 1 {
		t.Fatal("the designation the hub refused for good is still pending")
	}
	if r := d.joinSnapshot(); r.WitnessRot == nil {
		t.Fatal("witness_rot went with the dropped designation")
	}
	if n := d.self.CurrentSeq(); n != 0 {
		t.Fatalf("the node signs under seq %d after the drop, want the icp's", n)
	}
	// Another hub takes it: a new designation, naming that hub.
	other := newFakeHub(t)
	if code, out := opsJoinWith(t, d, other.URL, 2); code != http.StatusOK || out["state"] != JoinRegistered {
		t.Fatalf("the join of another hub after the drop: %d %v", code, out)
	}
	if w := identityFactsOf(d.self).Witness; w != hubAIDOf(other.URL) {
		t.Fatalf("the witness is %q, want the other hub", w)
	}

	// No answer: the designation stays, and the same bytes go again.
	srv2 := newFakeHub(t)
	fh2 := fakeHubAt(t, srv2.URL)
	e := opsDaemon(t)
	fh2.mu.Lock()
	fh2.registerScript = []fakeRegisterAnswer{{status: 200, body: map[string]any{"status": "admitted"}}, {drop: true}}
	fh2.mu.Unlock()
	opsJoinWith(t, e, srv2.URL, 2)
	p, ok := e.self.Pending()
	if !ok || p.Kind != identity.PendingWitness {
		t.Fatal("a designation whose registration got no answer was dropped")
	}
	e.kickJoin()
	waitJoinState(t, e, JoinRegistered)
	if kel := e.self.Accepted(); len(kel) != 2 {
		t.Fatal("the designation was not adopted on the retry")
	}
	if got, _ := pendingEvent(p); got == nil || e.self.Accepted()[1].EventID != got.EventID {
		t.Fatal("the retry registered another designation than the one built (KEL D-14)")
	}
}

// KEL D-30, P-1, P-5, M21: a planned rotation reveals the committed key and
// commits the next one; the witness accepts it, the rot is the tip, the
// key set and the card are signed under it, the chain records it. A seed
// that is not the committed key is recovery_mismatch and changes nothing;
// a dry run changes nothing; a second change at the same time is
// rotation_pending.
func TestScheduledRotation(t *testing.T) {
	d, _, fh := opsNode(t)
	waitIdentity(t, d, IdentityOK)
	before := mustRead(t, d.layout.IdentityPath())
	// M21: forged material.
	for _, body := range []string{
		rotateBody(rotateScheduled, 3, 4, ""),
		`{"mode":"scheduled","revealed_seed":"` + b64std(make([]byte, 32)) + `","next_digest":"` + b64std(kelDigest(3)) + `"}`,
	} {
		code, out := controlCall(t, d.hIdentityRotate, "/identity/rotate", body)
		if code != http.StatusConflict || out["reason"] != reasonRecoveryMismatch {
			t.Fatalf("a rotation with a key the KEL did not commit: %d %v", code, out)
		}
		if after := mustRead(t, d.layout.IdentityPath()); string(after) != string(before) {
			t.Fatal("a refused rotation changed identity.kel")
		}
		if d.identityState().State != IdentityOK {
			t.Fatalf("a refused rotation changed the state: %+v", d.identityState())
		}
	}
	code, out := controlCall(t, d.hIdentityRotate, "/identity/rotate", `{"mode":"scheduled","dry_run":true}`)
	if code != http.StatusOK || out["state"] != IdentityOK {
		t.Fatalf("dry run: %d %v", code, out)
	}
	if after := mustRead(t, d.layout.IdentityPath()); string(after) != string(before) {
		t.Fatal("a dry run changed identity.kel")
	}
	// P-5.
	d.idOps.change.Lock()
	code, out = controlCall(t, d.hIdentityRotate, "/identity/rotate", rotateBody(rotateScheduled, 2, 3, ""))
	d.idOps.change.Unlock()
	if code != http.StatusConflict || out["reason"] != reasonRotationPending {
		t.Fatalf("a second change at the same time: %d %v", code, out)
	}
	code, out = controlCall(t, d.hIdentityRotate, "/identity/rotate", rotateBody(rotateScheduled, 2, 3, ""))
	if code != http.StatusOK || out["key_seq"] != float64(2) {
		t.Fatalf("the rotation: %d %v", code, out)
	}
	kel := d.self.Accepted()
	if len(kel) != 3 || kel[2].Event.Emergency || string(kel[2].Event.NextDigest) != string(kelDigest(3)) {
		t.Fatalf("the KEL after the rotation: %d events", len(kel))
	}
	waitIdentity(t, d, IdentityOK)
	if got := rotations(t, d); len(got) != 2 || got[1] != rotationScheduled {
		t.Fatalf("anet.identity.rotated: %v", got)
	}
	if seq, _ := identity.SeqOf(d.self.Snapshot()); seq != 2 {
		t.Fatalf("the node signs under seq %d, want 2", seq)
	}
	fh.mu.Lock()
	keys := fh.agents[d.AID()].keyset
	fh.mu.Unlock()
	if ks := keySetSignedSeq(t, keys); ks != 2 {
		t.Fatalf("the hub holds a key set signed under seq %d, want 2 (FS-K17)", ks)
	}
	// The witness gone: a planned rotation needs it (D-30 step 2).
	setHubURL(d, deadHubURL)
	d.join.mu.Lock()
	d.join.rec.Hub = deadHubURL
	d.join.mu.Unlock()
	code, out = controlCall(t, d.hIdentityRotate, "/identity/rotate", rotateBody(rotateScheduled, 3, 4, ""))
	if code != http.StatusServiceUnavailable || out["reason"] != reasonWitnessUnreachable {
		t.Fatalf("a planned rotation with the witness gone: %d %v", code, out)
	}
}

// keySetSignedSeq is the key_state_seq a key set encoding was signed under.
func keySetSignedSeq(t *testing.T, signed []byte) uint64 {
	t.Helper()
	s, err := seal.UnmarshalSignedEncKeySet(signed)
	if err != nil {
		t.Fatal(err)
	}
	return s.KeyStateSeq
}

// KEL D-1 second part: once the identity is stopped or deactivated the
// direct transports close and a direct delivery is refused (the sender's
// transport hears a nack); in no_hub, publishing and notice_stale it is
// taken as ever.
func TestStoppedClosesP2P(t *testing.T) {
	for _, end := range []string{identity.StateStopped, identity.StateDeactivated} {
		d := opsDaemon(t)
		tr := &closableTransport{}
		d.RegisterTransport(tr)
		if d.identityRefusesInbound() {
			t.Fatal("no_hub refuses direct deliveries")
		}
		if err := (inbound{d}).receive(context.Background(), []byte("not an envelope")); err == errIdentityGone {
			t.Fatal("no_hub refuses direct deliveries")
		}
		switch end {
		case identity.StateStopped:
			d.enterStopped("test")
		default:
			if err := d.self.SetState(identity.StateDeactivated); err != nil {
				t.Fatal(err)
			}
			d.closeP2P()
		}
		if tr.closed.Load() != 1 {
			t.Fatalf("%s: the transport was closed %d times, want once", end, tr.closed.Load())
		}
		if d.p2pPath(context.Background(), "x", d.nowMS()) != nil {
			t.Fatalf("%s: a direct path is still offered", end)
		}
		if err := (inbound{d}).receive(context.Background(), []byte("x")); err != errIdentityGone {
			t.Fatalf("%s: a direct delivery was not refused: %v", end, err)
		}
	}
}

// closableTransport is a transport that records Close.
type closableTransport struct{ closed atomic.Int32 }

func (*closableTransport) Name() string                           { return "test-p2p" }
func (*closableTransport) Reachable(context.Context, string) bool { return true }
func (*closableTransport) Send(context.Context, string, []byte) ([]byte, bool, error) {
	return nil, false, nil
}
func (c *closableTransport) Close() error { c.closed.Add(1); return nil }

// KEL D-50: a notice about a lower seq of the same AID never replaces the
// one held, whatever its time.
func TestAnOlderNoticeNeverReplacesANewerOne(t *testing.T) {
	var g identityGateInputs
	newer := &keyStateNotice{Body: identity.NoticeBody{AID: "a", Seq: 3, Head: "h3", Witness: "w", ObservedAt: 1000}}
	older := &keyStateNotice{Body: identity.NoticeBody{AID: "a", Seq: 2, Head: "h2", Witness: "w", ObservedAt: 5000}}
	if !g.noteNotice(newer, "") {
		t.Fatal("the first notice was not taken")
	}
	if g.noteNotice(older, "") {
		t.Fatal("a notice about seq 2 replaced the one about seq 3")
	}
	if n, _, _ := g.read(); n != newer {
		t.Fatal("the notice held is not the newer one")
	}
	later := &keyStateNotice{Body: identity.NoticeBody{AID: "a", Seq: 4, Head: "h4", Witness: "w2", ObservedAt: 10}}
	if !g.noteNotice(later, "") {
		t.Fatal("a notice about a later tip, from a hub whose clock is behind, was not taken")
	}
}

// interactionsCountOpen is how many of d's tasks are open.
func interactionsCountOpen(t *testing.T, d *Daemon) int {
	t.Helper()
	n, err := d.ix.Count(interactions.ListFilter{Active: true})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// refusalKey is the reason a /status-like answer carries.
func refusalKey(out map[string]any) string {
	s, _ := out["reason"].(string)
	return s
}

var _ = a2ashape.Lang
var _ = strings.Contains
var _ = ed25519.SeedSize

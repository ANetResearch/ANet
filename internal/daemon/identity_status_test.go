package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// What the node says about its identity (KEL §2.8, §4.3.1, D-1, D-3, D-4,
// D-15) and what it refuses for it.

// identityStatusOf is POST /identity/status of d, decoded.
func identityStatusOf(t *testing.T, d *Daemon) IdentityStatus {
	t.Helper()
	rec := httptest.NewRecorder()
	d.hIdentityStatus(rec, httptest.NewRequest(http.MethodPost, "/identity/status", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("/identity/status: %d %s", rec.Code, rec.Body)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"last_rotation", "witness", "notice", "pending", "commitment"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("/identity/status has no %s (a must member, null when empty): %s", k, rec.Body)
		}
	}
	if l, ok := raw["old_witness_unacked"].([]any); !ok || l == nil {
		t.Fatalf("old_witness_unacked is not an array: %s", rec.Body)
	}
	var s IdentityStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// sameSentence checks that a status carries key's sentence and command.
func checkSaid(t *testing.T, step string, s IdentityStatus, state, key string, args a2ashape.Args) {
	t.Helper()
	lang := a2ashape.Lang()
	if s.State != state || s.Reason != key || s.Message != a2ashape.Sentence(key, lang, args) || s.Next != a2ashape.Next(key, lang, args) {
		t.Fatalf("%s: {%s %s %q %q}, want {%s %s %q %q}", step, s.State, s.Reason, s.Message, s.Next,
			state, key, a2ashape.Sentence(key, lang, args), a2ashape.Next(key, lang, args))
	}
}

// /identity/status in each state of the table (KEL §2.8, §4.3.1), with the
// sentence and the facts beside it.
func TestIdentityStatusSaysEachState(t *testing.T) {
	// No hub: the join's sentence, nothing about keys yet.
	fresh, _ := joinDaemon(t)
	s := identityStatusOf(t, fresh)
	checkSaid(t, "no hub", s, IdentityNoHub, "no_hub_yet", nil)
	if s.AID != fresh.AID() || s.KeySeq != 0 || s.LastRotation != nil || s.Witness != nil || s.Notice != nil ||
		s.Pending != nil || s.Recovery.State != RecoveryNone || s.Recovery.Message != "" ||
		!bytes.Equal(s.Commitment, fresh.self.KEL()[0].Event.NextDigest) {
		t.Fatalf("no hub: %+v", s)
	}
	// Waiting for admission: still no hub, said in the hub row's own words,
	// its hub and the admit line included (ux UX-C9).
	asrv := newFakeHub(t)
	afh := fakeHubAt(t, asrv.URL)
	afh.mu.Lock()
	afh.admission = fakeAdmitClosed
	afh.mu.Unlock()
	waiting, _ := joinDaemon(t)
	if code, out := controlCall(t, waiting.hHubRegister, "/hub-register", hubRegisterBody(asrv.URL, `,"next_digest":"`+testDigest+`"`)); code != 200 ||
		out["state"] != JoinPendingAdmission {
		t.Fatalf("join a closed hub: %d %v", code, out)
	}
	s = identityStatusOf(t, waiting)
	if hv := waiting.HubView(); s.State != IdentityNoHub || s.Reason != reasonPending || s.Reason != hv.Reason ||
		s.Message != hv.Message || s.Next != hv.Next || !strings.Contains(s.Message, asrv.URL) {
		t.Fatalf("waiting for admission: %+v, the hub row says {%s %q %q}", s, hv.Reason, hv.Message, hv.Next)
	}
	// Refused: the hub's code is part of the hub row's sentence, and so of
	// the identity's.
	waiting.join.mu.Lock()
	refused := *waiting.join.rec
	refused.State, refused.Reason, refused.HubCode, refused.NextAttemptAt = JoinRefused, reasonHubRefused, "kel_invalid", 0
	waiting.join.rec = &refused
	waiting.join.mu.Unlock()
	s = identityStatusOf(t, waiting)
	if hv := waiting.HubView(); s.State != IdentityNoHub || s.Reason != reasonHubRefused || s.Message != hv.Message ||
		s.Next != hv.Next || !strings.Contains(s.Message, "kel_invalid") {
		t.Fatalf("refused: %+v, the hub row says {%s %q %q}", s, hv.Reason, hv.Message, hv.Next)
	}

	d, srv, fh := witnessedNode(t, true)
	now := uint64(time.Now().UnixMilli())
	d.setClock(func() uint64 { return now })
	n, _, _ := d.idGate.read()
	s = identityStatusOf(t, d)
	rot := d.self.Accepted()[1].Event
	if s.State != IdentityOK || s.Reason != reasonIdentityOK || s.KeySeq != 1 || !bytes.Equal(s.Commitment, rot.NextDigest) ||
		s.LastRotation == nil || s.LastRotation.Mode != rotationWitness || s.LastRotation.At != rfc3339(rot.Timestamp) ||
		s.Witness == nil || *s.Witness != (IdentityWitness{URL: srv.URL, AID: fh.self.AID()}) ||
		s.Notice == nil || s.Notice.ObservedAt != rfc3339(n.Body.ObservedAt) || s.Pending != nil ||
		len(s.OldWitnessUnacked) != 0 || s.Recovery.State != RecoveryProtected || s.Recovery.Message != "" {
		t.Fatalf("witnessed and confirmed: %+v", s)
	}
	checkSaid(t, "ok", s, IdentityOK, reasonIdentityOK, a2ashape.Args{"name": IdentityNameForDir(d.layout.Root),
		"aid": d.AID(), "hub": srv.URL, "ago": time.Duration(now-n.Body.ObservedAt) * time.Millisecond,
		"now": time.UnixMilli(int64(now))})

	now = n.Body.ObservedAt + NoticeResealAge + minuteMS
	s = identityStatusOf(t, d)
	checkSaid(t, "aging", s, IdentityOK, reasonConfirmAging, a2ashape.Args{"dur": time.Duration(NoticeResealAge+minuteMS) * time.Millisecond})
	if s.Notice == nil || s.Notice.AgeS != (NoticeResealAge+minuteMS)/1000 {
		t.Fatalf("aging: notice %+v", s.Notice)
	}
	now = n.Body.ObservedAt + NoticeStopLine + minuteMS
	checkSaid(t, "stale", identityStatusOf(t, d), IdentityNoticeStale, reasonNoticeStale,
		a2ashape.Args{"dur": time.Duration(NoticeStopLine+minuteMS) * time.Millisecond})

	// A planned rotation being published.
	next, err := identitytest.NextKey(d.self)
	if err != nil {
		t.Fatal(err)
	}
	commit := identitytest.NewNext()
	if _, err := d.self.BuildRotation(next, commit, fh.self.AID(), false, now); err != nil {
		t.Fatal(err)
	}
	s = identityStatusOf(t, d)
	checkSaid(t, "publishing", s, IdentityPublishing, reasonRotationPending, nil)
	if s.KeySeq != 2 || !bytes.Equal(s.Commitment, commit) || s.Pending == nil || s.Pending.Kind != identity.PendingRot ||
		s.LastRotation.Mode != rotationWitness {
		t.Fatalf("publishing: %+v", s)
	}
	d.idGate.notePublishError(hubKELTSOutOfWindow)
	checkSaid(t, "the witness refused its time", identityStatusOf(t, d), IdentityPublishing, reasonClockSkew, nil)

	// Accepted, the rot is the tip, and the notice held states the one
	// before it: nothing confirms the key signed under now, so there is no
	// notice to show, and the wait counts from the rot.
	if err := (witnessJoinHooks{d}).Adopt(context.Background(), RegisterResult{}); err != nil {
		t.Fatal(err)
	}
	d.idGate.notePublishError("")
	s = identityStatusOf(t, d)
	checkSaid(t, "accepted, not yet confirmed", s, IdentityNoticeStale, reasonNoticeStale, a2ashape.Args{"dur": time.Duration(0)})
	if s.Notice != nil || s.KeySeq != 2 || s.Pending != nil || s.LastRotation == nil || s.LastRotation.Mode != rotationScheduled {
		t.Fatalf("accepted, not yet confirmed: %+v", s)
	}

	// Stopped and deactivated.
	if err := d.self.SetState(identity.StateStopped); err != nil {
		t.Fatal(err)
	}
	checkSaid(t, "stopped", identityStatusOf(t, d), IdentityStopped, reasonStopped,
		a2ashape.Args{"name": IdentityNameForDir(d.layout.Root)})

	gone := newTestDaemon(t, "", false)
	if err := identitytest.SwitchWitness(gone.self, identitytest.Witness, gone.nowMS()-hourMS); err != nil {
		t.Fatal(err)
	}
	if err := identitytest.RotateEmergency(gone.self, gone.nowMS()-minuteMS); err != nil {
		t.Fatal(err)
	}
	s = identityStatusOf(t, gone)
	if s.LastRotation == nil || s.LastRotation.Mode != rotationEmergency {
		t.Fatalf("after an emergency rotation: %+v", s.LastRotation)
	}
	if err := identitytest.Deactivate(gone.self, gone.nowMS()); err != nil {
		t.Fatal(err)
	}
	s = identityStatusOf(t, gone)
	checkSaid(t, "deactivated", s, IdentityDeactivated, reasonDeactivated, nil)
	if s.Commitment != nil {
		t.Fatalf("a dip commits to nothing: %x", s.Commitment)
	}
}

// /status carries the identity as /identity/status gives it, which MCP
// node_status passes on (KEL M-2).
func TestStatusCarriesTheIdentity(t *testing.T) {
	d, _, _ := witnessedNode(t, true)
	rec := httptest.NewRecorder()
	d.hStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var out struct {
		Identity IdentityStatus `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	want := identityStatusOf(t, d)
	if out.Identity.State != want.State || out.Identity.Reason != want.Reason || out.Identity.AID != want.AID ||
		out.Identity.Recovery != want.Recovery || out.Identity.Witness == nil || *out.Identity.Witness != *want.Witness {
		t.Fatalf("/status.identity %+v, /identity/status %+v", out.Identity, want)
	}
}

// localSeedJoin is a node that joined without a terminal (KEL D-11): the
// CLI kept the recovery seed rs in the data directory and sent the
// commitment it makes for seq 2, with unprotected.
func localSeedJoin(t *testing.T) (d *Daemon, rs [32]byte, srvURL string, fh *fakeHub, answer map[string]any) {
	t.Helper()
	srv := newFakeHub(t)
	fh = fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	fh.notices = true
	fh.mu.Unlock()
	d, _ = joinDaemon(t)
	d.join.hooks = witnessJoinHooks{d}
	rs = newSeed(t)
	digest, err := identity.NextDigest(rs, d.AID(), 2)
	if err != nil {
		t.Fatal(err)
	}
	code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL,
		`,"next_digest":"`+base64.StdEncoding.EncodeToString(digest[:])+`","unprotected":true`))
	if code != 200 || out["state"] != JoinRegistered {
		t.Fatalf("join: %d %v", code, out)
	}
	d.stopRelayLoop()
	b, err := os.ReadFile(d.layout.IdentityPath())
	if err != nil {
		t.Fatal(err)
	}
	f, err := identity.ParseIdentityFile(b)
	if err != nil || !bytes.Equal(f.LocalCommit, digest[:]) {
		t.Fatalf("identity file key 7 is %x (%v), want the commitment %x", f.LocalCommit, err, digest)
	}
	return d, rs, srv.URL, fh, out
}

// protectStep is one of the two rotations of `anet identity protect` (KEL
// D-16): it reveals key j of from and commits key j+1 of to, accepted by
// the witness at once.
func protectStep(t *testing.T, d *Daemon, from, to [32]byte, j uint64) {
	t.Helper()
	revealed, err := identity.RecoverySeed(from, d.AID(), j)
	if err != nil {
		t.Fatal(err)
	}
	next, err := identity.NextDigest(to, d.AID(), j+1)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := d.self.BuildRotation(revealed, next[:], identityFactsOf(d.self).Witness, false, d.nowMS())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.self.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	if err := saveIdentity(d.layout, d.self); err != nil {
		t.Fatal(err)
	}
}

// reminders are the four places of KEL D-15 (M47) for d, and the
// /hub-register answer: what each says about the recovery passphrase.
type reminders struct {
	status   recoveryView // /identity/status.recovery
	node     recoveryView // /status.identity.recovery, which MCP node_status passes on
	summary  string       // the recovery line of the status summary, "" when there is none
	task     any          // anet.identity_note of a task this node projects (control /tasks/get, local A2A)
	a2a      any
	register *recoveryView
}

func remindersOf(t *testing.T, d *Daemon, hub, peer, task string) reminders {
	t.Helper()
	var r reminders
	r.status = identityStatusOf(t, d).Recovery
	rec := httptest.NewRecorder()
	d.hStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var st struct {
		Identity IdentityStatus `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	r.node = st.Identity.Recovery
	var in a2ashape.StatusInput
	d.IdentityStatus().FillSummary(&in)
	for _, l := range a2ashape.StatusSummary(in, a2ashape.Lang()) {
		if l.Topic == a2ashape.TopicRecovery {
			r.summary = l.Text + "\n" + l.Next
		}
	}
	code, out := controlCall(t, d.hTasksGet, "/tasks/get", `{"task_id":"`+task+`"}`)
	if code != 200 {
		t.Fatalf("/tasks/get: %d %v", code, out)
	}
	meta, _ := out["metadata"].(map[string]any)
	r.task = meta[a2ashape.KeyIdentityNote]
	at, err := (&DaemonTaskSeam{d: d}).Get(context.Background(), peer, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.a2a = at.Metadata[a2ashape.KeyIdentityNote]
	code, out = controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(hub, ""))
	if code != 200 {
		t.Fatalf("/hub-register (update): %d %v", code, out)
	}
	if v, ok := out["recovery"]; ok {
		b, _ := json.Marshal(v)
		var rv recoveryView
		if err := json.Unmarshal(b, &rv); err != nil {
			t.Fatal(err)
		}
		r.register = &rv
	}
	return r
}

// checkReminders asserts every place says key's sentence, or none says
// anything when key is "".
func checkReminders(t *testing.T, step string, r reminders, state, key string) {
	t.Helper()
	lang := a2ashape.Lang()
	want := recoveryView{State: state}
	var note any
	summary := ""
	if key != "" {
		want.Message, want.Next = a2ashape.Sentence(key, lang, nil), a2ashape.Next(key, lang, nil)
		note = want.Message
		summary = want.Message + "\n" + want.Next
	}
	if r.status != want || r.node != want {
		t.Errorf("%s: /identity/status.recovery %+v, /status.identity.recovery %+v, want %+v", step, r.status, r.node, want)
	}
	if r.summary != summary {
		t.Errorf("%s: the summary's recovery line %q, want %q", step, r.summary, summary)
	}
	if r.task != note || r.a2a != note {
		t.Errorf("%s: anet.identity_note %v (control) and %v (local A2A), want %v", step, r.task, r.a2a, note)
	}
	switch {
	case key == "" && r.register != nil:
		t.Errorf("%s: the /hub-register answer has a reminder: %+v", step, *r.register)
	case key != "" && (r.register == nil || *r.register != want):
		t.Errorf("%s: the /hub-register answer's recovery %+v, want %+v", step, r.register, want)
	}
}

// M47 (KEL D-15; ux UX-T1): while the identity has no recovery
// passphrase, /identity/status, MCP node_status (/status), the status
// summary's recovery line, the anet.identity_note of every task the
// control plane and the local A2A interface project, and the /hub-register
// answer say the same sentence (doctor's identity.recovery: cmd/anet
// TestDoctorSaysTheUnprotectedReminder); after the first rotation of
// protect they all say .resume, and after the second none says anything.
func TestUnprotectedReminderEverywhere(t *testing.T) {
	d, rs, hub, fh, answer := localSeedJoin(t)
	if rv, ok := answer["recovery"].(map[string]any); !ok || rv["state"] != RecoveryUnprotected ||
		rv["message"] != a2ashape.Sentence(reasonUnprotected, a2ashape.Lang(), nil) {
		t.Fatalf("the join's answer: %v", answer)
	}
	peer := newTestDaemon(t, hub, true)
	if err := peer.RegisterWithHub(context.Background(), hub, "p", nil); err != nil {
		t.Fatal(err)
	}
	code, out := controlCall(t, d.hTasksSend, "/tasks/send", `{"to":"`+peer.AID()+`","text":"hi","return_immediately":true}`)
	if code != 200 || out["id"] == nil {
		t.Fatalf("a send of an unprotected identity: %d %v (the reminder is not a gate)", code, out)
	}
	task := out["id"].(string)
	if meta, _ := out["metadata"].(map[string]any); meta[a2ashape.KeyIdentityNote] != a2ashape.Sentence(reasonUnprotected, a2ashape.Lang(), nil) {
		t.Errorf("the task SendMessage returned has no anet.identity_note: %v", out["metadata"])
	}
	checkReminders(t, "joined without a terminal", remindersOf(t, d, hub, peer.AID(), task), RecoveryUnprotected, reasonUnprotected)

	pass := newSeed(t)
	protectStep(t, d, rs, pass, 2)
	fh.mu.Lock()
	fh.notices = false // the hub's answers say nothing of the new tips
	fh.mu.Unlock()
	checkReminders(t, "the first of protect's rotations", remindersOf(t, d, hub, peer.AID(), task), RecoveryUnprotected, reasonUnprotectedResume)
	protectStep(t, d, pass, pass, 3)
	checkReminders(t, "both of protect's rotations", remindersOf(t, d, hub, peer.AID(), task), RecoveryProtected, "")

	// A stopped identity is not reminded of a passphrase it could no
	// longer use.
	u, _, uhub, _, _ := localSeedJoin(t)
	if err := u.self.SetState(identity.StateStopped); err != nil {
		t.Fatal(err)
	}
	if s := identityStatusOf(t, u); s.Recovery.State != RecoveryUnprotected || s.Recovery.Message != "" || s.Recovery.Next != "" {
		t.Fatalf("stopped: %+v", s.Recovery)
	}
	var in a2ashape.StatusInput
	u.IdentityStatus().FillSummary(&in)
	for _, l := range a2ashape.StatusSummary(in, a2ashape.Lang()) {
		if l.Topic == a2ashape.TopicRecovery {
			t.Fatalf("stopped: a recovery line %q", l.Text)
		}
	}
	_ = uhub
}

// KEL D-11, D-14: local_commit is written for a join without a terminal,
// before witness_rot; a commitment sent with a terminal before the witness
// designation is built replaces witness_rot, and the identity is protected
// from then on, local_commit staying where it is. Waiting for admission
// changes none of it.
func TestLocalCommitAndTheCommitmentThatReplacesIt(t *testing.T) {
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	fh.admission = fakeAdmitClosed
	fh.mu.Unlock()
	d, _ := joinDaemon(t)
	local, other := digestOf(1), digestOf(2)
	b64 := base64.StdEncoding.EncodeToString
	code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+b64(local)+`","unprotected":true`))
	if code != 200 || out["state"] != JoinPendingAdmission {
		t.Fatalf("join: %d %v", code, out)
	}
	if rv, _ := out["recovery"].(map[string]any); rv["state"] != RecoveryUnprotected {
		t.Fatalf("waiting for admission, unprotected: %v", out)
	}
	if !bytes.Equal(d.self.LocalCommit(), local) || d.recoveryState().State != RecoveryUnprotected {
		t.Fatalf("local_commit %x, %+v", d.self.LocalCommit(), d.recoveryState())
	}
	code, out = controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+b64(other)+`"`))
	if code != 200 || out["recovery"] != nil {
		t.Fatalf("a passphrase set before admission: %d %v", code, out)
	}
	if !bytes.Equal(d.self.LocalCommit(), local) || d.recoveryState().State != RecoveryProtected {
		t.Fatalf("local_commit %x, %+v: it stays, and the identity is protected", d.self.LocalCommit(), d.recoveryState())
	}
	// A request that is not unprotected writes no local_commit.
	e, _ := joinDaemon(t)
	controlCall(t, e.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+b64(other)+`"`))
	if e.self.LocalCommit() != nil || e.recoveryState().State != RecoveryProtected {
		t.Fatalf("a join with a terminal: local_commit %x, %+v", e.self.LocalCommit(), e.recoveryState())
	}
	// Once the witness designation is built its commitment is fixed (KEL
	// D-14): a later request without a terminal leaves local_commit as the
	// designation's, in memory and on disk.
	built, _ := joinDaemon(t)
	controlCall(t, built.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+b64(local)+`","unprotected":true`))
	if _, err := built.self.BuildWitnessDesignation(fh.self.AID(), local, built.nowMS()); err != nil {
		t.Fatal(err)
	}
	if err := saveIdentity(built.layout, built.self); err != nil {
		t.Fatal(err)
	}
	controlCall(t, built.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+b64(other)+`","unprotected":true`))
	disk, err := os.ReadFile(built.layout.IdentityPath())
	if err != nil {
		t.Fatal(err)
	}
	f, err := identity.ParseIdentityFile(disk)
	if err != nil || !bytes.Equal(built.self.LocalCommit(), local) || !bytes.Equal(f.LocalCommit, local) {
		t.Fatalf("the designation built: local_commit %x (on disk %x, %v), want %x", built.self.LocalCommit(), f.LocalCommit, err, local)
	}
	// Once the identity has its witness, hub-register at that hub only
	// updates the profile and its next_digest is ignored (KEL D-14), with
	// unprotected or without: local_commit stays the join's, so a caller
	// of the control plane cannot make an identity without a passphrase
	// read as protected and silence the reminder.
	w, _, whub, _, _ := localSeedJoin(t)
	joined := w.self.LocalCommit()
	if code, out := controlCall(t, w.hHubRegister, "/hub-register", hubRegisterBody(whub, `,"next_digest":"`+b64(other)+`","unprotected":true`)); code != 200 {
		t.Fatalf("an update with a next_digest: %d %v", code, out)
	}
	disk, err = os.ReadFile(w.layout.IdentityPath())
	if err != nil {
		t.Fatal(err)
	}
	f, err = identity.ParseIdentityFile(disk)
	if err != nil || !bytes.Equal(w.self.LocalCommit(), joined) || !bytes.Equal(f.LocalCommit, joined) ||
		w.recoveryState().State != RecoveryUnprotected {
		t.Fatalf("an update: local_commit %x (on disk %x, %v), %+v; want %x, unprotected", w.self.LocalCommit(), f.LocalCommit, err,
			w.recoveryState(), joined)
	}
}

// sendRoutes are the send surfaces of the control plane (MCP send_message
// is /tasks/send).
func sendRoutes(d *Daemon, peer string) []struct {
	h    http.HandlerFunc
	path string
	body string
} {
	return []struct {
		h    http.HandlerFunc
		path string
		body string
	}{
		{d.hTasksSend, "/tasks/send", `{"to":"` + peer + `","text":"hi","return_immediately":true}`},
		{d.hTasksSend, "/tasks/send", `{"task_id":"task-1","text":"more","return_immediately":true}`},
		{d.hDelegate, "/delegate", `{"provider":"` + peer + `","goal":"hi"}`},
		{d.hMessage, "/message", `{"interaction_id":"ix_1","body":"hi"}`},
	}
}

// checkSendRefused asserts every send surface refuses at once with key and
// writes nothing.
func checkSendRefused(t *testing.T, step string, d *Daemon, peer, key string, args a2ashape.Args) {
	t.Helper()
	lang := a2ashape.Lang()
	for _, c := range sendRoutes(d, peer) {
		rec := httptest.NewRecorder()
		c.h(rec, httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body)))
		var body a2ashape.ErrorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusConflict || body.Reason != key || body.Error != a2ashape.Sentence(key, lang, args) ||
			body.Next != a2ashape.Next(key, lang, args) {
			t.Errorf("%s: %s %s: %d %s, want %s", step, c.path, c.body, rec.Code, rec.Body, key)
		}
	}
	// The local A2A interface: before the binding, REST 400
	// FAILED_PRECONDITION and JSON-RPC −32602 (ux UX-R7, core G1).
	var ref *a2ashape.Refusal
	if !errors.As((&DaemonTaskSeam{d: d}).SendGate(context.Background()), &ref) || ref.Key != key {
		t.Errorf("%s: the local A2A send gate: %v, want %s", step, ref, key)
	} else if e := ref.A2A(lang); e.HTTPStatus != http.StatusBadRequest || e.Status != "FAILED_PRECONDITION" || e.RPCCode != -32602 ||
		e.Message != a2ashape.Sentence(key, lang, args) {
		t.Errorf("%s: the local A2A error %+v", step, e)
	}
	if n, err := d.ix.Count(interactions.ListFilter{}); err != nil || n != 0 {
		t.Errorf("%s: a refused send wrote %d tasks (%v)", step, n, err)
	}
	if n, _ := d.ix.OutboxLen(); n != 0 {
		t.Errorf("%s: a refused send queued %d rows", step, n)
	}
}

// M36, KEL D-3 (ux UX-R7): a stopped or deactivated identity refuses every
// send at once with its own code and queues nothing; that code wins over
// the hub's when both gates are closed; a key event being published or a
// stale notice does not refuse a send, which is queued (D-4).
func TestSendRefusedWhenStopped(t *testing.T) {
	d, srv, _ := witnessedNode(t, true)
	peer := newTestDaemon(t, srv.URL, true)
	if err := peer.RegisterWithHub(context.Background(), srv.URL, "p", nil); err != nil {
		t.Fatal(err)
	}
	if err := d.self.SetState(identity.StateStopped); err != nil {
		t.Fatal(err)
	}
	// The identity has a name here (the default one: its data directory is
	// the home), and the sentence's command names it.
	t.Setenv("ANET_HOME", d.layout.Root)
	if IdentityNameForDir(d.layout.Root) == "" {
		t.Fatal("the data directory of ANET_HOME has no identity name")
	}
	checkSendRefused(t, "stopped", d, peer.AID(), reasonStopped, a2ashape.Args{"name": IdentityNameForDir(d.layout.Root)})

	dd, dsrv, _ := witnessedNode(t, true)
	dpeer := newTestDaemon(t, dsrv.URL, true)
	if err := dpeer.RegisterWithHub(context.Background(), dsrv.URL, "p", nil); err != nil {
		t.Fatal(err)
	}
	if err := identitytest.Deactivate(dd.self, dd.nowMS()); err != nil {
		t.Fatal(err)
	}
	checkSendRefused(t, "deactivated", dd, dpeer.AID(), reasonDeactivated, nil)

	// Both gates closed: no hub, and stopped.
	nohub, _ := joinDaemon(t)
	if err := nohub.self.SetState(identity.StateStopped); err != nil {
		t.Fatal(err)
	}
	checkSendRefused(t, "stopped without a hub", nohub, peer.AID(), reasonStopped, a2ashape.Args{"name": IdentityNameForDir(nohub.layout.Root)})

	// Publishing, and a stale notice: the send is taken.
	q, qsrv, _ := witnessedNode(t, false) // no notice: notice_stale
	qpeer := newTestDaemon(t, qsrv.URL, true)
	if err := qpeer.RegisterWithHub(context.Background(), qsrv.URL, "p", nil); err != nil {
		t.Fatal(err)
	}
	if s := q.identityState(); s.State != IdentityNoticeStale {
		t.Fatalf("%+v", s)
	}
	if code, out := controlCall(t, q.hTasksSend, "/tasks/send", `{"to":"`+qpeer.AID()+`","text":"hi","return_immediately":true}`); code != 200 {
		t.Fatalf("a send while the notice is stale: %d %v", code, out)
	}
	next, err := identitytest.NextKey(q.self)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.self.BuildRotation(next, identitytest.NewNext(), identityFactsOf(q.self).Witness, false, q.nowMS()); err != nil {
		t.Fatal(err)
	}
	if code, out := controlCall(t, q.hTasksSend, "/tasks/send", `{"to":"`+qpeer.AID()+`","text":"hi","return_immediately":true}`); code != 200 {
		t.Fatalf("a send while a rotation is published: %d %v", code, out)
	}
	if n, _ := q.ix.Count(interactions.ListFilter{}); n != 2 {
		t.Fatalf("%d tasks, want the two sends", n)
	}
}

// M38, KEL D-4 (ux appendix B-1): a row the gate holds back because a key
// event is being published or the notice is stale shows the gate's state
// as anet.delivery_wait — rotation_pending or notice_stale, waits that
// resolve on their own — read from the gate, from real states of the
// identity.
func TestDeliveryWaitShowsIdentityState(t *testing.T) {
	for _, c := range []struct {
		g    GateState
		want string
	}{
		{GateState{false, IdentityPublishing, reasonRotationPending}, "rotation_pending"},
		{GateState{false, IdentityPublishing, reasonClockSkew}, "rotation_pending"},
		{GateState{false, IdentityNoticeStale, reasonNoticeStale}, "notice_stale"},
		{GateState{false, IdentityNoticeStale, reasonQuarantine}, "notice_stale"},
		{GateState{false, IdentityNoticeStale, reasonPending}, "notice_stale"},
		{GateState{true, IdentityOK, reasonIdentityOK}, ""},
		{GateState{true, IdentityOK, reasonConfirmAging}, ""},
		{GateState{false, IdentityNoHub, reasonNoHub}, ""},
		{GateState{false, IdentityStopped, reasonStopped}, ""},
		{GateState{false, IdentityDeactivated, reasonDeactivated}, ""},
	} {
		if got := gateDeliveryWait(c.g); got != c.want {
			t.Errorf("%+v: %q, want %q", c.g, got, c.want)
		}
	}
	for _, key := range []string{"rotation_pending.wait", "notice_stale.wait"} {
		r, ok := a2ashape.Lookup(key)
		if !ok || r.Class&a2ashape.ClassW == 0 {
			t.Errorf("%s is not a wait (class W) of the reason table: %+v", key, r)
		}
	}
	d, _, _ := witnessedNode(t, false)
	if w := gateDeliveryWait(d.identityGate()); w != "notice_stale" {
		t.Fatalf("no notice: delivery_wait %q", w)
	}
	d.noteHubNotice(hubNotice(t, fakeHubAt(t, d.config().HubURL).self, d.self, d.nowMS()), "")
	if w := gateDeliveryWait(d.identityGate()); w != "" {
		t.Fatalf("confirmed: delivery_wait %q", w)
	}
	next, err := identitytest.NextKey(d.self)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.self.BuildRotation(next, identitytest.NewNext(), identityFactsOf(d.self).Witness, false, d.nowMS()); err != nil {
		t.Fatal(err)
	}
	if w := gateDeliveryWait(d.identityGate()); w != "rotation_pending" {
		t.Fatalf("publishing: delivery_wait %q", w)
	}
}

// standInEngine is what the outbound engine (engine ENG-K13, ENG-1a/1b)
// does with the gate on one attempt: it reads the gate, and only when it
// is open takes a Snapshot, seals with the notice that confirms its tip,
// and hands the envelope to a transport. It counts what it signs and what
// it transmits, and the anonymous status query it may make in any state.
type standInEngine struct {
	d                         *Daemon
	signed, sent, statusAsked int
}

func (e *standInEngine) attempt(t *testing.T) {
	t.Helper()
	e.statusAsked++ // an anonymous status query is not gated
	if !e.d.identityGate().Open {
		return
	}
	s := e.d.self.Snapshot()
	n := e.d.tipNotice(s)
	if n == nil {
		return
	}
	sig, _ := s.Sign([]byte("an envelope"))
	if len(sig) == 0 {
		t.Fatal("the stand-in signed nothing")
	}
	e.signed++
	e.sent++
}

// D-1 (KEL §4.3.1; engine E-T12, SI-84): with the gate as the identity's
// real state makes it — no hub, publishing, notice_stale, stopped,
// deactivated — an engine that keeps to identityGate signs nothing and
// hands nothing over; only ok lets one envelope out.
func TestGateMatrixWithAStandInEngine(t *testing.T) {
	check := func(step string, d *Daemon, state string) {
		t.Helper()
		e := &standInEngine{d: d}
		e.attempt(t)
		open := state == IdentityOK
		if g := d.identityGate(); g.State != state || g.Open != open {
			t.Fatalf("%s: gate %+v, want %s", step, g, state)
		}
		want := 0
		if open {
			want = 1
		}
		if e.signed != want || e.sent != want || e.statusAsked != 1 {
			t.Fatalf("%s: signed %d, sent %d, status queries %d", step, e.signed, e.sent, e.statusAsked)
		}
	}
	fresh, _ := joinDaemon(t)
	check("no hub", fresh, IdentityNoHub)
	d, _, fh := witnessedNode(t, false)
	check("no notice", d, IdentityNoticeStale)
	d.noteHubNotice(hubNotice(t, fh.self, d.self, d.nowMS()), "")
	check("confirmed", d, IdentityOK)
	next, err := identitytest.NextKey(d.self)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.self.BuildRotation(next, identitytest.NewNext(), fh.self.AID(), true, d.nowMS()); err != nil {
		t.Fatal(err)
	}
	check("an emergency rotation being published: the old key signs nothing", d, IdentityPublishing)
	if err := d.self.SetState(identity.StateStopped); err != nil {
		t.Fatal(err)
	}
	check("stopped", d, IdentityStopped)
	gone, _, gfh := witnessedNode(t, true)
	gone.noteHubNotice(hubNotice(t, gfh.self, gone.self, gone.nowMS()), "")
	check("confirmed, before deactivation", gone, IdentityOK)
	if err := identitytest.Deactivate(gone.self, gone.nowMS()); err != nil {
		t.Fatal(err)
	}
	check("deactivated", gone, IdentityDeactivated)
}

// KEL S-2: an identity file of anet 0.2 is refused at start with
// identity_v02 alone, by the check before the start and by New, and the
// data directory is left as it was: no new identity over it. A damaged
// identity file is not called 0.2's (moving it away is not the advice for
// it).
func TestIdentityV02RefusesToStart(t *testing.T) {
	c, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	seed := bytes.Repeat([]byte{7}, 32)
	old, err := coredet.Marshal(map[uint64]any{1: seed, 2: seed, 3: c.KEL()})
	if err != nil {
		t.Fatal(err)
	}
	layout := v02DataDir(t, false, func(l Layout) {
		if err := os.WriteFile(l.IdentityPath(), old, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	refuseV02(t, layout)
	if _, err := ReadIdentityStatus(layout, a2ashape.Lang()); err == nil {
		t.Fatal("ReadIdentityStatus read a 0.2 identity")
	}
	// The identity loader itself, which EnsureLayoutInit uses without the
	// check before the start, refuses it too and incepts nothing over it.
	_, err = LoadOrGenerateIdentity(layout)
	assertV02Refusal(t, "LoadOrGenerateIdentity", err)
	if b, err := os.ReadFile(layout.IdentityPath()); err != nil || !bytes.Equal(b, old) {
		t.Fatalf("the 0.2 identity file changed (%v)", err)
	}
	for name, b := range map[string][]byte{"garbage": []byte("not an identity"), "truncated": old[:len(old)/2]} {
		damaged := v02DataDir(t, false, func(l Layout) {
			if err := os.WriteFile(l.IdentityPath(), b, 0o600); err != nil {
				t.Fatal(err)
			}
		})
		// A damaged file is its own sentence, identity_file_invalid, never 0.2's (KEL-10).
		if err := CheckDataDirForStart(damaged); !isRefusal(err, "identity_file_invalid") {
			t.Errorf("%s: the check before the start said %v", name, err)
		}
		d, err := New(damaged)
		if d != nil {
			d.Close()
		}
		if !isRefusal(err, "identity_file_invalid") {
			t.Errorf("%s: New said %v, want identity_file_invalid", name, err)
		}
	}
}

// KEL §1.4, §5.1 (ux §2.3): the rotation grace is a constant. config.json's
// rotation_grace stops the start (the check `anet daemon` and `anet up`
// make before it) with config_key_removed, in whatever case it is written,
// and nothing in the daemon reads it any more.
func TestRotationGraceConfigKeyRejected(t *testing.T) {
	for _, key := range []string{"rotation_grace", "Rotation_Grace"} {
		root := t.TempDir()
		writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", key: "3h"})
		err := CheckConfigForStart(NewLayout(root))
		var cr *ConfigRefusal
		if !errors.As(err, &cr) || len(cr.Refusals) != 1 || cr.Refusals[0].Code() != "config_key_removed" ||
			cr.Refusals[0].Args["key"] != key || cr.Refusals[0].Args["instead"] != "rotation_grace" {
			t.Fatalf("%s: %v, want config_key_removed", key, err)
		}
	}
	for i := 0; i < reflect.TypeFor[Config]().NumField(); i++ {
		f := reflect.TypeFor[Config]().Field(i)
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); strings.EqualFold(name, "rotation_grace") || f.Name == "RotationGrace" {
			t.Fatalf("Config still has %s", f.Name)
		}
	}
	if identity.RotationGrace != uint64(time.Hour/time.Millisecond) {
		t.Fatalf("identity.RotationGrace = %d ms, want one hour", identity.RotationGrace)
	}
}

// KEL P-3, P-4: a browser console session may read the identity's status
// (read only), and nothing that changes it.
func TestIdentityRoutesForASession(t *testing.T) {
	rule, ok := sessionRoutes["POST /identity/status"]
	if !ok || rule.jsonFields != nil || rule.multipart || rule.noCSRF {
		t.Fatalf("POST /identity/status: %+v %v, want a read-only session route", rule, ok)
	}
	for _, p := range []string{"POST /identity/rotate", "POST /identity/deactivate", "POST /hub-register"} {
		if _, ok := sessionRoutes[p]; ok {
			t.Errorf("%s is open to a session (KEL P-3)", p)
		}
	}
}

// The status ReadIdentityStatus reads from the data directory is the one
// the running daemon gives, but for what only the daemon knows.
func TestReadIdentityStatusFromTheDataDirectory(t *testing.T) {
	d, rs, _, _, _ := localSeedJoin(t)
	live := identityStatusOf(t, d)
	off, err := ReadIdentityStatus(d.layout, a2ashape.Lang())
	if err != nil {
		t.Fatal(err)
	}
	if off.State != live.State || off.Reason != live.Reason || off.AID != live.AID || off.KeySeq != live.KeySeq ||
		!bytes.Equal(off.Commitment, live.Commitment) || off.Recovery != live.Recovery || *off.LastRotation != *live.LastRotation ||
		off.Witness == nil || *off.Witness != *live.Witness || off.Notice == nil || off.RecoveryReminder() != reasonUnprotected {
		t.Fatalf("from the data directory %+v, from the daemon %+v", off, live)
	}
	protectStep(t, d, rs, newSeed(t), 2)
	if off, err = ReadIdentityStatus(d.layout, a2ashape.Lang()); err != nil || off.RecoveryReminder() != reasonUnprotectedResume {
		t.Fatalf("after protect's first rotation: %+v %v", off, err)
	}
	if _, err := ReadIdentityStatus(NewLayout(t.TempDir()), a2ashape.Lang()); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("an empty directory: %v", err)
	}
	_ = seal.MaxNoticeBytes
}

// UX-T12's identity rows (ux §4.7): no identity line while there is no hub
// (the hub line says it, UX-C9); with a witness, the identity line is the
// state's sentence.
func TestTheSummaryIdentityLine(t *testing.T) {
	lines := func(d *Daemon) map[string]a2ashape.SummaryLine {
		var in a2ashape.StatusInput
		d.IdentityStatus().FillSummary(&in)
		out := map[string]a2ashape.SummaryLine{}
		for _, l := range a2ashape.StatusSummary(in, a2ashape.Lang()) {
			out[l.Topic] = l
		}
		return out
	}
	fresh, _ := joinDaemon(t)
	if l, ok := lines(fresh)[a2ashape.TopicIdentity]; ok {
		t.Fatalf("no hub: an identity line %q", l.Text)
	}
	d, _, _ := witnessedNode(t, true)
	l, ok := lines(d)[a2ashape.TopicIdentity]
	if s := d.IdentityStatus(); !ok || l.Text != s.Message || l.Level != a2ashape.LevelOK {
		t.Fatalf("witnessed: %+v, want the status's sentence %q", l, s.Message)
	}
}

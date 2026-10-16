package daemon

// The join flow on a daemon against the fake hub (LEC AD-1 to AD-9, HN-2,
// HN-3, §2.3.2, §2.3.3): what /hub-register answers and writes, what the
// loop does after it, and the send gate.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// joinDaemon is a test daemon with no hub yet, and fake join hooks.
func joinDaemon(t *testing.T) (*Daemon, *fakeJoinHooks) {
	t.Helper()
	d := newTestDaemon(t, "", false)
	kel, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		t.Fatal(err)
	}
	hooks := newFakeJoinHooks(kel)
	d.setJoinHooks(hooks)
	return d, hooks
}

// testDigest is a next_digest in standard base64.
var testDigest = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

// controlCall calls a control-plane handler of d and returns the status and
// the decoded body.
func controlCall(t *testing.T, h http.HandlerFunc, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: %d %q", path, body, rec.Code, rec.Body)
	}
	return rec.Code, out
}

func hubRegisterBody(hub string, extra string) string {
	return `{"hub":"` + hub + `"` + extra + `}`
}

// waitJoinState waits until the join state is want.
func waitJoinState(t *testing.T, d *Daemon, want string) joinRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := d.joinSnapshot()
		if r != nil && r.State == want {
			return *r
		}
		if time.Now().After(deadline) {
			t.Fatalf("join state %+v, want %s", r, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// joinEvents are the states of anet.hub.admission on d's chain, in order.
func joinEvents(t *testing.T, d *Daemon) []string {
	t.Helper()
	_, recs := d.ledger.Evidence(EvidenceQuery{EventType: evtypes.HubAdmission, Limit: 1000})
	var out []string
	for _, r := range recs {
		out = append(out, r.Payload["state"].(string))
	}
	return out
}

// AD-1 to AD-4 on a hub that admits by AID: the first hub-register answers
// pending_admission with the admit line and asks the hub nothing but
// check_only; once the operator admits the AID the loop finishes on its
// own, calling Prepare once and registering once (M-AD-1).
func TestJoinAClosedHubWaitsForAdmissionThenFinishes(t *testing.T) {
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	fh.admission = fakeAdmitClosed
	fh.mu.Unlock()
	d, hooks := joinDaemon(t)

	code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"name":"coder","next_digest":"`+testDigest+`"`))
	if code != 200 || out["state"] != JoinPendingAdmission || out["reason"] != "hub_pending_admission" {
		t.Fatalf("first join: %d %v", code, out)
	}
	args := a2ashape.Args{"hub": srv.URL, "aid": d.AID()}
	if out["message"] != a2ashape.Sentence("hub_pending_admission", a2ashape.Lang(), args) ||
		out["next"] != "anet-hub admit "+d.AID() || out["aid"] != d.AID() || out["next_attempt_at"] == "" {
		t.Fatalf("the answer: %v", out)
	}
	if _, ok := out["recovery"]; ok {
		t.Error("a protected join answered with a recovery reminder")
	}
	fh.mu.Lock()
	checks, regs := fh.checkOnlyCalls, len(fh.registerKELs)
	fh.mu.Unlock()
	if checks < 1 || regs != 0 {
		t.Fatalf("before admission the hub got %d check_only and %d registrations", checks, regs)
	}
	if calls, _, _ := hooks.stats(); calls != 0 {
		t.Fatalf("Prepare was called %d times before admission (M-AD-1)", calls)
	}
	// What hub-register wrote (AD-1).
	if cfg, err := LoadConfig(d.layout); err != nil || cfg.HubURL != srv.URL || cfg.Name != "coder" {
		t.Fatalf("config.json: %+v %v", cfg, err)
	}
	rec, err := loadJoinRecord(d.layout)
	if err != nil || rec == nil || rec.State != JoinPendingAdmission || rec.WitnessRot == nil ||
		base64.StdEncoding.EncodeToString(rec.WitnessRot.NextDigest) != testDigest || rec.WitnessRot.HubAID != fh.self.AID() {
		t.Fatalf("hub_join.json: %+v %v", rec, err)
	}
	if fi, err := os.Stat(d.layout.hubJoinPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("hub_join.json mode: %v %v", fi, err)
	}
	// /status.hub (§2.3.3).
	if v := d.HubView(); v.State != JoinPendingAdmission || v.URL != srv.URL || v.Next != "anet-hub admit "+d.AID() {
		t.Fatalf("/status.hub: %+v", v)
	}

	// The operator admits the AID; the loop's next attempt (kicked here
	// instead of waiting 15 s) registers.
	fh.mu.Lock()
	fh.admitted[d.AID()] = true
	fh.mu.Unlock()
	d.kickJoin()
	got := waitJoinState(t, d, JoinRegistered)
	if got.Reason != "registered" || got.RegisteredAt == 0 {
		t.Fatalf("registered: %+v", got)
	}
	calls, adopted, built := hooks.stats()
	if calls != 1 || adopted != 1 || built != 1 {
		t.Fatalf("hooks: %d Prepare, %d Adopt, %d built", calls, adopted, built)
	}
	fh.mu.Lock()
	regs = len(fh.registerKELs)
	fh.mu.Unlock()
	if regs != 1 {
		t.Fatalf("the hub got %d registrations, want 1", regs)
	}
	// AD-7: one event per change.
	if ev := joinEvents(t, d); strings.Join(ev, ",") != "pending_admission,registered" {
		t.Fatalf("anet.hub.admission: %v", ev)
	}
}

// UX-O1, P-JOIN (e), M-AD-5: an address with no hub behind it is
// hub_unreachable, before anything is written, for dry_run and the join.
func TestJoinAnUnreachableHubWritesNothing(t *testing.T) {
	d, _ := joinDaemon(t)
	before, _ := os.ReadFile(d.layout.ConfigPath())
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	for _, body := range []string{hubRegisterBody(url, `,"dry_run":true`), hubRegisterBody(url, `,"next_digest":"`+testDigest+`"`)} {
		code, out := controlCall(t, d.hHubRegister, "/hub-register", body)
		if code != http.StatusBadGateway || out["reason"] != "hub_unreachable" ||
			out["error"] != a2ashape.Sentence("hub_unreachable", a2ashape.Lang(), a2ashape.Args{"hub": url}) ||
			out["next"] != "anet hub-register "+url {
			t.Fatalf("%s: %d %v", body, code, out)
		}
	}
	after, _ := os.ReadFile(d.layout.ConfigPath())
	if !bytes.Equal(before, after) {
		t.Fatal("config.json changed")
	}
	if _, err := os.Stat(d.layout.hubJoinPath()); !os.IsNotExist(err) {
		t.Fatalf("hub_join.json was written: %v", err)
	}
	// An address that answers, but not as a hub: the same.
	notHub := httptest.NewServer(http.NotFoundHandler())
	defer notHub.Close()
	if code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(notHub.URL, `,"dry_run":true`)); code != 502 || out["reason"] != "hub_unreachable" {
		t.Fatalf("not a hub: %d %v", code, out)
	}
}

// LEC §2.3.2: a first join without next_digest is config_invalid, a token
// or invite member invite_removed, and neither changes anything.
func TestJoinRefusalsChangeNothing(t *testing.T) {
	srv := newFakeHub(t)
	d, _ := joinDaemon(t)
	before, _ := os.ReadFile(d.layout.ConfigPath())
	code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, ""))
	if code != 400 || out["reason"] != "config_invalid" || !strings.Contains(out["error"].(string), "next_digest") {
		t.Fatalf("no next_digest: %d %v", code, out)
	}
	code, out = controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"token":"anetinv_x"`))
	if code != 400 || out["reason"] != "invite_removed" || out["next"] != "anet hub-register "+srv.URL {
		t.Fatalf("token: %d %v", code, out)
	}
	code, out = controlCall(t, d.hHubRegister, "/hub-register", `{"hub":"hub.example"}`)
	if code != 400 || out["reason"] != "config_invalid" {
		t.Fatalf("not a URL: %d %v", code, out)
	}
	after, _ := os.ReadFile(d.layout.ConfigPath())
	if !bytes.Equal(before, after) || d.joinSnapshot() != nil {
		t.Fatal("a refused hub-register changed something")
	}
	if fh := fakeHubAt(t, srv.URL); fh.registerCalls != 0 {
		t.Fatalf("a refused hub-register reached the hub %d times", fh.registerCalls)
	}
}

// dry_run's actions (§2.3.2): join while the identity has no witness
// (recovery_needed until hub_join.json holds witness_rot), update at the
// witness hub, switch at another; a hub-register that would switch is
// refused, switching hubs being a key rotation.
func TestDryRunActions(t *testing.T) {
	srv := newFakeHub(t)
	hubAID := fakeHubAt(t, srv.URL).self.AID()
	d, _ := joinDaemon(t)
	dry := func() map[string]any {
		t.Helper()
		code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"dry_run":true`))
		if code != 200 {
			t.Fatalf("dry_run: %d %v", code, out)
		}
		return out
	}
	if out := dry(); out["action"] != "join" || out["recovery_needed"] != true || out["hub_aid"] != hubAID {
		t.Fatalf("fresh: %v", out)
	}
	// dry_run writes nothing (§2.3.2): not even the hub's KEL is pinned.
	if _, err := d.ix.PeerIdentity(hubAID); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("dry_run pinned the hub's KEL: %v", err)
	}
	if code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+testDigest+`"`)); code != 200 || out["state"] != JoinRegistered {
		t.Fatalf("join: %d %v", code, out)
	}
	if out := dry(); out["action"] != "join" || out["recovery_needed"] != false {
		t.Fatalf("with witness_rot: %v", out)
	}
	d.join.witness = func() string { return hubAID }
	if out := dry(); out["action"] != "update" || out["recovery_needed"] != false {
		t.Fatalf("witnessed: %v", out)
	}
	d.join.witness = func() string { return "bafyreiotherhubotherhubotherhubotherhubotherhubotherhubother" }
	if out := dry(); out["action"] != "switch" {
		t.Fatalf("another witness: %v", out)
	}
	code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, ""))
	if code != http.StatusConflict || out["reason"] != "hub_switch_requires_rotation" {
		t.Fatalf("switch: %d %v", code, out)
	}
}

// CORE-V1 at dry_run: a hub that states another wire than this node's.
func TestDryRunChecksTheWire(t *testing.T) {
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	d, _ := joinDaemon(t)
	for wire, want := range map[int]string{4: "wire_too_old", 2: "wire_too_new"} {
		fh.mu.Lock()
		fh.wire = wire
		fh.mu.Unlock()
		code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"dry_run":true`))
		if code != http.StatusConflict || out["reason"] != want {
			t.Errorf("hub wire %d: %d %v, want %s", wire, code, out, want)
		}
	}
}

// HN-2, HN-3 on a hub that registers openly: check_only answers open, the
// first registration is answered pow_required, the second carries the
// proof and registers the account on probation; a spent network quota is
// hub_retrying.quota until the hub's Retry-After.
func TestJoinAnOpenHub(t *testing.T) {
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	fh.admission = fakeAdmitOpen
	fh.mu.Unlock()
	d, hooks := joinDaemon(t)
	code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+testDigest+`"`))
	if code != 200 || out["state"] != JoinRegistered || out["reason"] != "registered.probation" {
		t.Fatalf("open hub: %d %v", code, out)
	}
	if !strings.Contains(out["message"].(string), "probation") {
		t.Errorf("message %q", out["message"])
	}
	fh.mu.Lock()
	calls, checks := fh.registerCalls, fh.checkOnlyCalls
	fh.mu.Unlock()
	if checks != 1 || calls != 3 {
		t.Fatalf("the hub got %d requests (%d check_only), want check, register, register with the proof", calls, checks)
	}
	if c, a, _ := hooks.stats(); c != 1 || a != 1 {
		t.Fatalf("hooks: %d Prepare, %d Adopt", c, a)
	}

	srv2 := newFakeHub(t)
	fh2 := fakeHubAt(t, srv2.URL)
	fh2.mu.Lock()
	fh2.admission, fh2.quotaSpent = fakeAdmitOpen, true
	fh2.mu.Unlock()
	d2, _ := joinDaemon(t)
	start := time.Now()
	code, out = controlCall(t, d2.hHubRegister, "/hub-register", hubRegisterBody(srv2.URL, `,"next_digest":"`+testDigest+`"`))
	if code != 200 || out["state"] != JoinRetrying || out["reason"] != "hub_retrying.quota" ||
		out["next"] != "anet-hub admit "+d2.AID() {
		t.Fatalf("quota: %d %v", code, out)
	}
	at, err := time.Parse(time.RFC3339, out["next_attempt_at"].(string))
	if err != nil || at.Before(start.Add(59*time.Minute)) {
		t.Fatalf("next attempt %v (%v), want the hub's Retry-After of an hour", at, err)
	}
}

// P-JOIN (f), M-AD-6: without a terminal the CLI joins with
// unprotected; the hub answers decide the state exactly as for a join with
// a passphrase, on a hub that has not admitted the AID, one that has, and
// one that registers openly; only the recovery reminder differs.
func TestUnprotectedChangesOnlyTheRecoveryReminder(t *testing.T) {
	for _, mode := range []string{"closed", "admitted", "open"} {
		t.Run(mode, func(t *testing.T) {
			var seq [2][]string
			for i, unprotected := range []bool{false, true} {
				srv := newFakeHub(t)
				fh := fakeHubAt(t, srv.URL)
				d, _ := joinDaemon(t)
				fh.mu.Lock()
				switch mode {
				case "closed":
					fh.admission = fakeAdmitClosed
				case "admitted":
					fh.admission = fakeAdmitClosed
					fh.admitted[d.AID()] = true
				case "open":
					fh.admission = fakeAdmitOpen
				}
				fh.mu.Unlock()
				extra := `,"next_digest":"` + testDigest + `"`
				if unprotected {
					extra += `,"unprotected":true`
				}
				code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, extra))
				if code != 200 {
					t.Fatalf("%d %v", code, out)
				}
				rec, _ := out["recovery"].(map[string]any)
				if unprotected != (rec != nil) {
					t.Fatalf("unprotected %v: recovery %v", unprotected, out["recovery"])
				}
				if rec != nil && (rec["state"] != "unprotected" || rec["next"] != "anet identity protect" ||
					rec["message"] != a2ashape.Sentence("identity.unprotected", a2ashape.Lang(), nil)) {
					t.Fatalf("recovery %v", rec)
				}
				norm := func(s any) string {
					return strings.ReplaceAll(strings.ReplaceAll(s.(string), d.AID(), "<aid>"), srv.URL, "<hub>")
				}
				seq[i] = append(seq[i], out["state"].(string)+"|"+out["reason"].(string)+"|"+norm(out["message"]))
				if mode == "closed" {
					fh.mu.Lock()
					fh.admitted[d.AID()] = true
					fh.mu.Unlock()
					d.kickJoin()
					r := waitJoinState(t, d, JoinRegistered)
					seq[i] = append(seq[i], r.State+"|"+r.Reason)
				}
				for _, s := range seq[i] {
					if strings.Contains(s, "recovery_setup_required") {
						t.Fatalf("recovery_setup_required: %s", s)
					}
				}
			}
			if strings.Join(seq[0], "\n") != strings.Join(seq[1], "\n") {
				t.Fatalf("unprotected changed the join:\n%v\n%v", seq[0], seq[1])
			}
		})
	}
}

// AD-6, ux UX-R7, M-AD-3: until the hub registered this node, every send
// route answers at once with the join state's sentence and writes no task.
func TestSendingWaitsForTheHub(t *testing.T) {
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	fh.admission = fakeAdmitClosed
	fh.mu.Unlock()
	d, _ := joinDaemon(t)
	peer := newTestDaemon(t, srv.URL, true).AID()

	// No hub at all: no_hub_yet.
	code, out := controlCall(t, d.hTasksSend, "/tasks/send", `{"to":"`+peer+`","text":"hi","return_immediately":true}`)
	if code != http.StatusConflict || out["reason"] != "no_hub_yet" || out["next"] != "anet hub-register "+a2ashape.OfficialHubURL {
		t.Fatalf("no hub: %d %v", code, out)
	}
	// Waiting for admission: hub_pending_admission with the admit line.
	controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+testDigest+`"`))
	waitJoinState(t, d, JoinPendingAdmission)
	want := a2ashape.Sentence("hub_pending_admission", a2ashape.Lang(), a2ashape.Args{"hub": srv.URL, "aid": d.AID()})
	for _, c := range []struct {
		h    http.HandlerFunc
		path string
		body string
	}{
		{d.hTasksSend, "/tasks/send", `{"to":"` + peer + `","text":"hi","return_immediately":true}`},
		{d.hDelegate, "/delegate", `{"provider":"` + peer + `","goal":"hi"}`},
		{d.hMessage, "/message", `{"interaction_id":"ix_1","body":"hi"}`},
	} {
		rec := httptest.NewRecorder()
		c.h(rec, httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body)))
		var body a2ashape.ErrorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusConflict || body.Reason != "hub_pending_admission" || body.Error != want ||
			body.Next != "anet-hub admit "+d.AID() || rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s: %d %s", c.path, rec.Code, rec.Body)
		}
	}
	if n, err := d.ix.Count(interactions.ListFilter{}); err != nil || n != 0 {
		t.Fatalf("a refused send wrote %d tasks (%v)", n, err)
	}
	if n, _ := d.ix.OutboxLen(); n != 0 {
		t.Fatalf("a refused send queued %d rows", n)
	}
}

// AD-5, M-AD-4: a registered node whose hub fails for a while stays
// registered and goes on sending (the outbound queue delivers later); an
// operator who revokes the admission makes it pending_admission.
func TestARegisteredNodeKeepsSendingWhileItsHubIsAway(t *testing.T) {
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	d := newTestDaemon(t, srv.URL, false)
	peer := newTestDaemon(t, srv.URL, true)
	if err := d.RegisterWithHub(context.Background(), srv.URL, "n", nil); err != nil {
		t.Fatal(err)
	}
	if err := peer.RegisterWithHub(context.Background(), srv.URL, "p", nil); err != nil {
		t.Fatal(err)
	}
	fh.mu.Lock()
	fh.registerScript = []fakeRegisterAnswer{{status: 503, body: map[string]any{"reason": "internal", "error": "x"}},
		{drop: true}}
	fh.mu.Unlock()
	for i := 0; i < 2; i++ {
		d.regMu.Lock()
		r := d.attemptLocked(context.Background(), srv.URL, "n", nil)
		d.regMu.Unlock()
		if r.State != JoinRegistered || r.NextAttemptAt == 0 {
			t.Fatalf("attempt %d: %+v", i, r)
		}
	}
	code, out := controlCall(t, d.hTasksSend, "/tasks/send", `{"to":"`+peer.AID()+`","text":"hi","return_immediately":true}`)
	if code != 200 || out["id"] == nil {
		t.Fatalf("a send while the hub re-registration failed: %d %v", code, out)
	}
	fh.mu.Lock()
	fh.registerScript = []fakeRegisterAnswer{{status: 403, body: map[string]any{"reason": "admission_required", "error": "x"}}}
	fh.mu.Unlock()
	d.regMu.Lock()
	r := d.attemptLocked(context.Background(), srv.URL, "n", nil)
	d.regMu.Unlock()
	if r.State != JoinPendingAdmission {
		t.Fatalf("admission revoked: %+v", r)
	}
	if code, out := controlCall(t, d.hTasksSend, "/tasks/send", `{"to":"`+peer.AID()+`","text":"hi","return_immediately":true}`); code != 409 || out["reason"] != "hub_pending_admission" {
		t.Fatalf("send after revocation: %d %v", code, out)
	}
}

// AD-8: leaving a hub that has not admitted this node ends the join; the
// witness hub cannot be left.
func TestHubLeave(t *testing.T) {
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	fh.admission = fakeAdmitClosed
	fh.mu.Unlock()
	d, _ := joinDaemon(t)
	controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+testDigest+`"`))
	waitJoinState(t, d, JoinPendingAdmission)
	code, out := controlCall(t, d.hHubLeave, "/hub-leave", `{}`)
	if code != 200 || out["state"] != JoinNone {
		t.Fatalf("leave a pending join: %d %v", code, out)
	}
	if d.joinSnapshot() != nil || d.config().HubURL != "" || d.HubView().State != JoinNone {
		t.Fatal("the join is still there")
	}
	if _, err := os.Stat(d.layout.hubJoinPath()); !os.IsNotExist(err) {
		t.Fatalf("hub_join.json: %v", err)
	}
	if cfg, _ := LoadConfig(d.layout); cfg.HubURL != "" {
		t.Fatalf("config.json hub_url %q", cfg.HubURL)
	}

	// The witness hub.
	fh.mu.Lock()
	fh.admitted[d.AID()] = true
	fh.mu.Unlock()
	if code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+testDigest+`"`)); code != 200 || out["state"] != JoinRegistered {
		t.Fatalf("join: %d %v", code, out)
	}
	d.join.witness = func() string { return fh.self.AID() }
	code, out = controlCall(t, d.hHubLeave, "/hub-leave", `{"hub":"`+srv.URL+`"}`)
	if code != http.StatusConflict || out["reason"] != "witness_hub_leave" {
		t.Fatalf("leave the witness hub: %d %v", code, out)
	}
	if d.joinSnapshot() == nil || d.config().HubURL != srv.URL {
		t.Fatal("a refused hub-leave changed the join")
	}
}

// AD-3: a restarted node tries again at once, even after a refusal (an
// `anet update` clears a wire_too_old this way), and a registered one
// registers again (AD-5).
func TestARestartTriesAgain(t *testing.T) {
	srv := newFakeHub(t)
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "hub_url": srv.URL})
	rec := joinRecord{V: joinRecordVersion, Hub: srv.URL, State: JoinRefused, Reason: reasonWireTooOld,
		Since: time.Now().UnixMilli(), Attempts: 1}
	if err := saveJoinRecord(NewLayout(root), &rec); err != nil {
		t.Fatal(err)
	}
	d, err := New(NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	waitJoinState(t, d, JoinRegistered)
	if b, err := os.ReadFile(NewLayout(root).hubJoinPath()); err != nil || !strings.Contains(string(b), `"state":"registered"`) {
		t.Fatalf("hub_join.json after the restart: %s %v", b, err)
	}
}

// The relay loop asks for mail only once the hub registered this node: a
// node waiting for admission has no mailbox there.
func TestTheRelayLoopWaitsForRegistration(t *testing.T) {
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	fh.admission = fakeAdmitClosed
	fh.mu.Unlock()
	d, _ := joinDaemon(t)
	// The join designates the hub the witness, whose notice then confirms
	// the identity: polling is what the gate lets through first (KEL D-1).
	d.regMu.Lock()
	d.setJoinHooks(witnessJoinHooks{d})
	d.regMu.Unlock()
	// The gate is held open: what keeps the loop from polling while the
	// node waits for admission must be the loop's own wait, not a gate
	// that happens to be closed then.
	holdGateOpen(d)
	controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+testDigest+`"`))
	waitJoinState(t, d, JoinPendingAdmission)
	if !d.relayRunning() {
		t.Fatal("hub-register did not start the relay loop")
	}
	time.Sleep(2500 * time.Millisecond)
	if n := relayPollTriesFor(srv.URL, d.AID()); n != 0 {
		t.Fatalf("%d polls while waiting for admission", n)
	}
	fh.mu.Lock()
	fh.admitted[d.AID()] = true
	fh.mu.Unlock()
	d.kickJoin()
	waitJoinState(t, d, JoinRegistered)
	deadline := time.Now().Add(5 * time.Second)
	for relayPollsFor(srv.URL, d.AID()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no poll after registration")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// /status carries the hub object (§2.3.3; MCP node_status passes it on).
func TestStatusCarriesTheHub(t *testing.T) {
	d, _ := joinDaemon(t)
	rec := httptest.NewRecorder()
	d.hStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var out struct {
		Hub hubView `json:"hub"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Hub.State != JoinNone || out.Hub.Reason != "no_hub_yet" ||
		out.Hub.Message != a2ashape.Sentence("no_hub_yet", a2ashape.Lang(), nil) {
		t.Fatalf("/status.hub: %s (%v)", rec.Body, err)
	}
}

// AD-3: the loop makes the attempt hub-register scheduled, without being
// asked: a node admitted after its first attempt finishes on its own.
//
// The loop is first seen waiting on the attempt hub-register scheduled 15 s
// on, with no wake-up left for it to take (the join wait hook). Only then
// is the schedule brought forward: the loop learns of it from the record's
// wake-up alone, and without one (exec.py exe13-wake) it sleeps through
// the whole wait below, every time.
func TestTheLoopRetriesWhenDue(t *testing.T) {
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	fh.admission = fakeAdmitClosed
	fh.mu.Unlock()
	d, _ := joinDaemon(t)
	waits := make(chan int64, 256)
	d.setJoinWait(func(next int64) {
		select {
		case waits <- next:
		default:
		}
	})
	controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+testDigest+`"`))
	r := waitJoinState(t, d, JoinPendingAdmission)
	if r.NextAttemptAt <= time.Now().UnixMilli() {
		t.Fatalf("hub-register scheduled no later attempt: %+v", r)
	}
	armed := time.After(10 * time.Second)
	for settled := false; !settled; {
		select {
		case next := <-waits:
			// Waiting for the scheduled attempt, with no wake-up queued:
			// nothing will make it look at the record again but a new
			// wake-up, or that attempt's time.
			settled = next == r.NextAttemptAt && len(d.join.wake) == 0 && len(d.join.kick) == 0
		case <-armed:
			t.Fatal("the join loop never waited for the attempt hub-register scheduled")
		}
	}
	fh.mu.Lock()
	fh.admitted[d.AID()] = true
	fh.mu.Unlock()
	// Bring the attempt forward, as the record would hold it 15 s later.
	// The wait below ends well before the 15 s timer the loop sleeps on
	// would.
	deadline := time.UnixMilli(r.NextAttemptAt).Add(-2 * time.Second)
	if time.Until(deadline) < 3*time.Second {
		t.Fatalf("the loop took until %v to wait; too late to tell its wake-up from its timer",
			time.Until(time.UnixMilli(r.NextAttemptAt)))
	}
	r.NextAttemptAt = time.Now().Add(200 * time.Millisecond).UnixMilli()
	d.keepJoinRecord(srv.URL, r)
	for {
		if cur := d.joinSnapshot(); cur != nil && cur.State == JoinRegistered {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the loop did not make the attempt brought forward: %+v", d.joinSnapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// AD-6 on the send routes TestSendingWaitsForTheHub does not take: a
// message on an existing task (/tasks/send with task_id), and the console's
// multipart /delegate and /message. A node with no hub answers each at once
// with no_hub_yet and writes nothing.
func TestTheOtherSendRoutesWaitForTheHub(t *testing.T) {
	d, _ := joinDaemon(t)
	form := func(fields map[string]string) (string, []byte) {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		for k, v := range fields {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		return mw.FormDataContentType(), b.Bytes()
	}
	peer := newTestDaemon(t, "", true).AID()
	delegateType, delegateBody := form(map[string]string{"provider": peer, "goal": "hi"})
	messageType, messageBody := form(map[string]string{"interaction_id": "ix_1", "body": "hi"})
	for _, c := range []struct {
		name  string
		h     http.HandlerFunc
		path  string
		ctype string
		body  []byte
	}{
		{"a message on a task", d.hTasksSend, "/tasks/send", "application/json",
			[]byte(`{"task_id":"task-1","text":"more","return_immediately":true}`)},
		{"multipart delegate", d.hDelegate, "/delegate", delegateType, delegateBody},
		{"multipart message", d.hMessage, "/message", messageType, messageBody},
	} {
		req := httptest.NewRequest(http.MethodPost, c.path, bytes.NewReader(c.body))
		req.Header.Set("Content-Type", c.ctype)
		rec := httptest.NewRecorder()
		c.h(rec, req)
		var body a2ashape.ErrorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusConflict || body.Reason != "no_hub_yet" ||
			body.Error != a2ashape.Sentence("no_hub_yet", a2ashape.Lang(), nil) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	if n, err := d.ix.Count(interactions.ListFilter{}); err != nil || n != 0 {
		t.Fatalf("a refused send wrote %d tasks (%v)", n, err)
	}
	if n, _ := d.ix.OutboxLen(); n != 0 {
		t.Fatalf("a refused send queued %d rows", n)
	}
}

// doctor reads the hub item from the data directory when no daemon answers: ReadHubView says what
// /status.hub says, from hub_join.json alone, and no record is no hub.
func TestReadHubViewIsTheStatusViewFromTheFile(t *testing.T) {
	l := NewLayout(t.TempDir())
	v, err := ReadHubView(l, a2ashape.English)
	if err != nil || v.State != JoinNone || v.Reason != "no_hub_yet" || v.Message != a2ashape.Sentence("no_hub_yet", a2ashape.English, nil) {
		t.Fatalf("no record: %+v %v", v, err)
	}
	rec := joinRecord{V: joinRecordVersion, Hub: "https://hub.example", State: JoinPendingAdmission, Reason: reasonPending,
		Since: time.Now().UnixMilli()}
	if err := saveJoinRecord(l, &rec); err != nil {
		t.Fatal(err)
	}
	v, err = ReadHubView(l, a2ashape.English)
	if err != nil || v.State != JoinPendingAdmission || v.Reason != reasonPending || v.URL != "https://hub.example" ||
		!strings.Contains(v.Message, "https://hub.example") {
		t.Fatalf("pending admission: %+v %v", v, err)
	}
	if err := os.WriteFile(l.hubJoinPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHubView(l, a2ashape.English); err == nil {
		t.Fatal("a record that cannot be read is an error, not no hub")
	}
}

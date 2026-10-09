package daemon

// Tests of the join machinery's pure parts (LEC §7.2 P-BACKOFF, P-JOIN; HN-1
// to HN-5 and ux UX-R6 on the hub's answers), and the fuzz targets of its
// decoders.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/regpow"
)

// P-BACKOFF: the schedule of AD-3, never shorter than the hub's
// Retry-After, never longer than 10 minutes on its own.
func TestJoinBackoff(t *testing.T) {
	for _, c := range []struct {
		attempts int
		retry    time.Duration
		want     time.Duration
	}{
		{0, 0, 15 * time.Second},
		{1, 0, 15 * time.Second},
		{2, 0, 30 * time.Second},
		{3, 0, time.Minute},
		{4, 0, 2 * time.Minute},
		{5, 0, 5 * time.Minute},
		{6, 0, 10 * time.Minute},
		{7, 0, 10 * time.Minute},
		{1000, 0, 10 * time.Minute},
		{1, 90 * time.Second, 90 * time.Second},
		{6, 3 * time.Hour, 3 * time.Hour},
		{1, 48 * time.Hour, 24 * time.Hour},
	} {
		if got := joinBackoff(c.attempts, c.retry); got != c.want {
			t.Errorf("joinBackoff(%d, %v) = %v, want %v", c.attempts, c.retry, got, c.want)
		}
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 100000; i++ {
		a := rng.Intn(64)
		ra := time.Duration(rng.Int63n(int64(30 * time.Hour)))
		if rng.Intn(4) == 0 {
			ra = 0
		}
		d0, d1 := joinBackoff(a, ra), joinBackoff(a+1, ra)
		if d1 < d0 {
			t.Fatalf("not monotone: joinBackoff(%d, %v) = %v > joinBackoff(%d) = %v", a, ra, d0, a+1, d1)
		}
		if ra <= joinRetryAfterMax && d0 < ra {
			t.Fatalf("joinBackoff(%d, %v) = %v is shorter than Retry-After", a, ra, d0)
		}
		if ra <= joinBackoffMax && d0 > joinBackoffMax {
			t.Fatalf("joinBackoff(%d, %v) = %v exceeds 10 minutes", a, ra, d0)
		}
		if d0 > joinRetryAfterMax || d0 < 15*time.Second {
			t.Fatalf("joinBackoff(%d, %v) = %v out of bounds", a, ra, d0)
		}
	}
}

// refusalBody is a wire-3 refusal body.
func refusalBody(reason string, extra map[string]any) []byte {
	m := map[string]any{"reason": reason, "error": "The hub says " + reason + "; do what it says."}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return b
}

// challengeBody is pow_required with a challenge of bits.
func challengeBody(bits int) []byte {
	seed := make([]byte, 32)
	return refusalBody("pow_required", map[string]any{"challenge": map[string]any{
		"seed": base64.RawURLEncoding.EncodeToString(seed), "bits": bits, "not_after": 1}})
}

// HN-1, HN-4 and ux UX-R6: each hub answer of the table, and the state
// and sentence it leaves a node that has not registered yet in.
func TestHubAnswersMapAsUXR6(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	reg := hubReply{Status: 200, Body: []byte(`{"aid":"a","status":"registered","keys_status":"ok","card_status":"absent"}`)}
	for _, c := range []struct {
		name   string
		phase  joinPhase
		reply  hubReply
		state  string
		reason string
		why    string
		code   string
		act    joinAct
	}{
		{"check admitted", phaseCheck, hubReply{Status: 200, Body: []byte(`{"status":"admitted"}`)}, JoinRetrying, reasonRetrying, whyUnreachable, "", actPrepare},
		{"check open", phaseCheck, hubReply{Status: 200, Body: []byte(`{"status":"open"}`)}, JoinRetrying, reasonRetrying, whyUnreachable, "", actPrepare},
		{"registered", phaseRegister, reg, JoinRegistered, reasonRegistered, "", "", actAdopt},
		{"403 admission_required", phaseCheck, hubReply{Status: 403, Body: refusalBody("admission_required", map[string]any{"aid": "x"})}, JoinPendingAdmission, reasonPending, "", "", actWait},
		{"403 admission_required at register", phaseRegister, hubReply{Status: 403, Body: refusalBody("admission_required", nil)}, JoinPendingAdmission, reasonPending, "", "", actWait},
		{"403 pow_required: solve", phaseRegister, hubReply{Status: 403, Body: challengeBody(22)}, JoinRetrying, reasonRetrying, whyUnreachable, "", actSolve},
		{"403 pow_required again", phaseRegisterPow, hubReply{Status: 403, Body: challengeBody(22)}, JoinRetrying, reasonRetrying, whyPow, "", actWait},
		{"403 pow_required for 23 bits", phaseRegister, hubReply{Status: 403, Body: challengeBody(23)}, JoinRetrying, reasonRetrying, whyPow, "", actWait},
		{"429 registration_quota", phaseRegisterPow, hubReply{Status: 429, RetryAfter: "7200", Body: refusalBody("registration_quota", nil)}, JoinRetrying, reasonRetryingQuota, "", "", actWait},
		{"429 rate_limited", phaseRegister, hubReply{Status: 429, Body: refusalBody("rate_limited", nil)}, JoinRetrying, reasonRetrying, whyBusy, "", actWait},
		{"500", phaseRegister, hubReply{Status: 500, Body: refusalBody("internal", nil)}, JoinRetrying, reasonRetrying, whyInternal, "", actWait},
		{"503 without a body", phaseCheck, hubReply{Status: 503}, JoinRetrying, reasonRetrying, whyInternal, "", actWait},
		{"no answer", phaseCheck, hubReply{Err: errors.New("dial: refused")}, JoinRetrying, reasonRetrying, whyUnreachable, "", actWait},
		{"410 aid_deactivated", phaseRegister, hubReply{Status: 410, Body: refusalBody("aid_deactivated", nil)}, JoinRefused, reasonDeactivated, "", "", actWait},
		{"409 kel_fork", phaseRegister, hubReply{Status: 409, Body: refusalBody("kel_fork", nil)}, JoinRefused, reasonStopped, "", "", actWait},
		{"409 kel_rollback", phaseRegister, hubReply{Status: 409, Body: refusalBody("kel_rollback", nil)}, JoinRefused, reasonStopped, "", "", actWait},
		{"400 kel_ts_out_of_window", phaseRegister, hubReply{Status: 400, Body: refusalBody("kel_ts_out_of_window", nil)}, JoinRetrying, reasonClockSkew, "", "", actWait},
		{"400 witness_not_this_hub", phaseRegister, hubReply{Status: 400, Body: refusalBody("witness_not_this_hub", nil)}, JoinRefused, reasonSwitch, "", "", actWait},
		{"426 wire_too_old", phaseCheck, hubReply{Status: 426, Body: refusalBody("wire_too_old", map[string]any{"required_wire": 4, "required_anet": "0.4.0"})}, JoinRefused, reasonWireTooOld, "", "", actWait},
		{"400 wire_too_new", phaseCheck, hubReply{Status: 400, Body: refusalBody("wire_too_new", nil)}, JoinRefused, reasonWireTooNew, "", "", actWait},
		{"400 kel_invalid", phaseRegister, hubReply{Status: 400, Body: refusalBody("kel_invalid", nil)}, JoinRefused, reasonHubRefused, "", "kel_invalid", actWait},
		{"400 invite_removed", phaseRegister, hubReply{Status: 400, Body: refusalBody("invite_removed", nil)}, JoinRefused, reasonHubRefused, "", "invite_removed", actWait},
		{"400 bad_request", phaseRegister, hubReply{Status: 400, Body: refusalBody("bad_request", nil)}, JoinRefused, reasonHubRefused, "", "bad_request", actWait},
		{"401 unauthorized", phaseRegister, hubReply{Status: 401, Body: refusalBody("unauthorized", nil)}, JoinRefused, reasonHubRefused, "", "unauthorized", actWait},
		{"401 ts_stale", phaseRegister, hubReply{Status: 401, Body: refusalBody("ts_stale", nil)}, JoinRetrying, reasonClockSkew, "", "", actWait},
		{"401 replayed", phaseRegister, hubReply{Status: 401, Body: refusalBody("replayed", nil)}, JoinRetrying, reasonRetrying, whyBusy, "", actWait},
		{"403 without a code", phaseRegister, hubReply{Status: 403, Body: []byte(`{"error":"no"}`)}, JoinRefused, reasonHubRefused, "", "http_403", actWait},
		{"a 2xx that is no registration", phaseRegister, hubReply{Status: 200, Body: []byte(`{"status":"maybe"}`)}, JoinRetrying, reasonRetrying, whyInternal, "", actWait},
		// A 2xx that states another wire is a wire refusal, never a
		// success (core CORE-V1).
		{"registered over an older wire", phaseRegister, hubReply{Status: 200, Wire: "2", Body: reg.Body}, JoinRefused, reasonWireTooNew, "", "", actWait},
		{"registered over a newer wire", phaseRegister, hubReply{Status: 200, Wire: "4", Body: reg.Body}, JoinRefused, reasonWireTooOld, "", "", actWait},
		{"admitted over a newer wire", phaseCheck, hubReply{Status: 200, Wire: "4", Body: []byte(`{"status":"admitted"}`)}, JoinRefused, reasonWireTooOld, "", "", actWait},
		{"registered over this wire", phaseRegister, hubReply{Status: 200, Wire: "3", Body: reg.Body}, JoinRegistered, reasonRegistered, "", "", actAdopt},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := *freshJoinRecord("https://hub.example", now)
			a := classifyHubReply(c.phase, c.reply)
			got, act := joinStep(r, a, now)
			if act != c.act || got.State != c.state || got.Reason != c.reason || got.Why != c.why || got.HubCode != c.code {
				t.Fatalf("got act %d state %s reason %s why %q code %q; want act %d %s %s %q %q",
					act, got.State, got.Reason, got.Why, got.HubCode, c.act, c.state, c.reason, c.why, c.code)
			}
			if err := got.check(); err != nil {
				t.Fatalf("the record does not pass its own check: %v", err)
			}
			switch {
			case act != actWait:
			case got.State == JoinRefused && got.NextAttemptAt != 0:
				t.Error("a refused join schedules another attempt")
			case got.State != JoinRefused && got.NextAttemptAt <= now.UnixMilli():
				t.Error("a waiting join schedules no attempt")
			}
		})
	}
	// The hub's sentence is kept only beside a code this node does not know
	// (CORE-K20), as one bounded line.
	a := classifyHubReply(phaseRegister, hubReply{Status: 400, Body: []byte(`{"reason":"new_code","error":"line one\nline\ttwo"}`)})
	if a.hubError != "line one line two" || a.code != "new_code" {
		t.Errorf("unknown code: %+v", a)
	}
	if a := classifyHubReply(phaseRegister, hubReply{Status: 400, Body: refusalBody("bad_request", nil)}); a.hubError != "" {
		t.Errorf("a known code kept the hub's sentence: %q", a.hubError)
	}
	// 426 names the release the hub needs ({version}), and is wire_too_old
	// even without a body.
	if r, _ := joinStep(*freshJoinRecord("https://hub.example", now), classifyHubReply(phaseRegister, hubReply{Status: 426,
		Body: refusalBody("wire_too_old", map[string]any{"required_wire": 4, "required_anet": "0.4.0"})}), now); r.Version != "0.4.0" {
		t.Errorf("426: version %q", r.Version)
	}
	if r, _ := joinStep(*freshJoinRecord("https://hub.example", now), classifyHubReply(phaseCheck, hubReply{Status: 426}), now); r.State != JoinRefused || r.Reason != reasonWireTooOld {
		t.Errorf("426 without a body: %s/%s", r.State, r.Reason)
	}
	// A probation end a time cannot hold is no probation (it would wrap to
	// before 1970 in the sentence).
	if r, _ := joinStep(*freshJoinRecord("https://hub.example", now), classifyHubReply(phaseRegister, hubReply{Status: 200,
		Body: []byte(`{"aid":"a","status":"registered","probation_until":18446744073709551615}`)}), now); r.Reason != reasonRegistered || r.ProbationUntil != 0 || r.check() != nil {
		t.Errorf("probation_until beyond int64: %+v", r)
	}
	// The quota's Retry-After decides the next attempt (AD-3).
	r, _ := joinStep(*freshJoinRecord("https://hub.example", now), classifyHubReply(phaseRegisterPow,
		hubReply{Status: 429, RetryAfter: "7200", Body: refusalBody("registration_quota", nil)}), now)
	if r.NextAttemptAt != now.Add(2*time.Hour).UnixMilli() {
		t.Errorf("registration_quota's Retry-After: next attempt at %v", time.UnixMilli(r.NextAttemptAt).Sub(now))
	}
}

// AD-5: once a node registered at the hub, a temporary failure leaves the
// state alone; a definite answer still changes it.
func TestATemporaryFailureAfterRegistrationChangesNothing(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	r := *freshJoinRecord("https://hub.example", now)
	r, act := joinStep(r, classifyHubReply(phaseRegister, hubReply{Status: 200,
		Body: []byte(`{"status":"registered"}`)}), now)
	if act != actAdopt || r.State != JoinRegistered || r.RegisteredAt == 0 {
		t.Fatalf("registered: %+v", r)
	}
	for _, rep := range []hubReply{{Err: errors.New("down")}, {Status: 503}, {Status: 429, Body: refusalBody("rate_limited", nil)},
		{Status: 429, Body: refusalBody("registration_quota", nil)}, {Status: 400, Body: refusalBody("kel_ts_out_of_window", nil)},
		{Status: 403, Body: challengeBody(30)}} {
		got, _ := joinStep(r, classifyHubReply(phaseRegister, rep), now)
		if got.State != JoinRegistered || got.Reason != reasonRegistered {
			t.Errorf("%d %s: a registered node became %s/%s", rep.Status, rep.Body, got.State, got.Reason)
		}
		if got.NextAttemptAt <= now.UnixMilli() {
			t.Errorf("%d: no retry scheduled", rep.Status)
		}
	}
	got, _ := joinStep(r, classifyHubReply(phaseRegister, hubReply{Status: 403, Body: refusalBody("admission_required", nil)}), now)
	if got.State != JoinPendingAdmission {
		t.Errorf("admission revoked: %s", got.State)
	}
	// And after that, a temporary failure leaves pending_admission alone:
	// retrying is a state before the first registration only.
	got2, _ := joinStep(got, classifyHubReply(phaseCheck, hubReply{Status: 503}), now)
	if got2.State != JoinPendingAdmission {
		t.Errorf("after revocation, a temporary failure made %s", got2.State)
	}
	got, _ = joinStep(r, classifyHubReply(phaseRegister, hubReply{Status: 410, Body: refusalBody("aid_deactivated", nil)}), now)
	if got.State != JoinRefused || got.Reason != reasonDeactivated {
		t.Errorf("deactivated: %s/%s", got.State, got.Reason)
	}
}

// --- P-JOIN ---

// scriptHub is a hub whose answers a source of choices decides, recording
// what P-JOIN asserts on.
type scriptHub struct {
	t      testing.TB
	choose func(n int) int
	// checkOK: the check of the current attempt answered admitted or open.
	checkOK bool
	// lastRegisterOK: the last registration was answered 200.
	lastRegisterOK bool
	// timeRefused: a registration was answered kel_ts_out_of_window, after
	// which the identity area signs the rot again (KEL D-36).
	timeRefused   bool
	registrations int
}

var (
	checkReplies = []hubReply{
		{Status: 200, Body: []byte(`{"status":"admitted"}`)},
		{Status: 200, Body: []byte(`{"status":"open"}`)},
		{Status: 403, Body: refusalBody("admission_required", nil)},
		{Status: 403, Body: challengeBody(1)},
		{Status: 429, RetryAfter: "30", Body: refusalBody("rate_limited", nil)},
		{Status: 503},
		{Err: errors.New("unreachable")},
		{Status: 426, Body: refusalBody("wire_too_old", nil)},
		{Status: 400, Body: refusalBody("bad_request", nil)},
	}
	registerReplies = []hubReply{
		{Status: 200, Body: []byte(`{"aid":"a","status":"registered","keys_status":"absent","card_status":"absent"}`)},
		{Status: 200, Body: []byte(`{"aid":"a","status":"registered","keys_status":"absent","card_status":"absent","probation_until":99999999999999,"trial":"pending"}`)},
		{Status: 403, Body: refusalBody("admission_required", nil)},
		{Status: 403, Body: challengeBody(1)},
		{Status: 429, RetryAfter: "3600", Body: refusalBody("registration_quota", nil)},
		{Status: 429, Body: refusalBody("rate_limited", nil)},
		{Status: 500},
		{Err: errors.New("reset")},
		{Status: 410, Body: refusalBody("aid_deactivated", nil)},
		{Status: 409, Body: refusalBody("kel_fork", nil)},
		{Status: 409, Body: refusalBody("kel_rollback", nil)},
		{Status: 400, Body: refusalBody("kel_ts_out_of_window", nil)},
		{Status: 400, Body: refusalBody("witness_not_this_hub", nil)},
		{Status: 400, Body: refusalBody("wire_too_new", nil)},
		{Status: 400, Body: refusalBody("kel_invalid", nil)},
	}
)

func (h *scriptHub) checkOnly(context.Context, *powProof) hubReply {
	r := checkReplies[h.choose(len(checkReplies))]
	a := classifyHubReply(phaseCheck, r)
	h.checkOK = a.kind == ansAdmitted || a.kind == ansOpen
	return r
}

func (h *scriptHub) register(context.Context, []byte, *powProof) hubReply {
	h.registrations++
	r := registerReplies[h.choose(len(registerReplies))]
	a := classifyHubReply(phaseRegister, r)
	h.lastRegisterOK = r.Status == 200 && a.kind == ansRegistered
	if a.kind == ansClockSkew {
		h.timeRefused = true
	}
	return r
}

// guardedHooks are fakeJoinHooks that check P-JOIN (a) at every Prepare,
// and count the calls per (hub, tip) in one process.
type guardedHooks struct {
	*fakeJoinHooks
	hub   *scriptHub
	calls map[string]int
	fail  func(format string, a ...any)
}

func (g *guardedHooks) Prepare(ctx context.Context, hubAID string) ([]byte, error) {
	if !g.hub.checkOK {
		g.fail("P-JOIN (a): Prepare called before the hub confirmed admission")
	}
	if g.hub.timeRefused {
		// The rot's time was refused: asking again is KEL D-36's re-sign.
		g.hub.timeRefused = false
		delete(g.calls, preparedKey(hubAID, g.tip))
	}
	g.calls[preparedKey(hubAID, g.tip)]++
	if g.calls[preparedKey(hubAID, g.tip)] > 1 {
		g.fail("P-JOIN (b): Prepare called again in one process for %s", preparedKey(hubAID, g.tip))
	}
	return g.fakeJoinHooks.Prepare(ctx, hubAID)
}

// pJoinRun drives one random sequence of attempts and restarts and checks
// P-JOIN (a) to (d) (and that the record always passes its own check).
// choose returns a number below n; it decides every hub answer, the
// solver's outcome and when the daemon restarts. It returns the states and
// reasons the sequence went through.
func pJoinRun(choose func(n int) int, steps int, fail func(format string, a ...any)) []string {
	const hubAID = "bafyreihubhubhubhubhubhubhubhubhubhubhubhubhubhubhubhubhubhub"
	now := time.UnixMilli(1_800_000_000_000)
	hooks := newFakeJoinHooks([]byte("kel"))
	hub := &scriptHub{choose: choose}
	rec := *freshJoinRecord("https://hub.example", now)
	var trace []string
	newProcess := func() (*preparedKELs, *guardedHooks) {
		return &preparedKELs{}, &guardedHooks{fakeJoinHooks: hooks, hub: hub, calls: map[string]int{}, fail: fail}
	}
	prepared, g := newProcess()
	for i := 0; i < steps; i++ {
		if choose(8) == 0 {
			// A restart: the record comes back from hub_join.json; what
			// the process remembered does not.
			b, err := json.Marshal(rec)
			if err != nil {
				fail("encode: %v", err)
			}
			r, err := decodeJoinRecord(b)
			if err != nil {
				fail("the record did not come back: %v", err)
				return trace
			}
			rec = *r
			prepared, g = newProcess()
		}
		hub.checkOK = false
		before := rec
		env := joinEnv{hub: hub, hooks: g, hubURL: "https://hub.example", hubAID: hubAID, aid: "a",
			ownKEL: []byte("own"), tip: hooks.tip, prepared: prepared,
			solve: func(context.Context, powChallenge) ([]byte, error) {
				if choose(4) == 0 {
					return nil, errors.New("no time")
				}
				return []byte{1}, nil
			},
			now: func() time.Time { return now }}
		out := runJoinAttempt(context.Background(), rec, env)
		rec = out.rec
		trace = append(trace, rec.State+"/"+rec.Reason)
		if err := rec.check(); err != nil {
			fail("the record does not pass its check: %v (%+v)", err, rec)
		}
		if rec.State == JoinRegistered && before.State != JoinRegistered && !hub.lastRegisterOK {
			fail("P-JOIN (c): registered without an accepted registration")
		}
		if before.RegisteredAt != 0 && rec.State == JoinRetrying {
			fail("P-JOIN (d): retrying after the first registration")
		}
		if rec.State == "recovery_setup_required" {
			fail("P-JOIN (f): recovery_setup_required is not a join state")
		}
		_, _, maxBuilt := hooks.stats()
		if maxBuilt > 1 {
			fail("P-JOIN (b): %d rots built for one key event", maxBuilt)
		}
		now = now.Add(time.Duration(choose(3600)) * time.Second)
	}
	return trace
}

// P-JOIN over random sequences of hub answers and restarts.
func TestPJoin(t *testing.T) {
	for seed := int64(0); seed < 400; seed++ {
		rng := rand.New(rand.NewSource(seed))
		failed := false
		pJoinRun(rng.Intn, 60, func(format string, a ...any) {
			if !failed {
				t.Errorf("seed %d: "+format, append([]any{seed}, a...)...)
			}
			failed = true
		})
	}
}

// fuzzChooser turns fuzz bytes into choices; when they run out, every
// choice is 0.
func fuzzChooser(b []byte) func(n int) int {
	return func(n int) int {
		if len(b) == 0 || n <= 1 {
			return 0
		}
		v := int(b[0])
		b = b[1:]
		return v % n
	}
}

// FuzzJoinSequence is P-JOIN with the fuzzer choosing the hub's answers,
// the solver's outcome and the restarts.
func FuzzJoinSequence(f *testing.F) {
	for seed := int64(0); seed < 8; seed++ {
		b := make([]byte, 128)
		rand.New(rand.NewSource(seed)).Read(b)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		pJoinRun(fuzzChooser(b), 1+len(b)/3, func(format string, a ...any) { t.Fatalf(format, a...) })
	})
}

// --- the decoders ---

// FuzzDecodeJoinRecord: hub_join.json never panics the reader; what it
// accepts passes check and survives a round trip.
func FuzzDecodeJoinRecord(f *testing.F) {
	now := time.UnixMilli(1_800_000_000_000)
	for _, r := range goldenJoinRecords(now) {
		b, _ := json.Marshal(r.rec)
		f.Add(b)
	}
	f.Add([]byte(`{"v":1,"hub":"https://h","hub_aid":"","state":"retrying","reason":"hub_retrying","since":0,"attempts":0,"next_attempt_at":0}`))
	f.Add([]byte(`{"v":1} {}`))
	f.Add([]byte(`{"v":2,"x":1}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := decodeJoinRecord(b)
		if err != nil {
			return
		}
		if err := r.check(); err != nil {
			t.Fatalf("accepted a record that fails its check: %v", err)
		}
		b2, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		r2, err := decodeJoinRecord(b2)
		if err != nil {
			t.Fatalf("an accepted record does not read back: %v\n%s", err, b2)
		}
		b3, _ := json.Marshal(r2)
		if string(b2) != string(b3) {
			t.Fatalf("not stable:\n%s\n%s", b2, b3)
		}
	})
}

// FuzzClassifyHubReply: any hub answer reads as one of the kinds, never
// panics, never puts more than a code's form or a bounded line of
// printable text from the hub into what is shown, and leaves a record that
// passes its check.
func FuzzClassifyHubReply(f *testing.F) {
	for _, r := range append(append([]hubReply{}, checkReplies...), registerReplies...) {
		f.Add(uint8(phaseCheck), r.Status, r.RetryAfter, r.Body)
		f.Add(uint8(phaseRegister), r.Status, r.RetryAfter, r.Body)
	}
	f.Add(uint8(phaseRegisterPow), 403, "1", challengeBody(5))
	f.Add(uint8(phaseRegister), 400, "x", []byte(`{"reason":"\u0000","error":"‮"}`))
	f.Fuzz(func(t *testing.T, phase uint8, status int, retry string, body []byte) {
		p := joinPhase(phase%4 + 1)
		a := classifyHubReply(p, hubReply{Status: status, RetryAfter: retry, Body: body})
		if a.kind < ansAdmitted || a.kind > ansRefused {
			t.Fatalf("kind %d", a.kind)
		}
		if a.code != "" && !reasonCodeRe.MatchString(a.code) {
			t.Fatalf("code %q", a.code)
		}
		if len(a.hubError) > maxHubErrorBytes || !utf8.ValidString(a.hubError) || strings.ContainsFunc(a.hubError, func(r rune) bool {
			return !unicode.IsPrint(r)
		}) {
			t.Fatalf("hub sentence %q", a.hubError)
		}
		if a.kind == ansRefused && !joinReasons[a.refusal] {
			t.Fatalf("refusal %q", a.refusal)
		}
		if c := a.challenge; c != nil && (c.Bits < 1 || c.Bits > regpow.Bits) {
			t.Fatalf("challenge of %d bits", c.Bits)
		}
		if a.retryAfter < 0 || a.retryAfter > joinRetryAfterMax {
			t.Fatalf("retry after %v", a.retryAfter)
		}
		now := time.UnixMilli(1_800_000_000_000)
		r, _ := joinStep(*freshJoinRecord("https://hub.example", now), a, now)
		if err := r.check(); err != nil {
			t.Fatalf("the record does not pass its check: %v", err)
		}
	})
}

// FuzzHubRegisterRequest: a /hub-register body is read or refused with
// one of the reason table's codes, never a panic; what is read is a hub
// address, a 32-byte digest or none, and unprotected only with a digest.
func FuzzHubRegisterRequest(f *testing.F) {
	digest := base64.StdEncoding.EncodeToString(make([]byte, 32))
	for _, s := range []string{
		`{"hub":"https://hub.example"}`,
		`{"hub":"https://hub.example","dry_run":true}`,
		`{"hub":"https://hub.example","name":"n","caps":["a"],"next_digest":"` + digest + `","unprotected":true}`,
		`{"hub":"https://hub.example","token":"x"}`,
		`{"hub":"ftp://x","invite":""}`,
		`{"hub":"https://hub.example","unprotected":true}`,
		`{"hub":"https://hub.example","next_digest":"AAAA"}`,
		`[]`, `{"hub":1}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		req, ref := decodeHubRegisterReq(b)
		if ref != nil {
			switch ref.Code() {
			case reasonConfigInvalid, reasonInviteRemoved:
			default:
				t.Fatalf("refused with %q", ref.Code())
			}
			return
		}
		if checkHubURL(req.Hub) != nil {
			t.Fatalf("accepted hub %q", req.Hub)
		}
		if req.NextDigest != nil && len(req.NextDigest) != nextDigestLen {
			t.Fatalf("digest of %d bytes", len(req.NextDigest))
		}
		if req.Unprotected && req.NextDigest == nil {
			t.Fatal("unprotected without a digest")
		}
	})
}

// The decoder of /hub-register on the cases LEC §2.3.2 names.
func TestHubRegisterRequestRules(t *testing.T) {
	digest := base64.StdEncoding.EncodeToString(make([]byte, 32))
	for _, c := range []struct {
		body, code, key string
	}{
		{`{"hub":"https://hub.example/"}`, "", ""},
		{`{"hub":"https://hub.example","token":"anetinv_x"}`, reasonInviteRemoved, ""},
		{`{"hub":"https://hub.example","invite":""}`, reasonInviteRemoved, ""},
		{`{"hub":"hub.example"}`, reasonConfigInvalid, "hub"},
		{`{"hub":"https://u:p@hub.example"}`, reasonConfigInvalid, "hub"},
		{`{"hub":""}`, reasonConfigInvalid, "hub"},
		{`{"hub":"https://hub.example","next_digest":"` + digest[:10] + `"}`, reasonConfigInvalid, "next_digest"},
		{`{"hub":"https://hub.example","unprotected":true}`, reasonConfigInvalid, "unprotected"},
		{`{"hub":"https://hub.example","next_digest":"` + digest + `","unprotected":true}`, "", ""},
	} {
		req, ref := decodeHubRegisterReq([]byte(c.body))
		switch {
		case c.code == "" && ref != nil:
			t.Errorf("%s: refused %v", c.body, ref)
		case c.code == "" && req.Hub != "https://hub.example":
			t.Errorf("%s: hub %q", c.body, req.Hub)
		case c.code != "" && (ref == nil || ref.Code() != c.code):
			t.Errorf("%s: %v, want %s", c.body, ref, c.code)
		case c.key != "" && ref.Args["key"] != c.key:
			t.Errorf("%s: key %v, want %s", c.body, ref.Args["key"], c.key)
		}
	}
}

// A pow_required challenge is solved with the attempt's context and the
// hub's bits (PAY-1: Solve takes 1 to 22 bits), and the proof goes back in
// base64url.
func TestTheChallengeIsSolvedAndSentBack(t *testing.T) {
	ch := parseChallenge(json.RawMessage(`{"seed":"` + base64.RawURLEncoding.EncodeToString(make([]byte, 32)) + `","bits":8,"not_after":5}`))
	if ch == nil || ch.Bits != 8 || ch.NotAfter != 5 {
		t.Fatalf("challenge %+v", ch)
	}
	for _, bad := range []string{`{"seed":"AAAA","bits":8}`, `{"seed":"` + base64.StdEncoding.EncodeToString(make([]byte, 32)) + `","bits":8}`,
		`{"seed":"` + base64.RawURLEncoding.EncodeToString(make([]byte, 32)) + `","bits":0}`, `null`, `[]`} {
		if parseChallenge(json.RawMessage(bad)) != nil {
			t.Errorf("accepted %s", bad)
		}
	}
	nonce, err := regpow.Solve(context.Background(), "hub", "aid", ch.Seed, ch.Bits)
	if err != nil || !regpow.Check("hub", "aid", ch.Seed, nonce, ch.Bits) {
		t.Fatalf("solve: %v", err)
	}
	p := wirePow(&powProof{Seed: ch.Seed, Nonce: nonce})
	if n, err := base64.RawURLEncoding.Strict().DecodeString(p.Nonce); err != nil || binary.BigEndian.Uint64(n) != binary.BigEndian.Uint64(nonce) {
		t.Fatalf("pow on the wire %+v", p)
	}
}

// listHub answers each check_only and each registration with the next of
// its lists (the last one again once a list runs out).
type listHub struct {
	checks, registers []hubReply
	nc, nr            int
	kels              [][]byte
}

func (h *listHub) checkOnly(context.Context, *powProof) hubReply {
	r := h.checks[min(h.nc, len(h.checks)-1)]
	h.nc++
	return r
}

func (h *listHub) register(_ context.Context, kel []byte, _ *powProof) hubReply {
	h.kels = append(h.kels, kel)
	r := h.registers[min(h.nr, len(h.registers)-1)]
	h.nr++
	return r
}

// KEL D-36 through AD-4: when the hub refuses the time of the registered
// rot (kel_ts_out_of_window), the next attempt asks Prepare again, so the
// identity area can sign the rot anew; after any other failure the next
// attempt registers what Prepare gave, and Prepare is not asked again
// (P-JOIN (b)).
func TestARefusedRotTimeIsPreparedAgain(t *testing.T) {
	const hubAID = "bafyreihubhubhubhubhubhubhubhubhubhubhubhubhubhubhubhubhubhub"
	now := time.UnixMilli(1_800_000_000_000)
	admitted := hubReply{Status: 200, Body: []byte(`{"status":"admitted"}`)}
	accepted := hubReply{Status: 200, Body: []byte(`{"aid":"a","status":"registered","keys_status":"absent","card_status":"absent"}`)}
	for _, c := range []struct {
		name     string
		first    hubReply
		prepares int
	}{
		{"kel_ts_out_of_window", hubReply{Status: 400, Body: refusalBody("kel_ts_out_of_window", nil)}, 2},
		{"503", hubReply{Status: 503}, 1},
		{"no answer", hubReply{Err: errors.New("reset")}, 1},
		{"rate_limited", hubReply{Status: 429, Body: refusalBody("rate_limited", nil)}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			hooks := newFakeJoinHooks([]byte("kel"))
			hub := &listHub{checks: []hubReply{admitted}, registers: []hubReply{c.first, accepted}}
			env := joinEnv{hub: hub, hooks: hooks, hubURL: "https://hub.example", hubAID: hubAID, aid: "a",
				ownKEL: []byte("own"), tip: hooks.tip, prepared: &preparedKELs{},
				now: func() time.Time { return now }}
			out := runJoinAttempt(context.Background(), *freshJoinRecord("https://hub.example", now), env)
			if out.reg != nil || out.rec.State != JoinRetrying {
				t.Fatalf("first attempt: %+v", out.rec)
			}
			out = runJoinAttempt(context.Background(), out.rec, env)
			if out.reg == nil || out.rec.State != JoinRegistered {
				t.Fatalf("second attempt: %+v", out.rec)
			}
			if calls, adopted, _ := hooks.stats(); calls != c.prepares || adopted != 1 {
				t.Fatalf("Prepare asked %d times (want %d), %d adopted", calls, c.prepares, adopted)
			}
			if len(hub.kels) != 2 {
				t.Fatalf("%d registrations", len(hub.kels))
			}
		})
	}
}

// A hub's sentence beside hub_refused is cut to a bounded line in time
// linear in its length: a hub that answers with megabytes of text does not
// hold the join (and the registration lock) for good.
//
// The bound is generous on purpose: under -race on a busy host, decoding the
// 5 MiB JSON body alone takes several seconds, while a cut quadratic in the
// length of 5 MiB takes hours, so 30 seconds still tells the two apart.
func TestAHugeHubSentenceIsCutQuickly(t *testing.T) {
	const quick = 30 * time.Second
	long := strings.Repeat("é", 4<<20) + "\n" + strings.Repeat("x", 1<<20)
	start := time.Now()
	s := cleanHubSentence(long)
	if d := time.Since(start); d > quick {
		t.Fatalf("cleanHubSentence took %v", d)
	}
	if len(s) > maxHubErrorBytes || len(s) < maxHubErrorBytes-utf8.UTFMax || !utf8.ValidString(s) {
		t.Fatalf("cut to %d bytes (valid %v)", len(s), utf8.ValidString(s))
	}
	if got := cleanHubSentence("a\tb\x00c\n d"); got != "a bc d" {
		t.Fatalf("short sentence: %q", got)
	}
	body, _ := json.Marshal(map[string]any{"reason": "new_code", "error": long})
	start = time.Now()
	a := classifyHubReply(phaseRegister, hubReply{Status: 400, Body: body})
	if d := time.Since(start); d > quick || len(a.hubError) > maxHubErrorBytes {
		t.Fatalf("classify took %v, sentence of %d bytes", d, len(a.hubError))
	}
}

// AD-4: when Adopt fails after the hub accepted the registration, the node
// counts as registered (the hub holds the registration) and the loop tries
// again soon; that attempt registers the same rot again and adopts it,
// without asking Prepare for another (P-JOIN (b)).
func TestAFailedAdoptIsRetriedWithTheSameRot(t *testing.T) {
	const hubAID = "bafyreihubhubhubhubhubhubhubhubhubhubhubhubhubhubhubhubhubhub"
	now := time.UnixMilli(1_800_000_000_000)
	hooks := newFakeJoinHooks([]byte("kel"))
	hooks.adoptErr = errors.New("the identity file could not be written")
	hub := &listHub{checks: []hubReply{{Status: 200, Body: []byte(`{"status":"admitted"}`)}},
		registers: []hubReply{{Status: 200, Body: []byte(`{"aid":"a","status":"registered","keys_status":"absent","card_status":"absent"}`)}}}
	env := joinEnv{hub: hub, hooks: hooks, hubURL: "https://hub.example", hubAID: hubAID, aid: "a",
		ownKEL: []byte("own"), tip: hooks.tip, prepared: &preparedKELs{},
		now: func() time.Time { return now }}
	out := runJoinAttempt(context.Background(), *freshJoinRecord("https://hub.example", now), env)
	if out.reg == nil || out.rec.State != JoinRegistered || out.rec.NextAttemptAt <= now.UnixMilli() {
		t.Fatalf("after a failed Adopt: %+v", out.rec)
	}
	hooks.mu.Lock()
	hooks.adoptErr = nil
	hooks.mu.Unlock()
	out = runJoinAttempt(context.Background(), out.rec, env)
	if out.reg == nil || out.rec.State != JoinRegistered {
		t.Fatalf("the retry: %+v", out.rec)
	}
	if calls, adopted, built := hooks.stats(); calls != 1 || adopted != 1 || built != 1 {
		t.Fatalf("%d Prepare, %d Adopt, %d rots built", calls, adopted, built)
	}
	if len(hub.kels) != 2 || string(hub.kels[0]) != string(hub.kels[1]) {
		t.Fatalf("registrations %q", hub.kels)
	}
}

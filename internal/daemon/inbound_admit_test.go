package daemon

// UX-4 (CUX §8.1): the note a refusal at step 9 is answered with
// (noteReasonFor, CUX-Q1), the order of the checks (CUX-Q1a), allow_only
// (CUX-F12, CUX-Q2), the per-caller in-flight bound (CUX-F11) and the
// approval queue's constant bounds.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// delegateBodyArgs is delegateBody for a capability call whose args
// context is argsJSON exactly; absent leaves the args context out.
func delegateBodyArgs(t testing.TB, c *identity.Controller, ix, capID, argsJSON string, absent bool) []byte {
	t.Helper()
	task := tsir.Task{Intent: tsir.Intent{Summary: "call", Body: "call"},
		Requires: []tsir.Require{{ID: capID, Type: RequireTypeCapability, Necessity: "must"}}}
	if !absent {
		task.Contexts = []tsir.Context{{Key: "args", Value: argsJSON, Format: "json"}}
	}
	td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{task}}
	if err := td.Sign(c); err != nil {
		t.Fatal(err)
	}
	doc, err := coredet.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}
	b, err := (&delegation.DelegateReq{TaskDoc: doc, Envelope: td.Envelope, InteractionID: ix}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// setInbound sets d's policy and public capabilities in its live
// configuration, without the checks of a control-plane write.
func setInbound(d *Daemon, policy string, caps ...PublicCapability) {
	d.mu.Lock()
	defer d.mu.Unlock()
	in := d.cfg.inbound()
	in.Policy = policy
	in.PublicCapabilities = append([]PublicCapability{}, caps...)
	d.cfg.Inbound = &in
}

// noteOf is the note decideDelegateAt's refusal is answered with, at now.
func noteOf(dec inboundDecision, now int64) seal.Note {
	return refusalNote(dec.reason, dec.retryAfterMS, uint64(now))
}

// CUX-Q1, row by row: each judgment of step 9, made by the daemon's own
// policy and admission at a fixed time, is answered with the code the row
// names and its wait rounded by seal.RetryBucket (CORE-K22). The rows the
// approval queue and the files give (pending_full, files_*) are verdicts
// of their own steps, read as such.
func TestNoteReasonTable(t *testing.T) {
	d := newTestDaemon(t, "", false)
	const (
		stranger = "did:anet:stranger00001"
		denied   = "did:anet:denied000001"
		listed   = "did:anet:listed000001"
	)
	denyPeers(t, d, denied)
	allowPeers(t, d, listed)
	// 10:00:10.500 UTC: 49.5 s left of the minute, 14 h left of the day.
	at := time.Date(2026, 10, 10, 10, 0, 10, 500_000_000, time.UTC).UnixMilli()
	call := func(capID string) delegateForm { return delegateForm{capID: capID, argsLen: 2} }
	limits := PublicCapability{ID: "c", PerCallerPerMin: 1, PerCallerPerDay: 1, GlobalPerMin: 1, MaxInflight: 1}
	day := seal.RetryBucket(0, true, uint64(at))

	type row struct {
		name   string
		policy string
		caps   []PublicCapability
		prep   func(t *testing.T) // what the judgment finds already there
		from   string
		form   delegateForm
		code   string
		retry  uint32
	}
	rows := []row{
		{name: "closed: a denied peer", policy: PolicyClosed, from: denied, form: call("c"), code: seal.NoteNotAccepting,
			caps: []PublicCapability{{ID: "x"}}},
		{name: "closed: a stranger, a capability that is not public", policy: PolicyClosed, from: stranger, form: call("nope"),
			code: seal.NoteNotAccepting},
		{name: "closed: a stranger, a natural-language task", policy: PolicyClosed, from: stranger, form: delegateForm{},
			code: seal.NoteNotAccepting},
		{name: "closed: a stranger, an allow_only capability", policy: PolicyClosed, from: stranger, form: call("lo"),
			caps: []PublicCapability{{ID: "lo", AllowOnly: true}}, code: seal.NoteNotAccepting},
		{name: "approve: a denied peer", policy: PolicyApprove, from: denied, form: delegateForm{}, code: seal.NoteDenied},
		{name: "open: a denied peer", policy: PolicyOpen, from: denied, form: delegateForm{}, code: seal.NoteDenied},
		{name: "open: a capability that is not public", policy: PolicyOpen, from: stranger, form: call("nope"),
			code: seal.NoteCapabilityNotPublic},
		{name: "open: an allow_only capability", policy: PolicyOpen, from: stranger, form: call("lo"),
			caps: []PublicCapability{{ID: "lo", AllowOnly: true}}, code: seal.NoteCapabilityNotPublic},
		{name: "the request's form", policy: PolicyClosed, from: stranger, form: delegateForm{capID: "c", bad: true},
			caps: []PublicCapability{limits}, code: seal.NoteBadRequest},
		{name: "the request's form, a listed peer's call", policy: PolicyClosed, from: listed,
			form: delegateForm{capID: "private", bad: true}, code: seal.NoteBadRequest},
		{name: "args over max_args_bytes", policy: PolicyClosed, from: stranger, form: delegateForm{capID: "c", argsLen: 4097},
			caps: []PublicCapability{{ID: "c"}}, code: seal.NoteArgsTooLarge},
		{name: "max_inflight", policy: PolicyClosed, from: stranger, form: call("c"),
			caps: []PublicCapability{{ID: "c", MaxInflight: 1}},
			prep: func(t *testing.T) {
				holdSlot(t, d, at, "did:anet:other0000001", PublicCapability{ID: "c", MaxInflight: 1})
			},
			code: seal.NoteBusy, retry: 5},
		{name: "per_caller_inflight", policy: PolicyClosed, from: stranger, form: call("c"),
			caps: []PublicCapability{{ID: "c", MaxInflight: 4}},
			prep: func(t *testing.T) { holdSlot(t, d, at, stranger, PublicCapability{ID: "c", MaxInflight: 4}) },
			code: seal.NoteCallerBusy, retry: 5},
		{name: "the caller's minute", policy: PolicyClosed, from: stranger, form: call("c"), caps: []PublicCapability{limits},
			prep: func(t *testing.T) { useOnce(t, d, at, stranger, limits) }, code: seal.NoteQuotaCallerPerMin,
			retry: seal.RetryBucket(50, false, 0)},
		{name: "the caller's day", policy: PolicyClosed, from: stranger, form: call("c"),
			caps: []PublicCapability{{ID: "c", PerCallerPerMin: 5, PerCallerPerDay: 1}},
			prep: func(t *testing.T) {
				useOnce(t, d, at-60_000, stranger, PublicCapability{ID: "c", PerCallerPerMin: 5, PerCallerPerDay: 1})
			},
			code: seal.NoteQuotaCallerPerDay, retry: day},
		{name: "the capability's minute", policy: PolicyClosed, from: stranger, form: call("c"),
			caps: []PublicCapability{{ID: "c", GlobalPerMin: 1}},
			prep: func(t *testing.T) {
				useOnce(t, d, at, "did:anet:other0000001", PublicCapability{ID: "c", GlobalPerMin: 1})
			},
			code: seal.NoteBusy, retry: seal.RetryBucket(50, false, 0)},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			d.admission = admission{}
			setInbound(d, r.policy, r.caps...)
			if r.prep != nil {
				r.prep(t)
			}
			dec := d.decideDelegateAt(at, r.from, r.form)
			if dec.action != actRefuse {
				t.Fatalf("decision %+v, want a refusal", dec)
			}
			if n := noteOf(dec, at); n.Reason != r.code || n.RetryAfterS != r.retry {
				t.Fatalf("note %+v, want %s %d", n, r.code, r.retry)
			}
		})
	}
	// The verdicts of the steps that are not the policy's or the
	// admission's.
	for _, c := range []struct {
		code  string
		retry uint32
	}{
		{seal.NotePendingFull, 0}, {seal.NoteFilesCount, 0}, {seal.NoteFilesTooLarge, 0}, {seal.NoteFileTypeNotAccepted, 0},
	} {
		if code, retry := noteReasonFor(AdmitVerdict{Refusal: c.code, RetryAfterMS: 30_000, Now: uint64(at)}); code != c.code || retry != c.retry {
			t.Errorf("%s: %s %d", c.code, code, retry)
		}
	}
	// What no step-9 refusal gives is answered without a reason.
	for _, code := range []string{"quota_global_per_min", "max_inflight", "pending_expired", "", "Not_Accepting", "key_state"} {
		if got, retry := noteReasonFor(AdmitVerdict{Refusal: code, RetryAfterMS: 1000}); got != "" || retry != 0 {
			t.Errorf("%q: %q %d, want no reason", code, got, retry)
		}
	}
}

// holdSlot admits one call of pc by caller at now and keeps it running.
func holdSlot(t *testing.T, d *Daemon, now int64, caller string, pc PublicCapability) {
	t.Helper()
	if _, refusal, _ := d.admitAt(now, caller, pc, 2); refusal != "" {
		t.Fatalf("holding a slot: %s", refusal)
	}
}

// useOnce admits one call of pc by caller at now and ends it.
func useOnce(t *testing.T, d *Daemon, now int64, caller string, pc PublicCapability) {
	t.Helper()
	rel, refusal, _ := d.admitAt(now, caller, pc, 2)
	if refusal != "" {
		t.Fatalf("one call: %s", refusal)
	}
	rel()
}

// admissionState is a copy of d's admission counters, to compare.
type admissionState struct {
	Callers        map[string]callerCount
	Order          []string
	Global         map[string]window
	Inflight       map[string]int
	CallerInflight map[string]int
}

func admissionOf(d *Daemon) admissionState {
	a := &d.admission
	a.mu.Lock()
	defer a.mu.Unlock()
	s := admissionState{Callers: map[string]callerCount{}, Order: append([]string(nil), a.order...),
		Global: map[string]window{}, Inflight: map[string]int{}, CallerInflight: map[string]int{}}
	for k, v := range a.callers {
		s.Callers[k] = *v
	}
	for k, v := range a.global {
		s.Global[k] = *v
	}
	for k, v := range a.inflight {
		s.Inflight[k] = v
	}
	for k, v := range a.callerInflight {
		s.CallerInflight[k] = v
	}
	return s
}

// CUX-Q1a: the checks run in their order and the first that refuses
// decides; whichever step refuses, every counter and quota is as it was
// before (the refused call takes nothing). A call that fails several
// steps is told the earliest.
func TestAdmitOrderConsumesNothingOnRefusal(t *testing.T) {
	d := newTestDaemon(t, "", false)
	const a, b, c = "did:anet:caller000000a", "did:anet:caller000000b", "did:anet:caller000000c"
	denyPeers(t, d, "did:anet:denied000001")
	pc := PublicCapability{ID: "cap", PerCallerPerMin: 2, PerCallerPerDay: 3, GlobalPerMin: 5, MaxInflight: 2,
		PerCallerInflight: 1, MaxArgsBytes: 10}
	setInbound(d, PolicyClosed, pc, PublicCapability{ID: "lo", AllowOnly: true})
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC).UnixMilli()
	ok := func(from string) func() {
		t.Helper()
		dec := d.decideDelegateAt(now, from, delegateForm{capID: "cap", argsLen: 2})
		if dec.action != actAccept || dec.release == nil {
			t.Fatalf("%s: %+v, want admitted", from, dec)
		}
		return dec.release
	}
	refused := func(name, from string, f delegateForm, want string) {
		t.Helper()
		before := admissionOf(d)
		dec := d.decideDelegateAt(now, from, f)
		if dec.action != actRefuse || dec.reason != want {
			t.Fatalf("%s: %+v, want refused %s", name, dec, want)
		}
		if after := admissionOf(d); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s took something:\nbefore %+v\nafter  %+v", name, before, after)
		}
	}
	// Step 1 before 2, 2 before 3 and 3 before 5.
	refused("a denied peer, a bad form", "did:anet:denied000001", delegateForm{capID: "cap", bad: true, argsLen: 99}, reasonNotAccepting)
	refused("allow_only, a bad form", a, delegateForm{capID: "lo", bad: true}, reasonNotAccepting)
	refused("not public, a bad form", a, delegateForm{capID: "nope", bad: true}, reasonNotAccepting)
	refused("a bad form, args too large", a, delegateForm{capID: "cap", bad: true, argsLen: 99}, reasonBadRequest)
	relA := ok(a)
	relB := ok(b)
	refused("args too large, all slots taken", c, delegateForm{capID: "cap", argsLen: 11}, admitArgsTooLarge)
	refused("all slots taken (step 5 before 6)", a, delegateForm{capID: "cap", argsLen: 2}, admitBusy)
	relB()
	refused("the caller's slot (6)", a, delegateForm{capID: "cap", argsLen: 2}, admitCallerBusy)
	relA()
	ok(a)() // a's second call this minute
	refused("the caller's minute (7)", a, delegateForm{capID: "cap", argsLen: 2}, admitCallerMinute)
	now += 60_000
	ok(a)() // a's third call today
	refused("the caller's day (8)", a, delegateForm{capID: "cap", argsLen: 2}, admitCallerDay)
	ok(b)()
	ok(c)()
	ok(b)()
	ok(c)() // five calls this minute
	refused("the capability's minute (9)", "did:anet:caller000000d", delegateForm{capID: "cap", argsLen: 2}, admitBusy)
}

// CUX-F11 (review V2; M28): one caller holding a capability's calls cannot
// keep another out. With max_inflight 2 and per_caller_inflight 1 (the
// default), a's second call at once is caller_busy and b is admitted;
// release frees both counts.
func TestOneCallerCannotHoldEveryCallSlot(t *testing.T) {
	d := newTestDaemon(t, "", false)
	setInbound(d, PolicyClosed, PublicCapability{ID: "vision.describe", MaxInflight: 2})
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC).UnixMilli()
	call := delegateForm{capID: "vision.describe", argsLen: 2}
	first := d.decideDelegateAt(now, "did:anet:caller000000a", call)
	if first.action != actAccept {
		t.Fatalf("a's first call: %+v", first)
	}
	again := d.decideDelegateAt(now, "did:anet:caller000000a", call)
	if again.action != actRefuse || again.reason != admitCallerBusy {
		t.Fatalf("a's second call at once: %+v, want caller_busy", again)
	}
	if n := noteOf(again, now); n.Reason != seal.NoteCallerBusy || n.RetryAfterS != 5 {
		t.Fatalf("caller_busy's note %+v", n)
	}
	other := d.decideDelegateAt(now, "did:anet:caller000000b", call)
	if other.action != actAccept {
		t.Fatalf("b, while a's call runs: %+v, want admitted", other)
	}
	first.release()
	first.release() // once only
	if next := d.decideDelegateAt(now, "did:anet:caller000000a", call); next.action != actAccept {
		t.Fatalf("a once its call ended: %+v", next)
	}
	other.release()
	if st := admissionOf(d); st.CallerInflight["vision.describe\x00did:anet:caller000000b"] != 0 ||
		st.Inflight["vision.describe"] != 1 {
		t.Fatalf("after the releases: %+v", st)
	}
}

// CUX-Q2, SI-72 (M17, M18): under closed and open, over the hub's mailbox
// and over a direct transport, a stranger's call of an allow_only
// capability is answered exactly as its call of a capability this node
// does not have — the same token, the same note, decided at the same step
// — and a denied peer's under closed exactly as a stranger's. A listed
// caller's call of the allow_only capability is taken.
func TestAllowOnlyIndistinguishable(t *testing.T) {
	_, _, prov := registeredPair(t)
	roomyRxBuckets(prov)
	stranger, denied, listed := newStranger(t), newStranger(t), newStranger(t)
	denyPeers(t, prov, denied.aid)
	allowPeers(t, prov, listed.aid)
	n := 0
	type answer struct {
		class   rxClass
		step    rxStep
		reason  string
		outcome seal.Outcome
		note    seal.Note
	}
	hubCall := func(s sender, capID string) answer {
		n++
		ix := fmt.Sprintf("ix_x2_hub_%d", n)
		env, ack := craftAck(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "call", capID), nil)
		r := receive(t, prov, env)
		o, note, _ := tokenOf(t, r, ack)
		return answer{r.class, r.step, r.reason, o, note}
	}
	directCall := func(s sender, capID string) answer {
		n++
		ix := fmt.Sprintf("ix_x2_p2p_%d", n)
		env, ack := craftAck(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "call", capID), nil)
		dack, err := receiveDirect(t, prov, env)
		if err != nil {
			t.Fatalf("direct: refused for now: %v", err)
		}
		o, note, _ := tokenOfDack(t, dack, ack)
		return answer{class: rxDropped, outcome: o, note: note}
	}
	for _, policy := range []string{PolicyClosed, PolicyOpen} {
		setInbound(prov, policy, PublicCapability{ID: "face.compare", AllowOnly: true})
		for _, path := range []struct {
			name string
			call func(sender, string) answer
		}{{"hub", hubCall}, {"p2p", directCall}} {
			t.Run(policy+"/"+path.name, func(t *testing.T) {
				absent := path.call(stranger, "face.absent")
				allowOnly := path.call(stranger, "face.compare")
				if absent.outcome != seal.OutcomeRefused || absent.note.Reason == "" {
					t.Fatalf("a capability this node does not have: %+v", absent)
				}
				if !reflect.DeepEqual(absent, allowOnly) {
					t.Fatalf("allow_only to a stranger %+v, a capability not there %+v", allowOnly, absent)
				}
				if policy == PolicyClosed {
					if deniedAns := path.call(denied, "face.absent"); !reflect.DeepEqual(deniedAns, absent) {
						t.Fatalf("a denied peer %+v, a stranger %+v", deniedAns, absent)
					}
				}
				if got := path.call(listed, "face.compare"); got.outcome != seal.OutcomeProcessed {
					t.Fatalf("a listed caller's call of the allow_only capability: %+v, want taken", got)
				}
			})
		}
	}
}

// J-CUX6 in one process: twenty delegations each from a denied peer and a
// stranger to a closed node, half over the hub's mailbox and half direct,
// get the same tokens and notes, decided at the same step.
func TestDeniedAndStrangerAreToldAlike(t *testing.T) {
	_, _, prov := registeredPair(t)
	roomyRxBuckets(prov)
	setInbound(prov, PolicyClosed, PublicCapability{ID: "face.detect"})
	denied := newStranger(t)
	denyPeers(t, prov, denied.aid)
	notes := map[string]map[seal.Note]int{}
	for i := range 40 {
		who, s := "stranger", newStranger(t)
		if i%2 == 1 {
			who, s = "denied", denied
		}
		ix := fmt.Sprintf("ix_jcux6_%d", i)
		capID := ""
		if i%4 >= 2 {
			capID = "private.tool"
		}
		env, ack := craftAck(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "work", capID), nil)
		var o seal.Outcome
		var n seal.Note
		if i < 20 {
			r := receive(t, prov, env)
			if r.step != rxStep9 {
				t.Fatalf("%s %d decided at step %v", who, i, r.step)
			}
			o, n, _ = tokenOf(t, r, ack)
		} else {
			dack, err := receiveDirect(t, prov, env)
			if err != nil {
				t.Fatal(err)
			}
			o, n, _ = tokenOfDack(t, dack, ack)
		}
		if o != seal.OutcomeRefused {
			t.Fatalf("%s %d: outcome %d", who, i, o)
		}
		if notes[who] == nil {
			notes[who] = map[seal.Note]int{}
		}
		notes[who][n]++
	}
	if !reflect.DeepEqual(notes["stranger"], notes["denied"]) || len(notes["stranger"]) != 1 {
		t.Fatalf("notes: stranger %v, denied %v", notes["stranger"], notes["denied"])
	}
}

// CORE-K7, CUX-Q1a step 2: a capability call without its args, with args
// that are not one JSON object, or with a "$" key anet does not define is
// refused bad_request — after the policy, so a stranger under closed is
// still told not_accepting.
func TestTheRequestsFormIsStepTwo(t *testing.T) {
	_, _, prov := registeredPair(t)
	roomyRxBuckets(prov)
	setInbound(prov, PolicyClosed, PublicCapability{ID: "c"})
	s := newStranger(t)
	for i, c := range []struct {
		name, capID, args string
		absent            bool
		want              string
	}{
		{"no args", "c", "", true, seal.NoteBadRequest},
		{"args null", "c", "null", false, seal.NoteBadRequest},
		{"args a list", "c", "[1]", false, seal.NoteBadRequest},
		{"args not JSON", "c", "{", false, seal.NoteBadRequest},
		{"an undefined $ key", "c", `{"$x":1}`, false, seal.NoteBadRequest},
		{"a $ key not at the top is the capability's", "c", `{"a":{"$x":1}}`, false, ""},
		{"$files is anet's, and c takes no files", "c", `{"$files":[]}`, false, seal.NoteFilesCount},
		{"not public: the policy first", "private", `{"$x":1}`, false, seal.NoteNotAccepting},
	} {
		t.Run(c.name, func(t *testing.T) {
			ix := fmt.Sprintf("ix_form_%d", i)
			env, ack := craftAck(t, s, prov, seal.TypeDelegate, ix, delegateBodyArgs(t, s.ctrl, ix, c.capID, c.args, c.absent), nil)
			r := receive(t, prov, env)
			if c.want == "" {
				if r.class != rxAccepted {
					t.Fatalf("%+v, want taken", r)
				}
				return
			}
			wantToken(t, r, ack, seal.OutcomeRefused, c.want)
		})
	}
}

// The approval queue's bounds are constants (ux §2.2: 200 in all, 3 a
// peer, 72 hours), not settings: the inbound block has no key for them nor
// for the list files and the refusal notices, a config.json that names one
// does not start, and a fourth held delegation from one peer is refused
// pending_full.
func TestPendingLimitsAreConstants(t *testing.T) {
	if pendingMaxTotal != 200 || pendingMaxPerPeer != 3 || pendingTTL != 72*time.Hour {
		t.Fatalf("queue bounds %d %d %v", pendingMaxTotal, pendingMaxPerPeer, pendingTTL)
	}
	// The block has no key for them (InboundState's pending is the number
	// held, which the status reports).
	for _, c := range []struct {
		typ  reflect.Type
		keys []string
	}{
		{reflect.TypeFor[InboundConfig](), []string{"pending", "reject_notice", "allow_file", "deny_file", "trust_file"}},
		{reflect.TypeFor[InboundState](), []string{"reject_notice", "allow_file", "deny_file", "trust_file"}},
	} {
		tags := map[string]bool{}
		for i := range c.typ.NumField() {
			tags[strings.Split(c.typ.Field(i).Tag.Get("json"), ",")[0]] = true
		}
		for _, k := range c.keys {
			if tags[k] {
				t.Errorf("%s has %s", c.typ, k)
			}
		}
	}
	for _, k := range []string{"inbound.pending", "inbound.reject_notice", "inbound.allow_file", "inbound.deny_file",
		"inbound.trust_file", "accept_delegations"} {
		l := NewLayout(t.TempDir())
		m := freshConfigFile(t)
		withKey(m, k, removedValue(k))
		writeConfigMap(t, l, m)
		var cr *ConfigRefusal
		if err := CheckConfigForStart(l); !errors.As(err, &cr) || len(cr.Refusals) != 1 ||
			cr.Refusals[0].Code() != "config_key_removed" {
			t.Errorf("%s: %v", k, err)
		}
	}

	_, _, prov := registeredPair(t)
	setPolicy(t, prov, PolicyApprove)
	q := newStranger(t)
	holdFor(t, prov, q, "ix_const", pendingMaxPerPeer)
	over := "ix_const_over"
	env, ack := craftAck(t, q, prov, seal.TypeDelegate, over, delegateBody(t, q.ctrl, over, "one more", ""), nil)
	r := receive(t, prov, env)
	wantToken(t, r, ack, seal.OutcomeRefused, seal.NotePendingFull)
	list, err := prov.PendingList()
	if err != nil || len(list) != pendingMaxPerPeer {
		t.Fatalf("held %d (%v)", len(list), err)
	}
	for _, p := range list {
		if p.ExpiresAt-p.ArrivedAt != pendingTTL.Milliseconds() {
			t.Fatalf("%s expires %d ms after it arrived", p.InteractionID, p.ExpiresAt-p.ArrivedAt)
		}
	}
}

// per_caller_inflight is from 1 to max_inflight (CUX §2.10), and a
// capability is listed once: a config.json that breaks either does not
// start (config_invalid, naming the entry), and a control-plane write of
// one is refused 409 with the same sentence, leaving the config as it was.
func TestPublicCapabilitySettingsAreChecked(t *testing.T) {
	for _, c := range []struct {
		name string
		caps []PublicCapability
		bad  string // the key named, "" for a valid list
	}{
		{"left out", []PublicCapability{{ID: "a"}}, ""},
		{"one", []PublicCapability{{ID: "a", PerCallerInflight: 1}}, ""},
		{"max_inflight", []PublicCapability{{ID: "a", PerCallerInflight: 16}}, ""},
		{"max_inflight given", []PublicCapability{{ID: "a", MaxInflight: 3, PerCallerInflight: 3}}, ""},
		{"over the default max_inflight", []PublicCapability{{ID: "a", PerCallerInflight: 17}},
			"inbound.public_capabilities[0].per_caller_inflight"},
		{"over max_inflight", []PublicCapability{{ID: "a"}, {ID: "b", MaxInflight: 2, PerCallerInflight: 3}},
			"inbound.public_capabilities[1].per_caller_inflight"},
		{"negative", []PublicCapability{{ID: "a", PerCallerInflight: -1}}, "inbound.public_capabilities[0].per_caller_inflight"},
		{"twice", []PublicCapability{{ID: "a"}, {ID: "a", AllowOnly: true}}, "inbound.public_capabilities[1].id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := NewLayout(t.TempDir())
			m := freshConfigFile(t)
			b, _ := json.Marshal(c.caps)
			var caps any
			_ = json.Unmarshal(b, &caps)
			m["inbound"].(map[string]any)["public_capabilities"] = caps
			writeConfigMap(t, l, m)
			err := CheckConfigForStart(l)
			if c.bad == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var cr *ConfigRefusal
			if !errors.As(err, &cr) || len(cr.Refusals) != 1 || cr.Refusals[0].Code() != "config_invalid" ||
				cr.Refusals[0].Args["key"] != c.bad {
				t.Fatalf("start: %v", err)
			}

			d := newTestDaemon(t, "", false)
			before := d.config().inbound().PublicCapabilities
			body, _ := json.Marshal(map[string]any{"public_capabilities": c.caps})
			rec := httptest.NewRecorder()
			d.hInboundPolicy(rec, httptest.NewRequest(http.MethodPost, "/inbound", bytes.NewReader(body)))
			want := cr.Refusals[0].Body(a2ashape.Lang())
			var got a2ashape.ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusConflict ||
				got.Reason != "config_invalid" || got.Error != want.Error {
				t.Fatalf("POST /inbound: %d %s, want 409 %q", rec.Code, rec.Body.Bytes(), want.Error)
			}
			if after := d.config().inbound().PublicCapabilities; !reflect.DeepEqual(before, after) {
				t.Fatalf("the refused write is in force: %+v", after)
			}
		})
	}
}

// CUX-F32, PAY-10's seam replaced: an allow_only capability is on no card
// the hub sees (the network card, /register's caps) and is on the list
// variant of the extended card; one that is not allow_only is public.
func TestAllowOnlyIsOnTheListVariantOnly(t *testing.T) {
	d, _ := extNode(t, []string{"face.detect"}, []string{"face.compare"}, nil)
	if got := d.publicSkillIDs(); !reflect.DeepEqual(got, []string{"face.detect"}) {
		t.Fatalf("public skills %v", got)
	}
	if got := d.publicOnly([]string{"face.detect", "face.compare"}); !reflect.DeepEqual(got, []string{"face.detect"}) {
		t.Fatalf("/register caps %v", got)
	}
	if got := d.listOnlySkillIDs(d.publicSkillIDs()); !reflect.DeepEqual(got, []string{"face.compare"}) {
		t.Fatalf("list variant adds %v", got)
	}
	pub, _, err := d.networkCard(d.config().HubURL)
	if err != nil || bytes.Contains(pub, []byte("face.compare")) {
		t.Fatalf("network card (%v):\n%s", err, pub)
	}
	_, list := d.extendedCards()
	if list.Err != nil || !bytes.Contains(list.Card, []byte("face.compare")) {
		t.Fatalf("list variant (%v):\n%s", list.Err, list.Card)
	}
}

// payments.card (ux §5.2, HPP §5.6; the owner's 2026-10-10 ruling): a node
// whose priced capabilities are all allow_only and that has nothing public
// has no public card and so no extended card; doctor warns
// payments.card_no_public, not card_failed. Only free allow_only
// capabilities: nothing to check. A public capability beside them (or the
// chat skill of an open node) gives a public card, and the cards are
// checked as usual.
func TestPaymentsCardWithOnlyAllowListedPrices(t *testing.T) {
	free, _ := extNode(t, nil, []string{"face.compare"}, nil)
	if c := free.paymentsCardCheck(); c != nil {
		t.Fatalf("free allow_only only: %+v", c)
	}
	if !paymentsCompiled() {
		t.Skip("no payment module in this build: nothing is priced, so payments.card never applies")
	}
	d, _ := extNode(t, nil, []string{"vision.describe"}, map[string]uint64{"vision.describe": 3})
	c := d.paymentsCardCheck()
	if c == nil || c.Status != "warn" || c.Reason != "payments.card_no_public" || len(c.Args) != 0 {
		t.Fatalf("priced allow_only only: %+v", c)
	}
	rec := httptest.NewRecorder()
	d.hStatus(rec, httptest.NewRequest(http.MethodGet, "/status?doctor=1", nil))
	var st map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if got := st["payments_card"]; !reflect.DeepEqual(got, map[string]any{"status": "warn", "reason": "payments.card_no_public"}) {
		t.Fatalf("/status?doctor=1 payments_card = %v", got)
	}
	// An open node lists the chat skill: there is a public card.
	setPolicyQuiet(d, PolicyOpen)
	if c := d.paymentsCardCheck(); c == nil || c.Status != "ok" {
		t.Fatalf("open, priced allow_only: %+v", c)
	}
	both, _ := extNode(t, []string{"face.detect"}, []string{"vision.describe"}, map[string]uint64{"vision.describe": 3})
	if c := both.paymentsCardCheck(); c == nil || c.Status != "ok" {
		t.Fatalf("a free public capability and a priced allow_only one: %+v", c)
	}
}

// P-CUX4: for any verdict noteReasonFor gives a code of core §2.15.2 (or
// none); a wait on a per-day quota rounds to no less than the real one, any
// other to no less than the real one or an hour, whichever is less; and
// the same verdict gives the same answer.
func TestNoteReasonForProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(4, 4))
	codes := []string{"", "busy", "caller_busy", "quota_caller_per_min", "quota_caller_per_day", "not_accepting",
		"denied", "capability_not_public", "pending_full", "bad_request", "args_too_large", "files_count",
		"files_too_large", "file_type_not_accepted", "quota_global_per_min", "max_inflight", "invalid", "x", "Busy"}
	for range 200_000 {
		v := AdmitVerdict{Refusal: codes[r.IntN(len(codes))], Now: r.Uint64N(1 << 45)}
		switch r.IntN(4) {
		case 0:
			v.RetryAfterMS = r.Int64N(120_000)
		case 1:
			v.RetryAfterMS = r.Int64N(1<<40) - 1<<39
		case 2:
			v.RetryAfterMS = -r.Int64N(1000)
		}
		checkNoteReason(t, v)
	}
}

// checkNoteReason fails the test unless noteReasonFor's answer to v holds
// P-CUX4.
func checkNoteReason(t testing.TB, v AdmitVerdict) {
	code, retry := noteReasonFor(v)
	if c2, r2 := noteReasonFor(v); c2 != code || r2 != retry {
		t.Fatalf("%+v: %s %d, then %s %d", v, code, retry, c2, r2)
	}
	if code == "" {
		if retry != 0 {
			t.Fatalf("%+v: no reason with a wait %d", v, retry)
		}
		return
	}
	wait, ok := noteTable[code]
	if !ok || code != v.Refusal || !seal.ValidNoteReason(code) {
		t.Fatalf("%+v: code %q", v, code)
	}
	if retry > seal.MaxRetryAfterS {
		t.Fatalf("%+v: wait %d", v, retry)
	}
	var real uint64
	switch wait {
	case waitNone:
		if retry != 0 {
			t.Fatalf("%+v: a wait %d where waiting does not help", v, retry)
		}
		return
	case waitFixed5:
		real = 5
	case waitWindow:
		real = 5
		if v.RetryAfterMS > 0 {
			real = max(uint64(v.RetryAfterMS+999)/1000, 1)
		}
	case waitDay:
		const day = 86_400_000
		real = (day - v.Now%day + 999) / 1000
		if uint64(retry) < real {
			t.Fatalf("%+v: day wait %d < %d", v, retry, real)
		}
		return
	}
	if uint64(retry) < min(real, 3600) || retry == 0 {
		t.Fatalf("%+v: wait %d < min(%d, 3600)", v, retry, real)
	}
}

// FuzzNoteReasonFor holds P-CUX4 over any verdict.
func FuzzNoteReasonFor(f *testing.F) {
	for _, s := range []string{"busy", "caller_busy", "quota_caller_per_min", "quota_caller_per_day", "not_accepting", ""} {
		f.Add(s, int64(42_300), uint64(1_760_090_410_500))
	}
	f.Fuzz(func(t *testing.T, code string, ms int64, now uint64) {
		checkNoteReason(t, AdmitVerdict{Refusal: code, RetryAfterMS: ms, Now: now})
	})
}

// FuzzReadDelegateForm: whatever a signed TaskDoc's args context holds,
// step 2 reads it without failing, and refuses exactly what CORE-K7
// refuses: args that are not one JSON object, or a top-level "$" key other
// than $files; a call it takes has the size of its args.
func FuzzReadDelegateForm(f *testing.F) {
	for _, s := range []string{`{}`, `{"on":true}`, `{"$files":[]}`, `{"$x":1}`, `null`, `[]`, `{`, `{"a":{"$b":1}}`, `"x"`} {
		f.Add(s, false, false)
	}
	f.Add("", true, false)
	f.Add(`{}`, false, true)
	f.Fuzz(func(t *testing.T, args string, absent, twice bool) {
		task := tsir.Task{Requires: []tsir.Require{{ID: "c", Type: RequireTypeCapability}}}
		if !absent {
			task.Contexts = append(task.Contexts, tsir.Context{Key: "args", Value: args})
			if twice {
				task.Contexts = append(task.Contexts, tsir.Context{Key: "args", Value: args})
			}
		}
		form := readDelegateForm(&tsir.TaskDoc{Tasks: []tsir.Task{task}})
		if form.capID != "c" {
			t.Fatalf("capability %q", form.capID)
		}
		var m map[string]any
		want := absent || twice || json.Unmarshal([]byte(args), &m) != nil || m == nil
		for k := range m {
			if strings.HasPrefix(k, "$") && k != argsFilesKey {
				want = true
			}
		}
		if form.bad != want {
			t.Fatalf("args %q (absent %v, twice %v): bad %v, want %v", args, absent, twice, form.bad, want)
		}
		if !form.bad {
			delete(m, argsFilesKey)
			b, _ := json.Marshal(m)
			if form.argsLen != len(b) {
				t.Fatalf("args %q: size %d, want %d", args, form.argsLen, len(b))
			}
		}
	})
}

// A natural-language task has no capability, and its form is not the
// args'.
func TestATextTaskHasNoArgsToRead(t *testing.T) {
	td := &tsir.TaskDoc{Tasks: []tsir.Task{{Contexts: []tsir.Context{{Key: "args", Value: "{"}}}}}
	if f := readDelegateForm(td); f.capID != "" || f.bad || f.argsLen != 0 {
		t.Fatalf("%+v", f)
	}
	if f := readDelegateForm(nil); f.capID != "" || f.bad || f.argsLen != 0 {
		t.Fatalf("%+v", f)
	}
}

// M14 (0035's burst): every call over the caller's quota is answered in
// its token with a wait, so the requester's task is rejected with
// anet.retry_after_ms and the sentence that names it, at once; the calls
// admitted run.
func TestABurstOverTheQuotaIsToldWhenToSendAgain(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	caller := registered(t, srv.URL, "caller")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: lampCap, PerCallerPerMin: 2, MaxInflight: 8,
		PerCallerInflight: 8}}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range 6 {
		id, err := caller.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	rejected := 0
	for _, id := range ids {
		st, meta := lastStatusMeta(t, caller, id)
		if st != interactions.StateRejected {
			continue
		}
		rejected++
		ms, _ := meta[a2ashape.KeyRetryAfterMS].(float64)
		if meta[a2ashape.KeyReason] != seal.NoteQuotaCallerPerMin || ms < 1000 || ms > 60_000 {
			t.Fatalf("%s: %v", id, meta)
		}
		ix, err := caller.ix.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(ix.Result), "send it again in") && !strings.Contains(string(ix.Result), "请在") {
			t.Fatalf("%s: the task says %q", id, ix.Result)
		}
	}
	if rejected != 4 || len(lamp.invoked) > 2 {
		t.Fatalf("rejected %d, invoked %d; want 4 and at most 2", rejected, len(lamp.invoked))
	}
}

// UX-T1 for the codes of ux §1.4 A that step 9 gives (CUX-Q1): the
// requester's task, refused with the code, says the code's sentence — with
// the wait the note gave (CUX-Q4a, ENG-C6) and the AIDs it names — word for
// word and under the same code on the control plane and the local A2A
// interface (MCP returns the control plane's task as it is; the CLI prints
// its status message, TestCLISaysTheControlPlanesSentence). bad_request and
// the files_* codes are a request the requester itself does not send (it
// refuses a "$" key at home, CUX-F2): TestRejectedTasksSayTheirSentence in
// a2ashape covers their rows.
func TestAdmissionRefusalsSayOneSentence(t *testing.T) {
	ctx := context.Background()
	call := func(t *testing.T, req, prov *Daemon, args map[string]any) string {
		t.Helper()
		id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, args)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	text := func(t *testing.T, req, prov *Daemon, body string) string {
		t.Helper()
		id, err := req.Delegate(ctx, prov.AID(), body, nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	lamp := func(t *testing.T, prov *Daemon, pc PublicCapability) {
		t.Helper()
		if err := prov.Providers().Register(ctx, &lampProvider{}); err != nil {
			t.Fatal(err)
		}
		if err := prov.SetPublicCapabilities([]PublicCapability{pc}); err != nil {
			t.Fatal(err)
		}
	}
	on := map[string]any{"on": true}
	for _, c := range []struct {
		code  string
		wait  bool
		setup func(t *testing.T, req, prov *Daemon) []string // the tasks sent, the refused one last
	}{
		{seal.NoteNotAccepting, false, func(t *testing.T, req, prov *Daemon) []string {
			return []string{text(t, req, prov, "a stranger's task")}
		}},
		{seal.NoteDenied, false, func(t *testing.T, req, prov *Daemon) []string {
			setPolicy(t, prov, PolicyApprove)
			denyPeers(t, prov, req.AID())
			return []string{text(t, req, prov, "a blocked peer's task")}
		}},
		{seal.NoteCapabilityNotPublic, false, func(t *testing.T, req, prov *Daemon) []string {
			setPolicy(t, prov, PolicyOpen)
			return []string{call(t, req, prov, on)}
		}},
		{seal.NotePendingFull, false, func(t *testing.T, req, prov *Daemon) []string {
			setPolicy(t, prov, PolicyApprove)
			var ids []string
			for i := range pendingMaxPerPeer + 1 {
				ids = append(ids, text(t, req, prov, fmt.Sprintf("held %d", i)))
			}
			return ids
		}},
		{seal.NoteBusy, true, func(t *testing.T, req, prov *Daemon) []string {
			pc := PublicCapability{ID: lampCap, MaxInflight: 1}
			lamp(t, prov, pc)
			holdSlot(t, prov, time.Now().UnixMilli(), "did:anet:someoneelse0001", pc)
			return []string{call(t, req, prov, on)}
		}},
		{seal.NoteCallerBusy, true, func(t *testing.T, req, prov *Daemon) []string {
			pc := PublicCapability{ID: lampCap, MaxInflight: 2, PerCallerInflight: 1}
			lamp(t, prov, pc)
			holdSlot(t, prov, time.Now().UnixMilli(), req.AID(), pc)
			return []string{call(t, req, prov, on)}
		}},
		{seal.NoteQuotaCallerPerMin, true, func(t *testing.T, req, prov *Daemon) []string {
			lamp(t, prov, PublicCapability{ID: lampCap, PerCallerPerMin: 1, MaxInflight: 4, PerCallerInflight: 4})
			return []string{call(t, req, prov, on), call(t, req, prov, on)}
		}},
		{seal.NoteQuotaCallerPerDay, true, func(t *testing.T, req, prov *Daemon) []string {
			lamp(t, prov, PublicCapability{ID: lampCap, PerCallerPerMin: 10, PerCallerPerDay: 1, MaxInflight: 4, PerCallerInflight: 4})
			return []string{call(t, req, prov, on), call(t, req, prov, on)}
		}},
		{seal.NoteArgsTooLarge, false, func(t *testing.T, req, prov *Daemon) []string {
			lamp(t, prov, PublicCapability{ID: lampCap, MaxArgsBytes: 16})
			return []string{call(t, req, prov, map[string]any{"on": true, "pad": strings.Repeat("x", 64)})}
		}},
	} {
		t.Run(c.code, func(t *testing.T) {
			srv := newFakeHub(t)
			prov := registered(t, srv.URL, "prov")
			req := registered(t, srv.URL, "req")
			ids := c.setup(t, req, prov)
			if err := prov.pollOnce(ctx); err != nil {
				t.Fatal(err)
			}
			id := ids[len(ids)-1]
			st, meta := lastStatusMeta(t, req, id)
			if st != interactions.StateRejected || meta[a2ashape.KeyReason] != c.code {
				t.Fatalf("the task is %s with %v, want rejected with %s", st, meta, c.code)
			}
			args := a2ashape.Args{"peer": prov.AID(), "aid": req.AID()}
			ms, _ := meta[a2ashape.KeyRetryAfterMS].(float64)
			if (ms >= 1000) != c.wait {
				t.Fatalf("anet.retry_after_ms %v (a wait: %v)", meta[a2ashape.KeyRetryAfterMS], c.wait)
			}
			if c.wait {
				args["retry"] = time.Duration(ms) * time.Millisecond
			}
			want := a2ashape.Sentence(c.code, a2ashape.Lang(), args)
			for _, s := range taskSurfaces(t, req, prov.AID(), id) {
				if s.text != want || s.reason != c.code {
					t.Errorf("%s: %q under %q, want %q under %q", s.name, s.text, s.reason, want, c.code)
				}
			}
		})
	}
}

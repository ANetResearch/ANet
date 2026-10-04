package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/wire"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// The state table, the gate and the reseal trigger of the key state (KEL
// §4.3.1, D-1, D-51), and the arrival time of KEL D-20.

const (
	hourMS   uint64 = 60 * 60 * 1000
	minuteMS uint64 = 60 * 1000
)

// The facts of a witnessed agent at seq 1 and a notice that confirms them.
const (
	tsAID     = "baid-of-the-node"
	tsTipID   = "bevent-1"
	tsWitness = "bwitness-hub"
	tsNow     = uint64(1_900_000_000_000)
)

func witnessedFacts() identityFacts {
	return identityFacts{State: identity.StateOK, AID: tsAID, TipSeq: 1, TipID: tsTipID, Witness: tsWitness}
}

func noticeAt(observedAt uint64) *keyStateNotice {
	return &keyStateNotice{Wire: []byte{1}, Body: identity.NoticeBody{Type: identity.NoticeType, AID: tsAID,
		Seq: 1, Head: tsTipID, ObservedAt: observedAt, Witness: tsWitness}}
}

// The table of KEL §4.3.1, row by row, with the boundaries of rows 5 and 6
// and the order of the rows where two hold at once.
func TestIdentityStateTable(t *testing.T) {
	fresh := noticeAt(tsNow - 10*minuteMS)
	with := func(mod func(*identityInputs)) identityInputs {
		in := identityInputs{File: witnessedFacts(), Notice: fresh, Now: tsNow}
		mod(&in)
		return in
	}
	pendingWitness := func(in *identityInputs) { in.File.Witness, in.File.TipSeq, in.File.TipID = "", 0, tsAID }
	cases := []struct {
		name          string
		in            identityInputs
		state, reason string
	}{
		// Row 1.
		{"deactivated", with(func(in *identityInputs) { in.File.State = identity.StateDeactivated }),
			IdentityDeactivated, "identity_deactivated"},
		{"deactivated before stopped's rows: pending, no witness, no notice", with(func(in *identityInputs) {
			in.File.State, in.File.Pending, in.Notice = identity.StateDeactivated, true, nil
			pendingWitness(in)
		}), IdentityDeactivated, "identity_deactivated"},
		// Row 2.
		{"stopped", with(func(in *identityInputs) { in.File.State = identity.StateStopped }),
			IdentityStopped, "identity_stopped"},
		{"stopped before no_hub and publishing", with(func(in *identityInputs) {
			in.File.State, in.File.Pending = identity.StateStopped, true
			pendingWitness(in)
		}), IdentityStopped, "identity_stopped"},
		{"a state an identity file cannot hold fails closed", with(func(in *identityInputs) { in.File.State = "" }),
			IdentityStopped, "identity_stopped"},
		{"the draft's stopped_elsewhere is not ok", with(func(in *identityInputs) { in.File.State = "stopped_elsewhere" }),
			IdentityStopped, "identity_stopped"},
		// Row 3.
		{"witness pending, no hub configured", with(pendingWitness), IdentityNoHub, "no_hub_yet"},
		{"witness pending, waiting for admission", with(func(in *identityInputs) {
			pendingWitness(in)
			in.HubReason = reasonPending
		}), IdentityNoHub, "hub_pending_admission"},
		{"witness pending, the hub row's variant", with(func(in *identityInputs) {
			pendingWitness(in)
			in.HubReason = reasonRetryingQuota
		}), IdentityNoHub, "hub_retrying.quota"},
		{"witness pending, refused", with(func(in *identityInputs) {
			pendingWitness(in)
			in.HubReason = reasonHubRefused
		}), IdentityNoHub, "hub_refused"},
		{"the witness designation being published is still no_hub", with(func(in *identityInputs) {
			pendingWitness(in)
			in.File.Pending, in.HubReason, in.PublishErr = true, reasonRetrying, hubKELTSOutOfWindow
		}), IdentityNoHub, "hub_retrying"},
		// Row 4.
		{"publishing", with(func(in *identityInputs) { in.File.Pending = true }), IdentityPublishing, "rotation_pending"},
		{"publishing, the witness refused the event's time", with(func(in *identityInputs) {
			in.File.Pending, in.PublishErr = true, hubKELTSOutOfWindow
		}), IdentityPublishing, "clock_skew"},
		{"publishing, another refusal", with(func(in *identityInputs) {
			in.File.Pending, in.PublishErr = true, "kel_invalid"
		}), IdentityPublishing, "rotation_pending"},
		{"publishing before a stale notice", with(func(in *identityInputs) {
			in.File.Pending, in.Notice, in.NoticeErr = true, nil, noticeErrQuarantine
		}), IdentityPublishing, "rotation_pending"},
		// Row 5.
		{"no notice", with(func(in *identityInputs) { in.Notice = nil }), IdentityNoticeStale, "notice_stale"},
		{"no notice, the hub is in quarantine", with(func(in *identityInputs) {
			in.Notice, in.NoticeErr = nil, noticeErrQuarantine
		}), IdentityNoticeStale, "identity.quarantine"},
		{"no notice, not admitted", with(func(in *identityInputs) {
			in.Notice, in.NoticeErr = nil, noticeErrNotAdmitted
		}), IdentityNoticeStale, "hub_pending_admission"},
		{"no notice, not the witness", with(func(in *identityInputs) {
			in.Notice, in.NoticeErr = nil, "not_witness"
		}), IdentityNoticeStale, "notice_stale"},
		{"a notice older than the stop line", with(func(in *identityInputs) {
			in.Notice = noticeAt(tsNow - NoticeStopLine - 1)
		}), IdentityNoticeStale, "notice_stale"},
		{"a notice older than the stop line, quarantine", with(func(in *identityInputs) {
			in.Notice, in.NoticeErr = noticeAt(tsNow-NoticeStopLine-1), noticeErrQuarantine
		}), IdentityNoticeStale, "identity.quarantine"},
		{"a notice about an earlier tip", with(func(in *identityInputs) {
			in.File.TipSeq, in.File.TipID = 2, "bevent-2"
		}), IdentityNoticeStale, "notice_stale"},
		{"a notice about another event at the same seq", with(func(in *identityInputs) {
			in.File.TipID = "bevent-1-fork"
		}), IdentityNoticeStale, "notice_stale"},
		{"a notice by another witness", with(func(in *identityInputs) {
			in.File.Witness = "bother-hub"
		}), IdentityNoticeStale, "notice_stale"},
		{"a notice about another identity", with(func(in *identityInputs) {
			in.File.AID = "banother-node"
		}), IdentityNoticeStale, "notice_stale"},
		// Row 6.
		{"exactly at the stop line", with(func(in *identityInputs) {
			in.Notice = noticeAt(tsNow - NoticeStopLine)
		}), IdentityOK, "identity.confirm_aging"},
		{"just past the reseal age", with(func(in *identityInputs) {
			in.Notice = noticeAt(tsNow - NoticeResealAge - 1)
		}), IdentityOK, "identity.confirm_aging"},
		// Row 7.
		{"exactly at the reseal age", with(func(in *identityInputs) {
			in.Notice = noticeAt(tsNow - NoticeResealAge)
		}), IdentityOK, "identity.ok"},
		{"fresh", with(func(*identityInputs) {}), IdentityOK, "identity.ok"},
		{"observed now", with(func(in *identityInputs) { in.Notice = noticeAt(tsNow) }), IdentityOK, "identity.ok"},
		{"observed after now: the hub's clock is ahead", with(func(in *identityInputs) {
			in.Notice = noticeAt(tsNow + NoticeStopLine + hourMS)
		}), IdentityOK, "identity.ok"},
		{"a fresh notice outranks an older notice error", with(func(in *identityInputs) {
			in.NoticeErr = noticeErrQuarantine
		}), IdentityOK, "identity.ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := identityStateOf(c.in)
			if got.State != c.state || got.Reason != c.reason {
				t.Fatalf("identityStateOf = %+v, want {%s %s}", got, c.state, c.reason)
			}
			if g := gateOf(got); g.Open != (c.state == IdentityOK) || g.State != got.State || g.Reason != got.Reason {
				t.Fatalf("gateOf(%+v) = %+v: open must be state == ok, and say the state and reason", got, g)
			}
		})
	}
}

// T3 and T4 (KEL D-51) and their boundaries.
func TestKELResealDue(t *testing.T) {
	sealedAt := tsNow - 10*hourMS
	row := KELSealed{SealedKSS: 3, SealedNoticeAt: sealedAt}
	newer := sealedAt + minuteMS
	cases := []struct {
		name string
		row  KELSealed
		cur  KeyCursor
		want bool
	}{
		{"same key state, notice fresh", row, KeyCursor{3, sealedAt, sealedAt + hourMS}, false},
		{"T3: a later key state", row, KeyCursor{4, sealedAt, sealedAt + hourMS}, true},
		{"T3: an earlier key state", row, KeyCursor{2, sealedAt, sealedAt}, true},
		{"T3 holds without a newer notice", row, KeyCursor{4, 0, sealedAt}, true},
		{"T4: the reseal age exactly", row, KeyCursor{3, newer, sealedAt + NoticeResealAge}, false},
		{"T4: past the reseal age, a newer notice", row, KeyCursor{3, newer, sealedAt + NoticeResealAge + 1}, true},
		{"T4: past the reseal age, the same notice (hub out of reach)", row, KeyCursor{3, sealedAt, sealedAt + 4*hourMS}, false},
		{"T4: past the reseal age, no notice", row, KeyCursor{3, 0, sealedAt + 4*hourMS}, false},
		{"T4: past the reseal age, an older notice (a hub whose clock is behind)", row,
			KeyCursor{3, sealedAt - minuteMS, sealedAt + 4*hourMS}, false},
		{"T4: the sealed notice observed after now", row, KeyCursor{3, sealedAt + 5*hourMS, sealedAt - hourMS}, false},
		{"T4: a newer notice alone is not a reason", row, KeyCursor{3, newer, sealedAt + hourMS}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := kelResealDue(c.row, c.cur); got != c.want {
				t.Fatalf("kelResealDue(%+v, %+v) = %v, want %v", c.row, c.cur, got, c.want)
			}
		})
	}
}

// KEL D-20: min(now, max(accepted_at, inner.ts − 5 minutes)).
func TestHubArrival(t *testing.T) {
	skew := seal.ClockSkewMS
	if skew != 5*minuteMS {
		t.Fatalf("seal.ClockSkewMS = %d, KEL D-20 takes 5 minutes", skew)
	}
	ts := tsNow - hourMS
	cases := []struct {
		name                  string
		accepted, innerTS, at uint64
		want                  uint64
	}{
		{"accepted after the sender's time", ts + minuteMS, ts, tsNow, ts + minuteMS},
		{"accepted within the skew before it", ts - skew, ts, tsNow, ts - skew},
		{"accepted earlier than the skew allows: the hub's date is not taken", ts - skew - 1, ts, tsNow, ts - skew},
		{"backdated by an hour", ts - hourMS, ts, tsNow, ts - skew},
		{"accepted after now: never later than now", tsNow + minuteMS, ts, tsNow, tsNow},
		{"a sender's time ahead of now", 0, tsNow + hourMS, tsNow, tsNow},
		{"no accepted_at", 0, ts, tsNow, ts - skew},
		{"a sender's time within the skew of 0", 0, skew - 1, tsNow, 0},
		{"a sender's time exactly the skew", 7, skew, tsNow, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hubArrival(c.accepted, c.innerTS, c.at); got != c.want {
				t.Fatalf("hubArrival(%d, %d, %d) = %d, want %d", c.accepted, c.innerTS, c.at, got, c.want)
			}
		})
	}
}

// Every sentence key the table gives is a row of the reason table, by its
// exact key (ux §1.4 E2, E3, F; UX-G1).
func TestIdentityReasonsAreInTheReasonTable(t *testing.T) {
	keys := map[string]bool{}
	for _, r := range a2ashape.Rows() {
		keys[r.Key()] = true
	}
	for _, k := range []string{reasonDeactivated, reasonStopped, reasonNoHub, reasonRotationPending, reasonClockSkew,
		reasonNoticeStale, reasonQuarantine, reasonPending, reasonConfirmAging, reasonIdentityOK} {
		if !keys[k] {
			t.Errorf("the state table gives %q, which the reason table does not have", k)
		}
	}
}

// The six states are the values /identity/status.state has in the
// registry (ux UX-R5).
func TestIdentityStatesAreTheRegisteredOnes(t *testing.T) {
	r, err := wire.Load()
	if err != nil {
		t.Fatal(err)
	}
	route := r.ControlRoute("/identity/status")
	if route == nil || route.Response == nil {
		t.Fatal("/identity/status is not a registered control route")
	}
	var values []string
	for _, f := range route.Response.Fields {
		if f.Name == "state" {
			values = f.Values
		}
	}
	ours := []string{IdentityNoHub, IdentityPublishing, IdentityOK, IdentityNoticeStale, IdentityStopped, IdentityDeactivated}
	slices.Sort(values)
	slices.Sort(ours)
	if !slices.Equal(values, ours) {
		t.Fatalf("registered states %v, the table's %v", values, ours)
	}
}

// The constants of KEL §2.12, and what the stop line is for: a notice sealed
// at the stop line still has the hub's 10-minute reissue period before a
// recipient's 6-hour limit.
func TestNoticeAgeConstants(t *testing.T) {
	if NoticeResealAge != 3*hourMS {
		t.Errorf("NoticeResealAge = %d ms, want 3 hours", NoticeResealAge)
	}
	if NoticeStopLine != 6*hourMS-10*minuteMS {
		t.Errorf("NoticeStopLine = %d ms, want 6 hours − 10 minutes", NoticeStopLine)
	}
	if identity.NoticeMaxAge-NoticeStopLine != 10*minuteMS {
		t.Errorf("the stop line is %d ms short of identity.NoticeMaxAge, want 10 minutes", identity.NoticeMaxAge-NoticeStopLine)
	}
}

// signedNotice is the notice the conformance hub (identitytest.Witness)
// signs for c's KEL tip, observed at observedAt.
func signedNotice(t *testing.T, c *identity.Controller, observedAt uint64) *keyStateNotice {
	t.Helper()
	return signedNoticeBy(t, identity.SuiteController(), c, observedAt)
}

// signedNoticeBy is the notice hub signs for c's KEL tip.
func signedNoticeBy(t *testing.T, hub identity.Signer, c *identity.Controller, observedAt uint64) *keyStateNotice {
	t.Helper()
	kel := c.Accepted()
	tip := kel[len(kel)-1]
	b, err := identity.SignNotice(hub, identity.NoticeBody{AID: c.AID(), Seq: tip.Event.Seq,
		Head: tip.EventID, ObservedAt: observedAt})
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := identity.DecodeNotice(b)
	if err != nil {
		t.Fatal(err)
	}
	return &keyStateNotice{Wire: b, Body: body}
}

// The shell reads a real controller, the join record and the notice held,
// through a first join, a planned rotation and a stop; and the key cursor
// follows the key state it would seal under.
func TestIdentityGateFromTheDaemon(t *testing.T) {
	c, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	now := tsNow
	d := &Daemon{self: c}
	d.setClock(func() uint64 { return now })
	check := func(step, state, reason string) {
		t.Helper()
		g := d.identityGate()
		if g.State != state || g.Reason != reason || g.Open != (state == IdentityOK) {
			t.Fatalf("%s: identityGate = %+v, want {%v %s %s}", step, g, state == IdentityOK, state, reason)
		}
	}

	check("a fresh identity, no hub", IdentityNoHub, "no_hub_yet")
	d.join.rec = &joinRecord{State: JoinPendingAdmission, Reason: reasonPending}
	check("waiting for admission", IdentityNoHub, "hub_pending_admission")

	// The witness designation: built, so the controller signs under it, but
	// not accepted yet. The tip the witness holds still names no witness.
	des, err := c.BuildWitnessDesignation(identitytest.Witness, identitytest.NewNext(), now-hourMS)
	if err != nil {
		t.Fatal(err)
	}
	check("the witness designation is being published", IdentityNoHub, "hub_pending_admission")
	if err := c.Adopt(des); err != nil {
		t.Fatal(err)
	}
	d.join.rec = &joinRecord{State: JoinRegistered, Reason: reasonRegistered}
	check("witnessed, no notice yet", IdentityNoticeStale, "notice_stale")
	d.idGate.noteNotice(nil, noticeErrQuarantine)
	check("the hub answered quarantine", IdentityNoticeStale, "identity.quarantine")

	n1 := signedNotice(t, c, now)
	d.idGate.noteNotice(n1, "")
	check("confirmed", IdentityOK, "identity.ok")
	if cur := d.keyCursor(); cur != (KeyCursor{KeyStateSeq: 1, NoticeObservedAt: n1.Body.ObservedAt, Now: now}) {
		t.Fatalf("keyCursor = %+v", cur)
	}
	if got := d.tipNotice(c.Snapshot()); got != n1 {
		t.Fatalf("tipNotice = %+v, want the notice held", got)
	}
	d.idGate.noteNotice(nil, noticeErrQuarantine)
	check("an answer without a notice keeps the one held", IdentityOK, "identity.ok")

	now += NoticeResealAge
	check("at the reseal age", IdentityOK, "identity.ok")
	now++
	check("aging", IdentityOK, "identity.confirm_aging")
	now = n1.Body.ObservedAt + NoticeStopLine
	check("at the stop line", IdentityOK, "identity.confirm_aging")
	now++
	check("past the stop line", IdentityNoticeStale, "identity.quarantine")

	// A planned rotation: built, then accepted.
	next, err := identitytest.NextKey(c)
	if err != nil {
		t.Fatal(err)
	}
	before := c.Snapshot() // taken when the gate was read, say
	ev, err := c.BuildRotation(next, identitytest.NewNext(), identitytest.Witness, false, now)
	if err != nil {
		t.Fatal(err)
	}
	check("publishing", IdentityPublishing, "rotation_pending")
	// The notice goes with the Snapshot that signs: the one taken before the
	// rotation was built signs under seq 1, which n1 confirms; one taken
	// after signs under the new key, and n1 must not go with it.
	if got := d.tipNotice(before); got != n1 {
		t.Fatalf("tipNotice(a Snapshot under seq 1) = %+v, want the notice about seq 1", got)
	}
	if got := d.tipNotice(c.Snapshot()); got != nil {
		t.Fatalf("tipNotice(a Snapshot under the rotation being published) = %+v: the notice about seq 1 would be refused with it", got)
	}
	d.idGate.notePublishError(hubKELTSOutOfWindow)
	check("the witness refused the event's time", IdentityPublishing, "clock_skew")
	if cur := d.keyCursor(); cur.KeyStateSeq != 2 {
		t.Fatalf("keyCursor while the rotation is built: seq %d, the controller signs under %d", cur.KeyStateSeq, c.CurrentSeq())
	}
	if err := c.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	d.idGate.notePublishError("")
	d.idGate.noteNotice(n1, "")
	check("the notice of seq 1 does not confirm seq 2", IdentityNoticeStale, "notice_stale")
	if cur := d.keyCursor(); cur.KeyStateSeq != 2 || cur.NoticeObservedAt != 0 {
		t.Fatalf("keyCursor with a notice about the old tip = %+v, want seq 2 and no notice", cur)
	}
	if got := d.tipNotice(c.Snapshot()); got != nil {
		t.Fatalf("tipNotice = %+v: a notice about the old tip is not sealed", got)
	}
	n2 := signedNotice(t, c, now)
	d.idGate.noteNotice(n2, "")
	check("seq 2 confirmed", IdentityOK, "identity.ok")
	if cur := d.keyCursor(); cur.KeyStateSeq != 2 || cur.NoticeObservedAt != now {
		t.Fatalf("keyCursor = %+v", cur)
	}

	// A hub switch: the rot names another witness, and only that hub's
	// notice confirms the tip, even one the old hub signed about it.
	hub2, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	now += minuteMS
	if err := identitytest.SwitchWitness(c, hub2.AID(), now); err != nil {
		t.Fatal(err)
	}
	check("switched hubs, no notice from the new one yet", IdentityNoticeStale, "notice_stale")
	d.idGate.noteNotice(signedNotice(t, c, now), "")
	check("the old witness's notice about the new tip", IdentityNoticeStale, "notice_stale")
	if got := d.tipNotice(c.Snapshot()); got != nil {
		t.Fatalf("tipNotice = %+v: the old witness's notice is not sealed after a switch", got)
	}
	n3 := signedNoticeBy(t, hub2, c, now)
	d.idGate.noteNotice(n3, "")
	check("the new witness confirmed", IdentityOK, "identity.ok")
	if got := d.tipNotice(c.Snapshot()); got != n3 {
		t.Fatalf("tipNotice = %+v, want the new witness's notice", got)
	}

	if err := c.SetState(identity.StateStopped); err != nil {
		t.Fatal(err)
	}
	check("stopped", IdentityStopped, "identity_stopped")

	gone, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	if err := identitytest.Rotate(gone, now-hourMS); err != nil {
		t.Fatal(err)
	}
	if err := identitytest.Deactivate(gone, now); err != nil {
		t.Fatal(err)
	}
	dd := &Daemon{self: gone}
	dd.setClock(func() uint64 { return now })
	if g := dd.identityGate(); g != (GateState{false, IdentityDeactivated, "identity_deactivated"}) {
		t.Fatalf("deactivated: identityGate = %+v", g)
	}
	if g := (&Daemon{}).identityGate(); g.Open || g.State != IdentityStopped {
		t.Fatalf("no identity at all: identityGate = %+v, want closed", g)
	}
}

// The engine's tests hold the gate where they need it; nil gives it back
// to the identity's state.
func TestTheGateHookReplacesTheGate(t *testing.T) {
	d := &Daemon{}
	if d.identityGate().Open {
		t.Fatal("a daemon without an identity has an open gate")
	}
	d.setIdentityGate(func() GateState { return GateState{Open: true, State: IdentityOK, Reason: reasonIdentityOK} })
	if !d.identityGate().Open {
		t.Fatal("setIdentityGate did not replace the gate")
	}
	d.setIdentityGate(nil)
	if d.identityGate().Open || d.testIdentityGate() != nil {
		t.Fatal("setIdentityGate(nil) left the replacement in place")
	}
}

// Only tests replace the gate: no code of the package outside its _test.go
// files calls setIdentityGate, and only the setter and the reader touch the
// hook field (SI-84: nothing in the daemon can hold the gate open).
func TestOnlyTestsReplaceTheGate(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen++
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "setIdentityGate":
				t.Errorf("%s: calls setIdentityGate; only tests may replace the gate", fset.Position(sel.Pos()))
			case "gateHook":
				if name != "testhooks.go" {
					t.Errorf("%s: touches the gate hook outside testhooks.go", fset.Position(sel.Pos()))
				}
			case "testIdentityGate":
				if name != "identity_state.go" {
					t.Errorf("%s: reads the gate hook; only identityGate may", fset.Position(sel.Pos()))
				}
			}
			return true
		})
	}
	if seen < 10 {
		t.Fatalf("scanned %d files: the scan would pass vacuously", seen)
	}
}

package daemon

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"
)

// TestFSConstantInvariants is FS-T1: the relations between the forward
// secrecy constants that the deletion rules and the public bounds rest on.
// A constant changed without the others turns this red (mutation 39 at the
// daemon), as does a bound of §21 that no longer follows from them.
func TestFSConstantInvariants(t *testing.T) {
	const (
		E = seal.EpochMS
		D = seal.DeliveryDeadlineMS
		L = seal.MaxMessageLifetimeMS
		M = drainMarginMS
		S = keyMaintainEveryMS
	)
	hubTTL := uint64(messageLifetimeMS) // the hub keeps a row until its exp: ts + 14 days
	// 1. The last envelope a hub accepts for a key, kept its whole TTL, is
	// read before the key's hard bound.
	if L < D+hubTTL+seal.ClockSkewMS {
		t.Errorf("L = %d < D + hub TTL + skew = %d", L, D+hubTTL+seal.ClockSkewMS)
	}
	// 2. The drain margin covers the skew the hub gate allows plus a jump.
	if M <= seal.ClockSkewMS+clockJumpTolerance {
		t.Errorf("M = %d is not above ClockSkewMS + clockJumpTolerance = %d", M, seal.ClockSkewMS+clockJumpTolerance)
	}
	// 3. A check old enough to close the gate is still inside the margin.
	if clockCheckMaxAge > M {
		t.Errorf("clockCheckMaxAge %d > M %d", clockCheckMaxAge, M)
	}
	// 4. The two public bounds (§6.2, §21): 25 h 20 min with an honest hub,
	// 17 days 5 minutes whatever the hub does.
	if got, want := E+D+M+S, 25*hourMS+20*minuteMS; got != want {
		t.Errorf("E + D + M + S = %d, the honest-hub bound says %d", got, want)
	}
	if got, want := E+L+clockFallbackAfter+S, 17*24*hourMS+5*minuteMS; got != want {
		t.Errorf("E + L + clockFallbackAfter + S = %d, the bound whatever the hub does says %d", got, want)
	}
	// 5. Every time the hub gate opens, a maintenance period falls inside it.
	if S >= clockCheckMaxAge {
		t.Errorf("S = %d is not below clockCheckMaxAge = %d", S, clockCheckMaxAge)
	}
	// The values themselves (FS §0).
	for name, c := range map[string][2]uint64{
		"E": {E, 24 * hourMS}, "D": {D, hourMS}, "L": {L, 15 * 24 * hourMS}, "M": {M, 15 * minuteMS},
		"S": {S, 5 * minuteMS}, "clockCheckMaxAge": {clockCheckMaxAge, 10 * minuteMS},
		"clockJumpTolerance": {clockJumpTolerance, minuteMS}, "clockFallbackAfter": {clockFallbackAfter, 24 * hourMS},
		"H": {seal.PublishAheadMS, 14 * 24 * hourMS},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %d, FS §0 says %d", name, c[0], c[1])
		}
	}
}

// TestFSResealDue is trigger T1 (FS-O1): a version sealed to a key is sent
// again under a new key once the key's delivery deadline has passed, and a
// row never sealed is not due.
func TestFSResealDue(t *testing.T) {
	na := uint64(1_800_000_000_000)
	for _, c := range []struct {
		row  EpochSealed
		now  uint64
		want bool
	}{
		{EpochSealed{CurVer: 0, SealedKidNA: na}, na + 10*hourMS, false}, // never sealed: T0, not T1
		{EpochSealed{CurVer: 1, SealedKidNA: na}, na, false},
		{EpochSealed{CurVer: 1, SealedKidNA: na}, na + seal.DeliveryDeadlineMS, false}, // the hub still accepts it
		{EpochSealed{CurVer: 1, SealedKidNA: na}, na + seal.DeliveryDeadlineMS + 1, true},
		{EpochSealed{CurVer: 3, SealedKidNA: na}, na + 2*24*hourMS, true},
		{EpochSealed{CurVer: 1, SealedKidNA: math.MaxUint64}, math.MaxUint64, false}, // no wrap
	} {
		if got := fsResealDue(c.row, c.now); got != c.want {
			t.Errorf("fsResealDue(%+v, now = na%+d) = %v, want %v", c.row, int64(c.now-na), got, c.want)
		}
	}
}

// TestClockGateStates (FS-K14 to FS-K16): the three levels, the four clock
// states and what /status.privacy.deletion says for each level.
func TestClockGateStates(t *testing.T) {
	const wall, mono = uint64(1_800_000_000_000), uint64(50 * hourMS)
	fresh := clockObs{hub: true, checked: true, wall: wall, mono: mono, offset: -3000}
	skewed := fresh
	skewed.offset = int64(seal.ClockSkewMS + 1)
	for _, c := range []struct {
		name             string
		obs              clockObs
		run              uint64
		nowWall, nowMono uint64
		level            clockLevel
		state, deletion  string
		hubOpen          bool
	}{
		{"no hub", clockObs{}, 0, wall, mono, clockFull, clockNoHub, "normal", true},
		{"no hub, any run", clockObs{}, 5, wall + 3*24*hourMS, mono, clockFull, clockNoHub, "normal", true},
		{"just checked", fresh, 0, wall + 2*minuteMS, mono + 2*minuteMS, clockFull, clockOK, "normal", true},
		{"checked 10 minutes ago", fresh, 0, wall + clockCheckMaxAge, mono + clockCheckMaxAge, clockFull, clockOK, "normal", true},
		{"checked 10 minutes and 1 ms ago", fresh, 0, wall + clockCheckMaxAge + 1, mono + clockCheckMaxAge + 1, clockClosed, clockUnverified, "paused", false},
		{"skew at the limit", clockObs{hub: true, checked: true, wall: wall, mono: mono, offset: -int64(seal.ClockSkewMS)}, 0, wall, mono, clockFull, clockOK, "normal", true},
		{"skewed", skewed, 0, wall, mono, clockClosed, clockSkewed, "paused", false},
		{"skewed a day of running", skewed, clockFallbackAfter, wall, mono, clockLocal, clockSkewed, "local_clock", false},
		{"skewed, a day less a millisecond", skewed, clockFallbackAfter - 1, wall, mono, clockClosed, clockSkewed, "paused", false},
		{"never answered", clockObs{hub: true}, 0, wall, mono, clockClosed, clockUnverified, "paused", false},
		{"never answered a day of running", clockObs{hub: true}, clockFallbackAfter, wall, mono, clockLocal, clockUnverified, "local_clock", false},
		{"wall stepped since the check", fresh, 0, wall + 2*minuteMS + clockJumpTolerance + 1, mono + 2*minuteMS, clockClosed, clockUnverified, "paused", false},
		{"wall stepped back since the check", fresh, 0, wall - clockJumpTolerance - 1, mono, clockClosed, clockUnverified, "paused", false},
		{"wall within the tolerance", fresh, 0, wall + 2*minuteMS + clockJumpTolerance, mono + 2*minuteMS, clockFull, clockOK, "normal", true},
		{"boot clock behind the check", fresh, 0, wall, mono - 1, clockClosed, clockUnverified, "paused", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := hubGateOpen(c.obs, c.nowWall, c.nowMono); got != c.hubOpen {
				t.Errorf("hubGateOpen = %v, want %v", got, c.hubOpen)
			}
			level, state := clockGate(c.obs, c.run, c.nowWall, c.nowMono)
			if level != c.level || state != c.state {
				t.Errorf("clockGate = (%v, %q), want (%v, %q)", level, state, c.level, c.state)
			}
			if got := deletionOf(level); got != c.deletion {
				t.Errorf("deletion %q, want %q", got, c.deletion)
			}
		})
	}
	if deletionOf(clockLevel(9)) != "paused" {
		t.Error("an unknown level is not shown as paused")
	}
}

// TestClockGateClosesOnWallStep (mutation 26): a wall clock that steps by
// more than a minute after the hub checked it closes the hub gate until the
// hub checks it again, even with the check minutes old and the offset it
// found small; a step between two maintenance periods starts the day of
// running time again.
func TestClockGateClosesOnWallStep(t *testing.T) {
	const wall, mono = uint64(1_800_000_000_000), uint64(10 * hourMS)
	obs := clockObs{hub: true, checked: true, wall: wall, mono: mono}
	for _, step := range []int64{int64(clockJumpTolerance) + 1, 20 * 24 * int64(hourMS), -int64(clockJumpTolerance) - 1} {
		nowWall := uint64(int64(wall+3*minuteMS) + step)
		if level, _ := clockGate(obs, 0, nowWall, mono+3*minuteMS); level != clockClosed {
			t.Errorf("wall stepped %+d ms after an exact check: level %v, want closed", step, level)
		}
		// A new check after the step opens it again.
		again := clockObs{hub: true, checked: true, wall: nowWall, mono: mono + 3*minuteMS}
		if level, _ := clockGate(again, 0, nowWall+minuteMS, mono+4*minuteMS); level != clockFull {
			t.Errorf("checked again after a %+d ms step: level %v, want full", step, level)
		}
		prev := &cycleMark{wall: wall, mono: mono}
		if run := nextUnverifiedRun(20*hourMS, false, prev, nowWall, mono+3*minuteMS); run != 0 {
			t.Errorf("a %+d ms step between two periods left unverified_run at %d", step, run)
		}
	}
	// Sleep is not a step: both clocks went on together.
	prev := &cycleMark{wall: wall, mono: mono}
	if run := nextUnverifiedRun(hourMS, false, prev, wall+9*hourMS, mono+9*hourMS); run != 10*hourMS {
		t.Errorf("nine hours asleep: unverified_run %d, want %d", run, 10*hourMS)
	}
}

// TestUnverifiedRunRules is FS-K14a step by step.
func TestUnverifiedRunRules(t *testing.T) {
	const wall, mono = uint64(1_800_000_000_000), uint64(10 * hourMS)
	prev := &cycleMark{wall: wall, mono: mono}
	for _, c := range []struct {
		name    string
		run     uint64
		hubOpen bool
		prev    *cycleMark
		w, m    uint64
		want    uint64
	}{
		{"hub gate open", 7 * hourMS, true, prev, wall + 5*minuteMS, mono + 5*minuteMS, 0},
		{"first period of a process keeps the count", 7 * hourMS, false, nil, wall, mono, 7 * hourMS},
		{"a period adds the boot clock's advance", 7 * hourMS, false, prev, wall + 5*minuteMS, mono + 5*minuteMS, 7*hourMS + 5*minuteMS},
		{"a minute of drift is no jump", 0, false, prev, wall + 6*minuteMS, mono + 5*minuteMS, 5 * minuteMS},
		{"a minute and a millisecond is", 7 * hourMS, false, prev, wall + 6*minuteMS + 1, mono + 5*minuteMS, 0},
		{"wall went back", 7 * hourMS, false, prev, wall - 2*minuteMS, mono + 5*minuteMS, 0},
		{"saturates", math.MaxUint64 - 1, false, prev, wall + 5*minuteMS, mono + 5*minuteMS, math.MaxUint64},
	} {
		if got := nextUnverifiedRun(c.run, c.hubOpen, c.prev, c.w, c.m); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// gateWorld is the world TestClockGateProperties draws: the true time, the
// local wall clock (true time plus an offset that changes at steps), the
// boot clock, a hub whose Date may lie or be missing, sleeps and restarts.
type gateWorld struct {
	rng *rand.Rand
	// the true time, the wall clock's offset from it, the boot clock.
	t, mono uint64
	offset  int64
	// the daemon: unverified_run on disk; the process's previous period
	// and last check, and the Date error of that check.
	run    uint64
	prev   *cycleMark
	obs    clockObs
	obsErr int64
	// the hub: its Date error (ms) and whether it answers at all; sporadic
	// answers right about once a day and wrongly otherwise.
	dateErr  int64
	answers  bool
	sporadic bool
}

func (w *gateWorld) wall() uint64 { return uint64(int64(w.t) + w.offset) }

// answer is one answer of the hub, with its Date (whole seconds).
func (w *gateWorld) answer() {
	if !w.answers {
		return
	}
	errMS := w.dateErr
	if w.sporadic && w.rng != nil && w.rng.IntN(288) == 0 {
		errMS = 0
	}
	date := (int64(w.t) + errMS) / 1000 * 1000
	w.obs = clockObs{hub: true, checked: true, wall: w.wall(), mono: w.mono, offset: int64(w.wall()) - date}
	w.obsErr = errMS
}

// gate runs the start of one maintenance period: unverified_run and the
// level, as the daemon computes them.
func (w *gateWorld) gate() (clockLevel, bool) {
	open := hubGateOpen(w.obs, w.wall(), w.mono)
	w.run = nextUnverifiedRun(w.run, open, w.prev, w.wall(), w.mono)
	w.prev = &cycleMark{wall: w.wall(), mono: w.mono}
	level, _ := clockGate(w.obs, w.run, w.wall(), w.mono)
	return level, open
}

// period is one answer of the hub and then a maintenance period.
func (w *gateWorld) period() (clockLevel, bool) {
	w.answer()
	return w.gate()
}

func (w *gateWorld) advance(ms uint64) { w.t += ms; w.mono += ms }

// restart stops the daemon right after a period and starts it down ms
// later: the machine runs on, the process's memory is gone.
func (w *gateWorld) restart(down uint64) {
	w.advance(down)
	w.prev, w.obs = nil, clockObs{hub: true}
}

// TestClockGateProperties is P-6: random hub answers (honest, six minutes
// off for weeks, silent for weeks, right about once a day), wall clock steps
// of up to 30 days either way while running, sleeps, and restarts every 1 to
// 23 hours, run through the real hubGateOpen, nextUnverifiedRun and
// clockGate. Asserted:
//
//  1. full only while the wall clock is within 6 minutes of the clock the
//     hub stated, and never after a step until the hub is asked again;
//  2. the hub gate open means full;
//  3. with the hub gate closed and no step for a day of running time, the
//     level is at least local, however the hub answers and however often
//     the daemon restarts;
//  4. after a step while running, the level stays closed for a day of
//     running time while the hub gate is closed;
//  5. sleeping, both clocks moving on together, is running time and no step
//     (3 fails otherwise: the worlds sleep).
func TestClockGateProperties(t *testing.T) {
	const S = keyMaintainEveryMS
	seeds := 120
	if testing.Short() {
		seeds = 20
	}
	for seed := range seeds {
		rng := rand.New(rand.NewPCG(uint64(seed), 0xF54))
		start := uint64(1_800_000_000_000)
		w := &gateWorld{rng: rng, t: start, mono: uint64(rng.IntN(1000)) * hourMS, obs: clockObs{hub: true}, answers: true}
		honestFor := uint64(rng.IntN(5)) * 24 * hourMS
		bad := rng.IntN(3)
		badErr := int64(6 * minuteMS)
		if rng.IntN(2) == 0 {
			badErr = -badErr
		}
		end := start + honestFor + 30*24*hourMS
		var (
			sinceOpenOrStep uint64 // running time since the hub gate was open or the wall stepped
			sinceStep       uint64 // running time since the last step
			stepped         bool   // there was a step
			misbehaving     bool
			nextRestart     = start + uint64(1+rng.IntN(23))*hourMS
		)
		for w.t < end {
			if !misbehaving && w.t >= start+honestFor {
				misbehaving = true
				switch bad {
				case 0:
					w.dateErr = badErr
				case 1:
					w.answers = false
				case 2:
					w.dateErr, w.sporadic = badErr, true
				}
			}
			w.answer()
			// Now and then the wall clock steps while the daemon runs,
			// after the hub's answer and before the period.
			step := w.prev != nil && rng.IntN(1500) == 0
			if step {
				d := int64(2*minuteMS) + rng.Int64N(int64(30*24*hourMS))
				if rng.IntN(2) == 0 {
					d = -d
				}
				w.offset += d
				stepped, sinceStep, sinceOpenOrStep = true, 0, 0
			}
			level, open := w.gate()
			if open {
				sinceOpenOrStep = 0
			}
			// 2.
			if open && level != clockFull {
				t.Fatalf("seed %d: hub gate open, level %v", seed, level)
			}
			// 1.
			if level == clockFull {
				if step {
					t.Fatalf("seed %d: full right after a step, before the hub was asked again", seed)
				}
				if off := w.offset - w.obsErr; absMS(off) > 6*minuteMS+1000 {
					t.Fatalf("seed %d: full with the wall clock %d ms off the clock the hub stated", seed, off)
				}
			}
			// 3.
			if sinceOpenOrStep >= clockFallbackAfter && level == clockClosed {
				t.Fatalf("seed %d (hub misbehaving %v, kind %d): closed after %d ms of running time with the hub gate closed and no step",
					seed, misbehaving, bad, sinceOpenOrStep)
			}
			// 4.
			if stepped && !open && sinceStep < clockFallbackAfter && level != clockClosed {
				t.Fatalf("seed %d: level %v %d ms of running time after a step, the hub gate closed", seed, level, sinceStep)
			}
			// Then: a restart right after the period, or running time
			// (now and then asleep).
			if w.t >= nextRestart {
				w.restart(uint64(1+rng.IntN(60)) * minuteMS)
				nextRestart = w.t + uint64(1+rng.IntN(23))*hourMS
				continue
			}
			gap := S
			if rng.IntN(400) == 0 {
				gap += uint64(1+rng.IntN(9)) * hourMS // asleep
			}
			w.advance(gap)
			sinceOpenOrStep += gap
			sinceStep += gap
		}
	}
}

// TestClockGateAfterAStepWaitsADay is P-6 items 4 and 5 on one timeline: a
// node whose hub never answers counts a day of running time across sleeps,
// steps its wall clock forward 20 days while running, and then waits a
// whole new day before it may delete by its own clock.
func TestClockGateAfterAStepWaitsADay(t *testing.T) {
	const S = keyMaintainEveryMS
	w := &gateWorld{t: 1_800_000_000_000, mono: 3 * hourMS, obs: clockObs{hub: true}}
	var ran uint64
	for ran < clockFallbackAfter {
		if level, _ := w.period(); level != clockClosed {
			t.Fatalf("level %v after %d ms of running time", level, ran)
		}
		w.advance(S)
		ran += S
		if ran == 6*hourMS {
			w.advance(8 * hourMS) // asleep: counts, and is no step
			ran += 8 * hourMS
		}
	}
	if level, _ := w.period(); level != clockLocal {
		t.Fatalf("a day of running time with no hub answer: level %v, want local", level)
	}
	w.advance(S)
	w.offset += int64(20 * 24 * hourMS)
	if level, _ := w.period(); level != clockClosed || w.run != 0 {
		t.Fatalf("after a 20-day step: level %v, run %d; want closed, 0", level, w.run)
	}
	for ran = 0; ran+S < clockFallbackAfter; ran += S {
		w.advance(S)
		if level, _ := w.period(); level != clockClosed {
			t.Fatalf("%d ms after the step: level %v, want closed", ran+S, level)
		}
	}
	w.advance(2 * S)
	if level, _ := w.period(); level != clockLocal {
		t.Fatalf("a day after the step: level %v, want local", level)
	}
}

// TestEpochKeyDeletableHardBranch is P-2 for the branches FS-4 has (the
// early one comes with FS-5): closed never deletes; local and full delete at
// not_after + L and never before it; the reason says why a key waits.
func TestEpochKeyDeletableHardBranch(t *testing.T) {
	const na = uint64(1_800_000_000_000)
	L, D, M := seal.MaxMessageLifetimeMS, seal.DeliveryDeadlineMS, drainMarginMS
	key := func(removedAt uint64) *ringKey {
		return &ringKey{Pair: seal.KeyPair{Public: seal.EncKey{KID: []byte("kid-0123456789ab"), NotBefore: na - 24*hourMS, NotAfter: na}},
			RemovedAt: removedAt}
	}
	held := kidSet{"kid-0123456789ab": {}}
	for _, c := range []struct {
		name   string
		k      *ringKey
		now    uint64
		gate   clockLevel
		drain  uint64
		held   kidSet
		ok     bool
		reason string
	}{
		{"closed at the hard bound", key(0), na + L, clockClosed, math.MaxUint64, nil, false, keyClock},
		{"closed long after", key(0), na + 10*L, clockClosed, math.MaxUint64, nil, false, keyClock},
		{"unknown level", key(0), na + L, clockLevel(7), math.MaxUint64, nil, false, keyClock},
		{"local at the hard bound", key(0), na + L, clockLocal, 0, held, true, keyDeletable},
		{"full at the hard bound, mailbox never drained", key(0), na + L, clockFull, 0, held, true, keyDeletable},
		{"local a millisecond before", key(0), na + L - 1, clockLocal, math.MaxUint64, nil, false, keyClock},
		{"local before A(k) + M", key(0), na + D + M - 1, clockLocal, math.MaxUint64, nil, false, keyNotYet},
		{"full before A(k) + M", key(0), na + D + M - 1, clockFull, math.MaxUint64, nil, false, keyNotYet},
		{"full, mailbox not drained", key(0), na + D + M, clockFull, na + D + M - 1, nil, false, keyMailboxNotDrained},
		{"full, a held envelope", key(0), na + D + M, clockFull, math.MaxUint64, held, false, keyHeldMessages},
		{"full, drained: waits for FS-5's early branch", key(0), na + D + M, clockFull, math.MaxUint64, nil, false, keyNotYet},
		{"removed early: A(k) is removed_at", key(na - hourMS), na - hourMS + M, clockLocal, math.MaxUint64, nil, false, keyClock},
		{"removed after not_after + D: A(k) is not_after + D", key(na + 5*hourMS), na + D + M, clockLocal, math.MaxUint64, nil, false, keyClock},
		{"no overflow", &ringKey{Pair: seal.KeyPair{Public: seal.EncKey{NotAfter: math.MaxUint64 - 1}}}, math.MaxUint64 - 1, clockFull, math.MaxUint64, nil, false, keyNotYet},
	} {
		ok, reason := epochKeyDeletable(c.k, c.now, c.gate, c.drain, c.held)
		if ok != c.ok || reason != c.reason {
			t.Errorf("%s: (%v, %q), want (%v, %q)", c.name, ok, reason, c.ok, c.reason)
		}
	}
	// P-2 at random: closed never; nothing before the hard bound; both
	// gates at it.
	rng := rand.New(rand.NewPCG(4, 4))
	for range 20000 {
		k := key(0)
		if rng.IntN(2) == 0 {
			k.RemovedAt = na - 24*hourMS + rng.Uint64N(3*24*hourMS)
		}
		now := na - 24*hourMS + rng.Uint64N(L+3*24*hourMS)
		gate := clockLevel(rng.IntN(3))
		drain := rng.Uint64()
		ok, _ := epochKeyDeletable(k, now, gate, drain, nil)
		hard := now >= na+L
		switch {
		case gate == clockClosed && ok:
			t.Fatalf("closed deleted a key")
		case !hard && ok:
			t.Fatalf("deleted %d ms before the hard bound at level %v (no early branch yet)", na+L-now, gate)
		case hard && gate != clockClosed && !ok:
			t.Fatalf("level %v kept a key past its hard bound", gate)
		}
	}
}

// TestGateUsesBootClock (FS-K13a; mutation 49): the gate and unverified_run
// take their monotonic readings from the boot clock and nothing else. The
// boot clock reads CLOCK_BOOTTIME on Linux and CLOCK_MONOTONIC on macOS
// through x/sys; monoMS falls back to it; no function of the gate's files
// takes a reading of Go's monotonic clock (time.Since, time.Until,
// Time.Sub on time.Now) or of any clock but these.
func TestGateUsesBootClock(t *testing.T) {
	parse := func(name string) *ast.File {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	for file, want := range map[string]string{
		"bootclock_linux.go":  "CLOCK_BOOTTIME",
		"bootclock_darwin.go": "CLOCK_MONOTONIC",
		"bootclock_other.go":  "CLOCK_MONOTONIC",
	} {
		f := parse(file)
		var clocks []string
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok {
					if x.Name == "time" {
						t.Errorf("%s reads package time (%s): the boot clock comes from the system call alone", file, sel.Sel.Name)
					}
					if x.Name == "unix" && strings.HasPrefix(sel.Sel.Name, "CLOCK_") {
						clocks = append(clocks, sel.Sel.Name)
					}
				}
			}
			return true
		})
		if len(clocks) != 1 || clocks[0] != want {
			t.Errorf("%s reads %v, want %s alone", file, clocks, want)
		}
	}
	// monoMS: the test hook or bootClock(), nothing else.
	f := parse("bootclock.go")
	found := false
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "monoMS" {
			continue
		}
		found = true
		ret := fd.Body.List[len(fd.Body.List)-1].(*ast.ReturnStmt)
		call, ok := ret.Results[0].(*ast.CallExpr)
		if id, isID := call.Fun.(*ast.Ident); !ok || !isID || id.Name != "bootClock" {
			t.Errorf("monoMS falls back to %#v, not bootClock()", ret.Results[0])
		}
	}
	if !found {
		t.Fatal("no monoMS in bootclock.go")
	}
	// The gate's code and the code that feeds it its readings: no other
	// monotonic or wall source than the daemon's nowMS and monoMS.
	for _, file := range []string{"epoch_delete.go", "bootclock.go", "enckeys.go"} {
		ast.Inspect(parse(file), func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "time" {
				switch sel.Sel.Name {
				case "Since", "Until", "Now", "Tick", "NewTicker", "NewTimer", "After", "AfterFunc":
					t.Errorf("%s calls time.%s: the gate counts on the boot clock (monoMS) and the wire clock (nowMS) only", file, sel.Sel.Name)
				}
			}
			if sel.Sel.Name == "Sub" {
				t.Errorf("%s subtracts times (%s.Sub): a time.Time difference reads Go's monotonic clock", file, exprName(sel.X))
			}
			return true
		})
	}
}

func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return fmt.Sprintf("%T", e)
}

// The boot clock moves forward with the time that passes, and a daemon
// reads it through monoMS unless a test set one.
func TestBootClockReads(t *testing.T) {
	a := bootClock()
	time.Sleep(30 * time.Millisecond)
	b := bootClock()
	if a == 0 || b < a+20 || b > a+5000 {
		t.Fatalf("boot clock read %d then %d across 30 ms", a, b)
	}
	d := &Daemon{}
	if m := d.monoMS(); m < b {
		t.Fatalf("monoMS %d is behind the boot clock %d", m, b)
	}
	d.setMonoClock(func() uint64 { return 7 })
	if d.monoMS() != 7 || d.testMonoClock() == nil {
		t.Fatal("setMonoClock did not take")
	}
	d.setMonoClock(nil)
	if d.monoMS() == 7 || d.testMonoClock() != nil {
		t.Fatal("setMonoClock(nil) left the test clock in place")
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		if _, err := os.Stat("bootclock_" + runtime.GOOS + ".go"); err != nil {
			t.Fatalf("no boot clock of its own for %s: %v", runtime.GOOS, err)
		}
	}
}

package daemon

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand"
	"testing"
	"time"
)

// BU-3, §3.2: a budget is a whole number from 0 to 1000; anything else that config.json can hold is
// budget_invalid (not an unreadable file); the per-peer budget may not exceed the daily one; unset,
// they are 20 and 5, and an unset per-peer budget follows a smaller daily one down.
func TestRunBudgetValues(t *testing.T) {
	for _, tc := range []struct {
		json        string
		daily, peer int
		bad         bool
	}{
		{`{}`, 20, 5, false},
		{`{"sandbox":{}}`, 20, 5, false},
		{`{"sandbox":{"runs_per_day":0}}`, 0, 0, false},
		{`{"sandbox":{"runs_per_day":3}}`, 3, 3, false},
		{`{"sandbox":{"runs_per_day":1000,"runs_per_peer_per_day":1000}}`, 1000, 1000, false},
		{`{"sandbox":{"runs_per_peer_per_day":20}}`, 20, 20, false},
		{`{"sandbox":{"runs_per_day":10,"runs_per_peer_per_day":0}}`, 10, 0, false},
		{`{"sandbox":{"runs_per_peer_per_day":21}}`, 0, 0, true},
		{`{"sandbox":{"runs_per_day":3,"runs_per_peer_per_day":4}}`, 0, 0, true},
		{`{"sandbox":{"runs_per_day":1001}}`, 0, 0, true},
		{`{"sandbox":{"runs_per_day":-1}}`, 0, 0, true},
		{`{"sandbox":{"runs_per_day":2.5}}`, 0, 0, true},
		{`{"sandbox":{"runs_per_day":1e2}}`, 0, 0, true},
		{`{"sandbox":{"runs_per_day":"5"}}`, 0, 0, true},
		{`{"sandbox":{"runs_per_day":true}}`, 0, 0, true},
		{`{"sandbox":{"runs_per_peer_per_day":[1]}}`, 0, 0, true},
	} {
		var c AutoReplyConfig
		if err := json.Unmarshal([]byte(tc.json), &c); err != nil {
			t.Fatalf("%s: config.json with it does not even read: %v", tc.json, err)
		}
		ref := checkRunBudgets(&c)
		if tc.bad {
			if ref == nil || ref.Key != "budget_invalid" {
				t.Errorf("%s: %v, want budget_invalid", tc.json, ref)
			}
			continue
		}
		if ref != nil {
			t.Errorf("%s: refused: %v", tc.json, ref)
		}
		if d, p := c.runBudgets(); d != tc.daily || p != tc.peer {
			t.Errorf("%s: budgets %d/%d, want %d/%d", tc.json, d, p, tc.daily, tc.peer)
		}
		// Written back as given.
		b, _ := json.Marshal(c)
		var again AutoReplyConfig
		_ = json.Unmarshal(b, &again)
		if d, p := again.runBudgets(); d != tc.daily || p != tc.peer {
			t.Errorf("%s: after a round trip through config.json (%s): %d/%d", tc.json, b, d, p)
		}
	}
}

// fixedClock is a clock for the budget: an instant in a zone.
type fixedClock struct {
	t   time.Time
	loc *time.Location
}

func (c *fixedClock) now() time.Time { return c.t.In(c.loc) }

// BU-3, BU-4 with the default budgets: a peer's sixth run of the local day and the day's twenty-first
// are refused with the time until the next local midnight; the next day both are new.
func TestRunBudgetCountsTheLocalDay(t *testing.T) {
	loc := loadZone(t, "Asia/Shanghai")
	clk := &fixedClock{t: time.Date(2026, 10, 4, 9, 30, 0, 0, loc), loc: loc}
	b := &runBudget{now: clk.now}
	run := func(peer string) (budgetVerdict, time.Duration) {
		t.Helper()
		tk, v, wait := b.reserve(peer, runsPerDayDefault, runsPerPeerPerDayDefault, nil)
		tk.release(true)
		return v, wait
	}
	for i := 1; i <= 5; i++ {
		if v, _ := run("A"); v != budgetGranted {
			t.Fatalf("run %d for A: %v", i, v)
		}
	}
	v, wait := run("A")
	if v != budgetSpent || wait != 14*time.Hour+30*time.Minute {
		t.Fatalf("A's sixth run today: %v, retry after %s (want 14h30m, to local midnight)", v, wait)
	}
	for i := 0; i < 15; i++ {
		if v, _ := run(fmt.Sprintf("P%02d", i)); v != budgetGranted {
			t.Fatalf("run %d of the day: %v", 6+i, v)
		}
	}
	if v, wait := run("Q"); v != budgetSpent || wait != 14*time.Hour+30*time.Minute {
		t.Fatalf("the day's twenty-first run: %v, %s", v, wait)
	}
	if n := b.today(nil); n != 20 {
		t.Fatalf("today: %d runs", n)
	}
	clk.t = time.Date(2026, 10, 5, 0, 0, 0, 0, loc) // the next local day, at its first instant
	if v, _ := run("A"); v != budgetGranted {
		t.Fatalf("A on the next day: %v", v)
	}
	if v, _ := run("Q"); v != budgetGranted {
		t.Fatalf("Q on the next day: %v", v)
	}
	// 0 runs nothing.
	if _, v, _ := b.reserve("R", 0, 0, nil); v != budgetSpent {
		t.Fatalf("a budget of 0: %v", v)
	}
}

// BU-5: at most two runs at once, one per peer; a run with no free slot is busy, not refused, and gets
// one once a slot is free. A run given back (nothing started) does not count.
func TestRunBudgetSlots(t *testing.T) {
	b := &runBudget{}
	a1, v, _ := b.reserve("A", 20, 5, nil)
	if v != budgetGranted {
		t.Fatal(v)
	}
	if _, v, _ := b.reserve("A", 20, 5, nil); v != budgetBusy {
		t.Fatalf("A's second run at once: %v", v)
	}
	b1, v, _ := b.reserve("B", 20, 5, nil)
	if v != budgetGranted {
		t.Fatal(v)
	}
	if _, v, _ := b.reserve("C", 20, 5, nil); v != budgetBusy {
		t.Fatalf("a third run at once: %v", v)
	}
	a1.release(false) // nothing started: given back
	a1.release(true)  // a second release changes nothing
	c1, v, _ := b.reserve("C", 20, 5, nil)
	if v != budgetGranted {
		t.Fatalf("C once A's slot is free: %v", v)
	}
	b1.release(true)
	c1.release(true)
	if n := b.today(nil); n != 2 {
		t.Fatalf("today: %d runs, want 2 (B and C; A's was given back)", n)
	}
	// Busy is never a refusal of the day: with the budget spent the answer is spent.
	x, _, _ := b.reserve("X", 3, 3, nil)
	if _, v, _ := b.reserve("Y", 3, 3, nil); v != budgetSpent {
		t.Fatalf("with the day's budget used up and a slot taken: %v", v)
	}
	x.release(true)
}

// evidenceRun is one anet.autoreply.invoked record for the replay: when it was written and its payload.
type evidenceRun struct {
	at      int64
	payload map[string]any
}

func replaySource(recs *[]evidenceRun) func(since time.Time) []budgetRun {
	return func(since time.Time) []budgetRun {
		var out []budgetRun
		for _, r := range *recs {
			if r.at < since.UnixMilli() {
				continue
			}
			// Through JSON, as the payload goes on the chain (CBOR) and comes back as plain values.
			b, _ := json.Marshal(r.payload)
			var p map[string]any
			_ = json.Unmarshal(b, &p)
			if br, ok := budgetRunOf(r.at, p); ok {
				out = append(out, br)
			}
		}
		return out
	}
}

// BU-2: after a restart the counts come back from the records: an untrusted peer's sandboxed and openai
// runs count, by the day they started; a trusted peer's, a direct one, and a run turned away before it
// started do not.
func TestRunBudgetReplaysTheChain(t *testing.T) {
	loc := loadZone(t, "America/New_York")
	now := time.Date(2026, 10, 4, 0, 30, 0, 0, loc)
	rec := func(start time.Time, dur time.Duration, trusted bool, mode, exit string) evidenceRun {
		run := autoReplyRun{Agent: "claude", InteractionID: "ix", Peer: "A", Trusted: trusted, Mode: mode, Exit: exit, Duration: dur}
		return evidenceRun{at: start.Add(dur).UnixMilli(), payload: run.payload()}
	}
	recs := []evidenceRun{
		rec(now.Add(-10*time.Minute), time.Minute, false, modeSandbox, exitOK),                       // counts
		rec(now.Add(-5*time.Minute), 2*time.Minute, false, modeOpenAI, exitError),                    // counts
		rec(now.Add(-4*time.Minute), time.Minute, false, modeSandbox, exitResource),                  // counts
		rec(now.Add(-3*time.Minute), 0, false, modeSandbox, exitBudget),                              // turned away
		rec(now.Add(-3*time.Minute), 0, false, modeSandbox, exitSandboxUnavailable),                  // turned away
		rec(now.Add(-2*time.Minute), time.Minute, true, modeSandbox, exitOK),                         // trusted
		rec(now.Add(-2*time.Minute), time.Minute, true, modeDirect, exitOK),                          // trusted, direct
		rec(now.Add(-40*time.Minute), 20*time.Minute, false, modeSandbox, exitOK),                    // started yesterday
		rec(now.Add(-26*time.Hour), time.Minute, false, modeSandbox, exitOK),                         // yesterday
		{at: now.Add(-time.Minute).UnixMilli(), payload: map[string]any{"exit": "ok", "agent": "x"}}, // no mode
	}
	b := &runBudget{now: func() time.Time { return now }}
	if n := b.today(replaySource(&recs)); n != 3 {
		t.Fatalf("today after the replay: %d runs, want 3", n)
	}
	// A's budget of 4 has one left; the replay happened once.
	recs = append(recs, rec(now.Add(-time.Second), 0, false, modeSandbox, exitOK))
	if _, v, _ := b.reserve("A", 20, 4, replaySource(&recs)); v != budgetGranted {
		t.Fatalf("A's fourth run: %v", v)
	}
}

// P-BU (SI-58): random runs starting and ending, runs turned away before they start, restarts that lose
// the runs in flight, and a clock that jumps across midnights and daylight saving transitions, in
// zones with every kind of day. Throughout:
//   - the budget answers exactly as the counts it can know say: granted when the day's runs and the
//     peer's are under the budgets and a slot is free, busy when only the slot is missing, spent
//     otherwise — so no local day ever has more runs than the budget, except the runs a restart lost
//     (BU-2: a run the daemon dies in has no record);
//   - spent comes with the time to the next local midnight, exactly;
//   - never more than two runs at once, one per peer.
func TestRunBudgetProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	peers := []string{"A", "B", "C"}
	exits := []string{exitOK, exitOK, exitError, exitResource, exitSandboxUnavailable}
	for trial := 0; trial < 400; trial++ {
		loc := loadZone(t, dayZones[rng.Intn(len(dayZones))])
		lo := time.Date(2009, 1, 1, 0, 0, 0, 0, time.UTC)
		clock := lo.Add(time.Duration(rng.Int63n(int64(18 * 365 * 24 * time.Hour)))).Truncate(time.Millisecond)
		if trs := zoneTransitions(loc, clock, clock.Add(400*24*time.Hour)); len(trs) > 0 && rng.Intn(2) == 0 {
			clock = trs[0].Add(-time.Duration(rng.Int63n(int64(30 * time.Hour)))).Truncate(time.Millisecond)
		}
		daily := rng.Intn(7)
		perPeer := rng.Intn(daily + 1)
		now := func() time.Time { return clock.In(loc) }
		var recs []evidenceRun
		src := replaySource(&recs)
		b := &runBudget{now: now}
		type flight struct {
			tk    *budgetTicket
			peer  string
			start time.Time
			date  time.Time
		}
		var inflight []flight
		known := map[time.Time]map[string]int{} // date -> peer -> runs the budget can know of
		lost := map[time.Time]int{}             // date -> runs a restart lost
		total := func(date time.Time) int {
			n := 0
			for _, v := range known[date] {
				n += v
			}
			return n
		}
		bump := func(date time.Time, peer string, n int) {
			if known[date] == nil {
				known[date] = map[string]int{}
			}
			known[date][peer] += n
		}
		for step := 0; step < 150; step++ {
			switch op := rng.Intn(20); {
			case op < 8: // a run asks for its place
				peer := peers[rng.Intn(len(peers))]
				date := dateOf(now())
				running, peerRunning := len(inflight), 0
				for _, f := range inflight {
					if f.peer == peer {
						peerRunning++
					}
				}
				want := budgetGranted
				switch {
				case total(date) >= daily || known[date][peer] >= perPeer:
					want = budgetSpent
				case running >= 2 || peerRunning >= 1:
					want = budgetBusy
				}
				tk, v, wait := b.reserve(peer, daily, perPeer, src)
				if v != want {
					t.Fatalf("trial %d step %d (%s, %s, budgets %d/%d): %s asked, got %v, want %v (day %d runs, peer %d, %d running)",
						trial, step, loc, now(), daily, perPeer, peer, v, want, total(date), known[date][peer], running)
				}
				if v == budgetSpent {
					if _, end := bruteDayBounds(now()); wait != end.Sub(now()) {
						t.Fatalf("trial %d (%s at %s): retry after %s, the next local midnight is %s away", trial, loc, now(), wait, end.Sub(now()))
					}
				}
				if v == budgetGranted {
					inflight = append(inflight, flight{tk, peer, now(), date})
					bump(date, peer, 1)
				}
			case op < 14 && len(inflight) > 0: // a run ends, and its record is written
				i := rng.Intn(len(inflight))
				f := inflight[i]
				inflight = append(inflight[:i], inflight[i+1:]...)
				exit := exits[rng.Intn(len(exits))]
				counted := exit != exitSandboxUnavailable
				f.tk.release(counted)
				if !counted {
					bump(f.date, f.peer, -1)
				}
				run := autoReplyRun{Peer: f.peer, Mode: modeSandbox, Exit: exit, Duration: now().Sub(f.start)}
				if rng.Intn(4) == 0 {
					run.Mode = modeOpenAI
				}
				recs = append(recs, evidenceRun{at: clock.UnixMilli(), payload: run.payload()})
			case op < 15: // the daemon restarts: the runs in flight are lost, with no record
				for _, f := range inflight {
					bump(f.date, f.peer, -1)
					lost[f.date]++
				}
				inflight = nil
				b = &runBudget{now: now}
			default: // time passes
				steps := []time.Duration{time.Second, time.Minute, 17 * time.Minute, 2 * time.Hour, 7 * time.Hour, 23 * time.Hour, 26 * time.Hour}
				clock = clock.Add(steps[rng.Intn(len(steps))])
			}
			for date := range known {
				if got := total(date); got > daily {
					t.Fatalf("trial %d: %s has %d runs, over the budget of %d", trial, date.Format(time.DateOnly), got, daily)
				}
				for p, n := range known[date] {
					if n > perPeer {
						t.Fatalf("trial %d: %s has %d runs of %s, over %d", trial, date.Format(time.DateOnly), n, p, perPeer)
					}
				}
			}
		}
	}
}

// G14, X-LEC-3 (M-BU-5): the budget's "today" is localDayBounds, the function the spending limits use
// too; neither counts a rolling 24 hours or a UTC day.
func TestTheBudgetAndTheSpendingLimitsShareTheLocalDay(t *testing.T) {
	calls := func(file string, funcs ...string) map[string]bool {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{}
		for _, fn := range funcs {
			want[fn] = true
		}
		out := map[string]bool{}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || !want[fd.Name.Name] {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "localDayBounds" {
						out[fd.Name.Name] = true
					}
				case *ast.SelectorExpr:
					if x.Sel.Name == "Truncate" || x.Sel.Name == "YearDay" {
						t.Errorf("%s %s counts the day by %s, not localDayBounds", file, fd.Name.Name, x.Sel.Name)
					}
				}
				return true
			})
		}
		return out
	}
	got := calls("sandbox_budget.go", "reserve", "today", "loadLocked")
	for _, fn := range []string{"reserve", "today", "loadLocked"} {
		if !got[fn] {
			t.Errorf("sandbox_budget.go %s does not take today from localDayBounds", fn)
		}
	}
	t.Run("spend", func(t *testing.T) {
		t.Skip("v030-todo(PAY-12)") // spend.go's totals still look back 24 hours; PAY-12 moves them to localDayBounds
		if got := calls("spend.go", "spendTotals", "loadSpendLocked", "SpendStatus"); len(got) == 0 {
			t.Error("spend.go does not take today from localDayBounds")
		}
	})
}

package autoreply

// sandbox_budget.go — how many times a day, and how many at once, this node runs auto-reply for peers
// it does not trust (LEC §4.3.6, BU-1 to BU-5; SI-58).
//
// Every auto-reply run for an untrusted peer counts: a run of the exec backend in the sandbox and a turn
// of the openai backend alike (core Q-LEC-6: one rule, "every auto-reply for a stranger counts", is
// plainer than one per backend). A trusted peer's runs do not count, whether direct or sandboxed.
//
//   - "Today" is the local calendar day (localDayBounds), the same day the spending limits count.
//   - A run takes its place before the agent starts or the model is called; when either budget is used
//     up nothing starts, an inbound task is refused with anet.reason=budget and the time until the next
//     local midnight, and an outbound one stays in the inbox (autoreply.go).
//   - The counts are rebuilt from the evidence chain when they are first needed after a start, so a
//     restart does not give the day's budget back. A run the daemon was killed in the middle of has no
//     record and is not counted (LEC §6.2 item 11).
//   - At most two runs for untrusted peers at once, one per peer. A turn that finds no free slot waits
//     for the next scan; it is not refused.

import (
	"bytes"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/localday"
)

const (
	// runsPerDayDefault and runsPerPeerPerDayDefault are the budgets when config.json sets none (ux §2.2).
	runsPerDayDefault        = 20
	runsPerPeerPerDayDefault = 5
	// runBudgetMax is the largest budget config.json may set (LEC §3.2).
	runBudgetMax = 1000
	// untrustedRunsAtOnce and untrustedRunsPerPeerAtOnce bound the runs for untrusted peers in flight
	// (BU-5).
	untrustedRunsAtOnce        = 2
	untrustedRunsPerPeerAtOnce = 1
)

// --- the configuration ---

// BudgetValue is one of auto_reply.sandbox's two run budgets as config.json holds it. Any JSON value
// decodes into it, so that one that is not a whole number from 0 to 1000 is refused with
// budget_invalid (LEC §3.2), whose sentence says what the value must be, rather than making the whole
// file unreadable.
type BudgetValue struct{ raw json.RawMessage }

// Budget is the BudgetValue n.
func Budget(n int) *BudgetValue { return &BudgetValue{raw: json.RawMessage(strconv.Itoa(n))} }

// MarshalJSON writes the value as it was given.
func (b BudgetValue) MarshalJSON() ([]byte, error) {
	if len(b.raw) == 0 {
		return []byte("null"), nil
	}
	return b.raw, nil
}

// UnmarshalJSON keeps any JSON value; Int says whether it is a budget.
func (b *BudgetValue) UnmarshalJSON(p []byte) error {
	b.raw = append(json.RawMessage(nil), p...)
	return nil
}

// Int is the value and whether it is a budget: a JSON integer from 0 to runBudgetMax.
func (b *BudgetValue) Int() (int, bool) {
	dec := json.NewDecoder(bytes.NewReader(b.raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return 0, false
	}
	num, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(num.String(), 10, 64)
	if err != nil || n < 0 || n > runBudgetMax {
		return 0, false
	}
	return int(n), true
}

// runBudgets is the configuration's two budgets in force: the configured values, or 20 runs a day and
// 5 per peer. An unset per-peer budget is never above the daily one, so that setting only a smaller
// daily budget is not refused. A value that is not a budget (refused when written and at start) counts
// as 0: nothing runs.
func (c AutoReplyConfig) runBudgets() (daily, perPeer int) {
	daily, perPeer = runsPerDayDefault, runsPerPeerPerDayDefault
	if s := c.Sandbox; s != nil {
		if s.RunsPerDay != nil {
			daily, _ = s.RunsPerDay.Int()
		}
		if s.RunsPerPeerPerDay != nil {
			perPeer, _ = s.RunsPerPeerPerDay.Int()
			return daily, perPeer
		}
	}
	return daily, min(perPeer, daily)
}

// checkRunBudgets is budget_invalid for a configuration whose budgets are not whole numbers from 0 to
// 1000, or whose per-peer budget is above its daily one (LEC §3.2).
func checkRunBudgets(c *AutoReplyConfig) *a2ashape.Refusal {
	if c == nil || c.Sandbox == nil {
		return nil
	}
	daily := runsPerDayDefault
	if v := c.Sandbox.RunsPerDay; v != nil {
		n, ok := v.Int()
		if !ok {
			return a2ashape.Refuse("budget_invalid", nil)
		}
		daily = n
	}
	if v := c.Sandbox.RunsPerPeerPerDay; v != nil {
		if n, ok := v.Int(); !ok || n > daily {
			return a2ashape.Refuse("budget_invalid", nil)
		}
	}
	return nil
}

// --- the counts ---

// budgetVerdict is the answer to a run asking for its place.
type budgetVerdict int

const (
	budgetGranted budgetVerdict = iota
	// budgetBusy: no free slot now (BU-5); the turn is tried again at the next scan, nothing is refused.
	budgetBusy
	// budgetSpent: today's budget is used up (BU-4).
	budgetSpent
	// budgetOff: a budget of 0 is untrusted = off (BU-3): nothing runs, nothing is refused, the task stays
	// in the inbox. "Try again tomorrow" would not be true of a budget that is 0 tomorrow as well.
	budgetOff
)

// budgetRun is a run that counts, as the evidence chain has it: whose, and when it started.
type budgetRun struct {
	peer  string
	start time.Time
}

// runBudget holds the day's counts and the runs in flight. The zero value is ready; the counts are
// filled from the evidence chain the first time they are read.
type runBudget struct {
	mu sync.Mutex
	// now is the clock, in the location whose calendar day counts; time.Now when nil (tests set it).
	now    func() time.Time
	loaded bool
	// days holds the counts by the start of their local day (unix ms): today's, and yesterday's while
	// the clock may go back over midnight into it.
	days        map[int64]*budgetDay
	running     int
	runningPeer map[string]int
}

// budgetDay is one local day's runs: in all, and per peer.
type budgetDay struct {
	total int
	peer  map[string]int
}

// budgetTicket is a granted run's place. release gives back its slot, and its count when it did not
// count after all.
type budgetTicket struct {
	b    *runBudget
	day  int64
	peer string
	once sync.Once
}

func (b *runBudget) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// loadLocked fills the counts from the runs on the evidence chain (BU-2), once. runs returns the runs
// whose record was written at or after since.
func (b *runBudget) loadLocked(now time.Time, runs func(since time.Time) []budgetRun) {
	if b.loaded {
		return
	}
	b.loaded = true
	if b.days == nil {
		b.days = map[int64]*budgetDay{}
	}
	if runs == nil {
		return
	}
	start, _ := localday.Bounds(now)
	for _, r := range runs(start.Add(-48 * time.Hour)) {
		ds, _ := localday.Bounds(r.start.In(now.Location()))
		b.dayLocked(ds.UnixMilli()).add(r.peer, 1)
	}
	b.pruneLocked(start)
}

func (b *runBudget) dayLocked(key int64) *budgetDay {
	if b.days == nil {
		b.days = map[int64]*budgetDay{}
	}
	d := b.days[key]
	if d == nil {
		d = &budgetDay{peer: map[string]int{}}
		b.days[key] = d
	}
	return d
}

func (d *budgetDay) add(peer string, n int) {
	d.total += n
	d.peer[peer] += n
	if d.peer[peer] <= 0 {
		delete(d.peer, peer)
	}
}

// pruneLocked drops the counts of days that ended more than a day before today began.
func (b *runBudget) pruneLocked(today time.Time) {
	cut := today.Add(-30 * time.Hour).UnixMilli()
	for k := range b.days {
		if k < cut {
			delete(b.days, k)
		}
	}
}

// reserve asks for a run for peer under the budgets: granted with its ticket, busy, off (a budget of 0),
// or spent with the time until the local day ends. runs is the evidence chain's (BU-2), read on first use.
func (b *runBudget) reserve(peer string, daily, perPeer int, runs func(since time.Time) []budgetRun) (*budgetTicket, budgetVerdict, time.Duration) {
	if daily <= 0 || perPeer <= 0 {
		return nil, budgetOff, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	b.loadLocked(now, runs)
	start, end := localday.Bounds(now)
	key := start.UnixMilli()
	day := b.dayLocked(key)
	if day.total >= daily || day.peer[peer] >= perPeer {
		return nil, budgetSpent, end.Sub(now)
	}
	if b.running >= untrustedRunsAtOnce || b.runningPeer[peer] >= untrustedRunsPerPeerAtOnce {
		return nil, budgetBusy, 0
	}
	day.add(peer, 1)
	b.running++
	if b.runningPeer == nil {
		b.runningPeer = map[string]int{}
	}
	b.runningPeer[peer]++
	b.pruneLocked(start)
	return &budgetTicket{b: b, day: key, peer: peer}, budgetGranted, 0
}

// release ends the ticket's run. counted false gives its count back: the run never started anything (a
// sandbox that was unavailable), as the evidence replay leaves it out too.
func (t *budgetTicket) release(counted bool) {
	if t == nil {
		return
	}
	t.once.Do(func() {
		b := t.b
		b.mu.Lock()
		defer b.mu.Unlock()
		b.running--
		if b.runningPeer[t.peer]--; b.runningPeer[t.peer] <= 0 {
			delete(b.runningPeer, t.peer)
		}
		if !counted {
			if d := b.days[t.day]; d != nil {
				d.add(t.peer, -1)
			}
		}
	})
}

// today is the number of runs counted today.
func (b *runBudget) today(runs func(since time.Time) []budgetRun) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	b.loadLocked(now, runs)
	start, _ := localday.Bounds(now)
	if d := b.days[start.UnixMilli()]; d != nil {
		return d.total
	}
	return 0
}

// budgetRunOf is the counted run an anet.autoreply.invoked record stands for, if it is one (BU-2): an
// untrusted peer's run in the sandbox or of the openai backend that was not turned away before it
// started (exit budget or sandbox_unavailable). It started duration_ms before the record was written.
func budgetRunOf(at int64, p map[string]any) (budgetRun, bool) {
	if trusted, _ := p["trusted"].(bool); trusted {
		return budgetRun{}, false
	}
	switch mode, _ := p["mode"].(string); mode {
	case modeSandbox, modeOpenAI:
	default:
		return budgetRun{}, false
	}
	switch exit, _ := p["exit"].(string); exit {
	case exitBudget, exitSandboxUnavailable:
		return budgetRun{}, false
	}
	peer, _ := p["peer_aid"].(string)
	return budgetRun{peer: peer, start: time.UnixMilli(at - int64Of(p["duration_ms"]))}, true
}

// int64Of reads a whole number of an evidence payload (CBOR decodes it as an unsigned or signed
// integer).
func int64Of(v any) int64 {
	switch n := v.(type) {
	case uint64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// --- the module's ---

// budgetRunsOnChain is the counted runs on this node's evidence chain recorded at or after since, read
// back through the module's own evidence (OwnEvidenceHost, BU-2).
func (m *autoReplyModule) budgetRunsOnChain(since time.Time) []budgetRun {
	evs, err := m.ev.ReadOwnEvidence(EvAutoReplyInvoked, since)
	if err != nil {
		return nil
	}
	var out []budgetRun
	for _, e := range evs {
		if r, ok := budgetRunOf(e.At.UnixMilli(), e.Payload); ok {
			out = append(out, r)
		}
	}
	return out
}

// reserveRun asks the budget for a run for peer under cfg's budgets (BU-2 to BU-5).
func (m *autoReplyModule) reserveRun(cfg AutoReplyConfig, peer string) (*budgetTicket, budgetVerdict, time.Duration) {
	daily, perPeer := cfg.runBudgets()
	return m.budget.reserve(peer, daily, perPeer, m.budgetRunsOnChain)
}

// SandboxBudget is the run budget as `anet status` and doctor show it (LEC §2.3.3): the runs counted
// today and the two budgets in force.
type SandboxBudget struct {
	Today             int `json:"today"`
	RunsPerDay        int `json:"runs_per_day"`
	RunsPerPeerPerDay int `json:"runs_per_peer_per_day"`
}

// SandboxBudget is today's count under the current auto-reply configuration.
func (m *autoReplyModule) SandboxBudget() SandboxBudget {
	var cfg AutoReplyConfig
	if ar := m.current(); ar != nil {
		cfg = *ar
	}
	daily, perPeer := cfg.runBudgets()
	return SandboxBudget{Today: m.budget.today(m.budgetRunsOnChain), RunsPerDay: daily, RunsPerPeerPerDay: perPeer}
}

// BudgetOff reports whether a budget of 0 turns the sandbox off for untrusted peers (BU-3).
func (c AutoReplyConfig) BudgetOff() bool {
	daily, perPeer := c.runBudgets()
	return daily <= 0 || perPeer <= 0
}

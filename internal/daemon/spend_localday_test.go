package daemon

// P-PAY-5 (HPP §7.2, PAY-D6): the spending totals count the local calendar
// day. Over random time zones — those with summer time among them, and
// instants close to their transitions — and random amounts, today's total is
// what was signed since local midnight, and the sum saturates rather than
// wraps.

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/module"
)

var payZones = []string{"UTC", "Asia/Shanghai", "America/New_York", "Europe/London", "Australia/Lord_Howe",
	"America/Santiago", "Pacific/Chatham", "America/Havana", "Asia/Tehran", "Pacific/Kiritimati", "America/St_Johns"}

// sameLocalDate reports whether a and b fall on the same date in loc.
func sameLocalDate(a, b time.Time, loc *time.Location) bool {
	ay, am, ad := a.In(loc).Date()
	by, bm, bd := b.In(loc).Date()
	return ay == by && am == bm && ad == bd
}

func TestPPay5(t *testing.T) {
	r := rand.New(rand.NewPCG(5, uint64(time.Now().UnixNano())))
	var locs []*time.Location
	for _, z := range payZones {
		loc, err := time.LoadLocation(z)
		if err != nil {
			t.Skipf("no zone data for %s: %v", z, err)
		}
		locs = append(locs, loc)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5000; i++ {
		loc := locs[r.IntN(len(locs))]
		now := base.Add(time.Duration(r.Int64N(int64(366 * 24 * time.Hour)))).In(loc)
		if r.IntN(3) == 0 {
			// Close to a change of offset, when the zone has one.
			if _, end := now.ZoneBounds(); !end.IsZero() {
				now = end.Add(time.Duration(r.Int64N(int64(6*time.Hour))) - 3*time.Hour).In(loc)
			}
		}
		var records []spendRecord
		var want, wantAgent uint64
		for j := 0; j < 1+r.IntN(20); j++ {
			// Signed at most two days before now, and never after it.
			at := now.Add(-time.Duration(r.Int64N(int64(48 * time.Hour))))
			amount := r.Uint64N(1000)
			if r.IntN(50) == 0 {
				amount = math.MaxUint64 - r.Uint64N(10)
			}
			purpose := []string{module.PurposeTaskAuto, module.PurposeTaskAgent, module.PurposeTaskManual, module.PurposeRedeem}[r.IntN(4)]
			records = append(records, spendRecord{at: at.UnixMilli(), amount: amount, purpose: purpose})
			// Brute force: on now's local date.
			if sameLocalDate(time.UnixMilli(at.UnixMilli()), now, loc) {
				want = spendSum(want, amount)
				if purpose == module.PurposeTaskAgent {
					wantAgent = spendSum(wantAgent, amount)
				}
			}
		}
		start, _ := localDayBounds(now)
		all, agent := spentSince(records, start.UnixMilli())
		if all != want || agent != wantAgent {
			t.Fatalf("%s at %s: today %d (agent %d), want %d (%d)", loc, now, all, agent, want, wantAgent)
		}
	}
}

// Saturation (spendSum): a total that would pass 2^64 stays at the top,
// above every daily limit, whatever the order of the records.
func TestTodaysTotalSaturates(t *testing.T) {
	records := []spendRecord{{at: 10, amount: math.MaxUint64 - 1, purpose: module.PurposeTaskAgent},
		{at: 11, amount: 5, purpose: module.PurposeTaskAgent}, {at: 12, amount: 1, purpose: module.PurposeTaskAuto}}
	all, agent := spentSince(records, 0)
	if all != math.MaxUint64 || agent != math.MaxUint64 {
		t.Fatalf("all %d, agent %d", all, agent)
	}
	if leftOf(50, all) != 0 {
		t.Fatal("something left after a saturated total")
	}
}

// AdmitSpend and SpendStatus count the local day on the daemon's clock: a
// payment signed before local midnight is not today's after it, and one
// signed after is.
func TestTheSpendingDayEndsAtLocalMidnight(t *testing.T) {
	d := newTestDaemon(t, "", false)
	d.mu.Lock()
	d.cfg.Payments = &PaymentsConfig{ExplicitMax: u64val(40), DailyMax: u64val(50)}
	d.mu.Unlock()
	_, end := localDayBounds(time.Now())
	clock := func(at time.Time) {
		ms := uint64(at.UnixMilli())
		d.setClock(func() uint64 { return ms })
	}
	clock(end.Add(-time.Minute))
	if err := d.AdmitSpend("did:anet:a", 40, module.PurposeTaskManual); err != nil {
		t.Fatal(err)
	}
	if err := d.AdmitSpend("did:anet:a", 20, module.PurposeTaskManual); err == nil {
		t.Fatal("60 in one day under a daily 50")
	}
	if st := d.SpendStatus(); st.SpentToday != 40 || st.LeftToday != 10 {
		t.Fatalf("before midnight: %+v", st)
	}
	clock(end.Add(time.Minute))
	if st := d.SpendStatus(); st.SpentToday != 0 || st.LeftToday != 50 {
		t.Fatalf("after midnight: %+v", st)
	}
	if err := d.AdmitSpend("did:anet:a", 20, module.PurposeTaskManual); err != nil {
		t.Fatalf("a new day: %v", err)
	}
}

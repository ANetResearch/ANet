package daemon

import (
	"math/rand"
	"testing"
	"time"
	_ "time/tzdata" // the zones below, whatever the host has installed
)

// dayZones are time zones with every kind of day: none (UTC, the extremes of the offset range), a
// one-hour daylight saving shift away from midnight (New York, London), a half-hour shift (Lord
// Howe), a shift at midnight (Santiago, Havana: the day begins at 01:00 or 23:00 comes twice), a
// shift back over midnight (Beirut: 00:00 becomes 23:00 of the day before, which is lived again), a
// zone that skipped a whole date (Apia, 30 December 2011) and a quarter-hour offset (Kathmandu).
var dayZones = []string{"UTC", "Pacific/Kiritimati", "Etc/GMT+12", "America/New_York", "Europe/London",
	"Australia/Lord_Howe", "America/Santiago", "America/Havana", "Asia/Beirut", "Pacific/Apia",
	"Asia/Kathmandu", "Asia/Shanghai"}

func loadZone(t testing.TB, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// dateOf is t's local date as a comparable UTC midnight.
func dateOf(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// bruteDayBounds is localDayBounds by walking the clock a minute at a time (every offset and every
// transition since 1990 falls on a whole minute): the first minute whose date is on or after now's,
// and the first minute after now whose date is later.
func bruteDayBounds(now time.Time) (start, end time.Time) {
	day := dateOf(now)
	t := now.Add(-50 * time.Hour).Truncate(time.Minute)
	for dateOf(t).Before(day) {
		t = t.Add(time.Minute)
	}
	start = t
	t = now.Truncate(time.Minute).Add(time.Minute)
	for !dateOf(t).After(day) {
		t = t.Add(time.Minute)
	}
	return start, t
}

// zoneTransitions lists the instants a zone changes its offset between from and to.
func zoneTransitions(loc *time.Location, from, to time.Time) []time.Time {
	var out []time.Time
	t := from.In(loc)
	for t.Before(to) {
		_, end := t.ZoneBounds()
		if end.IsZero() || !end.Before(to) {
			break
		}
		out = append(out, end)
		t = end
	}
	return out
}

// Some days the zone data gives, worked out by hand.
func TestLocalDayBoundsKnownDays(t *testing.T) {
	for _, tc := range []struct {
		zone       string
		now        string // RFC 3339, in UTC
		start, end string // RFC 3339, in UTC
		hours      float64
	}{
		// An ordinary day east of UTC: midnight to midnight, 24 hours.
		{"Asia/Shanghai", "2026-10-04T13:00:00Z", "2026-10-03T16:00:00Z", "2026-10-04T16:00:00Z", 24},
		// New York springs forward at 02:00: a 23-hour day.
		{"America/New_York", "2026-03-08T17:00:00Z", "2026-03-08T05:00:00Z", "2026-03-09T04:00:00Z", 23},
		// and falls back at 02:00: a 25-hour day.
		{"America/New_York", "2026-11-01T17:00:00Z", "2026-11-01T04:00:00Z", "2026-11-02T05:00:00Z", 25},
		// Beirut falls back at midnight (00:00 is 23:00 of the day before): 25 October 2026 begins at
		// 00:00 +03 (21:00Z on the 24th); the 24th, lived a second time from 21:00Z to 22:00Z, still
		// began at 00:00 +03 on the 23rd's evening, and ends again at 22:00Z.
		{"Asia/Beirut", "2026-10-24T21:30:00Z", "2026-10-23T21:00:00Z", "2026-10-24T22:00:00Z", 25},
		// Apia went from 29 to 31 December 2011: the 29th ended at the jump.
		{"Pacific/Apia", "2011-12-29T22:00:00Z", "2011-12-29T10:00:00Z", "2011-12-30T10:00:00Z", 24},
	} {
		loc := loadZone(t, tc.zone)
		now, _ := time.Parse(time.RFC3339, tc.now)
		start, end := localDayBounds(now.In(loc))
		ws, _ := time.Parse(time.RFC3339, tc.start)
		we, _ := time.Parse(time.RFC3339, tc.end)
		if !start.Equal(ws) || !end.Equal(we) {
			t.Errorf("%s at %s: [%s, %s), want [%s, %s)", tc.zone, tc.now, start.UTC(), end.UTC(), ws, we)
		}
		if h := end.Sub(start).Hours(); h != tc.hours {
			t.Errorf("%s at %s: the day lasts %vh, want %vh", tc.zone, tc.now, h, tc.hours)
		}
		if start.Location() != loc || end.Location() != loc {
			t.Errorf("%s: the bounds are not in the zone asked", tc.zone)
		}
	}
}

// localDayBounds agrees with the minute-by-minute walk for random instants in many zones, and for
// instants around every transition of those zones (P-BU's retry_after_ms is end − now).
func TestLocalDayBoundsMatchesTheClock(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	lo := time.Date(1995, 1, 1, 0, 0, 0, 0, time.UTC)
	hi := time.Date(2037, 1, 1, 0, 0, 0, 0, time.UTC)
	check := func(loc *time.Location, now time.Time) {
		t.Helper()
		now = now.In(loc)
		start, end := localDayBounds(now)
		bs, be := bruteDayBounds(now)
		if !start.Equal(bs) || !end.Equal(be) {
			t.Fatalf("%s at %s: [%s, %s), the clock says [%s, %s)", loc, now, start, end, bs, be)
		}
		if start.After(now) || !end.After(now) {
			t.Fatalf("%s at %s: [%s, %s) does not hold now", loc, now, start, end)
		}
	}
	for _, name := range dayZones {
		loc := loadZone(t, name)
		for i := 0; i < 150; i++ {
			check(loc, lo.Add(time.Duration(rng.Int63n(int64(hi.Sub(lo))))))
		}
		trs := zoneTransitions(loc, time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC))
		for i, tr := range trs {
			if i%3 != 0 && len(trs) > 30 {
				continue
			}
			for _, d := range []time.Duration{-25 * time.Hour, -time.Hour - time.Second, -time.Second, 0, time.Second,
				30 * time.Minute, time.Hour + 30*time.Second, 23 * time.Hour} {
				check(loc, tr.Add(d))
			}
		}
	}
}

package daemon

import (
	"testing"
	"time"
	_ "time/tzdata" // the zones below, whatever the host has installed
)

// The helpers of the local-day tests that moved to internal/localday, kept for the budget tests.

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

// bruteDayBounds is localday.Bounds by walking the clock a minute at a time (every offset and every
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

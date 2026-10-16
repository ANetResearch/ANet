// Package localday: "today" for this node: the local calendar day (LEC BU-2; core G14, X-LEC-3).
//
// The run budget for untrusted peers (sandbox_budget.go) and the spending limits (spend.go) both speak of
// "today", and both mean the calendar day of the machine's own time zone: what the person reading
// "today's runs" or "left today" has on the wall. One function says where that day starts and ends, so
// that the two can never count different days.
//
// A day is not 24 hours where the zone changes its offset. The clock can skip midnight (a spring-forward
// transition at 00:00, so the day starts at 01:00), and it can go back over midnight (a fall-back
// transition at 00:00 returns to 23:00 of the day before, which is then lived a second time). Within one
// stretch of fixed offset the wall clock only moves forward, so both bounds are found by walking the
// zone's stretches (time.Time.ZoneBounds) rather than by adding hours.

package localday

import "time"

// Bounds returns the local calendar day that holds now, in now's location: start is the first
// instant whose local date is now's date, and end is the first instant after now whose local date is
// later than now's — the next local midnight, or the transition that jumps over it. end − now is how
// long until the budget is new again (BU-4's anet.retry_after_ms).
func Bounds(now time.Time) (start, end time.Time) {
	y, m, d := now.Date()
	// The first instant of a date lies at most 14 hours (the largest offset east of UTC) before UTC
	// midnight of that date, plus whatever a transition shifts; a day and more covers it.
	from := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(-26 * time.Hour).In(now.Location())
	start, ok := firstOnOrAfterDate(y, m, d, from, now)
	if !ok {
		start = now // unreachable: now itself has the date
	}
	end, ok = firstOnOrAfterDate(y, m, d+1, now, now.Add(50*time.Hour))
	if !ok {
		end = now.Add(24 * time.Hour) // unreachable: no zone stays on one date for 50 hours
	}
	return start, end
}

// firstOnOrAfterDate is the first instant in [from, to] whose local date (in from's location) is on or
// after y-m-d (normalized, so d may be one past the month's end), and whether there is one. It walks the
// stretches of fixed offset from from: inside one the wall clock moves forward, so the first instant
// showing the date is where the clock reads its midnight, if the stretch lasts that long; otherwise the
// next stretch is tried from its first instant.
func firstOnOrAfterDate(y int, m time.Month, d int, from, to time.Time) (time.Time, bool) {
	want := time.Date(y, m, d, 0, 0, 0, 0, time.UTC) // the date, normalized, for comparison
	loc := from.Location()
	t := from
	for i := 0; i < 64 && !t.After(to); i++ {
		if !localDateBefore(t, want) {
			return t, true
		}
		_, offset := t.Zone()
		midnight := time.Date(want.Year(), want.Month(), want.Day(), 0, 0, 0, 0, time.FixedZone("", offset)).In(loc)
		_, next := t.ZoneBounds()
		if next.IsZero() || midnight.Before(next) {
			// The clock of this stretch reads the date's midnight at midnight, after t (t's date is
			// earlier), before the stretch ends.
			if midnight.After(to) {
				return time.Time{}, false
			}
			return midnight, true
		}
		t = next
	}
	return time.Time{}, false
}

// localDateBefore reports whether t's local date is before the date of want (a UTC midnight).
func localDateBefore(t, want time.Time) bool {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Before(want)
}

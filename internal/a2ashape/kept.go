package a2ashape

// kept.go is the "kept" line of a task (ux UX-C2 line 4, task.kept_until):
// until when this node keeps the content of an ended task.

import (
	"strconv"
	"time"
)

const keptDayMS = int64(24 * 60 * 60 * 1000)

// KeptLine is the sentence task.kept_until for a projected task, and false
// when the projection does not say (a task that has not ended, one kept for
// ever, a task from before the setting existed). The retention it names is
// the one that applies to this task, worked out from the two dates the
// projection carries: anet.content_expires_at is the start of the UTC day
// after state_at plus the retention (FS-L5), so the retention is the whole
// days between the day of the last state change and the day before the
// expiry. A public call is kept for 7 days at most, and says 7 days.
func KeptLine(t Task, lang Language, now time.Time) (string, bool) {
	s, _ := t.Metadata[KeyContentExpiresAt].(string)
	if s == "" || t.Status.Timestamp == nil {
		return "", false
	}
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", false
	}
	exp := at.UnixMilli()
	day := t.Status.Timestamp.UnixMilli()
	day -= ((day % keptDayMS) + keptDayMS) % keptDayMS
	n := (exp - keptDayMS - day) / keptDayMS
	if n < 1 {
		return "", false
	}
	return Sentence("task.kept_until", lang, Args{"time": at, "retention": strconv.FormatInt(n, 10) + "d"}), true
}

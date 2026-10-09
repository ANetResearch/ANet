package interactions

// ContentFacts is what the retention decision reads of a task, for a test.
type ContentFacts = contentFacts

// ContentDeletable is contentDeletable, for a test.
func ContentDeletable(ix ContentFacts, now int64, level ClockLevel, r Retention, hasOutboxRows, payPending bool) bool {
	return contentDeletable(ix, now, level, r, hasOutboxRows, payPending)
}

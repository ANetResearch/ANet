package daemon

// note_reason.go is the note a delegation's refusal at receive step 9 is
// answered with (CUX-Q1; engine §4.5 row 1): the code and retry_after its
// delivery token's note carries, which rxTokenFor puts in the token.

// AdmitVerdict is a delegation's refusal at receive step 9 as noteReasonFor
// reads it.
type AdmitVerdict struct {
	// Refusal is the code the inbound policy, the admission check or the
	// approval queue refused with (decideDelegate, admitAt, holdDelegate).
	Refusal string
	// RetryAfterMS is how long until the same call may be admitted, 0 when
	// waiting does not help.
	RetryAfterMS int64
	// Now is the time of the decision (unix ms): a per-day quota waits for
	// the next UTC midnight.
	Now uint64
}

// noteReasonFor is the note code and retry_after (seconds, rounded by
// seal.RetryBucket) a refusal is answered with (CUX-Q1).
//
// v030-todo(UX-4): CUX-Q1's table.
func noteReasonFor(v AdmitVerdict) (code string, retryS uint32) {
	return "", 0
}

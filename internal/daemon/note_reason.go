package daemon

// note_reason.go is the note a delegation's refusal at receive step 9 is
// answered with (CUX-Q1; engine §4.5 row 1): the code and retry_after its
// delivery token's note carries, which rxTokenFor puts in the token.

import "github.com/ANetResearch/ANetCore/seal"

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

// noteCodes are the note codes a refusal of the inbound policy, its
// admission and its approval queue can be answered with (core §2.15.2).
var noteCodes = map[string]bool{
	seal.NoteNotAccepting: true, seal.NoteDenied: true, seal.NoteCapabilityNotPublic: true, seal.NotePendingFull: true,
	seal.NoteBusy: true, seal.NoteCallerBusy: true, seal.NoteQuotaCallerPerMin: true, seal.NoteQuotaCallerPerDay: true,
	seal.NoteArgsTooLarge: true, seal.NoteFilesCount: true, seal.NoteFilesTooLarge: true,
	seal.NoteFileTypeNotAccepted: true, seal.NoteBadRequest: true,
}

// noteReasonFor is the note code and retry_after (seconds, rounded by
// seal.RetryBucket, CORE-K22) a refusal is answered with (CUX-Q1). The
// policy's and the admission's own codes are note codes already, but for
// the two a caller is not told apart (busy: the capability's global quota
// and its in-flight bound, review V9); anything else is answered without a
// reason.
//
// v030-todo(UX-4): CUX-Q1's table in full (caller_busy and the busy wait
// of 5 seconds, the file and request codes, allow_only).
func noteReasonFor(v AdmitVerdict) (code string, retryS uint32) {
	code = v.Refusal
	switch code {
	case admitGlobalMinute, admitInflightLimit:
		code = seal.NoteBusy
	}
	if !noteCodes[code] {
		return "", 0
	}
	day := code == seal.NoteQuotaCallerPerDay
	if v.RetryAfterMS <= 0 && !day {
		return code, 0
	}
	secs := (v.RetryAfterMS + 999) / 1000
	if secs > seal.MaxRetryAfterS {
		secs = seal.MaxRetryAfterS
	}
	return code, seal.RetryBucket(uint32(max(secs, 0)), day, v.Now)
}

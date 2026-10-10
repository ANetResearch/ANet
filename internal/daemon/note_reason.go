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

// noteWait is how a row of CUX-Q1 says when to send again.
type noteWait uint8

const (
	waitNone   noteWait = iota // 0: waiting does not help
	waitFixed5                 // 5 seconds, whatever the verdict says
	waitWindow                 // what is left of the 60-second window, rounded up, at least 1 second
	waitDay                    // to the next UTC midnight
)

// noteTable is CUX-Q1: each code a refusal at step 9 is answered with, and
// its wait. A refusal's code is its note code (decidePolicy and admitAt
// answer in note codes, busy for both the in-flight bound and the global
// quota, review V9); the only code noteReasonFor puts in a note that the
// verdict did not name is none at all.
//
//	closed: deny list, or not public to the caller          not_accepting    0
//	approve or open: deny list                               denied           0
//	open: a capability not public to the caller              capability_not_public 0
//	approve: the queue is full                               pending_full     0
//	the request's form (CORE-K7, "$" keys, $files)           bad_request      0
//	args over max_args_bytes                                 args_too_large   0
//	the files (CUX-F10 items 3–5)                            files_count, files_too_large, file_type_not_accepted 0
//	max_inflight                                             busy             5 s
//	per_caller_inflight (CUX-F11)                            caller_busy      5 s
//	the caller's minute                                      quota_caller_per_min  the window's rest
//	the caller's UTC day                                     quota_caller_per_day  to UTC midnight
//	the capability's global minute                           busy             the window's rest
var noteTable = map[string]noteWait{
	seal.NoteNotAccepting:        waitNone,
	seal.NoteDenied:              waitNone,
	seal.NoteCapabilityNotPublic: waitNone,
	seal.NotePendingFull:         waitNone,
	seal.NoteBadRequest:          waitNone,
	seal.NoteArgsTooLarge:        waitNone,
	seal.NoteFilesCount:          waitNone,
	seal.NoteFilesTooLarge:       waitNone,
	seal.NoteFileTypeNotAccepted: waitNone,
	seal.NoteBusy:                waitWindow,
	seal.NoteCallerBusy:          waitFixed5,
	seal.NoteQuotaCallerPerMin:   waitWindow,
	seal.NoteQuotaCallerPerDay:   waitDay,
}

// busyFixedWaitS is the wait of busy's and caller_busy's slot rows: 5
// seconds (CUX-Q1). A busy verdict without a wait is the in-flight bound's.
const busyFixedWaitS = admitBusyWaitMS / 1000

// noteReasonFor is the note code and retry_after (seconds, rounded by
// seal.RetryBucket, CORE-K22) a refusal at step 9 is answered with
// (CUX-Q1). A code that is not in the table — one no step-9 refusal
// should give — is answered without a reason, which the requester shows
// as refused; never a code core §2.15.2 does not have (P-CUX4).
//
// The rounded wait is never below the real one within an hour, and for a
// per-day quota never below it at all (CORE-K22); a wait over an hour is
// told as an hour, the most a note says (DLV-C-6), and a retry refused
// again is harmless. The same verdict gives the same answer.
func noteReasonFor(v AdmitVerdict) (code string, retryS uint32) {
	wait, ok := noteTable[v.Refusal]
	if !ok || !seal.ValidNoteReason(v.Refusal) {
		return "", 0
	}
	code = v.Refusal
	switch wait {
	case waitFixed5:
		return code, seal.RetryBucket(busyFixedWaitS, false, v.Now)
	case waitDay:
		return code, seal.RetryBucket(0, true, v.Now)
	case waitWindow:
		secs := int64(busyFixedWaitS)
		if v.RetryAfterMS > 0 {
			secs = max((v.RetryAfterMS+999)/1000, 1)
		}
		secs = min(secs, int64(seal.MaxRetryAfterS))
		return code, seal.RetryBucket(uint32(secs), false, v.Now)
	}
	return code, 0
}

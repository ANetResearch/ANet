package daemon

// outbox_step.go is the outbound engine's decision (engine §2): what one
// attempt at an outbox row does next, given the row, its versions and what
// the daemon knows at that moment. Everything here is pure (engine ENG-G2):
// no I/O, no clock — the time is an input — so that the attempt shell
// (outbox_attempt.go), the projection of anet.delivery_wait (RC-5) and the
// model test of the state machine (P-ENG-1) all decide by the same code.
//
//   - outboxStep is steps A0–A6 of an attempt (engine §2.4): abandon at the
//     deadline, withdraw a frozen delegation that never left, ask the hub
//     about what it holds, hold back while the gate is closed or an earlier
//     message is not out, or transmit.
//   - needsTransmit is ENG-A1: whether the row has to be handed over again.
//   - deliveryWait is ENG-P1: the anet.delivery_wait of a pending row.
//   - orderBlocked is rules 1–3 of ENG-O1–O3: whether an earlier row of the
//     same task and peer holds this one back.

import (
	"strings"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The abandonment reasons of a row whose deadline passed (engine §2.8):
// anet.reason and the evidence reason.
const (
	reasonUndeliverable       = "undeliverable"
	reasonDeliveryUnconfirmed = "delivery_unconfirmed"
)

// Row kinds (the outbox kind column): what an anet.message/1 row carries,
// the ChatMsg kind as the ordering rules read it (ENG-O1–O3), and for an
// anet.status/1 row the state it reports, or one of the three notices
// after the fact (ENG-Q7), which go only under a credential the requester
// gave (ENG-K2). Other rows have no kind.
const (
	rowKindText            = "text"
	rowKindEndRequest      = "end_request"
	rowKindCancel          = "cancel"
	rowKindPayment         = "payment"
	rowKindPendingApproval = "pending_approval"
	rowKindPendingExpired  = "pending_expired"
	rowKindOperatorReject  = "operator_rejected"
)

// RowView is one outbox row as the engine decides on it (engine §2.1): the
// columns of the row, without its body and envelope. It embeds what the
// current version recorded about the keys it was sealed under: the epoch
// key's deadline (EpochSealed, FS-4, trigger T1) and the key state and
// notice (KELSealed, KEL-8a, triggers T3 and T4).
type RowView struct {
	ID int64
	// IX, To, Type and Kind say what the row is: its interaction, its
	// recipient, the inner type and the row kind (rowKind*).
	IX, To, Type, Kind string
	// TS and Exp are the message's inner ts and exp (unix ms), fixed when
	// it was queued (ENG-Q2): Exp is the row's deadline.
	TS, Exp uint64
	// Attempts counts the failed attempts; NextAt (unix ms) is the earliest
	// the row may be transmitted again (the backoff).
	Attempts int
	NextAt   uint64
	// LastError is the last failure: a code, then ": " and the detail
	// (ENG-S4).
	LastError string
	CreatedAt uint64
	// HandedAt is when a byte of the row first left this process (unix
	// ms); 0 while none has.
	HandedAt uint64
	// Attempting: an attempt wrote its version and may be sending it now
	// (A8); a row found so at start was handed (CR2).
	Attempting bool
	// Frozen: the task was canceled while its delegation was queued; the
	// row is no longer transmitted (ENG-O4, O6).
	Frozen bool
	// CheckAt is when the hub is next asked about the row's versions (unix
	// ms), 0 for no query scheduled; CheckN counts the queued answers since
	// the last hand-over (ENG-D1).
	CheckAt uint64
	CheckN  int
	EpochSealed
	KELSealed
}

// VerView is one version of a row (outbox_ver): one sealing of the
// message, with its ack_key kept by the store.
type VerView struct {
	Ver uint64
	// HandedAt is when the version was first handed over; 0: never.
	HandedAt uint64
	// RID is the hub's id for it (16 bytes), nil when no hub took it or
	// the hub's answer carried none this node can use (DLV-W-12b).
	RID []byte
	// Hub is the hub (URL) it was sent to, ViaHub the hub it was queued
	// at when that is another (DLV-W-12a).
	Hub, ViaHub string
	// HubState is the hub's last answer about it: "" not asked yet,
	// "queued" or "unknown".
	HubState string
	// Stale: the version counts as unknown whatever its last answer,
	// because its hub is no longer this node's or has not answered a query
	// about it for an hour (ENG-D5). The shell sets it.
	Stale bool
	// QueuedSince is when the hub began answering queued for it without a
	// break (ENG-D7), 0 when it does not; kept in memory by the shell.
	QueuedSince uint64
}

// StepInput is what an attempt knows besides the row: the time, the gate,
// whether an earlier row holds this one back and which paths are open.
type StepInput struct {
	Now  uint64
	Gate GateState
	// HubOK: the configured hub may be asked now: there is one, and it has
	// not answered over another wire (core CORE-V1).
	HubOK bool
	// OrderBlocked is orderBlocked for the row (ENG-O1–O3).
	OrderBlocked bool
	// P2P: a direct path can take the row now (reachable, not backing
	// off).
	P2P bool
	// Reseal is resealNeeded for the row (engine §3.1).
	Reseal bool
	// Bypass: a status query of this attempt just found the hub holding a
	// version; a direct path may take it once (ENG-D7).
	Bypass bool
}

// StepAction is what an attempt does.
type StepAction uint8

const (
	// stepWait: nothing now; the row is looked at again at At, or when it
	// is kicked (At 0).
	stepWait StepAction = iota
	// stepAbandon: the deadline passed (A1); Reason is undeliverable or
	// delivery_unconfirmed.
	stepAbandon
	// stepWithdraw: a frozen delegation that never left (ENG-O5).
	stepWithdraw
	// stepQuery: ask the hub about the versions it took (A4).
	stepQuery
	// stepBlocked: an earlier row of the task and peer is not out yet (A5).
	stepBlocked
	// stepTransmit: hand the current version over (A7–A11), sealing a new
	// one first when Seal.
	stepTransmit
)

// StepDecision is outboxStep's answer.
type StepDecision struct {
	Action StepAction
	Reason string
	At     uint64
	Seal   bool
	// P2POnly: hand it over directly and not through the hub (ENG-D7).
	P2POnly bool
	// Park, for a wait with nothing to send before it, is where the row's
	// next_at moves (0: it stays).
	Park uint64
}

// The engine's timing (engine §0.4). Code constants: there is nothing for
// an operator to tune.
const (
	// statusFirstMS..: the status queries after a hand-over to the hub come
	// at these offsets (DLV-S-7); after the last, the interval doubles up to
	// statusMaxIntervalMS.
	statusMaxIntervalMS uint64 = 30 * 60 * 1000
	// statusStaleMS: a version whose hub has not answered a status query
	// for this long counts as unknown (ENG-D5, DLV-S-13).
	statusStaleMS uint64 = 3600 * 1000
	// p2pBypassMS: a version the hub has held queued this long may also be
	// handed over directly (ENG-D7, DLV-S-12).
	p2pBypassMS uint64 = 2 * 60 * 1000
)

// statusOffsetsMS are the first status queries after a hand-over, as
// offsets from it (DLV-S-7): 2, 5, 15, 30, 60 and 120 seconds.
var statusOffsetsMS = [...]uint64{2000, 5000, 15000, 30000, 60000, 120000}

// statusDelay is how long after a status answer of queued (the n-th since
// the hand-over, n ≥ 1) the next query comes: the next offset of the table,
// then twice the last interval, at most statusMaxIntervalMS.
func statusDelay(n int) uint64 {
	if n < 1 {
		return statusOffsetsMS[0]
	}
	if n < len(statusOffsetsMS) {
		return statusOffsetsMS[n] - statusOffsetsMS[n-1]
	}
	d := statusOffsetsMS[len(statusOffsetsMS)-1] - statusOffsetsMS[len(statusOffsetsMS)-2]
	for i := len(statusOffsetsMS); i <= n; i++ {
		d *= 2
		if d >= statusMaxIntervalMS {
			return statusMaxIntervalMS
		}
	}
	return d
}

// versionsHanded reports whether any version of row was handed over: what
// abandonment and withdrawal decide by (ENG-A5, ENG-X2).
func versionsHanded(row RowView, vers []VerView) bool {
	if row.HandedAt != 0 {
		return true
	}
	for _, v := range vers {
		if v.HandedAt != 0 {
			return true
		}
	}
	return false
}

// hubStateOf is the hub's last word on a version as the engine takes it:
// unknown for a stale version (ENG-D5), its last answer otherwise ("" not
// asked yet), and "" for one the hub gave no rid.
func hubStateOf(v VerView) string {
	switch {
	case v.RID == nil:
		return ""
	case v.Stale:
		return interactions.OutboxUnknown
	}
	return v.HubState
}

// needsTransmit is ENG-A1 without the backoff: whether row must be handed
// over (again) for its message to arrive. It does when (a) no version was
// ever handed over; (b) every version a hub took with a rid is known to be
// gone from it (unknown, ENG-D5 included), and none is waiting for its
// first status query; (c) versions were handed over, none with a rid (a
// direct send without a valid token, or a hub request that got no 200).
// A version the hub holds queued is not sent again (ENG-D6, SI-26).
func needsTransmit(row RowView, vers []VerView) bool {
	if !versionsHanded(row, vers) {
		return true // (a)
	}
	for _, v := range vers {
		if v.RID == nil {
			continue
		}
		if hubStateOf(v) != interactions.OutboxUnknown {
			return false // queued, or not asked yet
		}
	}
	return true // (b), or (c) when no version has a rid
}

// lastErrorCode is the code last_error begins with (ENG-S4): the part
// before ": ".
func lastErrorCode(lastErr string) string {
	code, _, _ := strings.Cut(lastErr, ":")
	return strings.TrimSpace(code)
}

// rowWaitCodes are the last_error codes that are an anet.delivery_wait
// (ENG-P1 item 2): the hub's waits, a first contact without a session, and
// a credential the hub refused that anet keeps trying (SSR-D11).
var rowWaitCodes = map[string]bool{
	"rate_limited": true, "strangers_busy": true, "redelivery_limited": true, "mailbox_full": true,
	"home_unreachable": true, "session_unavailable": true, "credential_refused": true,
}

// deliveryWait is ENG-P1: the anet.delivery_wait of a pending row, "" for
// none. First the gate: while it is closed for a key event being published
// or a notice too old, a row that has to be handed over (needsTransmit,
// the backoff aside) waits for it — rotation_pending or notice_stale, read
// from the gate now, never from last_error (KEL D-4); a row the hub holds
// is not held back by it. Then the wait last_error names. RC-5's projection
// and the variant inputs of an abandonment call it; nobody writes another.
func deliveryWait(row RowView, vers []VerView, gate GateState) string {
	if w := gateDeliveryWait(gate); w != "" && needsTransmit(row, vers) {
		return w
	}
	if c := lastErrorCode(row.LastError); rowWaitCodes[c] {
		return c
	}
	return ""
}

// orderBlocked is rules 1–3 (ENG-O1–O3): whether a row queued before row
// for the same task and peer (ahead) holds it back. Rule 1: a row goes
// only once every row queued before it has been handed over. Rule 2: a
// text or end_request message goes only once the task's delegation is
// confirmed, which is when its row is gone. Rule 3: a cancel or a payment
// waits for rule 1 alone. A5 and the projection's "waits for the message
// before it" (DLV-W-21d) both call it.
//
// For rule 1 a row an attempt is sending now is not handed over yet: the
// attempt may end with nothing of it gone, and a cancel that went ahead of
// it would then have left for a delegation that is withdrawn (E-T13). A
// row behind waits for the attempt to end, which is at most one attempt's
// time; a row left attempting by a process that stopped counts as handed
// over from the start on (CR2).
func orderBlocked(row RowView, ahead []RowView) bool {
	followUp := row.Type == seal.TypeMessage && (row.Kind == rowKindText || row.Kind == rowKindEndRequest)
	for _, a := range ahead {
		if a.ID >= row.ID {
			continue
		}
		if a.HandedAt == 0 {
			return true
		}
		if followUp && a.Type == seal.TypeDelegate {
			return true
		}
	}
	return false
}

// queryable reports whether the hub can be asked about version v now: it
// has a rid, and it is not stale (its hub is this node's and answers).
func queryable(v VerView) bool { return v.RID != nil && !v.Stale }

// queuedLong reports whether the hub has held a version of row queued for
// p2pBypassMS or more (ENG-D7).
func queuedLong(vers []VerView, now uint64) bool {
	for _, v := range vers {
		if v.QueuedSince != 0 && now >= v.QueuedSince && now-v.QueuedSince >= p2pBypassMS && hubStateOf(v) == interactions.OutboxQueued {
			return true
		}
	}
	return false
}

// outboxStep decides steps A0–A6 of one attempt at row (engine §2.4), in
// their normative order:
//
//	A1 the deadline: given up, undeliverable if no version was ever handed
//	   over, delivery_unconfirmed otherwise;
//	A3 a frozen delegation that never left and has no attempt under way is
//	   withdrawn (ENG-O5);
//	A4 the status query, when due: the only step a closed gate or a frozen
//	   row allows;
//	A2 the gate closed, or A3 frozen: nothing more;
//	A5 an earlier row holds it back: nothing, the backoff untouched;
//	A6 nothing to send (ENG-A1) or not yet (next_at): wait — except that
//	   right after a query that found a version queued for long, a direct
//	   path takes it once (ENG-D7);
//	   otherwise transmit, through the hub when it may be asked, directly
//	   when only a direct path is open.
//
// A wait says until when (At, 0 for until kicked) and, when nothing is to
// be sent before then, Park: the time the row's next_at moves to so the
// scan does not pick it up for nothing.
func outboxStep(row RowView, vers []VerView, in StepInput) StepDecision {
	handed := versionsHanded(row, vers)
	if in.Now > row.Exp {
		if handed {
			return StepDecision{Action: stepAbandon, Reason: reasonDeliveryUnconfirmed}
		}
		return StepDecision{Action: stepAbandon, Reason: reasonUndeliverable}
	}
	if row.Frozen && !handed && !row.Attempting {
		return StepDecision{Action: stepWithdraw}
	}
	if row.CheckAt != 0 && row.CheckAt <= in.Now && in.HubOK {
		for _, v := range vers {
			if queryable(v) {
				return StepDecision{Action: stepQuery}
			}
		}
	}
	// When nothing is to be sent, the row is next looked at for its status
	// query or its deadline.
	wake := row.Exp + 1
	if row.CheckAt != 0 && row.CheckAt < wake {
		wake = row.CheckAt
	}
	if !in.Gate.Open || row.Frozen {
		return StepDecision{Action: stepWait}
	}
	if in.OrderBlocked {
		return StepDecision{Action: stepBlocked}
	}
	if !needsTransmit(row, vers) {
		if in.Bypass && in.P2P && queuedLong(vers, in.Now) {
			return StepDecision{Action: stepTransmit, Seal: in.Reseal, P2POnly: true}
		}
		return StepDecision{Action: stepWait, At: wake, Park: wake}
	}
	if row.NextAt > in.Now {
		return StepDecision{Action: stepWait, At: min(row.NextAt, wake)}
	}
	if !in.HubOK && !in.P2P {
		return StepDecision{Action: stepWait}
	}
	return StepDecision{Action: stepTransmit, Seal: in.Reseal, P2POnly: !in.HubOK}
}

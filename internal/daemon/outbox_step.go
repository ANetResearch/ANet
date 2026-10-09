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
}

// outboxStep decides steps A0–A6 of one attempt at row (engine §2.4).
//
// v030-todo(ENG-1a): the state machine.
func outboxStep(row RowView, vers []VerView, in StepInput) StepDecision {
	return StepDecision{Action: stepWait}
}

// needsTransmit is ENG-A1 without the backoff: whether row must be handed
// over (again).
//
// v030-todo(ENG-1a): ENG-A1 (a)–(c).
func needsTransmit(row RowView, vers []VerView) bool { return false }

// deliveryWait is ENG-P1: the anet.delivery_wait of a pending row, "" for
// none.
//
// v030-todo(ENG-1a): the gate's code, then the wait code of last_error.
func deliveryWait(row RowView, vers []VerView, gate GateState) string { return "" }

// orderBlocked is rules 1–3 (ENG-O1–O3): whether a row queued before row
// for the same task and peer (ahead, oldest first) holds it back.
//
// v030-todo(ENG-1a): rules 1 and 2.
func orderBlocked(row RowView, ahead []RowView) bool { return false }

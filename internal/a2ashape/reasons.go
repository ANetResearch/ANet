package a2ashape

// reasons.go is the reason table (v0.3.0 ux §1): every sentence anet shows a
// person — a refusal, a failure, a wait, a status line, a terminal prompt, a
// doctor finding — in Chinese and English, with what to do next. The CLI, MCP,
// the local A2A interface, the control plane, the console and doctor take their
// sentences from here and nowhere else (UX-G1), and the same event gives the
// same code and the same words in all of them (UX-G3, SI-79).
//
// A row is a code (or a code and a variant, or a dotted sentence key), its
// classes (ux §1.1), how it reaches A2A, the two templates and the next step.
// Templates hold {placeholders}; their values come only from this node's own
// data, never from text a peer wrote (UX-G2, CUX-T6).
//
// Four functions render (UX-R8), all pure: Sentence, DeliveryText,
// PaymentNote and StatusSummary. The rest of the file is lookups the
// renderers and their callers share.

import (
	"time"
)

// OfficialHubURL is the official public hub (ux §2.6). It is not a setting:
// `anet up` and no_hub_yet name it so that the first command a new node is
// told to run joins a hub that needs nobody's approval.
const OfficialHubURL = "https://hub.agentnetwork.org.cn"

// Language is the language of a sentence.
type Language uint8

// The two languages. English is the zero value: an environment that names
// no language gets English (core G13).
const (
	English Language = iota
	Chinese
)

// Lang is this process's language (core G13): the first non-empty of LC_ALL,
// LC_MESSAGES and LANG; Chinese when it begins with "zh", English otherwise.
// There is no setting and no per-client choice: the daemon renders, and the
// CLI and MCP pass its sentences on unchanged (UX-C7).
func Lang() Language { return English } // v030-todo(UX-5)

// LangFrom is Lang over another environment.
func LangFrom(getenv func(string) string) Language { return English } // v030-todo(UX-5)

// Args are a template's placeholder values, by name. A value may be a
// string, an integer, a time.Duration (rendered "15 分钟" / "15 minutes"), a
// time.Time (local "MM-DD HH:MM") or, for a placeholder bound to a phrase
// set (an action, a reason short form, a tier), the phrase's key.
type Args map[string]any

// Class is a set of the classes of ux §1.1: where the sentence appears and
// how A2A carries it.
type Class uint16

// The classes.
const (
	// ClassR: the other side certainly did not run it (TASK_STATE_REJECTED).
	ClassR Class = 1 << iota
	// ClassF: a failed end (TASK_STATE_FAILED).
	ClassF
	// ClassFP: a payment did not settle (x402.payment.status payment-failed).
	ClassFP
	// ClassIP: waiting for someone to decide whether to pay
	// (TASK_STATE_INPUT_REQUIRED, payment-required).
	ClassIP
	// ClassW: waiting, recovers on its own (anet.delivery_wait).
	ClassW
	// ClassM: one message refused, the task unchanged.
	ClassM
	// ClassE: this node refused the request and sent nothing; an A2A error.
	ClassE
	// ClassC: only in the CLI or the control plane's error body.
	ClassC
	// ClassS: a status sentence (/status, doctor).
	ClassS
	// ClassI: information that changes no state.
	ClassI
	// ClassT: a terminal prompt or confirmation line.
	ClassT
	// ClassModel: the egress refusals of ux §1.4 I, read by the agent in the
	// sandbox and never by the user.
	ClassModel
)

// NextKind is what a sentence asks of the user (UX-G2).
type NextKind uint8

// The kinds of next step. Rows of class R, F, FP, IP, M, W, E or C declare
// one other than NextUnset; status, information and prompt rows may.
const (
	NextUnset NextKind = iota
	// NextCommand: run the row's command (Next).
	NextCommand
	// NextNoneNeeded: nothing to do.
	NextNoneNeeded
	// NextAuto: anet is handling it; the sentence says how it ends.
	NextAuto
	// NextRetryLater: send the same again after a while.
	NextRetryLater
	// NextChangeInput: change the request, an argument, a file or this
	// node's condition, then do it again.
	NextChangeInput
	// NextContact: ask the other side or an operator.
	NextContact
	// NextStartOver: start a new task.
	NextStartOver
	// NextFutile: trying again will not help.
	NextFutile
)

// Level is how a status sentence rates what it says: the statuses of
// `anet doctor` (UX-K3) and the level of a /status summary line.
type Level string

// The levels.
const (
	LevelOK      Level = "ok"
	LevelInfo    Level = "info"
	LevelWarn    Level = "warn"
	LevelFail    Level = "fail"
	LevelUnknown Level = "unknown"
)

// A2AKind is the local A2A error a class-E row is reported as (UX-R10,
// core G1).
type A2AKind uint8

// The A2A errors of reason codes.
const (
	A2ANone A2AKind = iota
	// A2AInvalidParams: the request itself is not acceptable (−32602, REST
	// 400 INVALID_ARGUMENT).
	A2AInvalidParams
	// A2AFailedPrecondition: a lasting state of this node, which does not
	// change until the user changes it (−32602, REST 400
	// FAILED_PRECONDITION).
	A2AFailedPrecondition
	// A2ATransient: a passing state; the same call may succeed later
	// (−32603, REST 503 with Retry-After).
	A2ATransient
	// A2ATaskNotFound: TaskNotFoundError (−32001, REST 404).
	A2ATaskNotFound
	// A2AUnsupportedOperation: UnsupportedOperationError (−32004, REST 400).
	A2AUnsupportedOperation
	// A2AExtendedCardNotConfigured: ExtendedAgentCardNotConfiguredError
	// (−32007, REST 400).
	A2AExtendedCardNotConfigured
	// A2AInvalidAgentResponse: InvalidAgentResponseError (−32006, REST 500),
	// the other side answered against the specification.
	A2AInvalidAgentResponse
)

// A2A is how a row reaches A2A beyond what its classes say (ux §1.1).
type A2A struct {
	// Effect is anet.effect_status on a capability task that ends with
	// this row (R and F): UNAVAILABLE, UNVERIFIED or FAILED.
	Effect string
	// X402 is x402.payment.error of an FP row (an a2a-x402 standard code).
	X402 string
	// Err is the local A2A error of an E row.
	Err A2AKind
}

// Row is one row of the reason table.
type Row struct {
	// Code is the reason code (the value of anet.reason, of a control-plane
	// reason, of an ErrorInfo), an a2a-x402 code written "x402:<CODE>", or a
	// dotted sentence key that is not a code (a status line, a prompt).
	Code string
	// Variant chooses among a code's sentences without changing the code
	// (UX-G1): the row's key is Code + "." + Variant.
	Variant string
	Class   Class
	// Level rates a status row; empty for the rest.
	Level Level
	A2A   A2A
	// Zh and En are the templates.
	Zh, En string
	// NextKind and Next are the next step. Next is a command template, the
	// same in both languages unless NextZh gives the Chinese form (a <…>
	// hint follows the language, UX-G8a).
	NextKind NextKind
	Next     string
	NextZh   string
	// NoSentence marks a code that has no sentence: the connection is
	// dropped (ux §1.4 I, egress_sni_mismatch and its two siblings).
	NoSentence bool
}

// Key is the row's key: its code, and its variant after a dot.
func (r Row) Key() string {
	if r.Variant == "" {
		return r.Code
	}
	return r.Code + "." + r.Variant
}

// Lookup is the row of a key. A variant the table does not have is its
// code's row: a variant only changes words.
func Lookup(key string) (Row, bool) { return Row{}, false } // v030-todo(UX-5)

// Rows is the table, in its order.
func Rows() []Row { return nil } // v030-todo(UX-5)

// ReasonCode is the code a key stands for in anet.reason, a control-plane
// reason and an ErrorInfo (CORE-K25): the key without its variant.
func ReasonCode(key string) string { return key } // v030-todo(UX-5)

// Sentence renders a key in a language. A code the table does not have, of
// legal form, is shown with the fallback unknown_code and the code itself
// (UX-G9). A placeholder with no value never shows as a placeholder or as
// empty brackets (UX-G8, UX-T4).
func Sentence(key string, lang Language, args Args) string { return "" } // v030-todo(UX-5)

// Next is a key's next step in a language with its placeholders filled: the
// command to print after "→" (UX-R2) and doctor's hint (UX-K4); empty when
// the row has none.
func Next(key string, lang Language, args Args) string { return "" } // v030-todo(UX-5)

// Label is a short label in a language: the labels of the status summary
// (ux §4.7), the human lines of a task command (UX-C2) and `anet identity
// status`. Keys: "status.<topic>", "task.<line>", "identity_status.<line>".
func Label(key string, lang Language) string { return "" } // v030-todo(UX-5)

// Delivery state values (core §2.13), as DeliveryText reads them.
const (
	DeliveryPending     = "pending"
	DeliveryConfirmed   = "confirmed"
	DeliveryUnconfirmed = "unconfirmed"

	DeliveryNotSent     = "not_sent"
	DeliverySent        = "sent"
	DeliveryQueuedAtHub = "queued_at_hub"
)

// DeliveryText is the sentence of a message's delivery state (ux §1.4 C3):
// anet.delivery, anet.delivery_detail, anet.delivery_wait, whether it waits
// for the task's previous message (DLV-S-22), and the moment the task gives
// up on it. A pending delivery with endsAt set ends with delivery.ends_at
// (UX-R11); a terminal task passes a zero endsAt. Empty for values it does
// not know.
func DeliveryText(delivery, detail, wait string, ordered bool, endsAt time.Time, lang Language) string {
	return "" // v030-todo(UX-5)
}

// Why an outbound row was given up (DLV-S-35).
const (
	// DeliveryCauseDeadline: the 14-day cutoff (engine A1), or a delegation
	// under ENG-T2.
	DeliveryCauseDeadline = "deadline"
	// DeliveryCauseScan: the no_response_after scan (ENG-T1).
	DeliveryCauseScan = "scan"
)

// DeliveryVariant chooses the variant of delivery_unconfirmed (ux §1.4 C1,
// DLV-S-35) from why the row was given up, the wait code of its last error
// and whether a version of it was last seen queued at a hub. The engine
// calls it in the transaction that gives the row up and stores the result.
func DeliveryVariant(cause, wait string, queued bool) string { return "" } // v030-todo(UX-5)

// UndeliverableVariant is the variant of undeliverable (engine ENG-U3): ux
// gives undeliverable one neutral sentence, so it is always "". The engine
// stores what it returns, and a variant added later needs no schema change
// (DLV-S-35).
func UndeliverableVariant(wait string, noKeys bool) string { return "" }

// PayNote is a payment decision as it was taken (HPP PAY-U5): the snapshot
// stored on the task, which PaymentNote renders without computing anything
// again (M-PAY-19).
type PayNote struct {
	// Code is a key of ux §1.4 D1, D2 or D3.
	Code string `json:"code"`
	// Amount is the quote; Price the signed price, when there is one.
	Amount uint64  `json:"amount"`
	Price  *uint64 `json:"price,omitempty"`
	// Tier is the payment tier: "auto", "agent" or "manual".
	Tier string `json:"tier"`
	// Single and Left are the tier's per-payment limit and what was left
	// today when the decision was taken (after the payment, for paid.*).
	Single uint64 `json:"single"`
	Left   uint64 `json:"left"`
	// Key and Value are the limit setting that would let the payment
	// through, for needs_operator_approval.manual_limit.
	Key   string  `json:"key,omitempty"`
	Value *uint64 `json:"value,omitempty"`
	// Why is the short reason of card_price_unavailable: timeout,
	// not_offered, bad_signature, text_task, hub_unreachable or a wait code.
	Why string `json:"why,omitempty"`
	// Peer is the payee's full AID.
	Peer string    `json:"peer,omitempty"`
	At   time.Time `json:"at"`
	// Task is the task id; not stored, the caller sets it to render.
	Task string `json:"-"`
}

// PaymentNote is the payment sentence of a snapshot (ux §1.4 D1–D3).
func PaymentNote(n PayNote, lang Language) string { return "" } // v030-todo(UX-5)

// Said is a sentence a status object chose: its key and arguments.
type Said struct {
	Key  string
	Args Args
}

// StatusInput is what the status summary is made of (ux §4.7): for each
// topic, the sentence the area that owns it chose, and the facts that decide
// which lines show. A nil sentence is not shown.
type StatusInput struct {
	// IdentityState is /identity/status.state; no_hub hides the identity
	// line (UX-C9).
	IdentityState string
	Identity      *Said
	// Recovery is /identity/status.recovery.state: none, unprotected or
	// protected. RecoveryResume says the passphrase is set and the second
	// rotation has not run yet (KEL D-16).
	Recovery       string
	RecoveryResume bool
	// Hub is the hub line; HubWire, when set (wire_too_old, wire_too_new),
	// takes its place.
	Hub     Said
	HubWire *Said
	// HubRegistered hides the clock sentences of the privacy line when
	// false.
	HubRegistered bool
	Delivery      *Said
	Inbound       Said
	// Sandbox and SandboxBudget make the sandbox line, shown when either
	// trust mode is sandbox.
	Sandbox       *Said
	SandboxBudget *Said
	Payments      Said
	// The privacy line: the first of LocalStateRepeated,
	// LocalStateDetected, Clock (only when HubRegistered) and Retention.
	Retention          Said
	Clock              *Said
	LocalStateRepeated *Said
	LocalStateDetected *Said
	Preview            *Said
}

// SummaryLine is one line of the status summary: /status.summary[i], which
// `anet status` prints and MCP node_status passes on.
type SummaryLine struct {
	Topic string `json:"topic"`
	Level Level  `json:"level"`
	Text  string `json:"text"`
	Next  string `json:"next,omitempty"`
}

// The summary topics, in their order (ux §4.7).
const (
	TopicIdentity = "identity"
	TopicRecovery = "recovery"
	TopicHub      = "hub"
	TopicDelivery = "delivery"
	TopicInbound  = "inbound"
	TopicSandbox  = "sandbox"
	TopicPayments = "payments"
	TopicPrivacy  = "privacy"
	TopicPreview  = "preview"
)

// StatusSummary is the status summary (ux §4.7).
func StatusSummary(in StatusInput, lang Language) []SummaryLine { return nil } // v030-todo(UX-5)

// reasonTable is the table. Rows are added in the order of ux §1.4, §4.7,
// §5.2.
var reasonTable = []Row{}

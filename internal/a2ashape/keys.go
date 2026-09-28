package a2ashape

import (
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/internal/x402a2a"
)

// Metadata keys. Every key anet puts on an A2A object is named here, so
// that the daemon, the control plane, MCP and module/a2a spell it the same
// way and a test can pin the string (A2A-DESIGN §17: "扩展 URI 与 metadata
// 键两侧钉字符串").
//
// The a2a-x402 vocabulary, and the anet.* keys the payment flow shares
// with the projection (anet.reason, anet.cancel_requested,
// anet.payment.accept), are defined once, in internal/x402a2a, which the
// payment flow in the kernel and module/x402 use; the names below are
// aliases of those constants, not a second spelling of the strings.

// Task metadata (A2A-DESIGN §11.5). The two SI-6 keys are the reason the
// rest exist: an A2A COMPLETED says the task ended normally and nothing
// about whether the effect happened or the receipt checked out, so those
// two facts travel beside the state instead of being folded into it.
const (
	// KeyEffectStatus is the capability effect status (OK, UNVERIFIED,
	// FAILED, UNAVAILABLE, PAYMENT_REQUIRED). Capability tasks only, and
	// always present once one is terminal.
	KeyEffectStatus = "anet.effect_status"
	// KeyReceiptVerified is whether this node could check the provider's
	// receipt: ReceiptVerified, ReceiptUnverified or ReceiptUnknown.
	// Present on every completed task and on any task that has a receipt.
	KeyReceiptVerified = "anet.receipt_verified"
	KeyRequestCID      = "anet.request_cid"
	KeyResultCID       = "anet.result_cid"
	// KeyPeerAID is the other party: the provider of an outbound task, the
	// requester of an inbound one.
	KeyPeerAID = "anet.peer_aid"
	// KeyRole is this node's side of the task, RoleRequester or
	// RoleProvider. The control plane lists both sides in one list, and a
	// user/agent role on a message means nothing until the reader knows
	// which of the two this node is.
	KeyRole = "anet.role"
	// KeySkill is the capability a capability task invokes. On an incoming
	// SendMessage it asks for a capability call (§11.5).
	KeySkill = "anet.skill"
	// KeyStateSeq is the task's state sequence number, which increases
	// with every state write. A client that waits for "the next state"
	// compares it rather than the state, which may not change (§4.1 [C35]).
	KeyStateSeq = "anet.state_seq"
	// KeyTrust is how an inbound task was admitted (peer, public,
	// public_cap, approved); absent on this node's own tasks.
	KeyTrust        = "anet.trust"
	KeyReason       = x402a2a.KeyReason
	KeyRetryAfterMS = "anet.retry_after_ms"
	// KeyReceipt is the provider's signed receipt covering the task's
	// output, as an object: the receipt (base64 CoreDet-CBOR), its fields
	// decoded, whether this node verified it, and the provider KEL it was
	// checked against. Present with the artifacts once a receipt covers a
	// result. It is evidence, not output, so it is not an artifact
	// (0017 Q21 P2; it was the anet.receipt artifact before).
	KeyReceipt = "anet.receipt"
	// KeyCancelRequested marks a requester's task whose cancel was sent
	// after its payment was submitted (§4.2, 0017 Q3).
	KeyCancelRequested = x402a2a.KeyCancelRequested
)

// Keys reserved on the daemon-to-daemon wire (A2A-DESIGN §3.4) and on
// messages from a local client (§11.5, §11.6).
const (
	// KeyServiceParameters carries A2A service parameters (A2A-Extensions,
	// A2A-Version) across a hop that has no HTTP headers — the relay.
	KeyServiceParameters = "a2a.serviceParameters"
	// KeyA2AError is an A2A error name in a status message.
	KeyA2AError = "anet.a2aError"
	// KeyState is the task state a provider message or result asks for.
	KeyState = "anet.state"
	// KeyFinal marks a provider's last message on a text task it is
	// completing (0017 Q30): sent with anet.state=working so the requester
	// does not stop at it (a blocking send would otherwise return at an
	// input-required the result ends a moment later), the result following
	// it. Unlike a progress note it is a conversation turn: it is in the
	// transcript the receipt covers and in the history. Its value is true.
	KeyFinal = "anet.final"
	// KeyInbound is the inbound-policy outcome a provider reports, such as
	// "pending_approval".
	KeyInbound = "anet.inbound"
	// KeyMessageID is the local client's own messageId, kept in message
	// metadata because the envelope carries the daemon's (§11.5).
	KeyMessageID = "a2a.messageId"
	// KeyTrusted tells a provider-side backend whether the peer is on the
	// trust list (§11.6).
	KeyTrusted = "anet.trusted"
)

// a2a-x402 v0.2 keys (A2A-DESIGN §8.2), as internal/x402a2a defines them.
const (
	X402ExtensionURI = x402a2a.ExtensionURI

	KeyX402Status   = x402a2a.KeyStatus
	KeyX402Required = x402a2a.KeyRequired
	KeyX402Payload  = x402a2a.KeyPayload
	KeyX402Receipts = x402a2a.KeyReceipts
	KeyX402Error    = x402a2a.KeyError
	// KeyPaymentAccept is the option a local client chose, copied from
	// x402.payment.required.accepts (§8.7).
	KeyPaymentAccept = x402a2a.KeyAccept
	// KeyQuoteExpiresAt is when a quote lapses (unix ms).
	KeyQuoteExpiresAt = x402a2a.KeyQuoteExpiresAt
	// KeySettlementReceipt is where a settlement response carries the
	// hub's signed receipt, in its extensions (ANetCore payment.ExtReceipt).
	KeySettlementReceipt = payment.ExtReceipt
)

// x402.payment.status values.
const (
	PaymentRequired  = x402a2a.StatusRequired
	PaymentSubmitted = x402a2a.StatusSubmitted
	PaymentVerified  = x402a2a.StatusVerified
	PaymentCompleted = x402a2a.StatusCompleted
	PaymentFailed    = x402a2a.StatusFailed
	PaymentRejected  = x402a2a.StatusRejected
)

// anet.receipt_verified values. Three, not two: "we checked and it holds",
// "we could not check" and "nobody recorded which" are different answers.
const (
	ReceiptVerified   = "verified"
	ReceiptUnverified = "unverified"
	ReceiptUnknown    = "unknown"
)

// anet.role values.
const (
	RoleRequester = "requester"
	RoleProvider  = "provider"
)

// Part metadata on a file part: the attachment's content id and size, which
// the A2A part has no field for.
const (
	KeyCID  = "anet.cid"
	KeySize = "anet.size"
	// KeyAttachmentCID marks a file part that carries the file's metadata
	// and not its bytes (0017 Q12): in the history and in stream events
	// always, and past the inline limit of a task read. Its value is the
	// content id the bytes are fetched by (GET /attachment on the control
	// plane, or `anet pull`); the part's url names the same attachment.
	KeyAttachmentCID = "anet.attachment_cid"
	// KeyTruncated marks, in a stream event, what was cut to keep the
	// event within MaxStreamEventBytes (stream.go): a message or artifact
	// whose parts were replaced by a notice (with anet.size, the bytes it
	// had as JSON), the metadata of one, or a task whose older history was
	// left out. Its value is true. GetTask gives the whole of it.
	KeyTruncated = "anet.truncated"
)

// Artifact ids: the deliverable, and nothing else. anet.reply is a text
// task's reply with its files; anet.result a capability call's deliverable.
// The receipt is task metadata (KeyReceipt).
const (
	ArtifactReply  = "anet.reply"
	ArtifactResult = "anet.result"
)

// ReasonUnavailable is the anet.reason given to an UNAVAILABLE capability
// answer that carried neither a reason nor a retry hint (A2A-DESIGN §4.3
// wants one of the two).
const ReasonUnavailable = "unavailable"

// ReasonUndeliverable is the anet.reason of a task this node asked for
// whose delegation, or the input it then waited on, could not be delivered
// to the provider before it expired or was refused for good (0017 Q5).
const ReasonUndeliverable = "undeliverable"

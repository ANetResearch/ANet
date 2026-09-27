package a2ashape

// Metadata keys. Every key anet puts on an A2A object is named here, once,
// so that the daemon, the control plane, MCP and module/a2a spell it the
// same way and a test can pin the string (A2A-DESIGN §17: "扩展 URI 与
// metadata 键两侧钉字符串").

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
	KeyTrust           = "anet.trust"
	KeyReason          = "anet.reason"
	KeyRetryAfterMS    = "anet.retry_after_ms"
	KeyCancelRequested = "anet.cancel_requested"
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

// a2a-x402 v0.2 keys (A2A-DESIGN §8.2). Reserved here; the payment flow
// (§8.3) fills them.
const (
	X402ExtensionURI = "https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2"

	KeyX402Status   = "x402.payment.status"
	KeyX402Required = "x402.payment.required"
	KeyX402Payload  = "x402.payment.payload"
	KeyX402Receipts = "x402.payment.receipts"
	KeyX402Error    = "x402.payment.error"
	// KeyPaymentAccept is the option a local client chose, copied from
	// x402.payment.required.accepts (§8.7).
	KeyPaymentAccept = "anet.payment.accept"
	// KeySettlementReceipt is where a settlement response carries the
	// hub's signed receipt, in its extensions.
	KeySettlementReceipt = "anet.settlement.receipt"
)

// x402.payment.status values.
const (
	PaymentRequired  = "payment-required"
	PaymentSubmitted = "payment-submitted"
	PaymentVerified  = "payment-verified"
	PaymentCompleted = "payment-completed"
	PaymentFailed    = "payment-failed"
	PaymentRejected  = "payment-rejected"
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
)

// Artifact ids. anet.reply and anet.result are the deliverable and come
// first; anet.receipt follows; attachments are anet.attachment.<n>.
const (
	ArtifactReply            = "anet.reply"
	ArtifactResult           = "anet.result"
	ArtifactReceipt          = "anet.receipt"
	ArtifactAttachmentPrefix = "anet.attachment."
)

// ReasonUnavailable is the anet.reason given to an UNAVAILABLE capability
// answer that carried neither a reason nor a retry hint (A2A-DESIGN §4.3
// wants one of the two).
const ReasonUnavailable = "unavailable"

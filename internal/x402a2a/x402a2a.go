// Package x402a2a is the vocabulary of a2a-x402 v0.2 as anet speaks it
// (A2A-DESIGN §8.2, §8.5, §2 X4): the extension URI, the metadata keys and
// their values, the a2a-x402 error codes, the anet.* keys and reasons the
// payment flow adds, and the task payment binding.
//
// Constants and one pure function, with no build tag and no dependency on
// the payment module: the kernel (internal/daemon), module/x402, the A2A
// projection (internal/a2ashape) and the local A2A interface (module/a2a)
// all spell the same strings, and the kernel may not import module/x402.
package x402a2a

import (
	"crypto/sha256"
	"encoding/hex"
)

// ExtensionURI is the a2a-x402 extension this node implements (§8.1).
const ExtensionURI = "https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2"

// Metadata keys (a2a-x402 v0.2 §7).
const (
	KeyStatus   = "x402.payment.status"
	KeyRequired = "x402.payment.required"
	KeyPayload  = "x402.payment.payload"
	KeyReceipts = "x402.payment.receipts"
	KeyError    = "x402.payment.error"
)

// Values of x402.payment.status.
const (
	StatusRequired  = "payment-required"
	StatusSubmitted = "payment-submitted"
	StatusRejected  = "payment-rejected"
	StatusVerified  = "payment-verified"
	StatusCompleted = "payment-completed"
	StatusFailed    = "payment-failed"
)

// a2a-x402 error codes (a2a-x402 v0.2 §9.1), the values of
// x402.payment.error.
const (
	CodeInsufficientFunds = "INSUFFICIENT_FUNDS"
	CodeInvalidSignature  = "INVALID_SIGNATURE"
	CodeExpiredPayment    = "EXPIRED_PAYMENT"
	CodeDuplicateNonce    = "DUPLICATE_NONCE"
	CodeNetworkMismatch   = "NETWORK_MISMATCH"
	CodeInvalidAmount     = "INVALID_AMOUNT"
	CodeSettlementFailed  = "SETTLEMENT_FAILED"
)

// anet.* keys the payment flow adds.
const (
	// KeyAccept is the option a local client chose, copied from
	// x402.payment.required.accepts (§8.7).
	KeyAccept = "anet.payment.accept"
	// KeyQuoteExpiresAt is when a quote lapses (unix ms), on the provider's
	// payment-required.
	KeyQuoteExpiresAt = "anet.quote_expires_at"
	// KeyCancelRequested marks a requester's task whose cancel was sent
	// after its payment was submitted: the task stays open and the
	// provider's answer decides (§4.2).
	KeyCancelRequested = "anet.cancel_requested"
	// KeyReason carries the reason behind a code (§8.5) and the reason a
	// task waits (needs_operator_approval).
	KeyReason = "anet.reason"
	// ExtAuthID is where a failure receipt keeps the authorization id the
	// facilitator put in its transaction field; the receipt's transaction
	// is "" as a2a-x402 §9.2 shows it.
	ExtAuthID = "anet.auth_id"
)

// Reasons of anet's own, carried as anet.reason. Each maps to
// SETTLEMENT_FAILED except ReasonQuoteExpired (EXPIRED_PAYMENT).
const (
	ReasonBindingMismatch          = "binding_mismatch"           // the authorization is bound to other work
	ReasonNoPendingQuote           = "no_pending_quote"           // nothing was quoted for the task
	ReasonQuoteExpired             = "quote_expired"              // the quote lapsed (24 hours)
	ReasonPayerMismatch            = "payer_mismatch"             // signed by someone other than the requester
	ReasonClientPayloadUnsupported = "client_payload_unsupported" // a local client sent its own payload (§8.7)
	ReasonOptionNotOffered         = "option_not_offered"         // the chosen option is not a quoted one (§8.7)
	ReasonProviderBusy             = "provider_busy"              // no long-call slot; nothing was settled
	ReasonNeedsOperatorApproval    = "needs_operator_approval"    // the quote is above the automatic tier
	// ReasonExtensionNotActivated is needs_operator_approval as a local A2A
	// client that did not activate this extension is told it (§8.7): it
	// cannot answer the quote itself until it does.
	ReasonExtensionNotActivated = "payment_extension_not_activated"
)

// Activated reports whether a request's A2A-Extensions (as the local A2A
// interface merged them) activate a2a-x402. The one place that decides
// which URIs count (0017 Q18 adds the reference library's v0.1 here).
func Activated(uris []string) bool {
	for _, u := range uris {
		if u == ExtensionURI {
			return true
		}
	}
	return false
}

// PayBind is what a task payment's authorization carries as its
// InteractionID (A2A-DESIGN §2 X4):
//
//	hex(SHA-256("anet/x402-bind/v1" 0x00 ‖ ix ‖ 0x00 ‖ task_nonce))
//
// taskNonce is the TaskDoc's anet.nonce context exactly as it is written
// (base64url, not decoded). The hub stores the binding to allow one
// settlement per task and cannot recover the interaction id from it; the
// two daemons can recompute it.
func PayBind(ix, taskNonce string) string {
	h := sha256.New()
	h.Write([]byte("anet/x402-bind/v1"))
	h.Write([]byte{0})
	h.Write([]byte(ix))
	h.Write([]byte{0})
	h.Write([]byte(taskNonce))
	return hex.EncodeToString(h.Sum(nil))
}

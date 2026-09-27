package a2ashape

import "github.com/ANetResearch/ANet/internal/x402a2a"

// A local client's payment message that this node refuses before anything
// is signed or sent (A2A-DESIGN §8.7): a PaymentPayload of the client's
// own (this node is the signing service; a payload whose payer is not this
// node could not settle even if forwarded), or a chosen option that is not
// one of the quoted accepts. The answer is the task as it stands — the
// quote still waits and may be paid properly — with an agent message that
// says payment-failed, the a2a-x402 code and anet.reason.
//
// The kernel builds the answer (its TaskSeam, the one path payment
// messages take, so a client messageId is deduplicated there like any
// other); the local A2A interface only recognises it, to end a stream
// that has nothing more to wait for.

// PaymentRefusal returns t with status.message saying the payment was not
// submitted. code is the x402.payment.error (SETTLEMENT_FAILED when
// empty), reason the anet.reason, detail a sentence for a person, msgID
// the message's id.
func PaymentRefusal(t Task, msgID, code, reason, detail string) Task {
	if code == "" {
		code = x402a2a.CodeSettlementFailed
	}
	msg := Message{
		ID: msgID, ContextID: t.ContextID, TaskID: t.ID, Role: RoleAgent,
		Parts: []Part{TextPart("payment not submitted: " + detail)},
		Metadata: map[string]any{
			KeyX402Status: PaymentFailed,
			KeyX402Error:  code,
			KeyReason:     reason,
		},
	}
	t.Status.Message = &msg
	return t
}

// IsPaymentRefusal reports an answer made by PaymentRefusal: status.message
// says payment-failed for one of the two reasons only the local signer
// gives (a provider's payment-failed carries a settlement or check reason).
func IsPaymentRefusal(t Task) bool {
	m := t.Status.Message
	if m == nil || m.Metadata[KeyX402Status] != PaymentFailed {
		return false
	}
	switch m.Metadata[KeyReason] {
	case x402a2a.ReasonClientPayloadUnsupported, x402a2a.ReasonOptionNotOffered:
		return true
	}
	return false
}

// PaymentRefusalDetail is the sentence PaymentRefusal gives for a reason.
func PaymentRefusalDetail(reason string) string {
	switch reason {
	case x402a2a.ReasonClientPayloadUnsupported:
		return "this node signs payments itself; a payload made by the client is not forwarded"
	case x402a2a.ReasonOptionNotOffered:
		return "the chosen option is not one of those in x402.payment.required.accepts"
	}
	return reason
}

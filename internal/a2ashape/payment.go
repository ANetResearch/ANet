package a2ashape

import "github.com/ANetResearch/ANet/internal/x402a2a"

// A local client's payment message that this node refuses before anything
// is signed or sent (A2A-DESIGN §8.7): a PaymentPayload of the client's
// own (this node is the signing service; a payload whose payer is not this
// node could not settle even if forwarded), a chosen option that is not
// one of the quoted accepts, or one on a ledger this node holds no credit
// on (an internal note item; the message names the options it can pay). The answer is the task as it stands — the
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
	case x402a2a.ReasonClientPayloadUnsupported, x402a2a.ReasonOptionNotOffered, x402a2a.ReasonRailNotPayable:
		return true
	}
	return false
}

// PaymentRefusalDetail is the sentence PaymentRefusal gives for a reason:
// the reason's row of the table in the node's language, the same sentence
// the CLI and the task's status line give (ux §1.4); a reason with no row
// gets the English text below.
func PaymentRefusalDetail(reason string) string {
	if _, ok := Lookup(reason); ok {
		return Sentence(reason, Lang(), nil)
	}
	switch reason {
	case x402a2a.ReasonClientPayloadUnsupported:
		return "this node signs payments itself; a payload made by the client is not forwarded"
	case x402a2a.ReasonOptionNotOffered:
		return "the chosen option is not one of those in x402.payment.required.accepts"
	case x402a2a.ReasonRailNotPayable:
		return "the chosen option settles on a ledger this node holds no credit on; " +
			"x402.payment.required.accepts lists the options it can pay first"
	}
	return reason
}

// A payment the agent tier did not make (HPP PAY-D2, PAY-D3): a local
// client's payment-submitted that decidePay left to a person — above
// agent_max or the agent's left today, a payee not on the list, a quote
// above the price the payee signed or with none. Nothing was signed or
// sent, and it is not an error: the task still waits, input-required, now
// for the operator, with the decision's anet.reason and its sentence as
// the status message (ux §1.4 D1). The quote's own keys stay, so an
// a2a-x402 client still sees what is asked.

// PaymentHold returns t answering a payment decision the operator has to
// make: status.message is an agent message with text, keeping the x402
// status and quote of the task, and anet.reason is reason in both the
// message and the task's metadata. msgID is the message's id.
func PaymentHold(t Task, msgID, reason, text string) Task {
	meta := map[string]any{KeyReason: reason}
	for _, k := range []string{KeyX402Status, KeyX402Required, KeyQuoteExpiresAt} {
		if v, ok := t.Metadata[k]; ok {
			meta[k] = v
		}
	}
	if _, ok := meta[KeyX402Status]; !ok {
		meta[KeyX402Status] = PaymentRequired
	}
	msg := Message{ID: msgID, ContextID: t.ContextID, TaskID: t.ID, Role: RoleAgent,
		Parts: []Part{TextPart(text)}, Metadata: meta}
	t.Status.Message = &msg
	md := make(map[string]any, len(t.Metadata)+1)
	for k, v := range t.Metadata {
		md[k] = v
	}
	md[KeyReason] = reason
	t.Metadata = md
	return t
}

// IsPaymentHold reports an answer made by PaymentHold: a task that still
// waits and whose status message gives a payment decision left to a person
// as the reason (ux §1.4 D1: needs_operator_approval and its variants,
// quote_above_card_price, card_price_unavailable). The local A2A interface
// ends a stream on it, as on a refusal: nothing was sent, and nothing
// follows until the operator acts.
//
// A client that did not activate a2a-x402 is told the same (HPP PAY-D9):
// the reason, status.message and anet.payment_note are the decision's own,
// word for word what the CLI and MCP show.
func IsPaymentHold(t Task) bool {
	m := t.Status.Message
	if t.Status.State != TaskStateInputRequired || m == nil {
		return false
	}
	r, _ := m.Metadata[KeyReason].(string)
	return r != "" && PayNoteWaits(r)
}

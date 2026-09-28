package a2ashape

// readable.go keeps the projection readable by a generic A2A client
// (0017 Q21, from the Hermes contract test 0018): a client that reads only
// the text of status.message and of the first artifact that has text, and
// never metadata.
//
//   - A status message never has an empty text part: a stored row with no
//     body and no file (a capability call's payment row, a status row that
//     carries only metadata) is given a sentence saying what it records.
//   - A status message about a payment says what is asked — amount, asset,
//     payee and network, from x402.payment.required.accepts — and, on the
//     requester's side, how a client that does not speak a2a-x402 gets it
//     paid (P3). The metadata is unchanged: an a2a-x402 client reads that.
//   - A failed, rejected or canceled task with no message but a reason
//     says "<state>: <reason>" (P4).
//
// Everything written here comes from stored rows and, for the quote, from
// the provider: it is cut to a bounded length and stripped of control
// characters before it becomes text a model may read.

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// Bounds on peer-written text placed in a synthesized sentence.
const (
	maxReasonRunes   = 200
	maxShortRunes    = 64  // amount, asset, scheme
	maxLongRunes     = 256 // payee, network
	maxQuotedOptions = 3
)

// clean makes a peer-written string safe to put in a sentence: control
// characters (newlines included) become spaces, invisible format
// characters (bidirectional overrides and isolates, zero-width joiners) are
// dropped so that a payee or an amount reads as it is, and it is cut to max
// runes.
func clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == utf8.RuneError || unicode.IsControl(r):
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// reasonText is the sentence of P4: the stored state name (the spelling
// Hermes and the CLI show) and the reason.
func reasonText(st interactions.State, reason string) string {
	return string(st) + ": " + clean(reason, maxReasonRunes)
}

// placeholder is the text of stored message i when it has neither a body
// nor a file: what its metadata records. A payment row says what happened
// to the payment; any other row names the state it asks for (or the
// task's) and the reason or inbound outcome it carries.
func (p *projector) placeholder(meta map[string]any) string {
	if s, _ := meta[KeyX402Status].(string); s != "" {
		if note := p.paymentNote(meta); note != "" {
			return paymentStatusText(s) + " " + note
		}
		return paymentStatusText(s)
	}
	st := p.ix.State
	if s, _ := meta[KeyState].(string); interactions.State(s).Valid() {
		st = interactions.State(s)
	}
	for _, k := range []string{KeyReason, KeyInbound} {
		if r, _ := meta[k].(string); strings.TrimSpace(r) != "" {
			return reasonText(st, r)
		}
	}
	return string(st)
}

// paymentStatusText is a sentence for an x402.payment.status value.
func paymentStatusText(status string) string {
	switch status {
	case PaymentRequired:
		return "Payment is required."
	case PaymentSubmitted:
		return "A payment was submitted; waiting for it to settle."
	case PaymentVerified:
		return "The payment was verified."
	case PaymentCompleted:
		return "Payment completed."
	case PaymentFailed:
		return "The payment failed."
	case PaymentRejected:
		return "The payment was rejected."
	}
	return "Payment status: " + clean(status, maxShortRunes) + "."
}

// paymentNote is the text a payment-required (or payment-failed, quote
// still open) message adds to whatever the provider wrote: the options of
// the quote and, on the requester's side, how to get it paid. Empty for
// any other payment status, and when no quote can be read.
func (p *projector) paymentNote(meta map[string]any) string {
	switch s, _ := meta[KeyX402Status].(string); s {
	case PaymentRequired, PaymentFailed:
	default:
		return ""
	}
	req := meta[KeyX402Required]
	if req == nil {
		req = p.payment[KeyX402Required]
	}
	if req == nil {
		req = p.x402Required()
	}
	summary := quoteSummary(req)
	if summary == "" {
		return ""
	}
	if !p.outbound {
		// This node is the provider: its own quote, nothing to pay.
		return "Quoted: " + summary + "."
	}
	s, _ := meta[KeyX402Status].(string)
	return "The provider asks to be paid " + summary + ". " + payHint(p.ix.ID, s == PaymentFailed)
}

// payHint tells a client how a quote on this node's task gets paid
// (A2A-DESIGN §8.6, §8.7): anet pays within its automatic spending tier by
// itself, once per quote and never again after a failed payment; otherwise
// the task waits for a decision. An a2a-x402 client decides by asking this
// node to sign — payment-submitted with no payload, since a payload of the
// client's own is refused (client_payload_unsupported).
func payHint(taskID string, failed bool) string {
	first := "Within this node's automatic spending tier anet pays by itself; above it the task waits here for a decision"
	if failed {
		first = "After a failed payment anet does not pay again by itself: the task waits here for a decision"
	}
	return first + " — the operator's `anet pay " + taskID + "`, or an agent's anet MCP tool submit_payment. " +
		"An a2a-x402 client answers on this task with x402.payment.status payment-submitted and no " +
		"x402.payment.payload (this node signs; anet.payment.accept names the option when there are several)."
}

// quoteSummary renders the accepted options of an x402 PaymentRequired
// ("5 credit to did:anet:x on hub:did:anet:h"; several are joined with
// "or"). Empty when there is no readable option.
func quoteSummary(required any) string {
	m, _ := required.(map[string]any)
	accepts, _ := m["accepts"].([]any)
	var opts []string
	for _, a := range accepts {
		o, _ := a.(map[string]any)
		if o == nil {
			continue
		}
		amount := field(o, "amount")
		if amount == "" {
			amount = field(o, "maxAmountRequired") // x402 v1
		}
		if amount == "" {
			continue
		}
		s := clean(amount, maxShortRunes)
		if asset := field(o, "asset"); asset != "" {
			s += " " + clean(asset, maxShortRunes)
		}
		if to := field(o, "payTo"); to != "" {
			s += " to " + clean(to, maxLongRunes)
		}
		if n := field(o, "network"); n != "" {
			s += " on " + clean(n, maxLongRunes)
		}
		if sc := field(o, "scheme"); sc != "" {
			s += " (" + clean(sc, maxShortRunes) + ")"
		}
		opts = append(opts, s)
		if len(opts) == maxQuotedOptions {
			break
		}
	}
	return strings.Join(opts, "; or ")
}

// field reads a scalar member of a decoded JSON object as text.
func field(o map[string]any, k string) string {
	switch v := o[k].(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case map[string]any, []any:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

//go:build !no_x402

package x402

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"

	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// The merchant's side of a task payment (A2A-DESIGN §8.4, §8.5).
//
// Two things live here: the check a provider makes before it presents a
// payment to the facilitator, and the table that turns a reason — the
// facilitator's errorReason or this check's own — into the a2a-x402 error
// code the requester is sent. They are one file because the check's
// reasons are rows of the table, and a reason the table does not know
// is a payment-failed with no honest code.

// ExtensionURI is the a2a-x402 extension this node speaks (§8.1).
const ExtensionURI = x402a2a.ExtensionURI

// Reasons of this node's own check, beside the x402 errorReason values in
// ANetCore payment (the strings are x402a2a's, shared with the kernel).
const (
	ReasonBindingMismatch          = x402a2a.ReasonBindingMismatch
	ReasonNoPendingQuote           = x402a2a.ReasonNoPendingQuote
	ReasonQuoteExpired             = x402a2a.ReasonQuoteExpired
	ReasonPayerMismatch            = x402a2a.ReasonPayerMismatch
	ReasonClientPayloadUnsupported = x402a2a.ReasonClientPayloadUnsupported
	ReasonOptionNotOffered         = x402a2a.ReasonOptionNotOffered
)

// errorCodes is the table of §8.5. Keys are exact strings: the hub sets
// errorReason to one of its constants with no suffix, so the lookup is on
// the whole value. A reason not listed maps to SETTLEMENT_FAILED.
//
// Every errorReason ANetCore defines is listed, so that adding one there
// and not here is caught by the pinning test rather than by a requester
// reading SETTLEMENT_FAILED for what was an expiry.
var errorCodes = map[string]string{
	payment.ReasonInsufficientFunds: x402a2a.CodeInsufficientFunds,
	payment.ReasonInvalidSignature:  x402a2a.CodeInvalidSignature,
	payment.ReasonExpiredPayment:    x402a2a.CodeExpiredPayment,
	payment.ReasonExpired:           x402a2a.CodeExpiredPayment, // the old spelling; no hub sends it
	ReasonQuoteExpired:              x402a2a.CodeExpiredPayment,
	payment.ReasonDuplicateNonce:    x402a2a.CodeDuplicateNonce,
	payment.ReasonDuplicateBinding:  x402a2a.CodeDuplicateNonce,
	payment.ReasonNetworkMismatch:   x402a2a.CodeNetworkMismatch,
	payment.ReasonInvalidAmount:     x402a2a.CodeInvalidAmount,

	payment.ReasonPayeeMismatch:       x402a2a.CodeSettlementFailed,
	payment.ReasonUnsupportedScheme:   x402a2a.CodeSettlementFailed,
	payment.ReasonUnknownPayer:        x402a2a.CodeSettlementFailed,
	payment.ReasonSettlementFailed:    x402a2a.CodeSettlementFailed,
	payment.ReasonMalformed:           x402a2a.CodeSettlementFailed,
	payment.ReasonInvalidRequirements: x402a2a.CodeSettlementFailed,
	ReasonBindingMismatch:             x402a2a.CodeSettlementFailed,
	ReasonNoPendingQuote:              x402a2a.CodeSettlementFailed,
	ReasonClientPayloadUnsupported:    x402a2a.CodeSettlementFailed,
	ReasonOptionNotOffered:            x402a2a.CodeSettlementFailed,
	ReasonPayerMismatch:               x402a2a.CodeSettlementFailed,
	x402a2a.ReasonProviderBusy:        x402a2a.CodeSettlementFailed,
}

// ErrorCode maps a reason to its x402.payment.error code. final is false
// for settlement_pending, which is not an outcome: nothing is sent for it
// and the payment is presented again.
func ErrorCode(reason string) (code string, final bool) {
	if reason == payment.ReasonSettlementPending {
		return "", false
	}
	if c, ok := errorCodes[reason]; ok {
		return c, true
	}
	return x402a2a.CodeSettlementFailed, true
}

// PaymentError implements module.Payer.
func (m *Module) PaymentError(reason string) (string, bool) { return ErrorCode(reason) }

// CheckPayment implements module.Payer: the merchant's check (§8.4).
//
// Every term the authorization states is compared with what this node
// quoted for the task, before the payment goes anywhere. The facilitator
// makes the same comparison against the requirements it is sent, and the
// requirements are exactly the quoted option this returns — but a check
// the payee does not make itself is a check it delegates to the party
// holding the money, and the binding to the task is something only the
// payee can check at all.
func (m *Module) CheckPayment(raw []byte, t module.PaymentTerms) module.PaymentCheck {
	var out module.PaymentCheck
	fail := func(reason, format string, args ...any) module.PaymentCheck {
		out.Reason = reason
		out.Code, _ = ErrorCode(reason)
		out.Detail = fmt.Sprintf(format, args...)
		return out
	}
	pp, auth, err := decodePayload(raw)
	if pp != nil && pp.Accepted.Scheme != payment.SchemeCredit {
		// Not a scheme this node quoted, whatever else is wrong with it.
		return fail(payment.ReasonNetworkMismatch, "scheme %q is not among the quoted options", pp.Accepted.Scheme)
	}
	if err != nil {
		return fail(payment.ReasonMalformed, "%v", err)
	}
	if id, err := auth.ID(); err == nil {
		out.AuthID = id
	}
	out.Payer, out.Amount = auth.Payer, auth.Amount
	if t.Quoted == nil || len(t.Quoted.Accepts) == 0 {
		return fail(ReasonNoPendingQuote, "no quote is pending for this task")
	}
	// The option the payment claims to take, matched on what the
	// authorization signs: scheme is the payload's, network the
	// authorization's. The payload's own claim of network must agree.
	var opt *payment.PaymentOption
	for i := range t.Quoted.Accepts {
		o := &t.Quoted.Accepts[i]
		if o.Scheme == pp.Accepted.Scheme && o.Network == auth.Network {
			opt = o
			break
		}
	}
	if opt != nil {
		out.Requirements = sanitizeRequirements(*opt)
		out.Matched = true
	}
	if t.QuoteExpiresAt > 0 && t.Now > t.QuoteExpiresAt {
		return fail(ReasonQuoteExpired, "the quote expired before the payment arrived")
	}
	self := m.AID()
	if auth.PayTo != self || pp.Accepted.PayTo != auth.PayTo {
		return fail(payment.ReasonPayeeMismatch, "the payment pays %s, not this node", auth.PayTo)
	}
	if t.Payer != "" && auth.Payer != t.Payer {
		return fail(ReasonPayerMismatch, "the payment is signed by %s, not the task's requester", auth.Payer)
	}
	if t.Bind == "" || auth.InteractionID != t.Bind {
		return fail(ReasonBindingMismatch, "the payment is bound to other work")
	}
	if opt == nil || pp.Accepted.Network != auth.Network {
		return fail(payment.ReasonNetworkMismatch,
			"%s on %s is not among the quoted options", pp.Accepted.Scheme, auth.Network)
	}
	want, err := payment.ParseAmount(opt.Amount)
	if err != nil {
		return fail(payment.ReasonSettlementFailed, "the stored quote is unreadable: %v", err)
	}
	// "At least the quote" has an upper end: what a credit ledger can
	// hold. The hub books amounts as int64, and one without the range
	// check booked an amount above math.MaxInt64 as a negative one — the
	// payer credited, this node debited — and answered success, so this
	// node would have run the paid call having been charged for it (red
	// team si9). An authorization for such an amount is not presented.
	if auth.Amount > math.MaxInt64 {
		return fail(payment.ReasonInvalidAmount,
			"the payment is for %d, more than a credit ledger can hold (%d)", auth.Amount, int64(math.MaxInt64))
	}
	accepted, err := payment.ParseAmount(pp.Accepted.Amount)
	if err != nil || accepted != auth.Amount || auth.Amount < want {
		return fail(payment.ReasonInvalidAmount, "the payment is for %d; the quote is %d", auth.Amount, want)
	}
	if auth.NotAfter <= auth.IssuedAt || t.Now > auth.NotAfter+payment.ClockSkew ||
		auth.IssuedAt > t.Now+payment.ClockSkew {
		return fail(payment.ReasonExpiredPayment, "the authorization is outside its validity window")
	}
	return out
}

// sanitizeRequirements is a quoted option as it goes to the facilitator:
// the terms the hub compares and nothing that describes the work (SI-1).
func sanitizeRequirements(o payment.PaymentOption) payment.PaymentRequirements {
	return payment.PaymentRequirements{
		Scheme: o.Scheme, Network: o.Network, Amount: o.Amount, Asset: o.Asset,
		PayTo: o.PayTo, MaxTimeoutSeconds: o.MaxTimeoutSeconds,
	}
}

// decodePayload reads a PaymentPayload and its anet-credit authorization.
func decodePayload(raw []byte) (*payment.PaymentPayload, *payment.Authorization, error) {
	var pp payment.PaymentPayload
	if err := json.Unmarshal(raw, &pp); err != nil {
		return nil, nil, fmt.Errorf("payment payload malformed: %w", err)
	}
	if pp.Accepted.Scheme != payment.SchemeCredit {
		return &pp, nil, fmt.Errorf("scheme %q is not %s", pp.Accepted.Scheme, payment.SchemeCredit)
	}
	enc, _ := pp.Payload["authorization"].(string)
	if enc == "" {
		return &pp, nil, fmt.Errorf("payload carries no authorization")
	}
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return &pp, nil, fmt.Errorf("authorization is not base64: %w", err)
	}
	auth, err := payment.UnmarshalAuthorization(b)
	if err != nil {
		return &pp, nil, fmt.Errorf("authorization malformed: %w", err)
	}
	return &pp, auth, nil
}

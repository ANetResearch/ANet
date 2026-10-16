package a2ashape

// paynote.go reads the payment decision a task stores (pay_note, HPP
// PAY-U5) and says what kind of sentence it is. The snapshot is written by
// the daemon's decidePay and rendered by PaymentNote (reasons.go); this file
// is the one decoder of the stored form, so the projection, the CLI and any
// later reader take the same snapshot or none.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ANetResearch/ANet/internal/jsonread"
)

// The payment tiers a snapshot names ({tier} of ux D1, HPP §0.3).
const (
	PayTierAuto   = "auto"
	PayTierAgent  = "agent"
	PayTierManual = "manual"
)

// maxPayNote bounds a stored snapshot: a code, a dozen numbers, a reason
// short form and an AID fit in far less.
const maxPayNote = 4 << 10

// ParsePayNote reads a stored snapshot. It is strict: one JSON object with
// PayNote's members and no others, each spelled one way (jsonread), a code
// of ux §1.4 D1, D2 or D3 and one of the three tiers when a tier is given.
// Anything else is an error, and a reader shows no payment sentence rather
// than one it would have to guess.
func ParsePayNote(raw []byte) (PayNote, error) {
	var n PayNote
	if len(raw) == 0 {
		return n, errors.New("no payment decision")
	}
	if len(raw) > maxPayNote {
		return n, fmt.Errorf("payment decision of %d bytes", len(raw))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&n); err != nil {
		return PayNote{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return PayNote{}, errors.New("payment decision: data after the object")
	}
	if !jsonread.ReadsAlike(raw, &n) {
		return PayNote{}, errors.New("payment decision: a member spelled two ways")
	}
	if !PayNoteCode(n.Code) {
		return PayNote{}, fmt.Errorf("payment decision: %q is not a payment sentence", n.Code)
	}
	switch n.Tier {
	case "", PayTierAuto, PayTierAgent, PayTierManual:
	default:
		return PayNote{}, fmt.Errorf("payment decision: tier %q", n.Tier)
	}
	return n, nil
}

// PayNoteCode reports a key of ux §1.4 D1, D2 or D3: the sentences a
// payment decision or a payment that did not settle is said in.
func PayNoteCode(code string) bool {
	if _, ok := Lookup(code); !ok {
		return false
	}
	return PayNoteWaits(code) || strings.HasPrefix(code, "paid.") || payNoteD3[code] ||
		strings.HasPrefix(code, "x402:")
}

// PayNoteWaits reports a code of ux §1.4 D1: nothing was paid, and the
// task waits for a person to decide (input-required). Its code — the key
// without the variant — is the task's anet.reason while it waits.
func PayNoteWaits(code string) bool {
	base, _, _ := strings.Cut(code, ".")
	switch base {
	case "needs_operator_approval", "quote_above_card_price", "card_price_unavailable":
		return true
	case "module_absent":
		return code == "module_absent.payment"
	}
	return false
}

// payNoteD3 are the anet codes of ux §1.4 D3 (the x402:<CODE> rows are
// recognised by their prefix).
var payNoteD3 = map[string]bool{
	"quote_expired": true, "rail_not_payable": true, "option_not_offered": true,
	"client_payload_unsupported": true, "no_pending_quote": true, "binding_mismatch": true,
	"payer_mismatch": true, "payee_not_local": true, "auth_window_too_long": true,
}

// PayNoteArgs are the placeholder values of a snapshot: its numbers as they
// were, the task and the payee.
func PayNoteArgs(n PayNote) Args {
	a := Args{"amount": n.Amount, "price": n.Price, "tier": n.Tier, "single": n.Single, "left": n.Left,
		"key": n.Key, "value": n.Value, "why": n.Why, "peer": n.Peer, "task": n.Task}
	if n.Code == "module_absent.payment" {
		// {why} and {fix} of the module sentences: the module is the payment
		// module, and Why holds how it is absent (module.Absent*).
		a["m"], a["reason"] = "x402", n.Why
		if n.Why == "" {
			a["reason"] = "not_compiled"
		}
	}
	return a
}

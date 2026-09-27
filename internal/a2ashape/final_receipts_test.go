package a2ashape_test

import (
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// a2a-x402 §7 (0017 Q18): the final message of every task that took part
// in the payment flow carries x402.payment.receipts, also when nothing was
// paid — a declined quote, a lapsed one, a cancel before paying — and says
// no payment status for a quote that simply ended unpaid.
func TestTheFinalMessageOfAQuotedTaskCarriesTheReceipts(t *testing.T) {
	quote := `{"x402Version":2,"accepts":[{"scheme":"anet-credit","network":"hub:did:anet:h","amount":"5","payTo":"did:anet:peer"}]}`
	for _, c := range []struct {
		name       string
		state      interactions.State
		payState   string
		kernel     map[string]any
		wantStatus any
	}{
		{"declined", interactions.StateCanceled, interactions.PayRejected,
			map[string]any{a2ashape.KeyX402Status: a2ashape.PaymentRejected, a2ashape.KeyX402Receipts: []json.RawMessage{}},
			"payment-rejected"},
		{"lapsed", interactions.StateFailed, interactions.PayFailed,
			map[string]any{a2ashape.KeyX402Status: a2ashape.PaymentFailed, a2ashape.KeyX402Error: "EXPIRED_PAYMENT",
				a2ashape.KeyX402Receipts: []json.RawMessage{json.RawMessage(
					`{"success":false,"errorReason":"quote_expired","network":"n","transaction":""}`)}},
			"payment-failed"},
		{"canceled with the quote open", interactions.StateCanceled, interactions.PayRequired,
			map[string]any{a2ashape.KeyX402Receipts: []json.RawMessage{}}, nil},
		{"no kernel reading, declined", interactions.StateCanceled, interactions.PayRejected, nil, "payment-rejected"},
		{"no kernel reading, canceled with the quote open", interactions.StateCanceled, interactions.PayRequired, nil, nil},
		// Completed with the quote never paid (a provider that answered
		// anyway): the receipts, empty, and no payment status.
		{"completed with the quote open", interactions.StateCompleted, interactions.PayRequired,
			map[string]any{a2ashape.KeyX402Receipts: []json.RawMessage{}}, nil},
		{"no kernel reading, completed with the quote open", interactions.StateCompleted, interactions.PayRequired, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := openStore(t)
			capTask(t, st, "ix_f", interactions.StateInputRequired, "", "")
			setState(t, st, "ix_f", c.state)
			src, err := a2ashape.Load(st, "ix_f")
			must(t, err)
			src.Interaction.PayState = c.payState
			src.Interaction.PayRequired = []byte(quote)
			src.Payment = c.kernel
			sdk := contract(t, a2ashape.Project(src, a2ashape.Options{}))
			m := sdk.Status.Message
			if m == nil {
				t.Fatalf("no final message: %+v", sdk.Status)
			}
			if _, ok := m.Metadata[a2ashape.KeyX402Receipts]; !ok {
				t.Fatalf("the final message carries no receipts: %v", m.Metadata)
			}
			if got := m.Metadata[a2ashape.KeyX402Status]; got != c.wantStatus {
				t.Errorf("x402.payment.status = %v, want %v", got, c.wantStatus)
			}
		})
	}

	// A task outside the payment flow has no x402 part on its final message.
	st := openStore(t)
	capTask(t, st, "ix_n", interactions.StateInputRequired, "", "")
	setState(t, st, "ix_n", interactions.StateCanceled)
	src, err := a2ashape.Load(st, "ix_n")
	must(t, err)
	src.Payment = map[string]any{}
	sdk := contract(t, a2ashape.Project(src, a2ashape.Options{}))
	if m := sdk.Status.Message; m != nil {
		if _, ok := m.Metadata[a2ashape.KeyX402Receipts]; ok {
			t.Errorf("receipts on a task nothing was quoted for: %v", m.Metadata)
		}
	}
}

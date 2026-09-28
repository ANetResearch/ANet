package daemon

import (
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANet/internal/x402a2a"
)

// A provider's quote is stored only when it reads the same to the client
// that shows it and to this node, which signs from it (docs/notes/0033,
// found by FuzzX402Quote). Go decodes payment.PaymentOption ignoring the
// case of member names, taking the last match; everyone else reads names
// as written. An option that spells a field twice, or only in another
// case, was shown with one amount (or payee, or network) and signed with
// another.
func TestAQuoteThatReadsDifferentlyIsNotTaken(t *testing.T) {
	opt := func(extra string) string {
		return `{"scheme":"credit","network":"anet:hub","amount":"1","asset":"credit","payTo":"peer","maxTimeoutSeconds":60` + extra + `}`
	}
	quote := func(accepts string) []byte {
		b, _ := json.Marshal(map[string]json.RawMessage{
			x402a2a.KeyStatus:   json.RawMessage(`"payment-required"`),
			x402a2a.KeyRequired: json.RawMessage(`{"x402Version":1,"accepts":[` + accepts + `]}`),
		})
		return b
	}
	for name, accepts := range map[string]string{
		"amount twice":         opt(`,"AMOUNT":"900"`),
		"amount in other case": `{"scheme":"credit","network":"anet:hub","Amount":"900","asset":"credit","payTo":"peer"}`,
		"payee twice":          opt(`,"payto":"someone-else"`),
		"network twice":        opt(`,"Network":"elsewhere"`),
		"accepts twice":        opt(``) + `],"ACCEPTS":[` + opt(`,"amount":"900"`),
	} {
		if _, ok := quoteOf(quote(accepts)); ok {
			t.Errorf("%s: a quote whose option reads differently was taken", name)
		}
	}
	for name, accepts := range map[string]string{
		"plain":               opt(``),
		"extra member":        opt(`,"extra":{"name":"credit","Name":"x"},"description":"d"`),
		"no timeout":          `{"scheme":"credit","network":"anet:hub","amount":"1","asset":"credit","payTo":"peer"}`,
		"a member left zero":  `{"scheme":"credit","network":"anet:hub","amount":"1","payTo":"peer"}`,
		"two options":         opt(``) + `,` + `{"scheme":"exact","network":"base","amount":"5","asset":"usdc","payTo":"0x1"}`,
		"exact name repeated": opt(`,"amount":"1"`),
	} {
		if _, ok := quoteOf(quote(accepts)); !ok {
			t.Errorf("%s: a quote that reads the same to everyone was not taken", name)
		}
	}
}

package a2ashape_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// FuzzParsePayNote: the stored payment decision's decoder (pay_note, HPP
// PAY-U5). Whatever it takes reads back the same after it is written again,
// and renders as a whole sentence in both languages; it never panics.
func FuzzParsePayNote(f *testing.F) {
	for _, s := range []string{
		`{"code":"paid.auto","amount":3,"tier":"auto","single":5,"left":44,"at":"2026-10-10T09:00:00Z"}`,
		`{"code":"needs_operator_approval.manual_limit","amount":12,"price":3,"tier":"auto","single":10,"left":50,"key":"explicit_max","value":12,"peer":"bafyreixtplpft7v4seh2kvj72ceuvw75efr6edt4sywb5wkh7d6kx2wxyz","at":"2026-10-10T09:00:00Z"}`,
		`{"code":"card_price_unavailable","amount":3,"tier":"agent","single":5,"left":1,"why":"strangers_busy","at":"2026-10-10T09:00:00Z"}`,
		`{"code":"x402:INVALID_SIGNATURE.rotated","amount":3,"tier":"","single":0,"left":0,"at":"2026-10-10T09:00:00Z"}`,
		`{"code":"quote_above_card_price","Code":"paid.auto","amount":3,"tier":"auto","single":5,"left":1,"at":"2026-10-10T09:00:00Z"}`,
		`{"code":"paid.auto","amount":3e2,"tier":"auto","single":5,"left":1,"at":"2026-10-10T09:00:00Z"}`,
		`null`, `{}`, `[]`, `{"code":"paid.manual"} {}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		n, err := a2ashape.ParsePayNote(raw)
		if err != nil {
			return
		}
		b, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		back, err := a2ashape.ParsePayNote(b)
		if err != nil {
			t.Fatalf("%s does not read back: %v", b, err)
		}
		b2, _ := json.Marshal(back)
		if string(b) != string(b2) {
			t.Fatalf("not stable:\n%s\n%s", b, b2)
		}
		n.Task = "t_9f3c"
		for _, lang := range []a2ashape.Language{a2ashape.English, a2ashape.Chinese} {
			s := a2ashape.PaymentNote(n, lang)
			if s == "" || strings.ContainsAny(s, "{}") {
				t.Fatalf("%s renders %q", raw, s)
			}
		}
	})
}

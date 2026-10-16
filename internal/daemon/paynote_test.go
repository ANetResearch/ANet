package daemon

// G-PAY-NOTE-1 (HPP §7.1): decidePay from its input to its snapshot, for
// every step of PAY-D2, the override and the manual tier, and the sentence
// a2ashape.PaymentNote makes of each snapshot in both languages (part of
// UX-T1). The cases below say what each decision must be; the files under
// testdata/paynote are the vectors they produce, written with
// -update-paynote and compared byte for byte otherwise, so a change to the
// decision, the snapshot's form or a sentence shows in review.

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

var updatePayNote = flag.Bool("update-paynote", false, "rewrite testdata/paynote from the cases")

// payNoteAt is the decision time of every vector.
var payNoteAt = time.Date(2026, 10, 10, 9, 30, 0, 0, time.UTC)

const payNotePeer = "bafyreixtplpft7v4seh2kvj72ceuvw75efr6edt4sywb5wkh7d6kx2wxyz"

// payNoteBase is a decision that pays at the automatic tier: auto_max 5,
// 6 left today, the payee listed, a signed price of 3 for a quote of 3.
func payNoteBase() PayInput {
	return PayInput{Purpose: module.PurposeTaskAuto, Amount: 3, CapID: "translate",
		Price: cardPrice{Have: true, Price: 3}, Single: 5, Left: 6, ManualSingle: 10, ManualLeft: 6, SpentToday: 44,
		PayeesReadable: true, PayeeListed: true, Peer: payNotePeer, At: payNoteAt}
}

// payNoteCase is one vector: the input, and what the snapshot must say.
type payNoteCase struct {
	name string
	in   func(*PayInput)
	// code, left, single and the optional fields are the snapshot's.
	code         string
	single, left uint64
	price        *uint64
	why, key     string
	value        *uint64
	tier         string
}

func payNoteCases() []payNoteCase {
	p := func(n uint64) *uint64 { return &n }
	return []payNoteCase{
		{name: "d2-1-off", in: func(in *PayInput) { in.Single = 0 }, code: "needs_operator_approval.off", single: 0, left: 6},
		{name: "d2-1-off-agent", in: func(in *PayInput) { in.Purpose, in.Single = module.PurposeTaskAgent, 0 },
			code: "needs_operator_approval.off", single: 0, left: 6, tier: "agent"},
		{name: "d2-2-text-task", in: func(in *PayInput) { in.CapID = ""; in.Price = cardPrice{} },
			code: "card_price_unavailable", single: 5, left: 6, why: "text_task"},
		{name: "d2-3-timeout", in: func(in *PayInput) { in.Price = cardPrice{Why: priceWhyTimeout} },
			code: "card_price_unavailable", single: 5, left: 6, why: "timeout"},
		{name: "d2-3-not-offered", in: func(in *PayInput) { in.Price = cardPrice{Why: priceWhyNotOffered} },
			code: "card_price_unavailable", single: 5, left: 6, why: "not_offered"},
		{name: "d2-3-bad-signature", in: func(in *PayInput) { in.Price = cardPrice{Why: priceWhyBadSignature} },
			code: "card_price_unavailable", single: 5, left: 6, why: "bad_signature"},
		{name: "d2-3-hub-unreachable", in: func(in *PayInput) { in.Price = cardPrice{Why: priceWhyHubUnreachable} },
			code: "card_price_unavailable", single: 5, left: 6, why: "hub_unreachable"},
		{name: "d2-3-wait-code", in: func(in *PayInput) { in.Price = cardPrice{Why: "strangers_busy"} },
			code: "card_price_unavailable", single: 5, left: 6, why: "strangers_busy"},
		{name: "d2-4-above-price", in: func(in *PayInput) { in.Amount = 5 },
			code: "quote_above_card_price", single: 5, left: 6, price: p(3)},
		{name: "d2-4-unlisted-skill", in: func(in *PayInput) { in.Price = cardPrice{Have: true, Price: 0} },
			code: "quote_above_card_price", single: 5, left: 6, price: p(0)},
		{name: "d2-5-payees-unreadable", in: func(in *PayInput) { in.PayeesReadable, in.PayeeListed = false, false },
			code: "needs_operator_approval.payees_unreadable", single: 5, left: 6, price: p(3)},
		{name: "d2-5-payee-not-allowed", in: func(in *PayInput) { in.PayeeListed = false },
			code: "needs_operator_approval.payee_not_allowed", single: 5, left: 6, price: p(3)},
		{name: "d2-6-over-single", in: func(in *PayInput) { in.Amount, in.Price.Price, in.ManualLeft = 6, 6, 50 },
			code: "needs_operator_approval.over_limit", single: 5, left: 6, price: p(6)},
		{name: "d2-6-over-left", in: func(in *PayInput) { in.Left, in.ManualLeft = 2, 2; in.Amount, in.Price.Price, in.ManualLeft = 3, 3, 30 },
			code: "needs_operator_approval.over_limit", single: 5, left: 2, price: p(3)},
		{name: "d2-6-agent-daily", in: func(in *PayInput) {
			in.Purpose, in.Single, in.Left, in.ManualLeft = module.PurposeTaskAgent, 5, 1, 6
		}, code: "needs_operator_approval.over_limit", single: 5, left: 1, price: p(3), tier: "agent"},
		{name: "d2-7-paid-auto", code: "paid.auto", single: 5, left: 3, price: p(3)},
		{name: "d2-7-paid-agent", in: func(in *PayInput) { in.Purpose = module.PurposeTaskAgent },
			code: "paid.agent", single: 5, left: 3, price: p(3), tier: "agent"},
		{name: "override-explicit-max", in: func(in *PayInput) { in.Single, in.Amount, in.ManualLeft = 0, 12, 40 },
			code: "needs_operator_approval.manual_limit", single: 10, left: 40, key: "explicit_max", value: p(12)},
		{name: "override-daily-max", in: func(in *PayInput) { in.Amount, in.Price.Price, in.ManualLeft = 8, 8, 6 },
			code: "needs_operator_approval.manual_limit", single: 10, left: 6, price: p(8), key: "daily_max", value: p(52)},
		{name: "override-both", in: func(in *PayInput) { in.Amount, in.ManualLeft = 30, 6 },
			code: "needs_operator_approval.manual_limit", single: 10, left: 6, price: p(3), key: "explicit_max=30 daily_max",
			value: p(74)},
		{name: "override-price", in: func(in *PayInput) { in.Amount, in.ManualLeft = 30, 50 },
			code: "needs_operator_approval.manual_limit", single: 10, left: 50, price: p(3), key: "explicit_max", value: p(30)},
		{name: "override-text-task", in: func(in *PayInput) { in.CapID, in.Price, in.Amount = "", cardPrice{}, 11 },
			code: "needs_operator_approval.manual_limit", single: 10, left: 6, why: "text_task", key: "explicit_max=11 daily_max",
			value: p(55)},
		{name: "manual-paid", in: func(in *PayInput) { in.Purpose = module.PurposeTaskManual },
			code: "paid.manual", single: 10, left: 3, tier: "manual"},
		{name: "manual-paid-no-price", in: func(in *PayInput) {
			in.Purpose, in.CapID, in.Price, in.PayeeListed = module.PurposeTaskManual, "", cardPrice{}, false
		}, code: "paid.manual", single: 10, left: 3, tier: "manual"},
	}
}

// payNoteVector is one file under testdata/paynote.
type payNoteVector struct {
	Input struct {
		Tier           string  `json:"tier"`
		Amount         uint64  `json:"amount"`
		Capability     string  `json:"capability"`
		SignedPrice    *uint64 `json:"signed_price"`
		PriceWhy       string  `json:"price_why,omitempty"`
		Single         uint64  `json:"single"`
		Left           uint64  `json:"left"`
		ManualSingle   uint64  `json:"manual_single"`
		ManualLeft     uint64  `json:"manual_left"`
		SpentToday     uint64  `json:"spent_today"`
		PayeesReadable bool    `json:"payees_readable"`
		PayeeListed    bool    `json:"payee_listed"`
	} `json:"input"`
	Snapshot json.RawMessage `json:"snapshot"`
	Zh       string          `json:"zh"`
	En       string          `json:"en"`
}

func (c payNoteCase) input() PayInput {
	in := payNoteBase()
	if c.in != nil {
		c.in(&in)
	}
	return in
}

func vectorOf(t *testing.T, in PayInput, n a2ashape.PayNote) []byte {
	t.Helper()
	var v payNoteVector
	v.Input.Tier, v.Input.Amount, v.Input.Capability = tierName(in.Purpose), in.Amount, in.CapID
	if in.Price.Have {
		price := in.Price.Price
		v.Input.SignedPrice = &price
	}
	v.Input.PriceWhy = in.Price.Why
	v.Input.Single, v.Input.Left, v.Input.ManualSingle, v.Input.ManualLeft = in.Single, in.Left, in.ManualSingle, in.ManualLeft
	v.Input.SpentToday, v.Input.PayeesReadable, v.Input.PayeeListed = in.SpentToday, in.PayeesReadable, in.PayeeListed
	snap, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	v.Snapshot = snap
	n.Task = "t_9f3c"
	v.Zh, v.En = a2ashape.PaymentNote(n, a2ashape.Chinese), a2ashape.PaymentNote(n, a2ashape.English)
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func TestGPayNote1(t *testing.T) {
	dir := filepath.Join("testdata", "paynote")
	seen := map[string]bool{}
	for _, c := range payNoteCases() {
		t.Run(c.name, func(t *testing.T) {
			in := c.input()
			n := decidePay(in)
			tier := c.tier
			if tier == "" {
				tier = "auto"
			}
			if n.Code != c.code || n.Single != c.single || n.Left != c.left || n.Why != c.why || n.Key != c.key ||
				n.Tier != tier || n.Peer != payNotePeer || !n.At.Equal(payNoteAt) ||
				!sameU64(n.Price, c.price) || !sameU64(n.Value, c.value) {
				t.Fatalf("decidePay = %+v (price %v, value %v)", n, deref(n.Price), deref(n.Value))
			}
			if !a2ashape.PayNoteCode(n.Code) && in.Purpose != module.PurposeTaskManual {
				t.Fatalf("%s is not a payment sentence", n.Code)
			}
			got := vectorOf(t, in, n)
			path := filepath.Join(dir, c.name+".json")
			seen[c.name+".json"] = true
			if *updatePayNote {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (go test -run TestGPayNote1 -update-paynote writes it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s differs from the decision and its sentences:\n got %s\nwant %s", path, got, want)
			}
			// The stored form reads back as it was (ParsePayNote), and the
			// sentence is made of the snapshot alone (PAY-U6).
			var v payNoteVector
			if err := json.Unmarshal(want, &v); err != nil {
				t.Fatal(err)
			}
			back, err := a2ashape.ParsePayNote(v.Snapshot)
			if err != nil {
				t.Fatalf("the snapshot does not read back: %v", err)
			}
			back.Task = "t_9f3c"
			if a2ashape.PaymentNote(back, a2ashape.Chinese) != v.Zh || a2ashape.PaymentNote(back, a2ashape.English) != v.En {
				t.Fatalf("the stored snapshot renders otherwise")
			}
		})
	}
	if *updatePayNote {
		return
	}
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if !seen[f.Name()] {
			t.Errorf("%s has no case", f.Name())
		}
	}
}

func sameU64(a, b *uint64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func deref(p *uint64) any {
	if p == nil {
		return nil
	}
	return *p
}

package daemon

// Fuzz targets for the x402 metadata a peer writes (docs/notes/0033): a
// provider's quote (x402.payment.required) and its settlement responses
// (x402.payment.receipts), as this node stores and restates them.

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
)

// FuzzX402Receipts: a provider's receipt list, restated with this node's
// verdicts (withVerdict) and split for display (a2ashape.SplitReceipts).
// Only a settlement whose transaction this node verified is stated as its
// own success; a verdict the provider wrote is never taken; what is stored
// is JSON; and bounding the list never drops a verified settlement.
func FuzzX402Receipts(f *testing.F) {
	f.Add([]byte(`[{"success":true,"transaction":"t1","network":"anet","payer":"p","extensions":{"anet.settlement_verified":"verified"}},{"success":false,"errorReason":"x"}]`), "t1")
	f.Add([]byte(`[{"success":true,"transaction":"t2","extensions":{"anet.settlement_verified":"verified","x402.receipt":"cmM="}}]`), "t1")
	f.Add([]byte(`[{"success":"true","transaction":"t1"},{"success":1},{"success":false,"extensions":{"anet.settlement_verified":"verified"}}]`), "t1")
	f.Add([]byte(`[{"success":true,"success":false,"transaction":"t1"},{"success":true,"transaction":"t1","transaction":"t3"}]`), "t1")
	f.Add([]byte(`[{"success":true,"transaction":"t1","amount":1e400}, null, [], "x"]`), "t1")
	f.Fuzz(func(t *testing.T, raw []byte, verifiedTxID string) {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) != nil {
			return
		}
		verified := map[string]settledFacts{}
		if verifiedTxID != "" {
			verified[verifiedTxID] = settledFacts{amount: "5", network: "anet:hub", receipt: "cmVjZWlwdA=="}
		}
		var stored []json.RawMessage
		for _, r := range list {
			out := withVerdict(r, verified, "payer-aid")
			if json.Valid(r) && !json.Valid(out) {
				t.Fatalf("withVerdict(%s) = %s, not JSON", r, out)
			}
			stored = append(stored, out)
		}
		if _, err := json.Marshal(stored); err != nil {
			t.Fatalf("the restated list does not encode: %v", err)
		}
		for _, payState := range []string{interactions.PayNone, interactions.PaySubmitted, interactions.PayFailed} {
			own, _ := a2ashape.SplitReceipts(stored, true, payState)
			for _, r := range own {
				var m map[string]any
				if json.Unmarshal(r, &m) != nil {
					continue // the projection drops what does not read
				}
				if m["success"] == false {
					continue
				}
				tx, _ := m["transaction"].(string)
				if _, ok := verified[tx]; !ok || tx == "" {
					t.Fatalf("a settlement this node did not verify is stated as its own (%s): %s", payState, r)
				}
			}
		}
		if len(stored) > 0 {
			bound := boundReceipts(stored, 1)
			nv := 0
			for _, r := range stored {
				if verifiedTx(r) != "" {
					nv++
				}
			}
			kept := 0
			for _, r := range bound {
				if verifiedTx(r) != "" {
					kept++
				}
			}
			if kept != nv {
				t.Fatalf("bounding dropped a verified settlement: %d of %d kept", kept, nv)
			}
		}
	})
}

// FuzzX402Quote: a provider's quote and a local client's choice of option.
// The option matched is one the provider quoted, field for field; putting
// this node's payable options first reorders the quote and changes
// nothing else; and canonical JSON is a fixed point.
func FuzzX402Quote(f *testing.F) {
	opt := `{"scheme":"exact","network":"anet:hubaid","maxAmountRequired":"100","resource":"r","description":"d","mimeType":"application/json","payTo":"p","maxTimeoutSeconds":60,"asset":"credit"}`
	other := `{"scheme":"exact","network":"base","maxAmountRequired":"100","payTo":"0x1","asset":"usdc"}`
	f.Add([]byte(`{"x402Version":1,"accepts":[`+other+`,`+opt+`],"error":""}`), []byte(opt), "anet:hubaid", uint16(0))
	f.Add([]byte(`{"x402Version":1,"accepts":[`+opt+`]}`), []byte(`{"maxAmountRequired":"100","scheme":"exact","network":"anet:hubaid","resource":"r","description":"d","mimeType":"application/json","payTo":"p","maxTimeoutSeconds":60,"asset":"credit"}`), "", uint16(0))
	f.Add([]byte(`{"accepts":[{"a":1.0}],"accepts":[{"a":1}]}`), []byte(`{"a":1} trailing`), "x", uint16(0))
	f.Add([]byte(`{"accepts":[{"scheme":"exact","network":"n","n":1e400}]}`), []byte(`{"n":1e400}`), "n", uint16(0))
	// A quote as module/x402 writes one (paymentRequired): the home
	// ledger's option and a second, clearable one.
	home := payment.CreditNetwork("bafyreihubaid")
	credit := func(network string) payment.PaymentOption {
		return payment.PaymentOption{Scheme: payment.SchemeCredit, Network: network, Amount: payment.Amount(5),
			Asset: payment.AssetCredit, PayTo: "bafyreiprovider", MaxTimeoutSeconds: 900}
	}
	real, _ := json.Marshal(payment.PaymentRequired{X402Version: payment.Version,
		Resource: &payment.Resource{URL: "anet:capability/text.echo", Description: "text.echo"},
		Accepts:  []payment.PaymentOption{credit(payment.CreditNetwork("bafyreiotherhub")), credit(home)}})
	chosen, _ := json.Marshal(credit(home))
	f.Add(real, chosen, home, uint16(0))
	f.Fuzz(func(t *testing.T, required, accept []byte, home string, flip uint16) {
		// A knob the mutator turns cheaply: the case of one letter of the
		// quote. Byte mutations rarely produce "PayTo" from "payTo" at the
		// one position that matters, and a case change adds no coverage
		// for the fuzzer to keep (docs/notes/0033).
		required = flipCase(required, flip)
		if i, ok := offeredOption(required, accept); ok {
			var stored struct {
				Accepts []json.RawMessage `json:"accepts"`
			}
			if json.Unmarshal(required, &stored) != nil || i < 0 || i >= len(stored.Accepts) {
				t.Fatalf("offeredOption matched option %d of a quote without it", i)
			}
			a, aok := canonicalJSON(accept)
			b, bok := canonicalJSON(stored.Accepts[i])
			if !aok || !bok || !bytes.Equal(a, b) {
				t.Fatalf("offeredOption matched %s to %s", accept, stored.Accepts[i])
			}
			// A choice is read as its first JSON value (what follows it is
			// not looked at); the option paid is the stored one, index i.
		}
		// The quote as stored reads the same to the client that shows it
		// (exact member names: a2ashape, a2a-go, any JSON reader) and to
		// this node when it signs a payment for one of its options
		// (payment.PaymentOption, which Go decodes ignoring case).
		meta, _ := json.Marshal(map[string]json.RawMessage{x402a2a.KeyRequired: required})
		if q, ok := quoteOf(meta); ok && json.Valid(required) {
			var shown struct {
				Accepts []map[string]any `json:"accepts"`
			}
			var signed payment.PaymentRequired
			dec := json.NewDecoder(bytes.NewReader(q))
			dec.UseNumber()
			if dec.Decode(&shown) != nil || json.Unmarshal(q, &signed) != nil || len(shown.Accepts) != len(signed.Accepts) {
				t.Fatalf("a stored quote reads differently: %s", q)
			}
			for i, o := range signed.Accepts {
				s := shown.Accepts[i]
				for k, v := range map[string]string{"scheme": o.Scheme, "network": o.Network, "amount": o.Amount,
					"asset": o.Asset, "payTo": o.PayTo} {
					if got, _ := s[k].(string); got != v {
						t.Fatalf("option %d of a stored quote shows %s=%q and is signed as %q: %s", i, k, got, v, q)
					}
				}
			}
		}
		if c, ok := canonicalJSON(accept); ok {
			if c2, ok := canonicalJSON(c); !ok || !bytes.Equal(c, c2) {
				t.Fatalf("canonicalJSON is not a fixed point: %s -> %s", c, c2)
			}
		}
		out := payableFirst(required, home)
		if !bytes.Equal(out, required) {
			var in, got map[string]json.RawMessage
			if json.Unmarshal(required, &in) != nil || json.Unmarshal(out, &got) != nil || len(in) != len(got) {
				t.Fatalf("payableFirst changed the quote's shape:\n%s\n%s", required, out)
			}
			var ia, ga []json.RawMessage
			_ = json.Unmarshal(in["accepts"], &ia)
			_ = json.Unmarshal(got["accepts"], &ga)
			if !sameMultiset(ia, ga) {
				t.Fatalf("payableFirst changed the options:\n%s\n%s", required, out)
			}
			for k, v := range in {
				if k != "accepts" && !bytes.Equal(compact(v), compact(got[k])) {
					t.Fatalf("payableFirst changed %q", k)
				}
			}
		}
		_ = x402a2a.PayBind(home, string(accept))
	})
}

func compact(b []byte) []byte {
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return b
	}
	return buf.Bytes()
}

func sameMultiset(a, b []json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	count := map[string]int{}
	for _, x := range a {
		count[string(compact(x))]++
	}
	for _, x := range b {
		count[string(compact(x))]--
	}
	for _, n := range count {
		if n != 0 {
			return false
		}
	}
	return true
}

// flipCase changes the case of the ASCII letter at i-1 (0: none) of b, in
// a copy.
func flipCase(b []byte, i uint16) []byte {
	if i == 0 || int(i) > len(b) {
		return b
	}
	c := b[i-1]
	if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
		b = append([]byte(nil), b...)
		b[i-1] = c ^ 0x20
	}
	return b
}

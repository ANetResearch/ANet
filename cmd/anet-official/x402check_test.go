package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"
)

func x402Check(t *testing.T, args string) *x402Result {
	t.Helper()
	out, err := handleX402Check(context.Background(), nil, []byte(args))
	if err != nil {
		t.Fatalf("%s: %v", args, err)
	}
	return out.(*x402Result)
}

func issueCodes(res *x402Result) map[string]string {
	out := map[string]string{}
	for _, i := range res.Issues {
		if _, seen := out[i.Code]; !seen || i.Severity == sevError {
			out[i.Code] = i.Severity
		}
	}
	return out
}

const (
	reqV2 = `{"scheme":"exact","network":"eip155:8453","amount":"1000","asset":"0xA0b8","payTo":"0xMerchant","maxTimeoutSeconds":60}`
	reqV1 = `{"scheme":"exact","network":"base","maxAmountRequired":"1000","resource":"https://api.example/x","description":"d","mimeType":"application/json","payTo":"0xM","maxTimeoutSeconds":60,"asset":"0xA"}`
)

func TestX402Metadata(t *testing.T) {
	cases := []struct {
		name  string
		meta  string
		valid bool
		codes map[string]string
	}{
		{"required, v2", `{"x402.payment.status":"payment-required","x402.payment.required":{"x402Version":2,"resource":{"url":"anet:capability/x"},"accepts":[` + reqV2 + `]}}`, true, nil},
		{"required, v1 as in the spec examples", `{"x402.payment.status":"payment-required","x402.payment.required":{"x402Version":1,"accepts":[` + reqV1 + `]}}`, true, nil},
		{"required without resource (anet quote)", `{"x402.payment.status":"payment-required","x402.payment.required":{"x402Version":2,"accepts":[` + reqV2 + `]}}`, true,
			map[string]string{"resource_missing": sevWarning}},
		{"embedded flow", `{"x402.payment.status":"payment-required"}`, true, map[string]string{"embedded_flow": sevInfo}},
		{"status missing", `{"x402.payment.required":{"x402Version":2,"accepts":[]}}`, false, map[string]string{"status_missing": sevError}},
		{"status unknown", `{"x402.payment.status":"paid"}`, false, map[string]string{"status_value": sevError}},
		{"no x402 at all", `{"other":1}`, false, map[string]string{"no_x402": sevError}},
		{"empty accepts", `{"x402.payment.status":"payment-required","x402.payment.required":{"x402Version":2,"resource":{"url":"u"},"accepts":[]}}`, false,
			map[string]string{"empty": sevError}},
		{"v1 field in v2", `{"x402.payment.status":"payment-required","x402.payment.required":{"x402Version":2,"resource":{"url":"u"},"accepts":[` +
			`{"scheme":"exact","network":"eip155:8453","maxAmountRequired":"1","asset":"a","payTo":"p","maxTimeoutSeconds":1}]}}`, false,
			map[string]string{"v1_field": sevError, "required": sevError}},
		{"bad amount", `{"x402.payment.status":"payment-required","x402.payment.required":{"x402Version":2,"resource":{"url":"u"},"accepts":[` +
			`{"scheme":"exact","network":"eip155:8453","amount":"1.5","asset":"a","payTo":"p","maxTimeoutSeconds":1}]}}`, false,
			map[string]string{"amount": sevError}},
		{"anet-credit network", `{"x402.payment.status":"payment-required","x402.payment.required":{"x402Version":2,"resource":{"url":"u"},"accepts":[` +
			`{"scheme":"anet-credit","network":"hub:bafyreicaw2uaernqv2rnhunxgkjwgsxrundhy4o2f2v423i27bk6yjh7ge","amount":"2","asset":"credit","payTo":"bafyrei","maxTimeoutSeconds":300}]}}`, true,
			map[string]string{"network_caip2": sevWarning}},
		{"submitted, v2", `{"x402.payment.status":"payment-submitted","x402.payment.payload":{"x402Version":2,"accepted":` + reqV2 + `,"payload":{"signature":"0x"}}}`, true, nil},
		{"submitted without accepted", `{"x402.payment.status":"payment-submitted","x402.payment.payload":{"x402Version":2,"payload":{}}}`, false,
			map[string]string{"required": sevError}},
		{"anet local signer", `{"x402.payment.status":"payment-submitted","anet.payment.accept":` + reqV2 + `}`, true, map[string]string{"anet_signer": sevInfo}},
		{"completed", `{"x402.payment.status":"payment-completed","x402.payment.receipts":[{"success":true,"transaction":"0xabc","network":"base","payer":"0xP"}]}`, true, nil},
		{"completed without receipts", `{"x402.payment.status":"payment-completed"}`, false, map[string]string{"receipts_missing": sevError}},
		{"completed with failed receipt", `{"x402.payment.status":"payment-completed","x402.payment.receipts":[{"success":false,"transaction":"","network":"base","errorReason":"x"}]}`, false,
			map[string]string{"receipt_not_success": sevError}},
		{"successful receipt without transaction", `{"x402.payment.status":"payment-completed","x402.payment.receipts":[{"success":true,"transaction":"","network":"base"}]}`, false,
			map[string]string{"empty": sevError}},
		{"failed", `{"x402.payment.status":"payment-failed","x402.payment.error":"EXPIRED_PAYMENT","x402.payment.receipts":[{"success":false,"errorReason":"late","network":"base","transaction":""}]}`, true, nil},
		{"failed without code", `{"x402.payment.status":"payment-failed"}`, false, map[string]string{"error_missing": sevError, "receipts_missing": sevWarning}},
		{"failed with an uncommon code", `{"x402.payment.status":"payment-failed","x402.payment.error":"OOPS","x402.payment.receipts":[{"success":false,"errorReason":"x","network":"n","transaction":""}]}`, true,
			map[string]string{"error_code": sevWarning}},
		{"misplaced keys", `{"x402.payment.status":"payment-verified","x402.payment.required":{"x402Version":2,"accepts":[]},"x402.payment.error":"X"}`, true,
			map[string]string{"misplaced_key": sevWarning}},
		{"unknown x402 key", `{"x402.payment.status":"payment-rejected","x402.payment.reason":"too much"}`, true, map[string]string{"unknown_key": sevWarning}},
	}
	for _, c := range cases {
		res := x402Check(t, `{"metadata":`+c.meta+`}`)
		if res.Valid != c.valid {
			t.Errorf("%s: valid=%v, want %v; %+v", c.name, res.Valid, c.valid, res.Issues)
		}
		got := issueCodes(res)
		for code, sev := range c.codes {
			if got[code] != sev {
				t.Errorf("%s: %s reported as %q, want %s; %+v", c.name, code, got[code], sev, res.Issues)
			}
		}
	}
}

func TestX402Sequence(t *testing.T) {
	req := `{"x402.payment.status":"payment-required","x402.payment.required":{"x402Version":2,"resource":{"url":"u"},"accepts":[` + reqV2 + `]}}`
	sub := `{"x402.payment.status":"payment-submitted","x402.payment.payload":{"x402Version":2,"accepted":` + reqV2 + `,"payload":{}}}`
	ver := `{"x402.payment.status":"payment-verified"}`
	r1 := `{"success":true,"transaction":"t1","network":"eip155:8453"}`
	done := `{"x402.payment.status":"payment-completed","x402.payment.receipts":[` + r1 + `]}`
	fail := `{"x402.payment.status":"payment-failed","x402.payment.error":"INVALID_AMOUNT","x402.payment.receipts":[{"success":false,"errorReason":"x","network":"n","transaction":""}]}`
	rej := `{"x402.payment.status":"payment-rejected"}`
	cases := []struct {
		name  string
		seq   []string
		valid bool
		codes map[string]string
	}{
		{"full flow", []string{req, sub, ver, done}, true, nil},
		{"settled without verified", []string{req, sub, done}, true, map[string]string{"transition": sevWarning}},
		{"rejected", []string{req, rej}, true, nil},
		{"fail then ask again", []string{req, sub, ver, fail, req}, true, map[string]string{"transition": sevWarning}},
		{"skips payment-required", []string{sub, ver, done}, false, map[string]string{"transition": sevError}},
		{"after completion", []string{req, sub, ver, done, sub}, false, map[string]string{"after_terminal": sevError}},
		{"verified before submitted", []string{req, ver}, false, map[string]string{"transition": sevError}},
		{"receipts history shrinks", []string{req, sub, ver, `{"x402.payment.status":"payment-failed","x402.payment.error":"SETTLEMENT_FAILED","x402.payment.receipts":[` + r1 + `,{"success":false,"errorReason":"x","network":"n","transaction":""}]}`,
			req, sub, ver, `{"x402.payment.status":"payment-completed","x402.payment.receipts":[` + r1 + `]}`}, true, map[string]string{"receipts_history": sevWarning}},
	}
	for _, c := range cases {
		res := x402Check(t, `{"sequence":[`+strings.Join(c.seq, ",")+`]}`)
		if res.Valid != c.valid {
			t.Errorf("%s: valid=%v, want %v; %+v", c.name, res.Valid, c.valid, res.Issues)
		}
		got := issueCodes(res)
		for code, sev := range c.codes {
			if got[code] != sev {
				t.Errorf("%s: %s reported as %q, want %s; %+v", c.name, code, got[code], sev, res.Issues)
			}
		}
		if len(res.Statuses) != len(c.seq) {
			t.Errorf("%s: statuses %v", c.name, res.Statuses)
		}
	}
}

func TestX402Objects(t *testing.T) {
	cases := []struct {
		name, args string
		kind       string
		valid      bool
	}{
		{"guess payment_required", `{"object":{"x402Version":2,"resource":{"url":"u"},"accepts":[` + reqV2 + `]}}`, "payment_required", true},
		{"guess payload", `{"object":{"x402Version":2,"accepted":` + reqV2 + `,"payload":{}}}`, "payment_payload", true},
		{"guess settlement", `{"object":{"success":true,"transaction":"t","network":"n"}}`, "settlement_response", true},
		{"guess requirements", `{"object":` + reqV2 + `}`, "payment_requirements", true},
		{"named kind", `{"object":{"success":"yes","network":"n"},"kind":"settlement_response"}`, "settlement_response", false},
		{"v1 payload", `{"object":{"x402Version":1,"scheme":"exact","network":"base","payload":{"signature":"0x"}},"kind":"payment_payload"}`, "payment_payload", true},
		{"bad version", `{"object":{"x402Version":3,"accepts":[]},"kind":"payment_required"}`, "payment_required", false},
	}
	for _, c := range cases {
		res := x402Check(t, c.args)
		if res.Kind != c.kind || res.Valid != c.valid {
			t.Errorf("%s: kind %s valid %v, want %s %v; %+v", c.name, res.Kind, res.Valid, c.kind, c.valid, res.Issues)
		}
	}
	for _, bad := range []string{`{}`, `{"metadata":{},"object":{}}`, `{"object":{"x":1}}`, `{"object":{},"kind":"nope"}`, `{"sequence":[]}`, `{"metadata":[]}`} {
		if _, err := handleX402Check(context.Background(), nil, []byte(bad)); err == nil {
			t.Errorf("%s: must be refused", bad)
		}
	}
}

// An anet-credit payload carries a signed CBOR authorization; the checker
// decodes it and compares it with the accepted option. So does a
// settlement receipt the hub signed.
func TestX402AnetCredit(t *testing.T) {
	payer, _ := identity.Incept()
	payee, _ := identity.Incept()
	network := payment.CreditNetwork("bafyhub")
	auth := &payment.Authorization{Payer: payer.AID(), PayTo: payee.AID(), Amount: 2, Network: network,
		Nonce: "n1", IssuedAt: 1000, NotAfter: 301000, InteractionID: "bind"}
	if err := auth.Sign(payer); err != nil {
		t.Fatal(err)
	}
	raw, err := auth.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	accepted := func(payTo, amount string) string {
		b, _ := json.Marshal(map[string]any{"scheme": payment.SchemeCredit, "network": network, "amount": amount,
			"asset": payment.AssetCredit, "payTo": payTo, "maxTimeoutSeconds": 300})
		return string(b)
	}
	payload := func(acc string) string {
		return `{"object":{"x402Version":2,"accepted":` + acc + `,"payload":{"authorization":"` +
			base64.StdEncoding.EncodeToString(raw) + `"}},"kind":"payment_payload"}`
	}
	if res := x402Check(t, payload(accepted(payee.AID(), "2"))); !res.Valid {
		t.Errorf("matching authorization: %+v", res.Issues)
	}
	res := x402Check(t, payload(accepted("someone-else", "2")))
	if issueCodes(res)["payee_mismatch"] != sevError {
		t.Errorf("payee mismatch not found: %+v", res.Issues)
	}
	res = x402Check(t, payload(accepted(payee.AID(), "5")))
	if issueCodes(res)["invalid_amount"] != sevError {
		t.Errorf("short authorization not found: %+v", res.Issues)
	}

	rc := &payment.Receipt{AuthID: "auth-1", Payer: payer.AID(), PayTo: payee.AID(), Amount: 2, Network: network, SettleAt: 2000}
	rb, err := rc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	settle := func(amount string) string {
		return `{"object":{"success":true,"transaction":"tx","network":"` + network + `","amount":"` + amount +
			`","extensions":{"anet.settlement.receipt":"` + base64.StdEncoding.EncodeToString(rb) + `"}}}`
	}
	if res := x402Check(t, settle("2")); !res.Valid || issueCodes(res)["anet_receipt"] != sevInfo {
		t.Errorf("receipt: %+v", res.Issues)
	}
	if res := x402Check(t, settle("3")); res.Valid {
		t.Errorf("a receipt for another amount must be reported: %+v", res.Issues)
	}
}

// maxTimeoutSeconds is read from the literal: a positive integer however
// written, and a huge exponent costs nothing (big.Rat parsed each
// "1e999999" in tens of milliseconds; 600 of them in a 64 KiB argument held
// a core for half a minute).
func TestX402TimeoutIsReadFromTheLiteral(t *testing.T) {
	for lit, ok := range map[string]bool{
		"60": true, "6e1": true, "60.0": true, "1e999999": true,
		"0": false, "-1": false, "1.5": false, "1e-999999": false, `"60"`: false,
	} {
		req := strings.Replace(reqV2, `"maxTimeoutSeconds":60`, `"maxTimeoutSeconds":`+lit, 1)
		res := x402Check(t, `{"object":`+req+`,"kind":"payment_requirements"}`)
		if got := issueCodes(res)["type"] != sevError; got != ok {
			t.Errorf("maxTimeoutSeconds %s: accepted %v, want %v (%+v)", lit, got, ok, res.Issues)
		}
	}
	accepts := strings.Repeat(`{"maxTimeoutSeconds":1e999999},`, 3000)
	began := time.Now()
	x402Check(t, `{"object":{"x402Version":2,"accepts":[`+accepts+reqV2+`]}}`)
	if d := time.Since(began); d > 5*time.Second {
		t.Errorf("3000 huge timeouts took %v", d)
	}
}

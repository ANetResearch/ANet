//go:build !no_x402

package x402

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// The errorReason → x402.payment.error table (A2A-DESIGN §8.5), pinned as
// literal strings on both sides: the hub's errorReason values are a wire
// contract with another repository, and the codes are a2a-x402's. A
// rename of either constant must fail here rather than silently map a
// reason to SETTLEMENT_FAILED.
func TestTheErrorReasonTableIsPinned(t *testing.T) {
	cases := map[string]string{
		"insufficient_funds":           "INSUFFICIENT_FUNDS",
		"invalid_signature":            "INVALID_SIGNATURE",
		"expired_payment":              "EXPIRED_PAYMENT",
		"expired":                      "EXPIRED_PAYMENT",
		"quote_expired":                "EXPIRED_PAYMENT",
		"duplicate_nonce":              "DUPLICATE_NONCE",
		"duplicate_binding":            "DUPLICATE_NONCE",
		"network_mismatch":             "NETWORK_MISMATCH",
		"invalid_amount":               "INVALID_AMOUNT",
		"payee_mismatch":               "SETTLEMENT_FAILED",
		"unsupported_scheme":           "SETTLEMENT_FAILED",
		"unknown_payer":                "SETTLEMENT_FAILED",
		"settlement_failed":            "SETTLEMENT_FAILED",
		"malformed_payment":            "SETTLEMENT_FAILED",
		"invalid_payment_requirements": "SETTLEMENT_FAILED",
		"provider_busy":                "SETTLEMENT_FAILED",
		"binding_mismatch":             "SETTLEMENT_FAILED",
		"no_pending_quote":             "SETTLEMENT_FAILED",
		"client_payload_unsupported":   "SETTLEMENT_FAILED",
		"option_not_offered":           "SETTLEMENT_FAILED",
		"something_nobody_listed":      "SETTLEMENT_FAILED",
	}
	for reason, want := range cases {
		got, final := ErrorCode(reason)
		if got != want || !final {
			t.Errorf("ErrorCode(%q) = %q, final %v; want %q, final", reason, got, final, want)
		}
	}
	// settlement_pending is not an outcome: no code, not final.
	if got, final := ErrorCode("settlement_pending"); got != "" || final {
		t.Errorf("settlement_pending maps to %q, final %v; want no code, not final", got, final)
	}
	// The hub side of the table: the ANetCore constants the hub emits are
	// the strings above.
	for c, want := range map[string]string{
		payment.ReasonInsufficientFunds: "insufficient_funds", payment.ReasonInvalidSignature: "invalid_signature",
		payment.ReasonExpiredPayment: "expired_payment", payment.ReasonDuplicateNonce: "duplicate_nonce",
		payment.ReasonDuplicateBinding: "duplicate_binding", payment.ReasonNetworkMismatch: "network_mismatch",
		payment.ReasonInvalidAmount: "invalid_amount", payment.ReasonPayeeMismatch: "payee_mismatch",
		payment.ReasonUnsupportedScheme: "unsupported_scheme", payment.ReasonUnknownPayer: "unknown_payer",
		payment.ReasonSettlementFailed: "settlement_failed", payment.ReasonSettlementPending: "settlement_pending",
		payment.ReasonMalformed: "malformed_payment", payment.ReasonInvalidRequirements: "invalid_payment_requirements",
		payment.ReasonExpired: "expired",
	} {
		if c != want {
			t.Errorf("ANetCore reason %q, want %q", c, want)
		}
	}
	// Every errorReason ANetCore defines has a row (a new constant must be
	// added to the table, and to this list).
	for _, r := range []string{payment.ReasonInsufficientFunds, payment.ReasonInvalidSignature,
		payment.ReasonExpired, payment.ReasonExpiredPayment, payment.ReasonUnknownPayer,
		payment.ReasonNetworkMismatch, payment.ReasonMalformed, payment.ReasonUnsupportedScheme,
		payment.ReasonInvalidAmount, payment.ReasonPayeeMismatch, payment.ReasonDuplicateNonce,
		payment.ReasonDuplicateBinding, payment.ReasonSettlementFailed, payment.ReasonInvalidRequirements} {
		if _, ok := errorCodes[r]; !ok {
			t.Errorf("errorReason %q has no row in the table", r)
		}
	}
	if ExtensionURI != "https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2" {
		t.Errorf("extension URI = %q", ExtensionURI)
	}
}

// signAuth builds a payload signed by payer with the given terms.
func signAuth(t *testing.T, payer *identity.Controller, a payment.Authorization, accepted payment.PaymentOption) []byte {
	t.Helper()
	a.Payer = payer.AID()
	if a.Nonce == "" {
		a.Nonce = hex.EncodeToString([]byte("0123456789ab"))
	}
	pre, err := a.CanonicalPreimage()
	if err != nil {
		t.Fatal(err)
	}
	sig, seq := payer.Sign(pre)
	a.Envelope = &aobj.Envelope{SignerAID: payer.AID(), KeyStateSeq: seq, Alg: aobj.AlgEdDSA, Sig: sig}
	raw, err := a.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(payment.PaymentPayload{X402Version: 2, Accepted: accepted,
		Payload: map[string]any{"authorization": base64.StdEncoding.EncodeToString(raw)}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The merchant's check (A2A-DESIGN §8.4): every term of the authorization
// against the stored quote, each failure with its own reason and code, and
// the correct payment passing with the quoted option as requirements.
func TestTheMerchantCheckRefusesEachWrongTerm(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	payer, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	quote := m.Quote("work.do", 120)
	opt := quote.Accepts[0]
	now := time.Now().UnixMilli()
	good := payment.Authorization{PayTo: h.AID(), Amount: 120, Network: opt.Network,
		IssuedAt: now, NotAfter: now + 300_000, InteractionID: "bind-1"}
	terms := module.PaymentTerms{Quoted: quote, Bind: "bind-1", Payer: payer.AID(),
		QuoteExpiresAt: now + 3600_000, Now: now}

	chk := m.CheckPayment(signAuth(t, payer, good, opt), terms)
	if chk.Reason != "" {
		t.Fatalf("the correct payment was refused: %+v", chk)
	}
	if !chk.Matched || chk.Requirements.PayTo != h.AID() || chk.Requirements.Amount != "120" ||
		chk.Requirements.Network != opt.Network || chk.Requirements.Extra != nil || chk.AuthID == "" {
		t.Errorf("requirements = %+v", chk.Requirements)
	}

	mutate := func(f func(a *payment.Authorization, o *payment.PaymentOption, tm *module.PaymentTerms)) module.PaymentCheck {
		a, o, tm := good, opt, terms
		f(&a, &o, &tm)
		return m.CheckPayment(signAuth(t, payer, a, o), tm)
	}
	for _, tc := range []struct {
		name, reason, code string
		f                  func(a *payment.Authorization, o *payment.PaymentOption, tm *module.PaymentTerms)
	}{
		{"underpaid", "invalid_amount", "INVALID_AMOUNT", func(a *payment.Authorization, o *payment.PaymentOption, _ *module.PaymentTerms) {
			a.Amount, o.Amount = 119, "119"
		}},
		{"accepted amount differs from the signed one", "invalid_amount", "INVALID_AMOUNT", func(a *payment.Authorization, o *payment.PaymentOption, _ *module.PaymentTerms) {
			o.Amount = "500"
		}},
		{"wrong payee", "payee_mismatch", "SETTLEMENT_FAILED", func(a *payment.Authorization, o *payment.PaymentOption, _ *module.PaymentTerms) {
			a.PayTo, o.PayTo = "did:anet:someone-else", "did:anet:someone-else"
		}},
		{"binding of other work", "binding_mismatch", "SETTLEMENT_FAILED", func(a *payment.Authorization, _ *payment.PaymentOption, _ *module.PaymentTerms) {
			a.InteractionID = "bind-2"
		}},
		{"no binding at all", "binding_mismatch", "SETTLEMENT_FAILED", func(a *payment.Authorization, _ *payment.PaymentOption, _ *module.PaymentTerms) {
			a.InteractionID = ""
		}},
		{"authorization expired", "expired_payment", "EXPIRED_PAYMENT", func(a *payment.Authorization, _ *payment.PaymentOption, _ *module.PaymentTerms) {
			a.IssuedAt, a.NotAfter = now-3600_000, now-3000_000
		}},
		{"quote expired", "quote_expired", "EXPIRED_PAYMENT", func(_ *payment.Authorization, _ *payment.PaymentOption, tm *module.PaymentTerms) {
			tm.QuoteExpiresAt = now - 1
		}},
		{"network not quoted", "network_mismatch", "NETWORK_MISMATCH", func(a *payment.Authorization, o *payment.PaymentOption, _ *module.PaymentTerms) {
			a.Network, o.Network = "hub:did:anet:other-hub", "hub:did:anet:other-hub"
		}},
		{"payload claims another network than it signs", "network_mismatch", "NETWORK_MISMATCH", func(_ *payment.Authorization, o *payment.PaymentOption, _ *module.PaymentTerms) {
			o.Network = "hub:did:anet:other-hub"
		}},
		{"scheme not quoted", "network_mismatch", "NETWORK_MISMATCH", func(_ *payment.Authorization, o *payment.PaymentOption, _ *module.PaymentTerms) {
			o.Scheme = "exact"
		}},
		{"nothing quoted", "no_pending_quote", "SETTLEMENT_FAILED", func(_ *payment.Authorization, _ *payment.PaymentOption, tm *module.PaymentTerms) {
			tm.Quoted = nil
		}},
		{"signed by someone other than the requester", "payer_mismatch", "SETTLEMENT_FAILED", func(_ *payment.Authorization, _ *payment.PaymentOption, tm *module.PaymentTerms) {
			tm.Payer = "did:anet:requester"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mutate(tc.f)
			if got.Reason != tc.reason || got.Code != tc.code {
				t.Errorf("reason %q code %q, want %q %q (%s)", got.Reason, got.Code, tc.reason, tc.code, got.Detail)
			}
		})
	}
	if got := m.CheckPayment([]byte("not json"), terms); got.Reason != payment.ReasonMalformed {
		t.Errorf("malformed payload: %+v", got)
	}
}

// A payment the spending policy refuses is not signed and not recorded
// (A2A-DESIGN §8.6 [C27]).
func TestARefusedSpendSignsAndRecordsNothing(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	h.spendRefusal = errors.New("over the limit")
	opt := payment.PaymentOption{Scheme: payment.SchemeCredit, Network: payment.CreditNetwork(h.hub.AID()),
		Amount: "50", Asset: payment.AssetCredit, PayTo: "did:anet:provider"}
	raw, err := m.Authorize(opt, "ix-1", "bind-1", module.PurposeTaskAuto)
	if err == nil || raw != nil {
		t.Fatalf("a refused spend was signed: %v %s", err, raw)
	}
	if !errors.Is(err, h.spendRefusal) {
		t.Errorf("the refusal is not carried to the caller: %v", err)
	}
	if evs := h.eventsOf(EvPaymentAuthorized); len(evs) != 0 {
		t.Errorf("a refused spend was recorded as authorized: %+v", evs)
	}
	// Redeem goes through the same gate, as a redeem.
	h.spends = nil
	if _, err := m.Redeem(context.Background(), 5, "ref"); err == nil {
		t.Error("a refused redemption went ahead")
	}
	if len(h.spends) != 1 || h.spends[0].purpose != module.PurposeRedeem || h.spends[0].payTo != h.hub.AID() {
		t.Errorf("redeem asked the policy %+v", h.spends)
	}
}

// What the facilitator is told (A2A-DESIGN §8.5, SI-1): x402 v2's
// {x402Version, paymentPayload, paymentRequirements}, with no description,
// extra or resource anywhere, and the three outcomes kept apart.
func TestSettleSendsRequirementsAndNothingAboutTheWork(t *testing.T) {
	var bodies [][]byte
	answer := payment.SettlementResponse{Success: true, Transaction: "tx-1", Network: "hub:x",
		Extensions: map[string]any{payment.ExtReplayed: true}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		_ = json.NewEncoder(w).Encode(answer)
	}))
	defer srv.Close()
	h := newHost(t)
	h.url = srv.URL
	m := newModule(t, h)
	opt := payment.PaymentOption{Scheme: payment.SchemeCredit, Network: payment.CreditNetwork(h.hub.AID()),
		Amount: "7", Asset: payment.AssetCredit, PayTo: "did:anet:provider",
		Extra: map[string]any{"capability": "work.secret"}}
	raw, err := m.Authorize(opt, "ix-1", "bind-1", module.PurposeTaskAgent)
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.Settle(context.Background(), raw, opt)
	if err != nil || st.Failed != "" || !st.Replayed || st.Response == nil || st.Response.Transaction != "tx-1" {
		t.Fatalf("settlement = %+v, %v", st, err)
	}
	if len(bodies) != 1 {
		t.Fatalf("%d settle calls", len(bodies))
	}
	var body map[string]map[string]any
	var top map[string]any
	if err := json.Unmarshal(bodies[0], &top); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for k := range top {
		keys = append(keys, k)
	}
	if len(keys) != 3 || top["x402Version"] != float64(2) || top["paymentPayload"] == nil || top["paymentRequirements"] == nil {
		t.Fatalf("body keys = %v", keys)
	}
	_ = json.Unmarshal(bodies[0], &body)
	req := body["paymentRequirements"]
	if req["payTo"] != "did:anet:provider" || req["amount"] != "7" || req["scheme"] != payment.SchemeCredit {
		t.Errorf("requirements = %v", req)
	}
	for _, forbidden := range []string{"extra", "description", "resource", "work.secret"} {
		if strings.Contains(string(bodies[0]), forbidden) {
			t.Errorf("the settle body carries %q: %s", forbidden, bodies[0])
		}
	}

	// A refusal carries its reason and code; settlement_pending is not an
	// outcome.
	answer = payment.SettlementResponse{Success: false, ErrorReason: payment.ReasonInsufficientFunds}
	st, err = m.Settle(context.Background(), raw, opt)
	if err != nil || st.Failed != payment.ReasonInsufficientFunds || st.Code != x402a2a.CodeInsufficientFunds || st.Pending {
		t.Errorf("refusal = %+v, %v", st, err)
	}
	answer = payment.SettlementResponse{Success: false, ErrorReason: payment.ReasonSettlementPending}
	st, err = m.Settle(context.Background(), raw, opt)
	if err != nil || !st.Pending || st.Code != "" {
		t.Errorf("pending = %+v, %v", st, err)
	}
}

// The account reads are signed as this node (relayauth v2): the hub serves
// balance and ledger to the account holder only (A2A-DESIGN §3.7).
func TestAccountReadsAreSignedByTheAccountHolder(t *testing.T) {
	h := newHost(t)
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts, _ := strconv.ParseUint(r.Header.Get(relayauth.HeaderTS), 10, 64)
		seq, _ := strconv.ParseUint(r.Header.Get(relayauth.HeaderSeq), 10, 64)
		sig, err := relayauth.DecodeSig(r.Header.Get(relayauth.HeaderSig))
		action := relayauth.ActionBalance
		if strings.Contains(r.URL.Path, "/ledger") {
			action = relayauth.ActionLedger
		}
		pre := relayauth.PreimageV2(action, h.AID(), h.hub.AID(), ts, r.Method, r.URL.RequestURI(), nil)
		if err != nil || r.Header.Get(relayauth.HeaderAID) != h.AID() ||
			identity.VerifyObject(h.self.KEL(), h.AID(), seq, ts, pre, sig) != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"auth"}`))
			return
		}
		seen = append(seen, action)
		if action == relayauth.ActionBalance {
			_, _ = w.Write([]byte(`{"aid":"x","credits":42}`))
		} else {
			_, _ = w.Write([]byte(`{"entries":[]}`))
		}
	}))
	defer srv.Close()
	h.url = srv.URL
	m := newModule(t, h)
	out, err := m.Balance(context.Background())
	if err != nil || out["balance"] != int64(42) {
		t.Fatalf("balance = %v, %v", out, err)
	}
	if strings.Join(seen, ",") != "balance,ledger" {
		t.Errorf("signed reads: %v", seen)
	}
}

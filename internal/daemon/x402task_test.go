//go:build !no_x402

package daemon

// a2a-x402 on the task (A2A-DESIGN §8): the quote, the payment and the
// settlement on one interaction, the spending policy, the merchant check,
// settlement whose outcome is not known, and cancels racing payments.
// Against the real payment module and the fake hub, whose facilitator
// takes the real request shape (paymentRequirements required).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

func u64(n uint64) *uint64 { return &n }

// payPolicy sets d's payments block and puts payees on its payee list.
func payPolicy(t *testing.T, d *Daemon, pc PaymentsConfig, payees ...string) {
	t.Helper()
	d.mu.Lock()
	d.cfg.Payments = &pc
	d.mu.Unlock()
	if len(payees) > 0 {
		appendPeerFile(t, d, d.config().Payments.limits().PayeesFile, payees...)
	}
}

// meteredWork is a priced capability that counts its runs; with gate set
// it runs long (off the poll loop) and waits for the gate.
type meteredWork struct {
	price   uint64
	invoked atomic.Int32
	gate    chan struct{}
	started chan struct{}
	// status, when set, is the effect every run reports.
	status effect.Status
}

func (p *meteredWork) ID() string { return "metered" }
func (p *meteredWork) Capabilities(context.Context) ([]string, error) {
	return []string{"work.do"}, nil
}
func (p *meteredWork) Describe(context.Context) (string, error) { return "", nil }
func (p *meteredWork) Health(context.Context) error             { return nil }
func (p *meteredWork) Price(cap string) (uint64, bool) {
	return p.price, cap == "work.do"
}
func (p *meteredWork) InvokeTimeout(string) (time.Duration, bool) {
	if p.gate == nil {
		return 0, false
	}
	return 10 * time.Minute, true
}
func (p *meteredWork) Invoke(ctx context.Context, _ provider.Call) (effect.Effect, error) {
	p.invoked.Add(1)
	if p.gate != nil {
		close(p.started)
		select {
		case <-p.gate:
		case <-ctx.Done():
		}
	}
	st := effect.OK
	if p.status != "" {
		st = p.status
	}
	return effect.Effect{Status: st, Record: &tsir.EffectRecord{Metrics: map[string]float64{"done": 1}},
		Evidence: &effect.Evidence{Protocol: "test", Requested: "work.do", NativeAck: true, VerifyTrust: 1}}, nil
}

// paidPair is a requester with 500 credits and a provider selling work.do.
func paidPair(t *testing.T, work *meteredWork) (string, *Daemon, *Daemon) {
	t.Helper()
	srv, req, prov := registeredPair(t)
	if err := prov.Providers().Register(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	grantOn(srv.URL, req.AID(), 500)
	return srv.URL, req, prov
}

func poll(t *testing.T, ds ...*Daemon) {
	t.Helper()
	for _, d := range ds {
		if err := d.pollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func getIX(t *testing.T, d *Daemon, id string) *interactions.Interaction {
	t.Helper()
	ix, err := d.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// shortSettleRetry makes the settlement retry loop fast for one test.
func shortSettleRetry(t *testing.T, d time.Duration) {
	t.Helper()
	old := settleRetryBase.set(d)
	t.Cleanup(func() { settleRetryBase.set(old) })
}

// The whole loop on one task: quote, automatic payment within auto_max,
// settlement, execution, result with receipts; one debit; what the hub was
// told carries nothing about the work (SI-1).
func TestAQuotedTaskIsPaidAndCompletedOnTheSameTask(t *testing.T) {
	work := &meteredWork{price: 120}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 200, AgentDailyMax: 500, DailyMax: u64(1000)}, prov.AID())
	ctx := context.Background()

	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	pix := getIX(t, prov, id)
	if pix.State != interactions.StateInputRequired || pix.PayState != interactions.PayRequired {
		t.Fatalf("provider after quoting: %s / %q", pix.State, pix.PayState)
	}
	if left := time.Until(time.UnixMilli(pix.QuoteExpiresAt)); left < 23*time.Hour || left > 25*time.Hour {
		t.Errorf("the quote stands for %s, want 24h", left)
	}
	poll(t, req) // the quote arrives and is paid within auto_max
	rix := getIX(t, req, id)
	if rix.PayState != interactions.PaySubmitted || rix.State != interactions.StateWorking || len(rix.PayAuthIDs) != 1 {
		t.Fatalf("requester after paying: %s / %q / %v", rix.State, rix.PayState, rix.PayAuthIDs)
	}
	_, auth, authID, err := payloadAuth(rix.PayPayload)
	if err != nil || auth.InteractionID != x402a2a.PayBind(id, rix.TaskNonce) || authID != rix.PayAuthIDs[0] {
		t.Fatalf("the authorization is not bound to the task: %v %+v", err, auth)
	}
	ev := lastLedgerPayload(t, req, EvPaymentAuthorized)
	if ev["interaction_id"] != id || ev["purpose"] != module.PurposeTaskAuto || ev["pay_bind"] != auth.InteractionID {
		t.Errorf("anet.payment.authorized = %v", ev)
	}
	poll(t, prov) // payment → check → settle → verified → execute → result
	if n := work.invoked.Load(); n != 1 {
		t.Fatalf("the paid work ran %d times", n)
	}
	poll(t, req, req)
	rix = getIX(t, req, id)
	if rix.State != interactions.StateCompleted || rix.PayState != interactions.PayCompleted {
		t.Fatalf("requester at the end: %s / %q", rix.State, rix.PayState)
	}
	if got := balanceOf(hub, req.AID()); got != 380 {
		t.Errorf("payer balance %d, want 380", got)
	}
	if n := debitsOn(hub, req.AID()); n != 1 {
		t.Errorf("%d debits, want 1", n)
	}
	var receipts []payment.SettlementResponse
	if err := json.Unmarshal(rix.PayReceipts, &receipts); err != nil || len(receipts) != 1 || !receipts[0].Success {
		t.Fatalf("stored receipts: %s (%v)", rix.PayReceipts, err)
	}
	settled := lastLedgerPayload(t, req, EvPaymentSettled)
	if settled["interaction_id"] != id || settled["verified"] != true || settled["auth_id"] != authID {
		t.Errorf("the payer's settlement evidence: %v", settled)
	}
	if got := lastLedgerPayload(t, prov, EvPaymentQuoted)["interaction_id"]; got != id {
		t.Errorf("anet.payment.quoted on the provider: %v", got)
	}
	// The provider's side: the payment-verified status went before the
	// result, and the result carries payment-completed with the receipts.
	var statuses []string
	msgs, _ := req.ix.Messages(id)
	for _, m := range msgs {
		if s, _ := decodeMeta([]byte(m.Metadata))[x402a2a.KeyStatus].(string); s != "" {
			statuses = append(statuses, m.Kind+":"+s)
		}
	}
	if strings.Join(statuses, ",") != "status:payment-required,payment:payment-submitted,status:payment-verified" {
		t.Errorf("payment messages on the requester: %v", statuses)
	}
	var res capabilityResult
	_ = json.Unmarshal(rix.Result, &res)
	if res.Status != string(effect.OK) || res.Paid == nil || res.Paid.Receipt == "" {
		t.Errorf("the deliverable: %+v", res)
	}
	// SI-1: the settle request names payer, payee, amount, ledger and
	// binding, and nothing about the work.
	bodies := settleBodiesOn(hub)
	if len(bodies) != 1 {
		t.Fatalf("%d settle calls", len(bodies))
	}
	var fr payment.FacilitatorRequest
	if err := json.Unmarshal(bodies[0], &fr); err != nil || fr.PaymentRequirements == nil {
		t.Fatalf("settle body: %s", bodies[0])
	}
	if r := fr.PaymentRequirements; r.PayTo != prov.AID() || r.Amount != "120" || r.Extra != nil {
		t.Errorf("requirements = %+v", r)
	}
	for _, leak := range []string{"work.do", "description", "resource", "extra", id} {
		if strings.Contains(string(bodies[0]), leak) {
			t.Errorf("the hub was told %q: %s", leak, bodies[0])
		}
	}
}

// Above auto_max the task waits for a person or an agent (A2A-DESIGN
// §8.6): nothing is signed, the task reads needs_operator_approval, and
// /tasks/pay (the agent tier) applies agent_max, the payee list and the
// §8.7 rules on the chosen option.
func TestAQuoteAboveTheAutoTierWaitsForADecision(t *testing.T) {
	work := &meteredWork{price: 5}
	_, req, prov := paidPair(t, work)
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	rix := getIX(t, req, id)
	if rix.State != interactions.StateInputRequired || rix.PayState != interactions.PayRequired ||
		PaymentReason(rix) != x402a2a.ReasonNeedsOperatorApproval {
		t.Fatalf("requester: %s / %q / %q", rix.State, rix.PayState, PaymentReason(rix))
	}
	if n := chainEvents(t, req, EvPaymentAuthorized); n != 0 {
		t.Fatalf("%d authorizations signed with auto_max 0", n)
	}
	// What the task shows as A2A while it waits (for internal/a2ashape).
	pm := req.PaymentStatusMeta(rix)
	if pm[x402a2a.KeyStatus] != x402a2a.StatusRequired || pm[x402a2a.KeyRequired] == nil ||
		pm[x402a2a.KeyReason] != x402a2a.ReasonNeedsOperatorApproval {
		t.Errorf("status metadata while waiting: %v", pm)
	}
	token, err := loadOrGenControlToken(req.layout)
	if err != nil {
		t.Fatal(err)
	}
	p := newPlaneFor(t, req, token)
	pay := func(body string) (int, map[string]any) {
		resp, b := p.req(t, "POST", "/tasks/pay", body, p.bearer)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}
	submit := `{"task_id":"` + id + `","decision":"submit"}`
	if code, out := pay(submit); code != http.StatusForbidden || out["reason"] != SpendOverSingle {
		t.Fatalf("agent tier at agent_max 0: %d %v", code, out)
	}
	payPolicy(t, req, PaymentsConfig{AgentMax: 10, AgentDailyMax: 10})
	if code, out := pay(submit); code != http.StatusForbidden || out["reason"] != SpendPayeeNotAllowed {
		t.Fatalf("a payee not on the list: %d %v", code, out)
	}
	payPolicy(t, req, PaymentsConfig{AgentMax: 10, AgentDailyMax: 10}, prov.AID())
	if n := chainEvents(t, req, EvPaymentAuthorized); n != 0 {
		t.Fatalf("refused payments were recorded as authorized: %d", n)
	}
	// §8.7: an option that is not the quoted one, and a payload of the
	// client's own, are refused as payment-failed with SETTLEMENT_FAILED.
	var stored struct {
		Accepts []map[string]any `json:"accepts"`
	}
	_ = json.Unmarshal(rix.PayRequired, &stored)
	other := map[string]any{}
	for k, v := range stored.Accepts[0] {
		other[k] = v
	}
	other["amount"] = "4"
	ob, _ := json.Marshal(map[string]any{"task_id": id, "decision": "submit", "accept": other})
	if code, out := pay(string(ob)); code != http.StatusUnprocessableEntity || out["reason"] != x402a2a.ReasonOptionNotOffered ||
		out["result"].(map[string]any)[x402a2a.KeyError] != "SETTLEMENT_FAILED" {
		t.Fatalf("an option not offered: %d %v", code, out)
	}
	if code, out := pay(`{"task_id":"` + id + `","decision":"submit","payload":{"x402Version":2}}`); code != http.StatusUnprocessableEntity ||
		out["reason"] != x402a2a.ReasonClientPayloadUnsupported {
		t.Fatalf("a client's own payload: %d %v", code, out)
	}
	// The option exactly as quoted, keys in another order, is accepted.
	ab, _ := json.Marshal(map[string]any{"task_id": id, "decision": "submit", "accept": stored.Accepts[0]})
	code, out := pay(string(ab))
	if code != http.StatusOK || out[x402a2a.KeyStatus] != x402a2a.StatusSubmitted {
		t.Fatalf("the quoted option: %d %v", code, out)
	}
	if ev := lastLedgerPayload(t, req, EvPaymentAuthorized); ev["purpose"] != module.PurposeTaskAgent {
		t.Errorf("purpose = %v, want task-agent", ev["purpose"])
	}
	// One payment in flight per task (§8.3).
	if code, _ := pay(submit); code != http.StatusConflict {
		t.Errorf("a second payment while one is pending: %d", code)
	}
	poll(t, prov, req, req)
	if rix := getIX(t, req, id); rix.State != interactions.StateCompleted || work.invoked.Load() != 1 {
		t.Fatalf("after paying: %s, ran %d", rix.State, work.invoked.Load())
	}
	// A console session cannot pay (§8.6).
	s := p.session(t)
	for _, route := range []string{"/tasks/pay", "/tasks/pay-manual", "/payments/limits"} {
		resp, b := p.req(t, "POST", route, submit, s.as(p))
		if !refusedForSession(resp, b) {
			t.Errorf("%s answered a console session: %d %s", route, resp.StatusCode, b)
		}
	}
}

// The manual tier pays within explicit_max; a rejection cancels the task
// on both sides and nothing is paid.
func TestManualPaymentAndRejection(t *testing.T) {
	work := &meteredWork{price: 5}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{}, prov.AID()) // defaults: explicit_max 10, daily_max 50
	ctx := context.Background()

	id, _ := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	poll(t, prov, req)
	if _, err := req.PayTask(ctx, PayRequest{TaskID: id, Decision: PayDecisionSubmit,
		Purpose: module.PurposeTaskManual}); err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req, req)
	if rix := getIX(t, req, id); rix.State != interactions.StateCompleted {
		t.Fatalf("manual payment: %s", rix.State)
	}
	if ev := lastLedgerPayload(t, req, EvPaymentAuthorized); ev["purpose"] != module.PurposeTaskManual {
		t.Errorf("purpose = %v", ev["purpose"])
	}

	id2, _ := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	poll(t, prov, req)
	out, err := req.PayTask(ctx, PayRequest{TaskID: id2, Decision: PayDecisionReject, Purpose: module.PurposeTaskManual})
	if err != nil || out.Status != x402a2a.StatusRejected {
		t.Fatalf("reject: %+v %v", out, err)
	}
	if rix := getIX(t, req, id2); rix.State != interactions.StateCanceled || rix.PayState != interactions.PayRejected {
		t.Fatalf("requester after rejecting: %s / %q", rix.State, rix.PayState)
	}
	poll(t, prov)
	pix := getIX(t, prov, id2)
	if pix.State != interactions.StateCanceled || pix.PayState != interactions.PayRejected {
		t.Fatalf("provider after the rejection: %s / %q", pix.State, pix.PayState)
	}
	if st, meta := lastStatusMeta(t, prov, id2); st != interactions.StateCanceled || meta[x402a2a.KeyStatus] != x402a2a.StatusRejected {
		t.Errorf("the provider's notice: %s %v", st, meta)
	}
	if work.invoked.Load() != 1 || balanceOf(hub, req.AID()) != 495 {
		t.Errorf("after a rejection: ran %d, balance %d", work.invoked.Load(), balanceOf(hub, req.AID()))
	}
}

// The spending policy (A2A-DESIGN §8.6 [C27]): each tier's single and
// daily limits, the payee list, redeem outside it, and nothing recorded
// for a refusal.
func TestTheSpendingPolicyTiers(t *testing.T) {
	srv := newFakeHub(t)
	d := newTestDaemon(t, srv.URL, false)
	if err := d.RegisterWithHub(context.Background(), srv.URL, "Spender", nil, ""); err != nil {
		t.Fatal(err)
	}
	payPolicy(t, d, PaymentsConfig{AutoMax: 10, AgentMax: 20, AgentDailyMax: 25, ExplicitMax: u64(30),
		DailyMax: u64(60)}, "did:anet:a")
	steps := []struct {
		payTo   string
		amount  uint64
		purpose string
		want    string
	}{
		{"did:anet:a", 11, module.PurposeTaskAuto, SpendOverSingle},
		{"did:anet:b", 5, module.PurposeTaskAuto, SpendPayeeNotAllowed},
		{"did:anet:a", 0, module.PurposeTaskAuto, SpendZeroAmount},
		{"did:anet:a", 1, "whatever", SpendUnknownPurpose},
		{"did:anet:a", 10, module.PurposeTaskAuto, ""},
		{"did:anet:a", 10, module.PurposeTaskAuto, ""},
		{"did:anet:a", 10, module.PurposeTaskAuto, SpendOverAgentDaily}, // auto payments split up still meet agent_daily_max
		{"did:anet:a", 21, module.PurposeTaskAgent, SpendOverSingle},
		{"did:anet:a", 5, module.PurposeTaskAgent, ""},
		{"did:anet:a", 1, module.PurposeTaskAgent, SpendOverAgentDaily},
		{"did:anet:a", 31, module.PurposeTaskManual, SpendOverSingle},
		{"did:anet:a", 30, module.PurposeTaskManual, ""},
		{"did:anet:a", 6, module.PurposeGateway, SpendOverDaily},
		{"did:anet:hub-not-a-payee", 5, module.PurposeRedeem, ""}, // redeem is not held to the payee list
		{"did:anet:hub-not-a-payee", 1, module.PurposeRedeem, SpendOverDaily},
	}
	for i, s := range steps {
		err := d.AdmitSpend(s.payTo, s.amount, s.purpose)
		got := ""
		if r, ok := isSpendRefusal(err); ok {
			got = r.Code
		} else if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got != s.want {
			t.Errorf("step %d (%s %d to %s): %q, want %q", i, s.purpose, s.amount, s.payTo, got, s.want)
		}
	}
	if st := d.SpendStatus(); st.Spent24h != 60 || st.AgentSpent24h != 25 {
		t.Errorf("totals = %+v", st)
	}
	// Through the module: a refusal signs and records nothing.
	before := chainEvents(t, d, EvPaymentAuthorized)
	_, err := d.payer().Authorize(payment.PaymentOption{Scheme: payment.SchemeCredit,
		Network: payment.CreditNetwork(hubAIDOf(srv.URL)), Amount: "1", Asset: payment.AssetCredit,
		PayTo: "did:anet:a"}, "ix", "bind", module.PurposeTaskManual)
	if _, ok := isSpendRefusal(err); !ok {
		t.Fatalf("over the daily limit through the module: %v", err)
	}
	if after := chainEvents(t, d, EvPaymentAuthorized); after != before {
		t.Errorf("a refused payment was recorded: %d → %d", before, after)
	}
}

// Two payments against the same headroom do not both pass (one lock over
// check and record).
func TestConcurrentSpendsDoNotBothPass(t *testing.T) {
	srv := newFakeHub(t)
	d := newTestDaemon(t, srv.URL, false)
	payPolicy(t, d, PaymentsConfig{ExplicitMax: u64(10), DailyMax: u64(10)}, "did:anet:a")
	for round := 0; round < 20; round++ {
		d.spend.mu.Lock()
		d.spend.records = nil
		d.spend.mu.Unlock()
		var wg sync.WaitGroup
		var ok atomic.Int32
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if d.AdmitSpend("did:anet:a", 6, module.PurposeTaskManual) == nil {
					ok.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if ok.Load() != 1 {
			t.Fatalf("round %d: %d of two payments of 6 passed a daily limit of 10", round, ok.Load())
		}
	}
}

// reopen closes d and starts a daemon on the same data directory.
func reopen(t *testing.T, d *Daemon) *Daemon {
	t.Helper()
	layout := d.layout
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	if n.relayStop != nil {
		n.relayStop()
		n.relayStop = nil
	}
	n.mu.Unlock()
	t.Cleanup(func() { n.Close() })
	return n
}

// The daily totals survive a restart: they are rebuilt from the
// anet.payment.authorized events of the last 24 hours, by purpose.
func TestSpendTotalsAreRebuiltAtStart(t *testing.T) {
	srv := newFakeHub(t)
	d := newTestDaemon(t, srv.URL, false)
	if err := d.RegisterWithHub(context.Background(), srv.URL, "Spender", nil, ""); err != nil {
		t.Fatal(err)
	}
	pc := PaymentsConfig{AgentMax: 10, AgentDailyMax: 10, ExplicitMax: u64(10), DailyMax: u64(20)}
	payPolicy(t, d, pc, "did:anet:a")
	opt := func(n uint64) payment.PaymentOption {
		return payment.PaymentOption{Scheme: payment.SchemeCredit, Network: payment.CreditNetwork(hubAIDOf(srv.URL)),
			Amount: payment.Amount(n), Asset: payment.AssetCredit, PayTo: "did:anet:a"}
	}
	if _, err := d.payer().Authorize(opt(7), "ix1", "b1", module.PurposeGateway); err != nil {
		t.Fatal(err)
	}
	if _, err := d.payer().Authorize(opt(3), "ix2", "b2", module.PurposeTaskAgent); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(d.layout, d.config()); err != nil {
		t.Fatal(err)
	}
	d = reopen(t, d)
	if st := d.SpendStatus(); st.Spent24h != 10 || st.AgentSpent24h != 3 {
		t.Fatalf("after a restart: %+v, want 10 in total and 3 in the agent tiers", st)
	}
	if err := d.AdmitSpend("did:anet:a", 8, module.PurposeTaskAgent); err == nil {
		t.Error("the agent daily limit forgot what was signed before the restart")
	}
	if err := d.AdmitSpend("did:anet:a", 7, module.PurposeTaskAgent); err != nil {
		t.Errorf("within the rebuilt headroom: %v", err)
	}
}

// sendPayment has req send a payment-submitted carrying raw on task id.
func sendPayment(t *testing.T, req, prov *Daemon, id string, raw []byte, mid string) {
	t.Helper()
	meta, _ := json.Marshal(map[string]any{x402a2a.KeyStatus: x402a2a.StatusSubmitted, x402a2a.KeyPayload: json.RawMessage(raw)})
	body, _ := (&delegation.ChatMsg{Kind: delegation.ChatText, MsgID: mid, Metadata: meta}).Marshal()
	if r := receive(t, prov, sealFrom(t, req, prov, seal.TypeMessage, id, body)); r.class != rxAccepted {
		t.Fatalf("payment message: %+v", r)
	}
}

// The merchant check on the provider (A2A-DESIGN §8.4): a payment bound to
// other work, or short of the quote, is answered payment-failed with its
// code before anything goes to the hub, and the task takes another payment.
func TestAWrongPaymentIsRefusedBeforeSettlement(t *testing.T) {
	work := &meteredWork{price: 50}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{ExplicitMax: u64(100), DailyMax: u64(1000)}, prov.AID())
	ctx := context.Background()
	id, _ := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	poll(t, prov, req)
	rix := getIX(t, req, id)
	opt := storedQuote(rix).Accepts[0]
	bind := x402a2a.PayBind(id, rix.TaskNonce)

	for i, tc := range []struct {
		name, bind, amount, code, reason string
	}{
		{"bound to other work", x402a2a.PayBind("ix_other", rix.TaskNonce), "50", "SETTLEMENT_FAILED", "binding_mismatch"},
		{"underpaid", bind, "49", "INVALID_AMOUNT", payment.ReasonInvalidAmount},
	} {
		o := opt
		o.Amount = tc.amount
		raw, err := req.payer().Authorize(o, id, tc.bind, module.PurposeGateway)
		if err != nil {
			t.Fatal(err)
		}
		sendPayment(t, req, prov, id, raw, "msg_bad_"+string(rune('a'+i)))
		pix := getIX(t, prov, id)
		if pix.State != interactions.StateInputRequired || pix.PayState != interactions.PayFailed {
			t.Fatalf("%s: provider %s / %q", tc.name, pix.State, pix.PayState)
		}
		_, meta := lastStatusMeta(t, prov, id)
		if meta[x402a2a.KeyStatus] != x402a2a.StatusFailed || meta[x402a2a.KeyError] != tc.code || meta["anet.reason"] != tc.reason ||
			meta[x402a2a.KeyRequired] == nil {
			t.Errorf("%s: status %v", tc.name, meta)
		}
	}
	if n := len(settleBodiesOn(hub)); n != 0 || work.invoked.Load() != 0 {
		t.Fatalf("a refused payment reached the hub (%d) or ran the work (%d)", n, work.invoked.Load())
	}
	// The right payment is taken after the wrong ones.
	raw, err := req.payer().Authorize(opt, id, bind, module.PurposeGateway)
	if err != nil {
		t.Fatal(err)
	}
	sendPayment(t, req, prov, id, raw, "msg_good")
	if pix := getIX(t, prov, id); pix.State != interactions.StateCompleted || work.invoked.Load() != 1 {
		t.Fatalf("the right payment: %s, ran %d", pix.State, work.invoked.Load())
	}
	// A second payment on a settled task is not presented.
	raw2, _ := req.payer().Authorize(opt, id, bind, module.PurposeGateway)
	sendPayment(t, req, prov, id, raw2, "msg_again")
	if n := len(settleBodiesOn(hub)); n != 1 || debitsOn(hub, req.AID()) != 1 {
		t.Errorf("after a second payment: %d settle calls, %d debits", n, debitsOn(hub, req.AID()))
	}
}

// A settlement whose outcome is not known is presented again, with the
// same payload, until it is: the hub settled while the provider was not
// listening, and the requester is charged once and the work runs once
// (A2A-DESIGN §8.3 [C25]).
func TestAnUnknownSettlementIsRetriedAndChargedOnce(t *testing.T) {
	shortSettleRetry(t, 20*time.Millisecond)
	for _, faults := range [][]string{
		{"settle-then-drop"},
		{"settle-then-pending"},
		{"pending", "drop", "pending"},
	} {
		t.Run(strings.Join(faults, "+"), func(t *testing.T) {
			work := &meteredWork{price: 30}
			hub, req, prov := paidPair(t, work)
			payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
			id, _ := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
			poll(t, prov, req)
			settleFaultsOn(hub, faults...)
			poll(t, prov)
			waitUntil(t, "the settlement to become known and the work to run", func() bool {
				return getIX(t, prov, id).State == interactions.StateCompleted
			})
			if n := work.invoked.Load(); n != 1 {
				t.Errorf("the work ran %d times", n)
			}
			if n := debitsOn(hub, req.AID()); n != 1 || balanceOf(hub, req.AID()) != 470 {
				t.Errorf("%d debits, balance %d", n, balanceOf(hub, req.AID()))
			}
			bodies := settleBodiesOn(hub)
			for _, b := range bodies[1:] {
				if string(b) != string(bodies[0]) {
					t.Fatal("a retry presented a different payment")
				}
			}
			waitUntil(t, "the requester to complete", func() bool {
				poll(t, req)
				return getIX(t, req, id).State == interactions.StateCompleted
			})
			if s := lastLedgerPayload(t, req, EvPaymentSettled); s["verified"] != true {
				t.Errorf("payer's evidence: %v", s)
			}
		})
	}
}

// The provider stops between the hub settling and recording it; at the
// next start the submitted payment is presented again, answered with the
// original receipt, and the work runs (A2A-DESIGN §8.3, §17 C25).
func TestASettlementInFlightIsResumedAfterARestart(t *testing.T) {
	work := &meteredWork{price: 30}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	id, _ := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
	poll(t, prov, req)
	old := settleRetryBase.set(time.Hour) // the first process never retries
	defer settleRetryBase.set(old)
	settleFaultsOn(hub, "settle-then-drop")
	poll(t, prov)
	if pix := getIX(t, prov, id); pix.PayState != interactions.PaySubmitted || work.invoked.Load() != 0 {
		t.Fatalf("before the restart: %q, ran %d", pix.PayState, work.invoked.Load())
	}
	settleRetryBase.set(20 * time.Millisecond)
	prov = reopen(t, prov)
	if err := prov.Providers().Register(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the resumed settlement to run the work", func() bool {
		return getIX(t, prov, id).State == interactions.StateCompleted
	})
	if work.invoked.Load() != 1 || debitsOn(hub, req.AID()) != 1 {
		t.Errorf("ran %d, %d debits", work.invoked.Load(), debitsOn(hub, req.AID()))
	}
}

// Cancel and payment racing (A2A-DESIGN §4.2, §17 C34), in the three
// orders. In each the payer's chain ends up holding the settlement.
func TestCancelAndPaymentRaces(t *testing.T) {
	shortSettleRetry(t, 50*time.Millisecond)
	ctx := context.Background()

	t.Run("cancel after payment-submitted, before settlement", func(t *testing.T) {
		work := &meteredWork{price: 30}
		hub, req, prov := paidPair(t, work)
		payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
		id, _ := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
		poll(t, prov, req)
		settleFaultsOn(hub, "pending", "pending")
		poll(t, prov) // the payment, not settled yet
		ix, err := req.CancelTask(ctx, id)
		if err != nil || ix.State != interactions.StateWorking {
			t.Fatalf("cancel with a payment submitted: %v, state %s (want unchanged)", err, ix.State)
		}
		if pm := req.PaymentStatusMeta(ix); pm[x402a2a.KeyCancelRequested] != true ||
			pm[x402a2a.KeyStatus] != x402a2a.StatusSubmitted {
			t.Errorf("status metadata after the cancel: %v", pm)
		}
		poll(t, prov) // the cancel: the payment decides
		waitUntil(t, "the paid work to complete", func() bool {
			return getIX(t, prov, id).State == interactions.StateCompleted
		})
		waitUntil(t, "the requester to complete", func() bool {
			poll(t, req)
			return getIX(t, req, id).State == interactions.StateCompleted
		})
		if s := lastLedgerPayload(t, req, EvPaymentSettled); s["verified"] != true || s["interaction_id"] != id {
			t.Errorf("payer's evidence: %v", s)
		}
	})

	t.Run("cancel after settlement, before the result", func(t *testing.T) {
		work := &meteredWork{price: 30, gate: make(chan struct{}), started: make(chan struct{})}
		_, req, prov := paidPair(t, work)
		payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
		id, _ := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
		poll(t, prov, req, prov)
		<-work.started // settled and running
		if _, err := req.CancelTask(ctx, id); err != nil {
			t.Fatal(err)
		}
		poll(t, prov) // the cancel reaches paid work: ignored
		if pix := getIX(t, prov, id); pix.IsTerminal() {
			t.Fatalf("paid work was canceled: %s", pix.State)
		}
		close(work.gate)
		waitUntil(t, "the requester to complete", func() bool {
			poll(t, req)
			return getIX(t, req, id).State == interactions.StateCompleted
		})
		if s := lastLedgerPayload(t, req, EvPaymentSettled); s["verified"] != true {
			t.Errorf("payer's evidence: %v", s)
		}
	})

	t.Run("canceled here, then the result arrives", func(t *testing.T) {
		work := &meteredWork{price: 30}
		_, req, prov := paidPair(t, work)
		payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
		id, _ := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
		poll(t, prov, req)
		// The task ended here while its payment was on its way.
		if _, err := req.ix.SetState(id, interactions.StateCanceled); err != nil {
			t.Fatal(err)
		}
		poll(t, prov, req, req)
		rix := getIX(t, req, id)
		if rix.State != interactions.StateCanceled || rix.PayState != interactions.PayCompleted || len(rix.Receipt) == 0 {
			t.Fatalf("requester: %s / %q / receipt %d", rix.State, rix.PayState, len(rix.Receipt))
		}
		s := lastLedgerPayload(t, req, EvPaymentSettled)
		if s["verified"] != true || s["after_terminal"] != true {
			t.Errorf("payer's evidence: %v", s)
		}
	})
}

// A quote that lapses unpaid fails the task with EXPIRED_PAYMENT, on both
// sides (A2A-DESIGN §8.3).
func TestALapsedQuoteFailsTheTask(t *testing.T) {
	work := &meteredWork{price: 30}
	_, req, prov := paidPair(t, work)
	ctx := context.Background()
	id, _ := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	poll(t, prov, req)
	past := time.Now().Add(-time.Minute).UnixMilli()
	if _, err := prov.ix.SetPayment(id, interactions.PayUpdate{QuoteExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	prov.expireQuotes(ctx)
	if pix := getIX(t, prov, id); pix.State != interactions.StateFailed || pix.PayState != interactions.PayFailed {
		t.Fatalf("provider: %s / %q", pix.State, pix.PayState)
	}
	st, meta := lastStatusMeta(t, req, id)
	if st != interactions.StateFailed || meta[x402a2a.KeyError] != "EXPIRED_PAYMENT" || meta["anet.reason"] != x402a2a.ReasonQuoteExpired {
		t.Errorf("requester: %s %v", st, meta)
	}
	// And a lapsed quote is not paid.
	if _, err := req.PayTask(ctx, PayRequest{TaskID: id, Decision: PayDecisionSubmit,
		Purpose: module.PurposeTaskManual}); !errors.Is(err, ErrTaskTerminal) {
		t.Errorf("paying a failed task: %v", err)
	}
}

// A receipt that does not belong to the task is recorded as unverified,
// not as a settlement of this task (A2A-DESIGN §8.3).
func TestAReceiptForAnotherAuthorizationIsNotVerified(t *testing.T) {
	work := &meteredWork{price: 30}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	ctx := context.Background()
	id, _ := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	poll(t, prov, req, prov, req, req)
	rix := getIX(t, req, id)
	var good []payment.SettlementResponse
	_ = json.Unmarshal(rix.PayReceipts, &good)
	if len(good) != 1 {
		t.Fatalf("receipts %s", rix.PayReceipts)
	}
	// The same hub-signed receipt presented on another task of ours.
	id2, _ := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	forged := good[0]
	forged.Transaction = "tx-elsewhere"
	req.notePaymentReceipts(id2, map[string]any{x402a2a.KeyReceipts: []any{forged}}, false)
	s := lastLedgerPayload(t, req, EvPaymentSettled)
	if s["interaction_id"] != id2 || s["verified"] != false || !strings.Contains(s["refused"].(string), "did not sign") {
		t.Errorf("evidence for a foreign receipt: %v", s)
	}
	if getIX(t, req, id2).PayState == interactions.PayCompleted {
		t.Error("a foreign receipt marked the task paid")
	}
	_ = hub
	_ = base64.StdEncoding
}

// A call that was paid for and then could not run is failed, not
// rejected, and the payer is shown the receipt (A2A-DESIGN §4.3).
func TestAPaidCallThatCannotRunFailsWithTheReceipts(t *testing.T) {
	work := &meteredWork{price: 30, status: effect.Unavailable}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	id, _ := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
	poll(t, prov, req, prov, req, req)
	rix := getIX(t, req, id)
	if rix.State != interactions.StateFailed || rix.PayState != interactions.PayCompleted {
		t.Fatalf("requester: %s / %q, want failed / completed", rix.State, rix.PayState)
	}
	var receipts []payment.SettlementResponse
	if json.Unmarshal(rix.PayReceipts, &receipts) != nil || len(receipts) != 1 || !receipts[0].Success {
		t.Errorf("the payer is not shown the receipt: %s", rix.PayReceipts)
	}
	if debitsOn(hub, req.AID()) != 1 {
		t.Errorf("%d debits", debitsOn(hub, req.AID()))
	}
}

// A node with no room for a long call does not settle a payment for one:
// the requester is told provider_busy and the task waits (A2A-DESIGN §8.3).
func TestABusyNodeDoesNotTakeThePayment(t *testing.T) {
	work := &meteredWork{price: 30, gate: make(chan struct{}), started: make(chan struct{})}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	id, _ := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
	poll(t, prov, req)
	for i := 0; i < maxConcurrentLongCalls; i++ {
		prov.longCalls <- struct{}{}
	}
	poll(t, prov)
	pix := getIX(t, prov, id)
	if pix.State != interactions.StateInputRequired || pix.PayState != interactions.PayFailed {
		t.Fatalf("provider: %s / %q", pix.State, pix.PayState)
	}
	_, meta := lastStatusMeta(t, prov, id)
	if meta[x402a2a.KeyReason] != x402a2a.ReasonProviderBusy || meta[x402a2a.KeyError] != x402a2a.CodeSettlementFailed {
		t.Errorf("status %v", meta)
	}
	if n := len(settleBodiesOn(hub)); n != 0 || work.invoked.Load() != 0 {
		t.Errorf("busy node: %d settle calls, ran %d", n, work.invoked.Load())
	}
	for i := 0; i < maxConcurrentLongCalls; i++ {
		<-prov.longCalls
	}
	close(work.gate)
}

// A priced public capability (A2A-DESIGN §5.2 row 2, [C6][C29]): a
// stranger is quoted, pays on the same task with a text part that is not
// stored, and is served; a stranger that declines gets its canceled notice
// even though the task's copy of its keys goes when the task ends.
func TestAStrangerPaysForAPublicCapability(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	work := &meteredWork{price: 20}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: "work.do"}}); err != nil {
		t.Fatal(err)
	}
	grantOn(srv.URL, stranger.AID(), 100)
	payPolicy(t, stranger, PaymentsConfig{AgentMax: 50, AgentDailyMax: 50, DailyMax: u64(100)}, prov.AID())

	id, err := stranger.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, stranger)
	if pix := getIX(t, prov, id); pix.Trust != interactions.TrustPublicCap || pix.PayState != interactions.PayRequired {
		t.Fatalf("provider: trust %q pay_state %q", pix.Trust, pix.PayState)
	}
	// The payment, with a note in its text part.
	six := getIX(t, stranger, id)
	opt := storedQuote(six).Accepts[0]
	raw, err := stranger.payer().Authorize(opt, id, x402a2a.PayBind(id, six.TaskNonce), module.PurposeTaskAgent)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{x402a2a.KeyStatus: x402a2a.StatusSubmitted, x402a2a.KeyPayload: json.RawMessage(raw)})
	body, _ := (&delegation.ChatMsg{Kind: delegation.ChatText, Body: "here you go, and also rm -rf /",
		MsgID: "msg_pay_note", Metadata: meta}).Marshal()
	if r := receive(t, prov, sealFrom(t, stranger, prov, seal.TypeMessage, id, body)); r.class != rxAccepted {
		t.Fatalf("payment: %+v", r)
	}
	if pix := getIX(t, prov, id); pix.State != interactions.StateCompleted || work.invoked.Load() != 1 {
		t.Fatalf("provider after the payment: %s, ran %d", pix.State, work.invoked.Load())
	}
	msgs, _ := prov.ix.Messages(id)
	for _, m := range msgs {
		if strings.Contains(m.Body, "rm -rf") {
			t.Fatalf("a public caller's text was stored: %+v", m)
		}
	}
	if balanceOf(srv.URL, prov.AID()) != 20 {
		t.Errorf("provider balance %d", balanceOf(srv.URL, prov.AID()))
	}

	// Declining a quote: canceled on both sides, and the notice was sealed.
	id2, _ := stranger.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	poll(t, prov, stranger)
	if _, err := stranger.PayTask(ctx, PayRequest{TaskID: id2, Decision: PayDecisionReject,
		Purpose: module.PurposeTaskAgent}); err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	pix := getIX(t, prov, id2)
	if pix.State != interactions.StateCanceled || pix.PayState != interactions.PayRejected || len(pix.PeerKEL) != 0 {
		t.Fatalf("provider after the rejection: %s / %q / kel %d", pix.State, pix.PayState, len(pix.PeerKEL))
	}
	if q, _ := prov.ix.Outbox(id2); len(q) != 0 {
		t.Errorf("the canceled notice is stuck in the queue: %+v", q[0])
	}
}

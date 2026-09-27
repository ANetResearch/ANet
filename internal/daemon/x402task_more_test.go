//go:build !no_x402

package daemon

// The rest of the same-task payment flow's cases (A2A-DESIGN §8, §17 C3
// supplementary cases; 0017 Q11, Q18, Q19): one automatic payment per
// task, the receipts on every terminal message and their failure form, the
// task nonce a priced call must carry, a second settlement for one task,
// the requester ending a quoted task, a deny landing between quote and
// payment, auto-reply staying out of a paid call, a redelivered paid call,
// late receipts, and every signing surface held to the spending policy.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// receiptsIn decodes x402.payment.receipts out of metadata, failing the
// test when the key is absent.
func receiptsIn(t *testing.T, meta map[string]any, what string) []map[string]any {
	t.Helper()
	raw, ok := meta[x402a2a.KeyReceipts]
	if !ok {
		t.Fatalf("%s carries no %s: %v", what, x402a2a.KeyReceipts, meta)
	}
	b, _ := json.Marshal(raw)
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: receipts %s: %v", what, b, err)
	}
	return out
}

// §8.3 (0017 Q11): the automatic tier pays a task once. A payment the hub
// refused for good returns the task to input-required; neither that nor a
// quote repeated after it is paid automatically again, the task waits with
// needs_operator_approval, and an operator's payment is still taken. The
// failure travels as a2a-x402 §9.2 has it (Q18): transaction "", the
// refused authorization's id under extensions["anet.auth_id"].
func TestATaskIsPaidAutomaticallyAtMostOnce(t *testing.T) {
	work := &meteredWork{price: 600} // more than the 500 the requester holds
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 1000, AgentDailyMax: 5000, ExplicitMax: u64(1000),
		DailyMax: u64(5000)}, prov.AID())
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req) // quoted, and paid automatically
	first := getIX(t, req, id)
	if first.PayState != interactions.PaySubmitted || authorizedCount(req, id) != 1 {
		t.Fatalf("after the quote: %q, %d signed", first.PayState, authorizedCount(req, id))
	}
	firstAuth := first.PayAuthIDs[0]
	poll(t, prov) // the hub refuses it: insufficient funds
	pix := getIX(t, prov, id)
	if pix.State != interactions.StateInputRequired || pix.PayState != interactions.PayFailed {
		t.Fatalf("provider after the refusal: %s / %q", pix.State, pix.PayState)
	}
	poll(t, req)
	rix := getIX(t, req, id)
	if rix.State != interactions.StateInputRequired || rix.PayState != interactions.PayFailed ||
		authorizedCount(req, id) != 1 || PaymentReason(rix) != x402a2a.ReasonNeedsOperatorApproval {
		t.Fatalf("requester after payment-failed: %s / %q, %d signed, reason %q",
			rix.State, rix.PayState, authorizedCount(req, id), PaymentReason(rix))
	}
	pm := req.PaymentStatusMeta(rix)
	if pm[x402a2a.KeyError] != x402a2a.CodeInsufficientFunds || pm[x402a2a.KeyReason] != x402a2a.ReasonNeedsOperatorApproval {
		t.Errorf("status metadata after the failure: %v", pm)
	}
	rs := receiptsIn(t, pm, "the requester's task")
	if ext, _ := rs[0]["extensions"].(map[string]any); len(rs) != 1 || rs[0]["success"] != false ||
		rs[0]["transaction"] != "" || ext[x402a2a.ExtAuthID] != firstAuth {
		t.Errorf("the failure receipt: %v", rs)
	}

	// The provider quotes again: still not paid automatically, and the
	// automatic tier refuses a second signature outright.
	providerQuote(t, prov, req, id, storedQuote(rix))
	if rix = getIX(t, req, id); authorizedCount(req, id) != 1 || PaymentReason(rix) != x402a2a.ReasonNeedsOperatorApproval {
		t.Fatalf("a quote after the failure: %d signed, reason %q", authorizedCount(req, id), PaymentReason(rix))
	}
	if _, err := req.PayTask(ctx, PayRequest{TaskID: id, Decision: PayDecisionSubmit,
		Purpose: module.PurposeTaskAuto}); !errors.Is(err, errAutoPaidOnce) {
		t.Fatalf("a second automatic payment: %v", err)
	}
	if authorizedCount(req, id) != 1 {
		t.Fatal("the refused automatic payment was signed")
	}

	// An operator decides, with the funds there now: a new authorization.
	grantOn(hub, req.AID(), 200)
	out, err := req.PayTask(ctx, PayRequest{TaskID: id, Decision: PayDecisionSubmit, Purpose: module.PurposeTaskManual})
	if err != nil || out.Reused || out.AuthID == firstAuth {
		t.Fatalf("the operator's payment: %+v %v", out, err)
	}
	poll(t, prov, req, req)
	if rix := getIX(t, req, id); rix.State != interactions.StateCompleted || work.invoked.Load() != 1 ||
		debitsOn(hub, req.AID()) != 1 || authorizedCount(req, id) != 2 {
		t.Fatalf("after the operator paid: %s, ran %d, %d debits, %d signed", rix.State, work.invoked.Load(),
			debitsOn(hub, req.AID()), authorizedCount(req, id))
	}
}

// §2 X4 (0017 Q19): a priced call whose TaskDoc carries no anet.nonce is
// rejected with task_nonce_required, and nothing is quoted: the payment
// could not be bound to it.
func TestAPricedCallWithoutATaskNonceIsRejected(t *testing.T) {
	work := &meteredWork{price: 10}
	_, req, prov := paidPair(t, work)
	const id = "ix_nononce"
	env := sealFrom(t, req, prov, seal.TypeDelegate, id, delegateBody(t, req.self, id, "work.do", "work.do"))
	if r := receive(t, prov, env); r.class != rxAccepted {
		t.Fatalf("delegate: %+v", r)
	}
	waitUntil(t, "the call to be answered", func() bool { return getIX(t, prov, id).IsTerminal() })
	pix := getIX(t, prov, id)
	meta := decodeMeta([]byte(pix.ResultMeta))
	if pix.State != interactions.StateRejected || meta[x402a2a.KeyReason] != x402a2a.ReasonTaskNonceRequired ||
		pix.PayState != interactions.PayNone || len(pix.PayRequired) != 0 {
		t.Fatalf("provider: %s / %v / pay_state %q", pix.State, meta, pix.PayState)
	}
	if n := chainEvents(t, prov, EvPaymentQuoted); n != 0 || work.invoked.Load() != 0 {
		t.Errorf("quoted %d times, ran %d", n, work.invoked.Load())
	}
	// A call with the nonce, from the same requester, is quoted.
	id2, err := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	if pix := getIX(t, prov, id2); pix.PayState != interactions.PayRequired {
		t.Errorf("a call with its nonce: pay_state %q", pix.PayState)
	}
}

// a2a-x402 §7 (0017 Q18): every terminal message of a task that took part
// in the payment flow carries x402.payment.receipts — the canceled after a
// declined quote, the canceled after a provider's cancel, the failed of a
// lapsed quote and a terminal status sent through SendStatus.
func TestTerminalPaymentMessagesCarryReceipts(t *testing.T) {
	work := &meteredWork{price: 30}
	_, req, prov := paidPair(t, work)
	ctx := context.Background()
	quoted := func() string {
		t.Helper()
		id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
		if err != nil {
			t.Fatal(err)
		}
		poll(t, prov, req)
		return id
	}
	// check reads the last status on d's record of task id: the provider's
	// own copy when the requester has ended the task first (a status for a
	// task already over is not stored there), the requester's otherwise.
	check := func(what string, d *Daemon, id string, want interactions.State, failures int) {
		t.Helper()
		st, meta := lastStatusMeta(t, d, id)
		if st != want {
			t.Fatalf("%s: the last status is %s, want %s (%v)", what, st, want, meta)
		}
		rs := receiptsIn(t, meta, what)
		if len(rs) != failures {
			t.Errorf("%s: %d receipts, want %d: %v", what, len(rs), failures, rs)
		}
		for _, r := range rs {
			if r["success"] == false && r["transaction"] != "" {
				t.Errorf("%s: a failure with transaction %v", what, r["transaction"])
			}
		}
	}

	declined := quoted()
	if _, err := req.PayTask(ctx, PayRequest{TaskID: declined, Decision: PayDecisionReject,
		Purpose: module.PurposeTaskManual}); err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	check("a declined quote", prov, declined, interactions.StateCanceled, 0)

	canceled := quoted()
	if _, err := prov.CancelTask(ctx, canceled); err != nil {
		t.Fatal(err)
	}
	check("a provider's cancel", req, canceled, interactions.StateCanceled, 0)

	lapsed := quoted()
	past := int64(1)
	if _, err := prov.ix.SetPayment(lapsed, interactions.PayUpdate{QuoteExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	prov.expireQuotes(ctx)
	check("a lapsed quote", req, lapsed, interactions.StateFailed, 1)

	replied := quoted()
	if err := prov.SendStatus(ctx, replied, interactions.StateFailed, "cannot do it after all", nil); err != nil {
		t.Fatal(err)
	}
	check("a terminal status", req, replied, interactions.StateFailed, 0)

	// A task nothing was quoted for has no x402 part.
	if m := withTerminalReceipts(&interactions.Interaction{}, interactions.StateFailed, nil); m != nil {
		t.Errorf("a task outside the payment flow was given receipts: %v", m)
	}
}

// A failure receipt in the form an anet hub answers (the refused
// authorization's id as its transaction) is shown in a2a-x402 §9.2 form,
// whichever side produced the list (0017 Q18).
func TestAFailureReceiptIsNormalized(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"success":false,"errorReason":"insufficient_funds","transaction":"auth-1","network":"hub:h"}`,
			`{"errorReason":"insufficient_funds","extensions":{"anet.auth_id":"auth-1"},"network":"hub:h","success":false,"transaction":""}`},
		{`{"success":false,"errorReason":"x","transaction":"auth-2","extensions":{"anet.auth_id":"kept","n":12345678901234567890}}`,
			`{"errorReason":"x","extensions":{"anet.auth_id":"kept","n":12345678901234567890},"success":false,"transaction":""}`},
		{`{"success":false,"errorReason":"x"}`, `{"errorReason":"x","success":false,"transaction":""}`},
		{`{"success":false,"errorReason":"x","transaction":""}`, `{"success":false,"errorReason":"x","transaction":""}`},
		{`{"success":true,"transaction":"tx-1","network":"hub:h"}`, `{"success":true,"transaction":"tx-1","network":"hub:h"}`},
		{`"not an object"`, `"not an object"`},
	} {
		if got := string(normalizeReceipt(json.RawMessage(c.in))); got != c.want {
			t.Errorf("normalizeReceipt(%s)\n = %s\nwant %s", c.in, got, c.want)
		}
	}

	// The requester stores what a provider sent in that form too.
	work := &meteredWork{price: 30}
	_, req, prov := paidPair(t, work)
	id, err := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	req.notePaymentReceipts(id, map[string]any{x402a2a.KeyReceipts: []any{map[string]any{
		"success": false, "errorReason": "invalid_signature", "transaction": "auth-9", "network": "hub:h"}}}, false)
	var stored []map[string]any
	_ = json.Unmarshal(getIX(t, req, id).PayReceipts, &stored)
	if ext, _ := stored[0]["extensions"].(map[string]any); len(stored) != 1 || stored[0]["transaction"] != "" ||
		ext[x402a2a.ExtAuthID] != "auth-9" {
		t.Errorf("stored receipts: %v", stored)
	}
}

// §8.3 [m]: a second verified settlement for one task is recorded and
// marked, so that `anet audit` shows it.
func TestASecondSettlementForOneTaskIsMarked(t *testing.T) {
	work := &meteredWork{price: 30}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 500, ExplicitMax: u64(100), DailyMax: u64(500)},
		prov.AID())
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req, prov, req, req)
	rix := getIX(t, req, id)
	if rix.State != interactions.StateCompleted {
		t.Fatalf("setup: %s", rix.State)
	}
	// A second authorization for the same task, bound differently so the
	// hub's one-settlement-per-binding rule does not stop it: what a hub
	// that lost that rule would let through.
	opt := storedQuote(rix).Accepts[0]
	raw, err := req.payer().Authorize(opt, id, x402a2a.PayBind(id, "another nonce"), module.PurposeTaskManual)
	if err != nil {
		t.Fatal(err)
	}
	_, _, secondID, _ := payloadAuth(raw)
	if _, err := req.ix.SetPayment(id, interactions.PayUpdate{AddAuthID: secondID}); err != nil {
		t.Fatal(err)
	}
	st, err := prov.payer().Settle(ctx, raw, quotedRequirementsOf(t, prov, id))
	if err != nil || st.Failed != "" {
		t.Fatalf("second settlement: %+v %v", st, err)
	}
	both := append(storedReceipts(getIX(t, req, id)), json.RawMessage(mustJSON(t, st.Response)))
	list := make([]any, 0, len(both))
	for _, r := range both {
		var v any
		_ = json.Unmarshal(r, &v)
		list = append(list, v)
	}
	req.notePaymentReceipts(id, map[string]any{x402a2a.KeyReceipts: list}, true)
	s := lastLedgerPayload(t, req, EvPaymentSettled)
	if s["verified"] != true || s["second_receipt"] != true || s["auth_id"] != secondID {
		t.Errorf("the second settlement's evidence: %v", s)
	}
	if debitsOn(hub, req.AID()) != 2 {
		t.Errorf("%d debits", debitsOn(hub, req.AID()))
	}
}

// §17 C6/C29: the requester ends a task that waits on its quote. Nothing
// was paid and nothing ran: the task is canceled on both sides, with no
// receipt signed, and the canceled notice carries the (empty) receipts.
func TestAQuotedTaskEndedByTheRequesterIsCanceledWithoutAReceipt(t *testing.T) {
	work := &meteredWork{price: 30}
	hub, req, prov := paidPair(t, work)
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	if err := req.RequestEnd(ctx, id); err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	pix := getIX(t, prov, id)
	if pix.State != interactions.StateCanceled || len(pix.Receipt) != 0 || len(pix.Result) != 0 {
		t.Fatalf("provider: %s, receipt %d, result %d", pix.State, len(pix.Receipt), len(pix.Result))
	}
	st, meta := lastStatusMeta(t, req, id)
	if st != interactions.StateCanceled {
		t.Fatalf("requester's last status: %s", st)
	}
	_ = receiptsIn(t, meta, "the canceled notice")
	if rix := getIX(t, req, id); rix.State != interactions.StateCanceled || len(rix.Receipt) != 0 {
		t.Errorf("requester: %s, receipt %d", rix.State, len(rix.Receipt))
	}
	if work.invoked.Load() != 0 || len(settleBodiesOn(hub)) != 0 {
		t.Errorf("ran %d, %d settle calls", work.invoked.Load(), len(settleBodiesOn(hub)))
	}
}

// §17 C6/C29: a stranger quoted for a public capability is denied before
// it pays. The payment is dropped at the door, nothing reaches the hub,
// and the sweep cancels the quoted task with its receipts.
func TestAPaymentFromAPeerDeniedAfterTheQuoteIsNotSettled(t *testing.T) {
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
	payPolicy(t, stranger, PaymentsConfig{ExplicitMax: u64(50), DailyMax: u64(100)}, prov.AID())
	id, err := stranger.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, stranger)
	six := getIX(t, stranger, id)
	raw, err := stranger.payer().Authorize(storedQuote(six).Accepts[0], id, x402a2a.PayBind(id, six.TaskNonce),
		module.PurposeTaskManual)
	if err != nil {
		t.Fatal(err)
	}
	denyPeers(t, prov, stranger.AID())
	meta, _ := json.Marshal(map[string]any{x402a2a.KeyStatus: x402a2a.StatusSubmitted, x402a2a.KeyPayload: json.RawMessage(raw)})
	body, _ := (&delegation.ChatMsg{Kind: delegation.ChatText, MsgID: "msg_pay_denied", Metadata: meta}).Marshal()
	if r := receive(t, prov, sealFrom(t, stranger, prov, seal.TypeMessage, id, body)); r.class != rxDropped || r.reason != dropDenied {
		t.Fatalf("a payment from a denied peer: %+v", r)
	}
	if n := len(settleBodiesOn(srv.URL)); n != 0 || work.invoked.Load() != 0 {
		t.Fatalf("%d settle calls, ran %d", n, work.invoked.Load())
	}
	prov.revocationSweep()
	pix := getIX(t, prov, id)
	if pix.State != interactions.StateCanceled || pix.PayState != interactions.PayRequired {
		t.Fatalf("provider after the sweep: %s / %q", pix.State, pix.PayState)
	}
	// The stranger is told, with the receipts of a task that was quoted.
	st, meta2 := lastStatusMeta(t, stranger, id)
	if st != interactions.StateCanceled {
		t.Errorf("the stranger's last status: %s %v", st, meta2)
	}
	_ = receiptsIn(t, meta2, "the canceled notice to a denied stranger")
	if balanceOf(srv.URL, stranger.AID()) != 100 {
		t.Errorf("stranger balance %d", balanceOf(srv.URL, stranger.AID()))
	}
}

// §17 C6/C29: both sides run the exec auto-reply backend and trust each
// other; a quoted, paid, completed capability call never reaches it.
func TestExecAutoReplyOnBothSidesStaysOutOfAPaidCall(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	stub := filepath.Join(t.TempDir(), "agent.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho called >> "+calls+"\necho a reply\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	setExecCommand(t, stub)
	work := &meteredWork{price: 30}
	_, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	trustPeers(t, prov, req.AID())
	trustPeers(t, req, prov.AID())
	cfg := AutoReplyConfig{Backend: "exec", Agent: "cursor"}
	ctx := context.Background()
	repliers := map[*Daemon]autoReplier{}
	for _, d := range []*Daemon{req, prov} {
		r, err := newAutoReplier(cfg, d.layout)
		if err != nil {
			t.Fatal(err)
		}
		repliers[d] = r
	}
	step := func(ds ...*Daemon) {
		for _, d := range ds {
			poll(t, d)
			d.autoReplyOnce(ctx, cfg, repliers[d])
		}
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	step(prov, req, prov, req, req)
	if rix := getIX(t, req, id); rix.State != interactions.StateCompleted || work.invoked.Load() != 1 {
		t.Fatalf("the paid call: %s, ran %d", rix.State, work.invoked.Load())
	}
	if b, err := os.ReadFile(calls); err == nil && len(b) > 0 {
		t.Fatalf("the exec backend ran %d times during a paid call", strings.Count(string(b), "\n"))
	}
}

// A redelivered delegation of a call that was paid and answered: the
// answer is sent again with its receipts, payment-completed and the effect
// status, and the requester, whose copy of the first was lost, verifies the
// settlement from it.
func TestARedeliveredPaidCallResendsItsReceipts(t *testing.T) {
	work := &meteredWork{price: 30}
	srv, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	hub := &httptest.Server{URL: srv}
	env := onlyQueuedEnvelope(t, hub, prov.AID())
	poll(t, prov, req, prov) // quoted, paid, settled, run, answered
	if pix := getIX(t, prov, id); pix.State != interactions.StateCompleted {
		t.Fatalf("provider: %s", pix.State)
	}
	clearMailbox(t, hub, req.AID()) // payment-verified and the result are lost
	if r := receive(t, prov, env); r.reason != dropDuplicate {
		t.Fatalf("redelivery: %+v", r)
	}
	poll(t, req)
	rix := getIX(t, req, id)
	meta := decodeMeta([]byte(rix.ResultMeta))
	if rix.State != interactions.StateCompleted || rix.PayState != interactions.PayCompleted ||
		meta[x402a2a.KeyStatus] != x402a2a.StatusCompleted || meta["anet.effect_status"] == nil {
		t.Fatalf("requester: %s / %q / %v", rix.State, rix.PayState, meta)
	}
	if rs := receiptsIn(t, meta, "the re-sent answer"); len(rs) != 1 || rs[0]["success"] != true {
		t.Errorf("re-sent receipts: %v", rs)
	}
	if s := lastLedgerPayload(t, req, EvPaymentSettled); s["verified"] != true || s["interaction_id"] != id {
		t.Errorf("payer's evidence: %v", s)
	}
	if work.invoked.Load() != 1 {
		t.Errorf("ran %d", work.invoked.Load())
	}
}

// §4.2: receipts on a status that arrives after the task ended here are
// still verified and recorded; the state does not move.
func TestReceiptsOnAStatusAfterTheEndAreRecorded(t *testing.T) {
	work := &meteredWork{price: 30}
	_, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req, prov) // the provider settles and answers
	if _, err := req.ix.SetState(id, interactions.StateCanceled); err != nil {
		t.Fatal(err)
	}
	pix := getIX(t, prov, id)
	mb, _ := json.Marshal(map[string]any{x402a2a.KeyStatus: x402a2a.StatusVerified,
		x402a2a.KeyReceipts: json.RawMessage(pix.PayReceipts)})
	body, _ := (&delegation.StatusMsg{State: string(interactions.StateWorking), Text: "paid", Metadata: mb,
		At: prov.nowMS()}).Marshal()
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeStatus, id, body)); r.class != rxAccepted {
		t.Fatalf("late status: %+v", r)
	}
	rix := getIX(t, req, id)
	if rix.State != interactions.StateCanceled {
		t.Errorf("a late status moved the task to %s", rix.State)
	}
	s := lastLedgerPayload(t, req, EvPaymentSettled)
	if s["verified"] != true || s["after_terminal"] != true || s["interaction_id"] != id {
		t.Errorf("late settlement evidence: %v", s)
	}
}

// §8.6 [C27]: every surface that signs is held to the spending policy.
// Over its tier's limit each is refused with 403 and its reason, and
// nothing is signed or recorded.
func TestEverySigningSurfaceIsHeldToTheSpendingPolicy(t *testing.T) {
	work := &meteredWork{price: 5}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 1, AgentMax: 1, AgentDailyMax: 100, ExplicitMax: u64(2),
		DailyMax: u64(100)}, prov.AID())
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req) // task-auto: 5 > auto_max 1, so it waits
	if rix := getIX(t, req, id); rix.PayState != interactions.PayRequired || PaymentReason(rix) == "" {
		t.Fatalf("auto tier: %q / %q", rix.PayState, PaymentReason(rix))
	}
	token, err := loadOrGenControlToken(req.layout)
	if err != nil {
		t.Fatal(err)
	}
	p := newPlaneFor(t, req, token)
	for _, c := range []struct{ route, body string }{
		{"/tasks/pay", `{"task_id":"` + id + `","decision":"submit"}`},        // task-agent: agent_max 1
		{"/tasks/pay-manual", `{"task_id":"` + id + `","decision":"submit"}`}, // task-manual: explicit_max 2
		{"/x402-authorize", `{"pay_to":"` + prov.AID() + `","amount":3}`},     // gateway: explicit_max 2
		{"/redeem", `{"amount":3,"reference":"c27-redeem"}`},                  // redeem: explicit_max 2
	} {
		resp, b := p.req(t, "POST", c.route, c.body, p.bearer)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		if resp.StatusCode != http.StatusForbidden || out["reason"] != SpendOverSingle {
			t.Errorf("%s over its limit: %d %s", c.route, resp.StatusCode, b)
		}
	}
	if n := chainEvents(t, req, EvPaymentAuthorized); n != 0 {
		t.Errorf("%d authorizations recorded for refused payments", n)
	}
	if st := req.SpendStatus(); st.Spent24h != 0 {
		t.Errorf("refused payments counted: %+v", st)
	}
	if len(settleBodiesOn(hub)) != 0 || balanceOf(hub, req.AID()) != 500 {
		t.Errorf("money moved: %d settle calls, balance %d", len(settleBodiesOn(hub)), balanceOf(hub, req.AID()))
	}
	_ = payment.SchemeCredit
}

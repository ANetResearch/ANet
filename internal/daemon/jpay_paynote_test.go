//go:build !no_x402

package daemon

// The payment decision end to end, in one process (HPP J-PAY-5, J-PAY-6,
// J-PAY-15's last step; the joint lab's sections of them are PAY-15's): a
// requester and a provider at one fake hub, the quote, the price card and
// the payment through the relay as they travel between two nodes.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// sentenceOf is ix's stored decision, rendered as every surface renders it.
func sentenceOf(t *testing.T, d *Daemon, id string) (a2ashape.PayNote, string) {
	t.Helper()
	n, ok := storedPayNote(getIX(t, d, id))
	if !ok {
		t.Fatalf("%s has no payment decision", id)
	}
	return n, a2ashape.PaymentNote(n, a2ashape.Lang())
}

// sameEverywhere checks the three places a task's payment sentence shows
// (UX-T1): the control plane's /tasks/get (the CLI's payment line, and MCP
// get_task, which passes its JSON on), and the task the local A2A interface
// is given (TaskSeam, the requester's scope); and, while the task waits on
// it, status.message in both.
func sameEverywhere(t *testing.T, d *Daemon, id, peer, want string, waits bool) {
	t.Helper()
	control := getTaskProjection(t, d, id)
	sc, err := peerScope(peer)
	if err != nil {
		t.Fatal(err)
	}
	a2a, err := d.getTask(sc, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, task := range map[string]a2ashape.Task{"control": control, "a2a": a2a} {
		if task.Metadata[a2ashape.KeyPaymentNote] != want {
			t.Errorf("%s: anet.payment_note %v, want %q", name, task.Metadata[a2ashape.KeyPaymentNote], want)
		}
		if !waits {
			continue
		}
		m := task.Status.Message
		if m == nil || len(m.Parts) != 1 || m.Parts[0].Text != want {
			t.Errorf("%s: status.message %+v, want %q", name, m, want)
		}
	}
}

// J-PAY-5: only UX §3.2 step 4 — auto_max 5 and the provider on the payee
// list, every other limit its default (agent_daily_max 0): a quote of 3 is
// paid automatically, and its sentence says what daily_max leaves today, the
// same in every place. With auto_max 0 the quote waits with the .off
// sentence, which is also the local A2A client's status.message.
func TestJPAY5PaidWithinTheAutomaticLimit(t *testing.T) {
	work := &meteredWork{price: 3}
	_, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 5, ExplicitMax: u64(defaultExplicitMax), DailyMax: u64(defaultDailyMax)},
		prov.AID())
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	if rix := getIX(t, req, id); rix.PayState != interactions.PaySubmitted {
		t.Fatalf("a quote of 3 within auto_max 5: %s / %q", rix.State, rix.PayState)
	}
	n, s := sentenceOf(t, req, id)
	if n.Code != "paid.auto" || n.Amount != 3 || n.Single != 5 || n.Left != defaultDailyMax-3 {
		t.Fatalf("the decision: %+v", n)
	}
	if st := req.SpendStatus(); st.LeftToday != n.Left || st.AgentSpentToday != 0 {
		t.Fatalf("{left} %d, and today %+v", n.Left, st)
	}
	sameEverywhere(t, req, id, prov.AID(), s, false)
	poll(t, prov, req, req)
	if rix := getIX(t, req, id); rix.State != interactions.StateCompleted {
		t.Fatalf("after paying: %s", rix.State)
	}
	sameEverywhere(t, req, id, prov.AID(), s, false) // to the end of the task

	// auto_max 0: the .off sentence, in every place, status.message too.
	payPolicy(t, req, PaymentsConfig{ExplicitMax: u64(defaultExplicitMax), DailyMax: u64(defaultDailyMax)})
	id2, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	n, s = sentenceOf(t, req, id2)
	if n.Code != "needs_operator_approval.off" || n.Tier != a2ashape.PayTierAuto {
		t.Fatalf("auto_max 0: %+v", n)
	}
	sameEverywhere(t, req, id2, prov.AID(), s, true)
	if r := PaymentReason(getIX(t, req, id2)); r != x402a2a.ReasonNeedsOperatorApproval {
		t.Fatalf("anet.reason %q", r)
	}
}

// J-PAY-6: a quote above the price the provider signed is not paid
// automatically (quote_above_card_price, with both numbers); a person's
// `anet pay` pays it. A quote of 30 is above the manual limit as well: the
// sentence names the setting first, and the command it gives makes `anet
// pay` succeed (P-PAY-6 ②).
func TestJPAY6AQuoteAboveTheSignedPrice(t *testing.T) {
	work := &meteredWork{price: 3}
	_, req, prov := paidPair(t, work) // the card signed at 3 is kept
	payPolicy(t, req, PaymentsConfig{AutoMax: 50, DailyMax: u64(100)}, prov.AID())
	work.price = 5 // the provider now quotes 5 for what it signed at 3
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	rix := getIX(t, req, id)
	n, s := sentenceOf(t, req, id)
	if rix.PayState != interactions.PayRequired || n.Code != "quote_above_card_price" || n.Amount != 5 ||
		n.Price == nil || *n.Price != 3 {
		t.Fatalf("a quote of 5 above a signed 3: %q, %+v", rix.PayState, n)
	}
	if chainEvents(t, req, EvPaymentAuthorized) != 0 {
		t.Fatal("signed a quote above the signed price")
	}
	if r := PaymentReason(rix); r != "quote_above_card_price" {
		t.Fatalf("anet.reason %q", r)
	}
	sameEverywhere(t, req, id, prov.AID(), s, true)
	if next := getTaskProjection(t, req, id).Metadata[a2ashape.KeyNext]; next != "anet pay "+id {
		t.Fatalf("anet.next %v", next)
	}
	// The second number `anet pay` shows is the price kept from the card
	// (pay.go signedPriceLine reads it through /agents/card).
	if p := req.signedPriceFor(context.Background(), prov.AID(), "work.do", 5); !p.Have || p.Price != 3 {
		t.Fatalf("the signed price kept: %+v", p)
	}
	// A person pays it (the manual tier: no price check, no payee list).
	token, err := loadOrGenControlToken(req.layout)
	if err != nil {
		t.Fatal(err)
	}
	p := newPlaneFor(t, req, token)
	resp, b := p.req(t, "POST", "/tasks/pay-manual", `{"task_id":"`+id+`","decision":"submit"}`, p.bearer)
	var out PayOutcome
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &out) != nil || out.Status != x402a2a.StatusSubmitted {
		t.Fatalf("anet pay: %d %s", resp.StatusCode, b)
	}
	n, s = sentenceOf(t, req, id)
	if n.Code != "paid.manual" || n.Left != 95 || out.Note != s {
		t.Fatalf("paid by hand: %+v, note %q", n, out.Note)
	}

	// 30: above the manual limit too.
	work.price = 30
	id2, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	n, s = sentenceOf(t, req, id2)
	if n.Code != "needs_operator_approval.manual_limit" || n.Key != "explicit_max" || n.Value == nil || *n.Value != 30 {
		t.Fatalf("a quote of 30: %+v", n)
	}
	sameEverywhere(t, req, id2, prov.AID(), s, true)
	next, _ := getTaskProjection(t, req, id2).Metadata[a2ashape.KeyNext].(string)
	if next != "anet payments set explicit_max=30" {
		t.Fatalf("anet.next %q", next)
	}
	// The command as it says, then anet pay.
	if err := req.SetSpendLimits(map[string]uint64{"explicit_max": 30}); err != nil {
		t.Fatal(err)
	}
	resp, b = p.req(t, "POST", "/tasks/pay-manual", `{"task_id":"`+id2+`","decision":"submit"}`, p.bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anet pay after %s: %d %s", next, resp.StatusCode, b)
	}
}

// J-PAY-15, its last step: a provider whose priced capability is on the
// list variant only; the caller in its list sets auto_max 5 and puts it on
// its payee list, and the call is paid automatically at the price the list
// variant signs.
func TestJPAY15AListSkillIsPaidAutomatically(t *testing.T) {
	srv := newFakeHub(t)
	withRegistry(t, srv.URL)
	d6 := cardLabProvider(t, srv, "Echo", []string{"echo"}, []string{"y"}, map[string]uint64{"y": 2})
	d4 := cardLabRequester(t, srv, "Friend")
	allowPeers(t, d6, d4.AID())
	heldCapFrom(t, d6, d4)
	grantOn(srv.URL, d4.AID(), 50)
	payPolicy(t, d4, PaymentsConfig{AutoMax: 5, DailyMax: u64(defaultDailyMax)}, d6.AID())
	id, err := d4.DelegateCapability(context.Background(), d6.AID(), "y", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, d6)
	// The quote arrives with no card kept: the decision asks d6 for it,
	// off the receive path, while both mailboxes are read.
	waitLong(t, "the automatic payment", func() bool {
		_ = d4.pollOnce(context.Background())
		_ = d6.pollOnce(context.Background())
		n, ok := storedPayNote(getIX(t, d4, id))
		return ok && n.Code == "paid.auto"
	})
	if ev := lastLedgerPayload(t, d4, EvPaymentAuthorized); ev["purpose"] != module.PurposeTaskAuto {
		t.Fatalf("purpose %v", ev["purpose"])
	}
}

// waitLong polls cond for up to 30 seconds: long enough for a price card
// asked for and answered through the relay, or not answered for its 10.
func waitLong(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not happen", why)
}

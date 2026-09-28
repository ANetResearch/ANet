//go:build !no_x402

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// 0017 Q28 (A2A-DESIGN §8.7). A cross-hub quote offers several ledgers,
// its own first; a requester can pay only on the ledger its credit is on.
// This node, the local client's signing service, hands the client the
// options it can pay first, each as quoted; and an option it cannot pay is
// refused before anything is signed — payment-failed with anet.reason
// rail_not_payable and a status message naming the options it can pay —
// rather than signed for a ledger that can only refuse it.
func TestAQuoteIsOrderedByWhatThisNodeCanPayAndAnUnpayableRailIsRefused(t *testing.T) {
	work := &meteredWork{price: 5}
	hub, req, prov := paidPair(t, work)
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req) // quoted; above the automatic tier (0), so it waits
	rix := getIX(t, req, id)
	pr := storedQuote(rix)
	if pr == nil || len(pr.Accepts) != 1 {
		t.Fatalf("stored quote %s", rix.PayRequired)
	}
	home := payment.CreditNetwork(hubAIDOf(hub))
	if pr.Accepts[0].Network != home {
		t.Fatalf("the quote is on %s, want this node's %s", pr.Accepts[0].Network, home)
	}
	// As a provider on another hub quotes: its own ledger first, then the
	// ones its hub clears against, this node's among them.
	foreign := pr.Accepts[0]
	foreign.Network = payment.CreditNetwork("did:anet:other-hub")
	quote, _ := json.Marshal(map[string]any{"x402Version": payment.Version, "resource": pr.Resource,
		"accepts": []payment.PaymentOption{foreign, pr.Accepts[0]}})
	if _, err := req.ix.SetPayment(id, interactions.PayUpdate{Required: quote}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := req.hubIdentity(ctx, hub); err != nil { // known here, as after any hub call
		t.Fatal(err)
	}

	// The quote as the local client is given it: this node's ledger first.
	task, err := req.TaskSeam().Get(ctx, prov.AID(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, where := range []map[string]any{task.Metadata, task.Status.Message.Metadata} {
		q, _ := where[a2ashape.KeyX402Required].(map[string]any)
		acc, _ := q["accepts"].([]any)
		if len(acc) != 2 || acc[0].(map[string]any)["network"] != home ||
			acc[1].(map[string]any)["network"] != foreign.Network {
			t.Fatalf("accepts as handed on: %v, want %s first", q["accepts"], home)
		}
	}
	// The stored quote is the provider's, in its order.
	if got := storedQuote(getIX(t, req, id)); got.Accepts[0].Network != foreign.Network {
		t.Fatalf("the stored quote was reordered: %+v", got.Accepts)
	}

	// Choosing the other hub's option: refused before signing.
	payPolicy(t, req, PaymentsConfig{AgentMax: 10, AgentDailyMax: 10}, prov.AID())
	choice, _ := json.Marshal(foreign)
	_, err = req.PayTask(ctx, PayRequest{TaskID: id, Decision: PayDecisionSubmit, Accept: choice,
		Purpose: module.PurposeTaskAgent})
	var refusal *PayRefusal
	if !errors.As(err, &refusal) || refusal.Outcome.Reason != x402a2a.ReasonRailNotPayable ||
		refusal.Outcome.Status != x402a2a.StatusFailed || refusal.Outcome.Error != x402a2a.CodeSettlementFailed {
		t.Fatalf("an option on another ledger: %v", err)
	}
	if msg := refusal.Outcome.Message; !strings.Contains(msg, home) || !strings.Contains(msg, "Options it can pay") {
		t.Errorf("the refusal does not name the payable option: %q", msg)
	}
	// Through the local A2A interface: the task, with the refusal as its
	// status message.
	task, err = req.TaskSeam().Send(ctx, prov.AID(), module.TaskSend{Message: payMsg(id, map[string]any{
		x402a2a.KeyStatus: x402a2a.StatusSubmitted, x402a2a.KeyAccept: foreign})})
	if err != nil {
		t.Fatal(err)
	}
	m := task.Status.Message
	if !a2ashape.IsPaymentRefusal(task) || m.Metadata[a2ashape.KeyReason] != x402a2a.ReasonRailNotPayable ||
		m.Metadata[a2ashape.KeyX402Status] != a2ashape.PaymentFailed || len(m.Parts) == 0 ||
		!strings.Contains(m.Parts[0].Text, home) {
		t.Fatalf("the refusal as the task: %+v", task.Status)
	}
	if n := chainEvents(t, req, EvPaymentAuthorized); n != 0 {
		t.Fatalf("%d authorizations signed for an option this node cannot pay", n)
	}
	if rix := getIX(t, req, id); rix.PayState != interactions.PayRequired || rix.State != interactions.StateInputRequired {
		t.Fatalf("after the refusal: %s / %s", rix.State, rix.PayState)
	}

	// Its own ledger's option pays, as the client copied it from the
	// ordered list.
	own, _ := json.Marshal(pr.Accepts[0])
	out, err := req.PayTask(ctx, PayRequest{TaskID: id, Decision: PayDecisionSubmit, Accept: own,
		Purpose: module.PurposeTaskAgent})
	if err != nil || out.Status != x402a2a.StatusSubmitted {
		t.Fatalf("the payable option: %+v %v", out, err)
	}
}

// payableFirst moves the options on the home ledger first and changes
// nothing else: each option's bytes, their order within each group, and
// the rest of the quote.
func TestPayableFirstReordersAndChangesNothingElse(t *testing.T) {
	q := []byte(`{"x402Version":2,"resource":{"url":"anet:capability/x"},"accepts":[` +
		`{"scheme":"anet-credit","network":"hub:b","amount":"5","payTo":"p","extra":{"k":1.50}},` +
		`{"scheme":"exact","network":"hub:a","amount":"5","payTo":"p"},` +
		`{"scheme":"anet-credit","network":"hub:a","amount":"5","payTo":"p"},` +
		`{"scheme":"anet-credit","network":"hub:c","amount":"5","payTo":"p"}]}`)
	got := payableFirst(q, "hub:a")
	var out struct {
		Version  int               `json:"x402Version"`
		Resource json.RawMessage   `json:"resource"`
		Accepts  []json.RawMessage `json:"accepts"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"scheme":"anet-credit","network":"hub:a","amount":"5","payTo":"p"}`,
		`{"scheme":"anet-credit","network":"hub:b","amount":"5","payTo":"p","extra":{"k":1.50}}`,
		`{"scheme":"exact","network":"hub:a","amount":"5","payTo":"p"}`,
		`{"scheme":"anet-credit","network":"hub:c","amount":"5","payTo":"p"}`,
	}
	if out.Version != 2 || string(out.Resource) != `{"url":"anet:capability/x"}` || len(out.Accepts) != len(want) {
		t.Fatalf("quote %s", got)
	}
	for i := range want {
		if string(out.Accepts[i]) != want[i] {
			t.Errorf("accepts[%d] = %s, want %s", i, out.Accepts[i], want[i])
		}
	}
	for _, home := range []string{"", "hub:z"} {
		if string(payableFirst(q, home)) != string(q) {
			t.Errorf("home %q: the quote changed", home)
		}
	}
}

// The review of Q28: the check was skipped while this node could not
// learn which ledger its credit is on ("the hub decides"), so a quote
// offering only another hub's ledger — or several, with the provider's
// first — was signed as chosen, with the automatic tier choosing the
// provider's first option. Unknown is not payable: refused before
// anything is signed, saying why, on both paths that pick a rail
// [redteam:Q28].
func TestNothingIsSignedWhileThisNodesLedgerIsUnknown(t *testing.T) {
	work := &meteredWork{price: 5}
	hub, req, prov := paidPair(t, work)
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	pr := storedQuote(getIX(t, req, id))
	if pr == nil || len(pr.Accepts) != 1 {
		t.Fatalf("stored quote %s", getIX(t, req, id).PayRequired)
	}
	foreign := pr.Accepts[0]
	foreign.Network = payment.CreditNetwork("did:anet:other-hub")
	quote, _ := json.Marshal(map[string]any{"x402Version": payment.Version, "resource": pr.Resource,
		"accepts": []payment.PaymentOption{foreign}})
	if _, err := req.ix.SetPayment(id, interactions.PayUpdate{Required: quote}); err != nil {
		t.Fatal(err)
	}
	payPolicy(t, req, PaymentsConfig{AgentMax: 10, AgentDailyMax: 10}, prov.AID())
	// Its hub unreachable, and not known under this address.
	req.mu.Lock()
	req.cfg.HubURL = "http://127.0.0.1:1"
	req.mu.Unlock()
	if p := req.payer(); p == nil || p.HomeNetwork() != "" {
		t.Fatal("precondition: this node's ledger must be unknown")
	}

	_, err = req.PayTask(ctx, PayRequest{TaskID: id, Decision: PayDecisionSubmit, Purpose: module.PurposeTaskAgent})
	var refusal *PayRefusal
	if !errors.As(err, &refusal) || refusal.Outcome.Reason != x402a2a.ReasonRailNotPayable {
		t.Fatalf("paying with this node's ledger unknown: %v", err)
	}
	if msg := refusal.Outcome.Message; !strings.Contains(msg, "could not learn") {
		t.Errorf("the refusal does not say why: %q", msg)
	}
	own := pr.Accepts[0]
	if _, err := req.PayAndRetry(ctx, prov.AID(), "work.do", nil,
		&payment.PaymentRequired{Accepts: []payment.PaymentOption{foreign, own}}); err == nil ||
		!strings.Contains(err.Error(), x402a2a.ReasonRailNotPayable) {
		t.Errorf("PayAndRetry with this node's ledger unknown: %v", err)
	}
	if n := chainEvents(t, req, EvPaymentAuthorized); n != 0 {
		t.Fatalf("%d authorizations signed while this node's ledger was unknown", n)
	}

	// Known again, PayAndRetry does not fall back to an option on another
	// ledger either.
	req.mu.Lock()
	req.cfg.HubURL = hub
	req.mu.Unlock()
	if _, err := req.PayAndRetry(ctx, prov.AID(), "work.do", nil,
		&payment.PaymentRequired{Accepts: []payment.PaymentOption{foreign}}); err == nil ||
		!strings.Contains(err.Error(), x402a2a.ReasonRailNotPayable) {
		t.Errorf("PayAndRetry with only another ledger's option: %v", err)
	}
	if n := chainEvents(t, req, EvPaymentAuthorized); n != 0 {
		t.Fatalf("%d authorizations signed for an option on another ledger", n)
	}
}

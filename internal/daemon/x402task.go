package daemon

// x402task.go is a2a-x402 inside the task (A2A-DESIGN §8.3): the price, the
// payment and its settlement travel as metadata on the same interaction,
// end to end encrypted between the two daemons, and the hub sees only the
// settlement request.
//
// Provider (inbound):
//
//	priced call, no payment   persist the quote (24 h) → status{input-required, payment-required}
//	payment-submitted          dispatched like a capability call (runCapabilityCall), then
//	                           merchant check (§8.4) → pay_state=submitted + auth id + payload
//	                           → POST /x402/settle {paymentPayload, paymentRequirements}
//	    settled                → status{working, payment-verified} → execute → result{payment-completed, receipts}
//	    refused                → status{input-required, or failed once the quote lapsed, payment-failed}
//	    not known              → the same payload again, with backoff, until known; resumed at start
//	payment-rejected           → canceled, status{canceled}
//	quote lapses unpaid        → status{failed, payment-failed, EXPIRED_PAYMENT}
//
// Requester (outbound):
//
//	payment-required           store the quote; pay it within auto_max (task-auto), or wait for
//	                           an operator (task-agent: /tasks/pay; task-manual: `anet pay`)
//	payment-failed             pay_state=failed: a new authorization may be signed
//	receipts, on any status or result, terminal or not
//	                           each verified against the hub's key, the authorizations this
//	                           node signed for the task, the peer as payee and the amount;
//	                           recorded once each as anet.payment.settled
//
// What is persisted, for whoever renders the task (internal/a2ashape):
// pay_state, pay_required (the PaymentRequired exactly as quoted or
// received), pay_auth_ids, pay_payload, pay_receipts (the SettlementResponse
// list, failures included), quote_expires_at, and every payment message and
// status with its metadata in the message table.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// EvPaymentQuoted records a price this node stated for a task.
const EvPaymentQuoted = "anet.payment.quoted"

// quoteLifetime is how long a quote stands (§8.3).
const quoteLifetime = 24 * time.Hour

// Settlement retry backoff for an outcome that is not known yet. Variables
// so tests can shorten them.
var (
	settleRetryBase = 2 * time.Second
	settleRetryMax  = 5 * time.Minute
	// quoteSweepEvery is how often lapsed quotes are failed.
	quoteSweepEvery = time.Minute
)

// paymentCode is the x402.payment.error for a reason, from the payment
// module's table; SETTLEMENT_FAILED in a build without one.
func (d *Daemon) paymentCode(reason string) string {
	if p := d.payer(); p != nil {
		code, _ := p.PaymentError(reason)
		return code
	}
	return x402a2a.CodeSettlementFailed
}

// storedQuote decodes an interaction's pay_required.
func storedQuote(ix *interactions.Interaction) *payment.PaymentRequired {
	if len(ix.PayRequired) == 0 {
		return nil
	}
	var pr payment.PaymentRequired
	if json.Unmarshal(ix.PayRequired, &pr) != nil || len(pr.Accepts) == 0 {
		return nil
	}
	return &pr
}

// storedReceipts decodes an interaction's pay_receipts.
func storedReceipts(ix *interactions.Interaction) []json.RawMessage {
	var out []json.RawMessage
	if len(ix.PayReceipts) > 0 {
		_ = json.Unmarshal(ix.PayReceipts, &out)
	}
	return out
}

// receiptList is pay_receipts plus extra, as metadata carries it.
func receiptList(ix *interactions.Interaction, extra ...json.RawMessage) []json.RawMessage {
	out := storedReceipts(ix)
	out = append(out, extra...)
	if out == nil {
		out = []json.RawMessage{}
	}
	return out
}

// payloadAuth reads the anet-credit authorization inside a PaymentPayload.
func payloadAuth(raw []byte) (*payment.PaymentPayload, *payment.Authorization, string, error) {
	var pp payment.PaymentPayload
	if err := json.Unmarshal(raw, &pp); err != nil {
		return nil, nil, "", err
	}
	enc, _ := pp.Payload["authorization"].(string)
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || enc == "" {
		return &pp, nil, "", fmt.Errorf("no readable authorization")
	}
	auth, err := payment.UnmarshalAuthorization(b)
	if err != nil {
		return &pp, nil, "", err
	}
	id, err := auth.ID()
	if err != nil {
		return &pp, nil, "", err
	}
	return &pp, auth, id, nil
}

// ---------------------------------------------------------------------------
// Provider

// providerStatus sends a status for an inbound task through the retry
// queue, in one transaction with write. With move set, the task moves to
// state and a terminal task is refused; without it the state is left as it
// is (a notice about a task that already ended, such as the canceled after
// a payment-rejected).
func (d *Daemon) providerStatus(ctx context.Context, ixID, peer string, state interactions.State, text string,
	meta map[string]any, move bool, write func(tx *interactions.Tx) error) error {
	mb, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	msgID, err := newMessageID()
	if err != nil {
		return err
	}
	payload, err := (&delegation.StatusMsg{State: string(state), Text: text, Metadata: mb, At: d.nowMS()}).Marshal()
	if err != nil {
		return err
	}
	var seq int64
	id, err := d.queueSend(ctx, peer, seal.TypeStatus, ixID, payload, func(tx *interactions.Tx) error {
		cur, err := tx.Get(ixID)
		if err != nil {
			return err
		}
		if move && cur.IsTerminal() {
			return ErrTaskTerminal
		}
		if write != nil {
			if err := write(tx); err != nil {
				return err
			}
		}
		if seq, _, err = tx.AddMessageRecord(interactions.MessageRecord{InteractionID: ixID, SenderAID: d.AID(),
			Kind: interactions.MsgStatus, Body: text, MsgID: msgID, Metadata: mb}); err != nil {
			return err
		}
		if move {
			_, err = tx.SetState(ixID, state)
		}
		return err
	})
	if err != nil {
		return err
	}
	if state.IsTerminal() && move {
		d.stopRunning(ixID)
	}
	d.publishMessage(ixID, seq, interactions.MsgStatus)
	d.publishState(ixID)
	if err := d.deliverQueued(ctx, id); err != nil {
		log.Printf("anet: %s: payment status queued for delivery (%v)", ixID, err)
	}
	return nil
}

// errAlreadyQuoted aborts a quote write for a task quoted since it was read.
var errAlreadyQuoted = errors.New("anet: the task was already quoted")

// recordQuote persists a quote for a priced capability call. With announce
// set it also sends status{input-required, payment-required} in the same
// transaction; a prepaid delegation (DelegateReq.Payment) records the quote
// silently and checks the payment against it at once.
func (d *Daemon) recordQuote(ctx context.Context, ix *interactions.Interaction, capID string, price uint64,
	announce bool) error {
	p := d.payer()
	if p == nil {
		return errNoPayments()
	}
	pr := p.Quote(capID, price)
	if pr == nil || len(pr.Accepts) == 0 {
		return fmt.Errorf("anet: the payment module quoted no way to pay for %s", capID)
	}
	prJSON, err := json.Marshal(pr)
	if err != nil {
		return err
	}
	expires := time.Now().Add(quoteLifetime).UnixMilli()
	write := func(tx *interactions.Tx) error {
		applied, err := tx.SetPayment(ix.ID, interactions.PayUpdate{From: []string{interactions.PayNone},
			State: interactions.PayState(interactions.PayRequired), Required: prJSON, QuoteExpiresAt: &expires})
		if err != nil {
			return err
		}
		if !applied {
			return errAlreadyQuoted
		}
		return nil
	}
	if announce {
		text := fmt.Sprintf("%s costs %d credits on %s", capID, price, pr.Accepts[0].Network)
		err = d.providerStatus(ctx, ix.ID, ix.PeerAID, interactions.StateInputRequired, text, map[string]any{
			x402a2a.KeyStatus: x402a2a.StatusRequired, x402a2a.KeyRequired: pr,
			x402a2a.KeyQuoteExpiresAt: expires,
		}, true, write)
	} else {
		err = d.ix.Update(write)
	}
	if err != nil {
		return err
	}
	networks := make([]string, 0, len(pr.Accepts))
	for _, o := range pr.Accepts {
		networks = append(networks, o.Network)
	}
	if _, lerr := d.ledger.Append(EvPaymentQuoted, map[string]any{
		"interaction_id": ix.ID, "capability": capID, "amount": pr.Accepts[0].Amount,
		"asset": pr.Accepts[0].Asset, "pay_to": pr.Accepts[0].PayTo, "networks": networks,
		"expires_at": expires,
	}); lerr != nil {
		log.Printf("anet: quote evidence: %v", lerr)
	}
	return nil
}

// quoteCapability answers a priced call that carried no payment. A task
// quoted before (a redelivered delegation) is not quoted again: the first
// status is in the retry queue.
func (d *Daemon) quoteCapability(ctx context.Context, ix *interactions.Interaction, capID string, price uint64) bool {
	if ix.PayState != interactions.PayNone {
		return true
	}
	err := d.recordQuote(ctx, ix, capID, price, true)
	switch {
	case err == nil, errors.Is(err, errAlreadyQuoted), errors.Is(err, ErrTaskTerminal):
	default:
		log.Printf("anet: %s: quote %s: %v", ix.ID, capID, err)
	}
	return true
}

// paymentTerms is what a payment for ix is checked against.
func (d *Daemon) paymentTerms(ix *interactions.Interaction) module.PaymentTerms {
	return module.PaymentTerms{Quoted: storedQuote(ix), Bind: x402a2a.PayBind(ix.ID, ix.TaskNonce), Payer: ix.PeerAID,
		QuoteExpiresAt: ix.QuoteExpiresAt, Now: time.Now().UnixMilli()}
}

// failureReceipt is the SettlementResponse a failed payment adds to the
// task's receipts (a2a-x402 §9.2).
func failureReceipt(reason, network string) json.RawMessage {
	b, _ := json.Marshal(payment.SettlementResponse{Success: false, ErrorReason: reason, Network: network})
	return b
}

// The pay_state values a payment failure may move from. A payment refused
// before it was taken (the merchant check, no room to run it, a lapsed
// quote) leaves an open quote; a settlement the facilitator refused ends a
// submitted one. Neither may touch the other: a refusal of a second
// payment must not end the settlement of the first (§8.3 [C25]).
var (
	payOpen      = []string{interactions.PayRequired, interactions.PayFailed}
	paySubmitted = []string{interactions.PaySubmitted}
)

// errPayStateMoved aborts a payment write whose pay_state changed since
// the caller read it; nothing is written and nothing is sent.
var errPayStateMoved = errors.New("anet: the task's payment state changed")

// paymentFailed records a definite payment failure on an inbound task and
// tells the requester: payment-failed with the code, the reason and every
// receipt so far. The task waits for another payment while its quote
// stands, and fails once it has lapsed. from is the pay_state the failure
// applies to (payOpen or paySubmitted); a task whose pay_state moved on
// since is left alone and told nothing.
func (d *Daemon) paymentFailed(ctx context.Context, ixID string, from []string, reason, code, detail string,
	receipt json.RawMessage) {
	ix, err := d.ix.Get(ixID)
	if err != nil || ix.IsTerminal() {
		return
	}
	state := interactions.StateInputRequired
	if reason == x402a2a.ReasonQuoteExpired || (ix.QuoteExpiresAt > 0 && time.Now().UnixMilli() > ix.QuoteExpiresAt) {
		state = interactions.StateFailed
	}
	if code == "" {
		code = d.paymentCode(reason)
	}
	meta := map[string]any{
		x402a2a.KeyStatus: x402a2a.StatusFailed, x402a2a.KeyError: code,
		x402a2a.KeyReceipts: receiptList(ix, receipt), x402a2a.KeyReason: reason,
	}
	if state == interactions.StateFailed {
		meta["anet.effect_status"] = "UNAVAILABLE"
	} else if len(ix.PayRequired) > 0 {
		// The quote stands: the requester may pay again, and a prepaid
		// call's requester learns the terms here first.
		meta[x402a2a.KeyRequired] = json.RawMessage(ix.PayRequired)
	}
	text := "payment failed: " + code
	if detail != "" {
		text += " (" + detail + ")"
	}
	err = d.providerStatus(ctx, ixID, ix.PeerAID, state, text, meta, true, func(tx *interactions.Tx) error {
		applied, err := tx.SetPayment(ixID, interactions.PayUpdate{From: from,
			State: interactions.PayState(interactions.PayFailed), AddReceipt: receipt})
		if err == nil && !applied {
			return errPayStateMoved
		}
		return err
	})
	switch {
	case err == nil, errors.Is(err, ErrTaskTerminal):
	case errors.Is(err, errPayStateMoved):
		log.Printf("anet: %s: payment refusal (%s) not sent: the task's payment moved on", ixID, reason)
	default:
		log.Printf("anet: %s: payment-failed status: %v", ixID, err)
	}
}

// takePayment handles a payment for an inbound priced call: the merchant
// check, the persisted submission, and the first settlement attempt. It
// reports whether the payment is settled, so the caller executes.
func (d *Daemon) takePayment(ctx context.Context, ix *interactions.Interaction, capID string, price uint64, raw []byte) bool {
	p := d.payer()
	if ix.PayState == interactions.PayNone {
		// A prepaid delegation: the quote is made now and the payment is
		// checked against it like any other.
		if err := d.recordQuote(ctx, ix, capID, price, false); err != nil && !errors.Is(err, errAlreadyQuoted) {
			log.Printf("anet: %s: quote for a prepaid call: %v", ix.ID, err)
			return false
		}
		cur, err := d.ix.Get(ix.ID)
		if err != nil {
			return false
		}
		ix = cur
	}
	_, _, authID, _ := payloadAuth(raw)
	switch ix.PayState {
	case interactions.PayCompleted:
		// The same payment again (a redelivery): it settled already.
		return authID != "" && len(ix.PayAuthIDs) > 0 && ix.PayAuthIDs[0] == authID
	case interactions.PaySubmitted:
		if authID != "" && len(ix.PayAuthIDs) > 0 && ix.PayAuthIDs[0] == authID {
			d.ensureSettling(ix.ID)
		} else {
			// §8.3 [C25]: one payment in flight per task. The payment
			// already submitted decides; this one is not presented.
			d.count("payment-while-submitted")
			log.Printf("anet: %s: a second payment arrived while one is being settled; not presented", ix.ID)
		}
		return false
	case interactions.PayRequired, interactions.PayFailed:
	default:
		return false
	}
	chk := p.CheckPayment(raw, d.paymentTerms(ix))
	if chk.Reason != "" {
		log.Printf("anet: %s: payment refused before settlement: %s (%s)", ix.ID, chk.Reason, chk.Detail)
		d.paymentFailed(ctx, ix.ID, payOpen, chk.Reason, chk.Code, chk.Detail,
			failureReceipt(chk.Reason, chk.Requirements.Network))
		return false
	}
	// Taken under the write lock, and only on a task still open: a cancel
	// or a lapsed quote that ended the task since it was read wins, and
	// the payment is not presented (§4.2 [C34]). Once this commits, the
	// task is not canceled (CancelTask reads pay_state under the same lock).
	var applied bool
	err := d.ix.Update(func(tx *interactions.Tx) error {
		cur, err := tx.Get(ix.ID)
		if err != nil {
			return err
		}
		if cur.IsTerminal() {
			return ErrTaskTerminal
		}
		applied, err = tx.SetPayment(ix.ID, interactions.PayUpdate{From: payOpen,
			State: interactions.PayState(interactions.PaySubmitted), AuthIDs: []string{chk.AuthID}, Payload: raw})
		return err
	})
	if errors.Is(err, ErrTaskTerminal) {
		log.Printf("anet: %s: a payment arrived for a task that ended; not presented", ix.ID)
		return false
	}
	if err != nil {
		log.Printf("anet: %s: record the payment: %v", ix.ID, err)
		return false
	}
	if !applied {
		// Another payment for the task was submitted since it was read.
		d.count("payment-while-submitted")
		return false
	}
	return d.settleTaskPayment(ctx, ix.ID, true)
}

// quotedRequirements is the quoted option a stored payload pays, as it
// goes to the facilitator.
func quotedRequirements(ix *interactions.Interaction) (payment.PaymentRequirements, bool) {
	pr := storedQuote(ix)
	pp, auth, _, err := payloadAuth(ix.PayPayload)
	if pr == nil || err != nil {
		return payment.PaymentRequirements{}, false
	}
	for _, o := range pr.Accepts {
		if o.Scheme == pp.Accepted.Scheme && o.Network == auth.Network {
			return payment.PaymentRequirements{Scheme: o.Scheme, Network: o.Network, Amount: o.Amount,
				Asset: o.Asset, PayTo: o.PayTo, MaxTimeoutSeconds: o.MaxTimeoutSeconds}, true
		}
	}
	return payment.PaymentRequirements{}, false
}

// settleTaskPayment presents the stored payload of a submitted task
// payment. It reports whether it settled. An outcome that is not known
// starts (on the first attempt) the retry loop; a refusal is reported to
// the requester.
func (d *Daemon) settleTaskPayment(ctx context.Context, ixID string, first bool) bool {
	p := d.payer()
	ix, err := d.ix.Get(ixID)
	if err != nil || p == nil || ix.PayState != interactions.PaySubmitted {
		return false
	}
	req, ok := quotedRequirements(ix)
	if !ok {
		d.paymentFailed(ctx, ixID, paySubmitted, payment.ReasonSettlementFailed, "", "the stored payment matches no quoted option",
			failureReceipt(payment.ReasonSettlementFailed, ""))
		return false
	}
	st, err := p.Settle(ctx, ix.PayPayload, req)
	switch {
	case err != nil || st.Pending:
		why := "settlement_pending"
		if err != nil {
			why = err.Error()
		}
		log.Printf("anet: %s: settlement outcome not known yet (%s); presenting the same payment again", ixID, why)
		if first {
			d.ensureSettling(ixID)
		}
		return false
	case st.Failed != "":
		d.paymentFailed(ctx, ixID, paySubmitted, st.Failed, st.Code, "", failedSettlement(st, req.Network))
		return false
	}
	// §8.4: a replayed answer is accepted only for the authorization this
	// task holds. A facilitator that answered with somebody else's
	// settlement has not settled this one.
	if st.Receipt != "" {
		facts, _ := p.VerifyReceipt(st.Receipt, ix.PeerAID)
		if len(ix.PayAuthIDs) == 0 || facts.AuthID != ix.PayAuthIDs[0] {
			d.paymentFailed(ctx, ixID, paySubmitted, payment.ReasonSettlementFailed, "", "the facilitator's receipt is for another payment",
				failureReceipt(payment.ReasonSettlementFailed, req.Network))
			return false
		}
	} else if st.Replayed {
		d.paymentFailed(ctx, ixID, paySubmitted, payment.ReasonSettlementFailed, "", "a replayed settlement without a receipt",
			failureReceipt(payment.ReasonSettlementFailed, req.Network))
		return false
	}
	resp := st.Response
	if resp == nil {
		resp = &payment.SettlementResponse{Success: true, Transaction: st.Transaction, Network: st.Network, Amount: st.Amount}
	}
	rb, _ := json.Marshal(resp)
	cur := ix
	meta := map[string]any{x402a2a.KeyStatus: x402a2a.StatusVerified, x402a2a.KeyReceipts: receiptList(cur, rb)}
	err = d.providerStatus(ctx, ixID, ix.PeerAID, interactions.StateWorking, "payment settled; working", meta, true,
		func(tx *interactions.Tx) error {
			applied, err := tx.SetPayment(ixID, interactions.PayUpdate{From: paySubmitted,
				State: interactions.PayState(interactions.PayCompleted), AddReceipt: rb})
			if err == nil && !applied {
				return errPayStateMoved
			}
			return err
		})
	if errors.Is(err, errPayStateMoved) {
		return false // settled by another attempt, which carries on
	}
	if err != nil && !errors.Is(err, ErrTaskTerminal) {
		// The hub settled and this node could not write it down. The
		// payment stays submitted, and presenting it again is answered
		// with the same receipt.
		log.Printf("anet: %s: record the settlement: %v", ixID, err)
		if first {
			d.ensureSettling(ixID)
		}
		return false
	}
	if errors.Is(err, ErrTaskTerminal) {
		// Paid, but the task ended here meanwhile. Record the money; the
		// payer is shown the receipt with whatever answer it gets.
		if _, perr := d.ix.SetPayment(ixID, interactions.PayUpdate{From: paySubmitted,
			State: interactions.PayState(interactions.PayCompleted), AddReceipt: rb}); perr != nil {
			log.Printf("anet: %s: record the settlement: %v", ixID, perr)
		}
	}
	capID := ix.Goal
	if td, err := decodeTaskDoc(ix.RequestDoc); err == nil {
		if c, _, ok := capabilityCall(td); ok {
			capID = c
		}
	}
	authID := ""
	if len(ix.PayAuthIDs) > 0 {
		authID = ix.PayAuthIDs[0]
	}
	if _, lerr := d.ledger.Append(EvPaymentSettled, map[string]any{
		"interaction_id": ixID, "capability": capID, "transaction": st.Transaction,
		"amount": st.Amount, "network": st.Network, "auth_id": authID, "replayed": st.Replayed,
	}); lerr != nil {
		log.Printf("anet: payment evidence: %v", lerr)
	}
	return err == nil
}

// failedSettlement is a refused settlement as the task's receipts carry
// it: a2a-x402 §9.2 shows transaction "", and an anet hub puts the refused
// authorization's id there, so it moves to extensions["anet.auth_id"].
func failedSettlement(st module.Settlement, network string) json.RawMessage {
	var r payment.SettlementResponse
	if st.Response != nil {
		r = *st.Response
	}
	if r.ErrorReason == "" {
		r.ErrorReason = st.Failed
	}
	if r.Network == "" {
		r.Network = network
	}
	r.Success = false
	if r.Transaction != "" {
		ext := map[string]any{}
		for k, v := range r.Extensions {
			ext[k] = v
		}
		ext[x402a2a.ExtAuthID] = r.Transaction
		r.Extensions, r.Transaction = ext, ""
	}
	b, _ := json.Marshal(r)
	return b
}

// ensureSettling runs the retry loop for a submitted payment, once per task.
func (d *Daemon) ensureSettling(ixID string) {
	if _, busy := d.settling.LoadOrStore(ixID, struct{}{}); busy {
		return
	}
	if !d.goBackground(func() {
		defer d.settling.Delete(ixID)
		d.settleLoop(ixID)
	}) {
		d.settling.Delete(ixID)
	}
}

// settleLoop presents the same payload until the outcome is known
// (§8.3: never a new authorization, never a second input-required), then
// executes the paid call.
func (d *Daemon) settleLoop(ixID string) {
	delay := settleRetryBase
	for {
		t := time.NewTimer(delay)
		select {
		case <-d.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
		settled := d.settleTaskPayment(ctx, ixID, false)
		cancel()
		if settled {
			d.runPaidCall(ixID)
			return
		}
		ix, err := d.ix.Get(ixID)
		if err != nil || ix.PayState != interactions.PaySubmitted {
			return
		}
		if delay *= 2; delay > settleRetryMax {
			delay = settleRetryMax
		}
	}
}

// runPaidCall executes a settled call through the ordinary executor.
func (d *Daemon) runPaidCall(ixID string) {
	ix, err := d.ix.Get(ixID)
	if err != nil {
		return
	}
	td, err := decodeTaskDoc(ix.RequestDoc)
	if err != nil {
		return
	}
	capID, args, ok := capabilityCall(td)
	if !ok {
		return
	}
	d.runCapabilityCall(ixID, capID, args, nil, nil)
}

// paidViewOf is the settlement of a paid task, for the deliverable.
func paidViewOf(ix *interactions.Interaction) *paidView {
	if ix.PayState != interactions.PayCompleted {
		return nil
	}
	list := storedReceipts(ix)
	for i := len(list) - 1; i >= 0; i-- {
		var r payment.SettlementResponse
		if json.Unmarshal(list[i], &r) == nil && r.Success {
			pv := &paidView{Transaction: r.Transaction, Amount: r.Amount, Network: r.Network}
			if enc, ok := r.Extensions[payment.ExtReceipt].(string); ok {
				pv.Receipt = enc
			}
			return pv
		}
	}
	return &paidView{}
}

// paymentResultMeta is the x402 part of a result's metadata: the receipts,
// whenever there are any, and payment-completed once settled (§8.2).
//
// a2a-x402 §7 wants the receipts on the final message of any task that
// took part in the payment flow, so a task that was quoted carries the
// key even when the list is empty.
func paymentResultMeta(ix *interactions.Interaction, meta map[string]any) {
	if ix.PayState == interactions.PayNone {
		return
	}
	meta[x402a2a.KeyReceipts] = receiptList(ix)
	if ix.PayState == interactions.PayCompleted {
		meta[x402a2a.KeyStatus] = x402a2a.StatusCompleted
	}
}

// canceledPaymentMeta is the metadata of a provider's canceled status: the
// receipts, when the task was quoted.
func canceledPaymentMeta(ix *interactions.Interaction) []byte {
	if ix.PayState == interactions.PayNone {
		return nil
	}
	b, _ := json.Marshal(map[string]any{x402a2a.KeyReceipts: receiptList(ix)})
	return b
}

// onRequesterPayment handles a payment message from the requester of an
// inbound task, after it was stored (§8.3, §5.2).
func (d *Daemon) onRequesterPayment(ctx context.Context, ixID string, prior *interactions.Interaction, meta []byte) {
	m := decodeMeta(meta)
	switch m[x402a2a.KeyStatus] {
	case x402a2a.StatusRejected:
		// Canceled here, not in the receive transaction: the notice is
		// sealed before the state is written, while a public caller's keys
		// are still on the row (they go when the task ends).
		if prior.PayState != interactions.PayRequired && prior.PayState != interactions.PayFailed {
			return
		}
		err := d.providerStatus(ctx, ixID, prior.PeerAID, interactions.StateCanceled, "the requester declined to pay",
			map[string]any{x402a2a.KeyStatus: x402a2a.StatusRejected, x402a2a.KeyReceipts: receiptList(prior)},
			true, func(tx *interactions.Tx) error {
				// Only an open quote is declined: a payment taken since
				// the row was read keeps the task (§4.1, §4.2).
				applied, err := tx.SetPayment(ixID, interactions.PayUpdate{From: payOpen,
					State: interactions.PayState(interactions.PayRejected)})
				if err == nil && !applied {
					return errPayStateMoved
				}
				return err
			})
		if err != nil {
			log.Printf("anet: %s: payment-rejected not applied: %v", ixID, err)
		}
	case x402a2a.StatusSubmitted:
		// The payload as the requester sent it.
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(meta, &fields)
		raw := []byte(fields[x402a2a.KeyPayload])
		if string(bytes.TrimSpace(raw)) == "null" {
			raw = nil
		}
		ix, err := d.ix.Get(ixID)
		if err != nil || ix.IsTerminal() {
			return
		}
		if ix.PayState == interactions.PayNone || !ix.IsCapability {
			// Nothing was quoted: there is nothing to pay. The state is
			// left where it is.
			code := d.paymentCode(x402a2a.ReasonNoPendingQuote)
			if err := d.providerStatus(ctx, ixID, ix.PeerAID, ix.State, "no payment is due for this task",
				map[string]any{x402a2a.KeyStatus: x402a2a.StatusFailed, x402a2a.KeyError: code,
					x402a2a.KeyReceipts: receiptList(ix, failureReceipt(x402a2a.ReasonNoPendingQuote, "")),
					x402a2a.KeyReason:   x402a2a.ReasonNoPendingQuote}, true, nil); err != nil && !errors.Is(err, ErrTaskTerminal) {
				log.Printf("anet: %s: payment without a quote: %v", ixID, err)
			}
			return
		}
		if len(raw) == 0 {
			d.paymentFailed(ctx, ixID, payOpen, payment.ReasonMalformed, "", "payment-submitted carried no payload",
				failureReceipt(payment.ReasonMalformed, ""))
			return
		}
		td, err := decodeTaskDoc(ix.RequestDoc)
		if err != nil {
			return
		}
		capID, args, ok := capabilityCall(td)
		if !ok {
			return
		}
		// Dispatched like the call itself (§8.3 [C29]): the same short and
		// long paths and the same bound on long calls. Never auto-reply.
		d.runCapabilityCall(ixID, capID, args, raw, nil)
	}
}

// startPayments rebuilds the spending totals, resumes settlements that
// were in flight when the previous process stopped, and starts the sweep
// that fails lapsed quotes.
func (d *Daemon) startPayments(ctx context.Context) {
	d.loadSpend()
	list, err := d.ix.ListPayState(interactions.RoleInbound, []string{interactions.PaySubmitted}, true)
	if err != nil {
		log.Printf("anet: resume settlements: %v", err)
	}
	for _, ix := range list {
		log.Printf("anet: %s: a payment was being settled when the daemon stopped; presenting it again", ix.ID)
		d.ensureSettling(ix.ID)
	}
	if open, err := d.ix.ListPayState(interactions.RoleInbound, payOpen, true); err == nil {
		for _, ix := range open {
			meta := d.untakenPayment(ix)
			if meta == nil {
				continue
			}
			log.Printf("anet: %s: a payment had arrived and was not taken when the daemon stopped; taking it now", ix.ID)
			ix := ix
			// After the same pause as a resumed settlement, off the start
			// path: taking it goes to the hub.
			d.goBackground(func() {
				t := time.NewTimer(settleRetryBase)
				defer t.Stop()
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				d.onRequesterPayment(ctx, ix.ID, ix, meta)
			})
		}
	}
	d.goBackground(func() {
		t := time.NewTicker(quoteSweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.expireQuotes(ctx)
			}
		}
	})
}

// untakenPayment is the metadata of a payment-submitted that arrived for an
// inbound priced call and was not taken: the message commits with the task
// set working, and the merchant check and the submitted write follow it,
// so a stop in between leaves the task working with its quote still open
// and the message acknowledged, never to come again. It is read back from
// the task's log and handled at start as if it had just arrived; the
// merchant check and the hub's one settlement per binding make that safe
// to repeat. nil for any other task.
func (d *Daemon) untakenPayment(ix *interactions.Interaction) []byte {
	if ix.Role != interactions.RoleInbound || !ix.IsCapability || ix.State != interactions.StateWorking ||
		(ix.PayState != interactions.PayRequired && ix.PayState != interactions.PayFailed) {
		return nil
	}
	msgs, err := d.ix.Messages(ix.ID)
	if err != nil {
		return nil
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Kind != interactions.MsgPayment || m.SenderAID != ix.PeerAID {
			continue
		}
		if decodeMeta([]byte(m.Metadata))[x402a2a.KeyStatus] == x402a2a.StatusSubmitted {
			return []byte(m.Metadata)
		}
		return nil
	}
	return nil
}

// expireQuotes fails every inbound task whose quote lapsed unpaid (§8.3).
func (d *Daemon) expireQuotes(ctx context.Context) {
	list, err := d.ix.ListPayState(interactions.RoleInbound,
		[]string{interactions.PayRequired, interactions.PayFailed}, true)
	if err != nil {
		return
	}
	now := time.Now().UnixMilli()
	for _, ix := range list {
		if ix.QuoteExpiresAt == 0 || now <= ix.QuoteExpiresAt {
			continue
		}
		network := ""
		if pr := storedQuote(ix); pr != nil {
			network = pr.Accepts[0].Network
		}
		sctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
		d.paymentFailed(sctx, ix.ID, payOpen, x402a2a.ReasonQuoteExpired, "", "the quote expired unpaid",
			failureReceipt(x402a2a.ReasonQuoteExpired, network))
		cancel()
	}
}

// ---------------------------------------------------------------------------
// Requester

// carriesPayment reports whether a provider's status metadata has an x402
// part: a payment status, or receipts alone (a canceled notice on a task
// that was quoted carries its receipts and no status).
func carriesPayment(meta []byte) bool {
	m := decodeMeta(meta)
	_, status := m[x402a2a.KeyStatus]
	_, receipts := m[x402a2a.KeyReceipts]
	return status || receipts
}

// onProviderPayment handles the x402 part of a status from the provider of
// an outbound task, after it was stored.
func (d *Daemon) onProviderPayment(ctx context.Context, ixID string, meta []byte) {
	m := decodeMeta(meta)
	status, _ := m[x402a2a.KeyStatus].(string)
	// The quote, whenever a status carries one: a payment-required, or a
	// payment-failed that leaves the quote standing. Kept as the provider
	// wrote it: a local client copies an option out of it and the choice
	// is compared with what was stored (§8.7).
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(meta, &fields)
	var quote []byte
	if raw := []byte(fields[x402a2a.KeyRequired]); len(raw) > 0 {
		var pr payment.PaymentRequired
		if json.Unmarshal(raw, &pr) == nil && len(pr.Accepts) > 0 {
			quote = raw
		} else {
			log.Printf("anet: %s: the provider asked to be paid and named no way to pay", ixID)
		}
	}
	expires := time.Now().Add(quoteLifetime).UnixMilli()
	if v, ok := m[x402a2a.KeyQuoteExpiresAt].(float64); ok && int64(v) > time.Now().UnixMilli() && int64(v) < expires {
		// The provider's own expiry, when it states one, and never later
		// than 24 hours from now.
		expires = int64(v)
	}
	switch status {
	case x402a2a.StatusRequired:
		if quote == nil {
			break
		}
		prior, err := d.ix.Get(ixID)
		if err != nil {
			break
		}
		// A quote again after a payment was submitted means the provider
		// did not take it; the same authorization may be sent again (§8.3).
		if _, err := d.ix.SetPayment(ixID, interactions.PayUpdate{
			From: []string{interactions.PayNone, interactions.PayRequired, interactions.PayFailed,
				interactions.PaySubmitted},
			State: interactions.PayState(interactions.PayRequired), Required: quote, QuoteExpiresAt: &expires}); err != nil {
			log.Printf("anet: %s: store the quote: %v", ixID, err)
			break
		}
		d.publishState(ixID)
		d.notePaymentReceipts(ixID, m, false)
		// Paid automatically once per quote: on the first one, or again
		// with the same authorization after a re-quote. After a definite
		// failure a person or an agent decides, so a provider cannot
		// drain the automatic tier by quoting again and again.
		if prior.PayState == interactions.PayNone || prior.PayState == interactions.PaySubmitted {
			d.autoPay(ctx, ixID, prior.PayState == interactions.PaySubmitted)
		}
		return
	case x402a2a.StatusFailed:
		upd := interactions.PayUpdate{From: []string{interactions.PaySubmitted, interactions.PayRequired, interactions.PayFailed},
			State: interactions.PayState(interactions.PayFailed)}
		if quote != nil {
			upd.Required, upd.QuoteExpiresAt = quote, &expires
		}
		if _, err := d.ix.SetPayment(ixID, upd); err != nil {
			log.Printf("anet: %s: record the failed payment: %v", ixID, err)
		}
		d.publishState(ixID)
	}
	d.notePaymentReceipts(ixID, m, false)
}

// autoPay pays a stored quote within the auto tier, or leaves the task for
// an operator. With resend set (a quote again after a payment was
// submitted) it only sends the same authorization again: the one already
// sent has no definite outcome, and a new one is not signed until it has
// (§8.3 [C13][C25]).
func (d *Daemon) autoPay(ctx context.Context, ixID string, resend bool) {
	if d.payer() == nil {
		return
	}
	_, err := d.PayTask(ctx, PayRequest{TaskID: ixID, Decision: PayDecisionSubmit, Purpose: module.PurposeTaskAuto,
		resendOnly: resend})
	if err == nil {
		return
	}
	if r, ok := isSpendRefusal(err); ok {
		log.Printf("anet: %s: the quote needs an operator's decision (%s); pay it with `anet pay %s`", ixID, r.Code, ixID)
		return
	}
	log.Printf("anet: %s: not paid automatically: %v", ixID, err)
}

// PaymentReason is the anet.reason of an outbound task that waits on a
// payment decision: needs_operator_approval while a quote stands unpaid
// and the task is input-required, "" otherwise.
func PaymentReason(ix *interactions.Interaction) string {
	if ix.Role == interactions.RoleOutbound && ix.State == interactions.StateInputRequired &&
		(ix.PayState == interactions.PayRequired || ix.PayState == interactions.PayFailed) {
		return x402a2a.ReasonNeedsOperatorApproval
	}
	return ""
}

// PaymentStatusMeta is the x402 part of a task's current status, derived
// from the stored payment columns, for whoever renders the task as A2A
// (internal/a2ashape, the control plane, module/a2a): x402.payment.status,
// x402.payment.required while a quote waits, x402.payment.receipts once
// the task took part in the flow, x402.payment.error after a failure, and
// anet.reason / anet.cancel_requested on the requester's side. Empty for a
// task nothing was quoted for.
func (d *Daemon) PaymentStatusMeta(ix *interactions.Interaction) map[string]any {
	out := map[string]any{}
	status := ""
	switch ix.PayState {
	case interactions.PayNone:
		return out
	case interactions.PayRequired:
		status = x402a2a.StatusRequired
	case interactions.PaySubmitted:
		status = x402a2a.StatusSubmitted
	case interactions.PayCompleted:
		status = x402a2a.StatusCompleted
	case interactions.PayFailed:
		status = x402a2a.StatusFailed
	case interactions.PayRejected:
		status = x402a2a.StatusRejected
	}
	if ix.IsTerminal() && ix.PayState == interactions.PayRequired {
		// Ended with a quote nobody paid: nothing is due any more.
		status = ""
	}
	if status != "" {
		out[x402a2a.KeyStatus] = status
	}
	out[x402a2a.KeyReceipts] = receiptList(ix)
	if (ix.PayState == interactions.PayRequired || ix.PayState == interactions.PayFailed) && !ix.IsTerminal() &&
		len(ix.PayRequired) > 0 {
		out[x402a2a.KeyRequired] = json.RawMessage(ix.PayRequired)
		out[x402a2a.KeyQuoteExpiresAt] = ix.QuoteExpiresAt
	}
	if ix.PayState == interactions.PayFailed {
		list := storedReceipts(ix)
		for i := len(list) - 1; i >= 0; i-- {
			var r payment.SettlementResponse
			if json.Unmarshal(list[i], &r) == nil && !r.Success && r.ErrorReason != "" {
				out[x402a2a.KeyError] = d.paymentCode(r.ErrorReason)
				out[x402a2a.KeyReason] = r.ErrorReason
				break
			}
		}
	}
	if reason := PaymentReason(ix); reason != "" {
		out[x402a2a.KeyReason] = reason
	}
	if ix.Role == interactions.RoleOutbound && !ix.IsTerminal() &&
		(ix.PayState == interactions.PaySubmitted || ix.PayState == interactions.PayCompleted) {
		if msgs, err := d.ix.Messages(ix.ID); err == nil {
			for _, m := range msgs {
				if m.Kind == interactions.MsgCancel && m.SenderAID == d.AID() {
					out[x402a2a.KeyCancelRequested] = true
				}
			}
		}
	}
	return out
}

// Pay decisions.
const (
	PayDecisionSubmit = "submit"
	PayDecisionReject = "reject"
)

// PayRequest is a decision on the quote of a task this node started.
type PayRequest struct {
	TaskID   string
	Decision string // submit | reject
	// Accept is the chosen option, copied from x402.payment.required.accepts;
	// it may be left out when the quote offers one option.
	Accept json.RawMessage
	// Payload is a client's own PaymentPayload. This node signs payments
	// itself, so it is refused (§8.7).
	Payload json.RawMessage
	// Purpose is the spending tier, set by the route (§8.6).
	Purpose string
	// ClientMsgID is a local A2A client's own messageId for the payment
	// message, if it gave one. It is kept on the stored payment message
	// (a2a.messageId, never sent to the peer), so the same message sent
	// again finds this one instead of deciding twice (§11.5).
	ClientMsgID string
	// resendOnly allows only the authorization sent last, on the same
	// terms; set for the automatic answer to a quote repeated after a
	// payment was submitted.
	resendOnly bool
}

// PayOutcome is what a decision did.
type PayOutcome struct {
	TaskID   string `json:"task_id"`
	State    string `json:"state"`
	PayState string `json:"pay_state"`
	Status   string `json:"x402.payment.status"`
	Error    string `json:"x402.payment.error,omitempty"`
	Reason   string `json:"anet.reason,omitempty"`
	AuthID   string `json:"authorization_id,omitempty"`
	// Reused is set when the authorization sent is one signed earlier for
	// the same terms (§8.3: a re-quote on the same terms).
	Reused bool `json:"reused,omitempty"`
	// SpendRefusal and Message are set on a decision held for the operator
	// (PayHold): the spending policy's code, and what happens now, for a
	// person or an agent to read.
	SpendRefusal string `json:"spend_refusal,omitempty"`
	Message      string `json:"message,omitempty"`
}

// Errors of PayTask.
var (
	ErrNoQuote        = errors.New("anet: the task has no quote waiting for payment")
	ErrPaymentPending = errors.New("anet: a payment for this task is submitted and has no outcome yet")
	ErrQuoteExpired   = errors.New("anet: the quote has expired; it cannot be paid")
	ErrNotRequester   = errors.New("anet: only the requester of a task pays for it")
	ErrPayeeNotPeer   = errors.New("anet: the quoted payee is not the task's provider")
)

// errNoResend refuses an automatic payment of a repeated quote whose terms
// are not those of the authorization sent last: a new authorization while
// the first has no outcome is an operator's decision (§8.3).
var errNoResend = errors.New("anet: the quote changed while a payment was outstanding; a new authorization needs a decision")

// PayRefusal is a decision refused with an a2a-x402 outcome (§8.7): the
// caller is told payment-failed with Code and Reason, and nothing was
// signed or sent.
type PayRefusal struct {
	Outcome PayOutcome
}

func (e *PayRefusal) Error() string {
	return fmt.Sprintf("anet: payment refused: %s (%s)", e.Outcome.Error, e.Outcome.Reason)
}

// PayHold is an agent-tier decision to pay that the spending policy did
// not allow (§8.3, §8.6): above agent_max or agent_daily_max, or to a
// payee not on the list. Nothing was signed or sent, and the task goes on
// waiting, input-required with anet.reason needs_operator_approval, for
// the operator's `anet pay` on a terminal. It is the answer, not a
// failure: /tasks/pay returns Outcome with 200 and the local A2A interface
// returns the task (a2ashape.PaymentHold). Unwrap gives the refusal.
type PayHold struct {
	Outcome PayOutcome
	Refusal *SpendRefusal
}

func (e *PayHold) Error() string {
	return fmt.Sprintf("anet: payment held for the operator: %v", e.Refusal)
}

func (e *PayHold) Unwrap() error { return e.Refusal }

// holdForOperator is the PayHold for a refused agent-tier payment of opt
// on ix.
func (d *Daemon) holdForOperator(ix *interactions.Interaction, opt payment.PaymentOption, r *SpendRefusal) *PayHold {
	out := PayOutcome{TaskID: ix.ID, State: string(ix.State), PayState: ix.PayState,
		Reason: x402a2a.ReasonNeedsOperatorApproval, SpendRefusal: r.Code}
	out.Status, _ = d.PaymentStatusMeta(ix)[x402a2a.KeyStatus].(string)
	if out.Status == "" {
		out.Status = x402a2a.StatusRequired
	}
	out.Message = operatorApprovalText(ix.ID, opt, r)
	return &PayHold{Outcome: out, Refusal: r}
}

// operatorApprovalText says why an agent's payment was not made and who
// can make it. The amount and the payee are the quote's, as stored; the
// provider wrote them, so they are made plain before they become text a
// model reads (the payee was already checked to be the task's peer).
func operatorApprovalText(ixID string, opt payment.PaymentOption, r *SpendRefusal) string {
	return fmt.Sprintf("Payment not submitted: %s. The quote (%s %s to %s) is still open: the operator of this "+
		"node has to approve it by running `anet pay %s` in a terminal. Nothing was signed or sent.",
		r.Detail, plainText(opt.Amount, 40), plainText(opt.Asset, 40), plainText(opt.PayTo, 256), ixID)
}

// plainText is peer-written s without control or bidirectional
// formatting characters, cut to max bytes.
func plainText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) || isBidiFormat(r) {
			return -1
		}
		return r
	}, s)
	return truncateUTF8(s, max)
}

// PayTask carries out a decision on a task's quote: submit signs (or, on
// the same terms, reuses) an authorization bound to the task and sends
// payment-submitted; reject sends payment-rejected and cancels the task
// here. The purpose decides the spending tier; every signature passes the
// spending policy (§8.6). The control plane (/tasks/pay, /tasks/pay-manual)
// and the local A2A interface call this.
func (d *Daemon) PayTask(ctx context.Context, req PayRequest) (PayOutcome, error) {
	unlock := d.outboxLocks.lock("pay:" + req.TaskID)
	defer unlock()
	ix, err := d.ix.Get(req.TaskID)
	if err != nil {
		return PayOutcome{}, err
	}
	out := PayOutcome{TaskID: ix.ID}
	refuse := func(reason string) (PayOutcome, error) {
		out.State, out.PayState = string(ix.State), ix.PayState
		out.Status, out.Error, out.Reason = x402a2a.StatusFailed, d.paymentCode(reason), reason
		return out, &PayRefusal{Outcome: out}
	}
	if ix.Role != interactions.RoleOutbound {
		return out, ErrNotRequester
	}
	if ix.IsTerminal() {
		return out, fmt.Errorf("%w (%s is %s)", ErrTaskTerminal, ix.ID, ix.State)
	}
	switch ix.PayState {
	case interactions.PayRequired, interactions.PayFailed:
	case interactions.PaySubmitted:
		return out, ErrPaymentPending
	default:
		return out, ErrNoQuote
	}
	if len(bytes.TrimSpace(req.Payload)) > 0 && string(bytes.TrimSpace(req.Payload)) != "null" {
		return refuse(x402a2a.ReasonClientPayloadUnsupported)
	}
	switch req.Decision {
	case PayDecisionReject:
		return d.rejectQuote(ctx, ix, req.ClientMsgID)
	case PayDecisionSubmit:
	default:
		return out, fmt.Errorf("anet: decision must be %q or %q", PayDecisionSubmit, PayDecisionReject)
	}
	if ix.QuoteExpiresAt > 0 && time.Now().UnixMilli() > ix.QuoteExpiresAt {
		return out, ErrQuoteExpired
	}
	p := d.payer()
	if p == nil {
		return out, errNoPayments()
	}
	pr := storedQuote(ix)
	if pr == nil {
		return out, ErrNoQuote
	}
	var opt *payment.PaymentOption
	if len(bytes.TrimSpace(req.Accept)) > 0 && string(bytes.TrimSpace(req.Accept)) != "null" {
		i, ok := offeredOption(ix.PayRequired, req.Accept)
		if !ok {
			return refuse(x402a2a.ReasonOptionNotOffered)
		}
		opt = &pr.Accepts[i]
	} else if len(pr.Accepts) == 1 {
		opt = &pr.Accepts[0]
	} else {
		opt = pickRail(p, pr.Accepts)
	}
	if opt == nil || opt.Scheme != payment.SchemeCredit {
		return refuse(x402a2a.ReasonOptionNotOffered)
	}
	if opt.PayTo != ix.PeerAID {
		// A receipt is checked against the peer as payee; a payment to
		// anyone else could never be shown to have paid for this task.
		return out, ErrPayeeNotPeer
	}
	raw, authID, reused := d.reusablePayment(ix, *opt)
	if raw == nil && req.resendOnly {
		return out, errNoResend
	}
	if raw == nil {
		signed, err := p.Authorize(*opt, ix.ID, x402a2a.PayBind(ix.ID, ix.TaskNonce), req.Purpose)
		if err != nil {
			if r, ok := isSpendRefusal(err); ok {
				logSpendRefusal(ix.ID, r)
				if req.Purpose == module.PurposeTaskAgent {
					// Above the agent tier the decision is the operator's
					// (§8.3): the task waits for it, told why.
					h := d.holdForOperator(ix, *opt, r)
					return h.Outcome, h
				}
				return out, r
			}
			return out, err
		}
		raw = signed
		if _, _, authID, err = payloadAuth(raw); err != nil {
			return out, err
		}
	}
	meta, err := json.Marshal(map[string]any{x402a2a.KeyStatus: x402a2a.StatusSubmitted,
		x402a2a.KeyPayload: json.RawMessage(raw)})
	if err != nil {
		return out, err
	}
	if err := d.requesterPaymentMessage(ctx, ix, meta, req.ClientMsgID, interactions.StateWorking, func(tx *interactions.Tx) error {
		cur, err := tx.Get(ix.ID)
		if err != nil {
			return err
		}
		if cur.PayState == interactions.PaySubmitted {
			return ErrPaymentPending
		}
		applied, err := tx.SetPayment(ix.ID, interactions.PayUpdate{
			From:  []string{interactions.PayRequired, interactions.PayFailed},
			State: interactions.PayState(interactions.PaySubmitted), AddAuthID: authID, Payload: raw})
		if err == nil && !applied {
			return ErrNoQuote
		}
		return err
	}); err != nil {
		return out, err
	}
	cur, _ := d.ix.Get(ix.ID)
	if cur != nil {
		out.State, out.PayState = string(cur.State), cur.PayState
	}
	out.Status, out.AuthID, out.Reused = x402a2a.StatusSubmitted, authID, reused
	return out, nil
}

// rejectQuote declines a task's quote: payment-rejected to the provider,
// the task canceled here (§8.3).
func (d *Daemon) rejectQuote(ctx context.Context, ix *interactions.Interaction, clientMsgID string) (PayOutcome, error) {
	meta, _ := json.Marshal(map[string]any{x402a2a.KeyStatus: x402a2a.StatusRejected})
	err := d.requesterPaymentMessage(ctx, ix, meta, clientMsgID, interactions.StateCanceled, func(tx *interactions.Tx) error {
		applied, err := tx.SetPayment(ix.ID, interactions.PayUpdate{
			From:  []string{interactions.PayRequired, interactions.PayFailed},
			State: interactions.PayState(interactions.PayRejected)})
		if err == nil && !applied {
			return ErrPaymentPending
		}
		return err
	})
	if err != nil {
		return PayOutcome{TaskID: ix.ID}, err
	}
	out := PayOutcome{TaskID: ix.ID, Status: x402a2a.StatusRejected}
	if cur, err := d.ix.Get(ix.ID); err == nil {
		out.State, out.PayState = string(cur.State), cur.PayState
	}
	return out, nil
}

// requesterPaymentMessage sends a payment message for an outbound task
// through the retry queue, storing it (metadata only, kind payment) and
// moving the task to state in the same transaction as write. A local
// client's messageId, when given, is added to the stored copy only.
func (d *Daemon) requesterPaymentMessage(ctx context.Context, ix *interactions.Interaction, meta []byte,
	clientMsgID string, state interactions.State, write func(tx *interactions.Tx) error) error {
	msgID, err := newMessageID()
	if err != nil {
		return err
	}
	stored := meta
	if clientMsgID != "" {
		m := decodeMeta(meta)
		m[a2ashape.KeyMessageID] = clientMsgID
		if stored, err = json.Marshal(m); err != nil {
			return err
		}
	}
	payload, err := (&delegation.ChatMsg{Kind: delegation.ChatText, MsgID: msgID, Metadata: meta}).Marshal()
	if err != nil {
		return err
	}
	var seq int64
	id, err := d.queueSend(ctx, ix.PeerAID, seal.TypeMessage, ix.ID, payload, func(tx *interactions.Tx) error {
		cur, err := tx.Get(ix.ID)
		if err != nil {
			return err
		}
		if cur.IsTerminal() {
			return fmt.Errorf("%w (%s is %s)", ErrTaskTerminal, ix.ID, cur.State)
		}
		if err := write(tx); err != nil {
			return err
		}
		if seq, _, err = tx.AddMessageRecord(interactions.MessageRecord{InteractionID: ix.ID, SenderAID: d.AID(),
			Kind: interactions.MsgPayment, MsgID: msgID, Metadata: stored}); err != nil {
			return err
		}
		_, err = tx.SetState(ix.ID, state)
		return err
	})
	if err != nil {
		return err
	}
	d.publishMessage(ix.ID, seq, interactions.MsgPayment)
	d.publishState(ix.ID)
	if err := d.deliverQueued(ctx, id); err != nil {
		log.Printf("anet: %s: payment message queued for delivery (%v)", ix.ID, err)
	}
	return nil
}

// reusablePayment returns the authorization this node last sent for the
// task when the provider quotes the same terms again and it has not
// expired (§8.3: the hub settles an authorization at most once, so sending
// it again cannot pay twice). After a payment-failed a new one is signed.
func (d *Daemon) reusablePayment(ix *interactions.Interaction, opt payment.PaymentOption) ([]byte, string, bool) {
	if ix.PayState != interactions.PayRequired || len(ix.PayPayload) == 0 {
		return nil, "", false
	}
	pp, auth, id, err := payloadAuth(ix.PayPayload)
	if err != nil {
		return nil, "", false
	}
	want := opt
	want.Extra = nil
	got := pp.Accepted
	got.Extra = nil
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	if !bytes.Equal(a, b) || auth.InteractionID != x402a2a.PayBind(ix.ID, ix.TaskNonce) {
		return nil, "", false
	}
	// Enough of the window left for the provider to present it.
	if time.Now().Add(30*time.Second).UnixMilli() > auth.NotAfter {
		return nil, "", false
	}
	return ix.PayPayload, id, true
}

// offeredOption finds the accepts entry of a stored PaymentRequired that
// a client's chosen option equals, comparing canonical JSON (keys sorted,
// numbers as written): the option must be the one quoted, field for field,
// with nothing added (§8.7).
func offeredOption(storedRequired, accept []byte) (int, bool) {
	var stored struct {
		Accepts []json.RawMessage `json:"accepts"`
	}
	if json.Unmarshal(storedRequired, &stored) != nil {
		return 0, false
	}
	want, ok := canonicalJSON(accept)
	if !ok {
		return 0, false
	}
	for i, a := range stored.Accepts {
		if got, ok := canonicalJSON(a); ok && bytes.Equal(got, want) {
			return i, true
		}
	}
	return 0, false
}

// canonicalJSON re-encodes a JSON value with object keys sorted and
// numbers kept as written.
func canonicalJSON(b []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	out, err := json.Marshal(v)
	return out, err == nil
}

// notePaymentReceipts verifies and records the receipts a provider sent
// on a status or a result (§8.3; §4.2: also when the task has ended here).
//
// The list is the provider's whole history for the task (§8.2), failures
// included, and is stored as pay_receipts for whoever renders the task.
// Each successful receipt is checked: signed by this node's hub for a
// payment by this node, for one of the authorizations this node signed for
// the task, paying the task's provider, for that authorization's amount.
// It is recorded once (by transaction) as anet.payment.settled, verified or
// not; a second verified settlement for one task is recorded and marked.
func (d *Daemon) notePaymentReceipts(ixID string, m map[string]any, afterTerminal bool) {
	list, ok := m[x402a2a.KeyReceipts].([]any)
	if !ok || len(list) == 0 {
		return
	}
	ix, err := d.ix.Get(ixID)
	if err != nil || ix.Role != interactions.RoleOutbound {
		return
	}
	if all, err := json.Marshal(list); err == nil && len(list) >= len(storedReceipts(ix)) {
		if _, err := d.ix.SetPayment(ixID, interactions.PayUpdate{Receipts: all}); err != nil {
			log.Printf("anet: %s: store the receipts: %v", ixID, err)
		}
	}
	// What this node already recorded for the task.
	recorded, verifiedBefore := map[string]bool{}, 0
	d.ledger.scan(EvPaymentSettled, 0, func(_ int64, p map[string]any) {
		if p["interaction_id"] != ixID {
			return
		}
		if tx, _ := p["transaction"].(string); tx != "" {
			recorded[tx] = true
		}
		if p["verified"] == true {
			verifiedBefore++
		}
	})
	p := d.payer()
	for _, item := range list {
		rb, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var sr payment.SettlementResponse
		if json.Unmarshal(rb, &sr) != nil || !sr.Success || sr.Transaction == "" || recorded[sr.Transaction] {
			continue
		}
		recorded[sr.Transaction] = true
		entry := map[string]any{"interaction_id": ixID, "transaction": sr.Transaction,
			"amount": sr.Amount, "network": sr.Network}
		if afterTerminal || ix.IsTerminal() {
			entry["after_terminal"] = true
		}
		verified := false
		enc, _ := sr.Extensions[payment.ExtReceipt].(string)
		switch {
		case p == nil:
			entry["refused"] = "this build has no payment module to check the receipt with"
		case enc == "":
			entry["refused"] = "the settlement carries no hub receipt"
		default:
			facts, sigOK := p.VerifyReceipt(enc, d.AID())
			entry["payee"], entry["auth_id"], entry["receipt"] = facts.Payee, facts.AuthID, enc
			ours := false
			for _, a := range ix.PayAuthIDs {
				ours = ours || a == facts.AuthID
			}
			amount, known := d.authorizedAmount(ix, facts.AuthID)
			switch {
			case !sigOK:
				entry["refused"] = "receipt signature or payer does not check out"
			case !ours:
				entry["refused"] = "receipt is for an authorization this node did not sign for the task"
			case facts.Payee != ix.PeerAID:
				entry["refused"] = "receipt pays someone other than the task's provider"
			case !known || amount != facts.Amount:
				entry["refused"] = "receipt amount differs from the authorization"
			default:
				verified = true
				entry["amount"] = payment.Amount(facts.Amount)
			}
		}
		entry["verified"] = verified
		if verified {
			if verifiedBefore > 0 {
				// §8.3 [m]: a second settlement for one task. Recorded, and
				// marked for audit to show.
				entry["second_receipt"] = true
			}
			verifiedBefore++
			if _, err := d.ix.SetPayment(ixID, interactions.PayUpdate{
				State: interactions.PayState(interactions.PayCompleted)}); err != nil {
				log.Printf("anet: %s: record the settlement: %v", ixID, err)
			}
		} else {
			log.Printf("anet: %s: settlement receipt did not check out: %v", ixID, entry["refused"])
		}
		if _, lerr := d.ledger.Append(EvPaymentSettled, entry); lerr != nil {
			log.Printf("anet: settlement evidence: %v", lerr)
		}
	}
}

// authorizedAmount is the amount of an authorization this node signed for
// the task: the stored payload's when it is that one, else from this
// node's own chain.
func (d *Daemon) authorizedAmount(ix *interactions.Interaction, authID string) (uint64, bool) {
	if _, auth, id, err := payloadAuth(ix.PayPayload); err == nil && id == authID {
		return auth.Amount, true
	}
	found, amount := false, uint64(0)
	d.ledger.scan(EvPaymentAuthorized, 0, func(_ int64, p map[string]any) {
		if p["authorization_id"] == authID && p["interaction_id"] == ix.ID {
			if n, err := payment.ParseAmount(fmt.Sprint(p["amount"])); err == nil {
				found, amount = true, n
			}
		}
	})
	return amount, found
}

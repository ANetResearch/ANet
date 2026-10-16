package daemon

// x402task.go is a2a-x402 inside the task (A2A-DESIGN §8.3): the price, the
// payment and its settlement travel as metadata on the same interaction,
// end to end encrypted between the two daemons, and the hub sees only the
// settlement request.
//
// Provider (inbound):
//
//	priced call, no payment   persist the quote (24 h) → status{input-required, payment-required}
//	payment-submitted          dispatched like a capability call (runPaidCapabilityCall), then
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
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/jsonread"
	"github.com/ANetResearch/ANet/internal/pathsafe"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// EvPaymentQuoted records a price this node stated for a task.
const EvPaymentQuoted = "anet.payment.quoted"

// quoteLifetime is how long a quote stands (§8.3).
const quoteLifetime = 24 * time.Hour

// Settlement retry backoff for an outcome that is not known yet. Variables
// so tests can shorten them; the base is a knob (testhooks.go) because a
// test shortens it while an earlier daemon's loop may still read it.
var (
	settleRetryBase = newKnob(2 * time.Second)
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

// receiptList is pay_receipts plus extra, as metadata carries it: never
// null, and every failure with transaction "" (x402receipts.go). On a task
// this node started, only the settlements it verified are in it
// (a2ashape.SplitReceipts); the rest are PaymentStatusMeta's
// anet.unverified_receipts [redteam:F11].
func receiptList(ix *interactions.Interaction, extra ...json.RawMessage) []json.RawMessage {
	out := storedReceipts(ix)
	out = append(out, extra...)
	own, _ := a2ashape.SplitReceipts(normalizeReceipts(out), ix.Role == interactions.RoleOutbound, ix.PayState)
	if own == nil {
		own = []json.RawMessage{}
	}
	return own
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
	mid, msgID := newWireMID()
	payload, err := (&delegation.StatusMsg{State: string(state), Text: text, Metadata: mb, At: d.nowMS()}).Marshal()
	if err != nil {
		return err
	}
	var seq int64
	id, err := d.queueSendAs(ctx, wireSend{to: peer, typ: seal.TypeStatus, ix: ixID, body: payload, mid: mid}, func(tx *interactions.Tx) error {
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
			_, err = tx.SetState(ixID, state, interactions.CauseProviderEnd.When(state))
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
		return d.noPayments()
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

// paymentTerms is what a payment for ix is checked against. A task whose id
// or nonce holds U+0000 has no binding (core CORE-E9; the decoders refuse
// such fields first): Bind stays empty and CheckPayment refuses every
// payment for it as binding_mismatch.
func (d *Daemon) paymentTerms(ix *interactions.Interaction) module.PaymentTerms {
	bind, _ := x402a2a.PayBind(ix.ID, ix.TaskNonce)
	return module.PaymentTerms{Quoted: storedQuote(ix), Bind: bind, Payer: ix.PeerAID,
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
		facts, signed := p.VerifyReceipt(st.Receipt, ix.PeerAID)
		quoted, _ := strconv.ParseUint(req.Amount, 10, 64)
		// Signed by the hub, for this task's authorization, to this node,
		// for at least the quote and on the network quoted: a receipt that
		// names the right authorization but pays someone else or less is
		// not this task's payment.
		if !signed || len(ix.PayAuthIDs) == 0 || facts.AuthID != ix.PayAuthIDs[0] ||
			facts.Payee != req.PayTo || facts.Amount < quoted || facts.Network != req.Network {
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
	delay := settleRetryBase.get()
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
	d.runCapabilityCall(ixID, capID, args, nil)
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
		d.runPaidCapabilityCall(ixID, capID, args, raw)
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
				t := time.NewTimer(settleRetryBase.get())
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

// providerPayment is the x402 part of a status from the provider of an
// outbound task (§8.3), in three steps ([redteam:F28]): planProviderPayment
// reads it, applyTx writes the pay columns inside the status's step-10
// transaction (with the status row, its state and its replay row), and
// afterProviderPayment does what follows the commit — announcing the
// state, recording settlement evidence, a pending cancel, the automatic
// payment. Written after the commit, a storage error on the quote was only
// logged: the status was acknowledged, its redelivery a duplicate, and the
// task sat in input-required with no quote to pay.
type providerPayment struct {
	status   string
	meta     map[string]any
	quote    []byte
	expires  int64
	receipts *receiptPlan
	// failNote is the snapshot a payment-failed leaves on the task (HPP
	// PAY-U5, ux D3); nil for any other status.
	failNote []byte

	// Filled by applyTx.
	prior    string // pay_state before the status
	quoted   bool   // the quote was stored
	failWrit bool   // the failed payment was recorded
}

// planProviderPayment reads the x402 part of a provider's status metadata.
// It writes nothing.
func (d *Daemon) planProviderPayment(ixID string, meta []byte) *providerPayment {
	m := decodeMeta(meta)
	pp := &providerPayment{meta: m}
	pp.status, _ = m[x402a2a.KeyStatus].(string)
	// The quote, whenever a status carries one: a payment-required, or a
	// payment-failed that leaves the quote standing.
	if raw, ok := quoteOf(meta); ok {
		pp.quote = raw
	} else if raw != nil {
		log.Printf("anet: %s: the provider asked to be paid and named no way to pay (none, or one that reads two ways)", ixID)
	}
	pp.expires = time.Now().Add(quoteLifetime).UnixMilli()
	if v, ok := m[x402a2a.KeyQuoteExpiresAt].(float64); ok && int64(v) > time.Now().UnixMilli() && int64(v) < pp.expires {
		// The provider's own expiry, when it states one, and never later
		// than 24 hours from now.
		pp.expires = int64(v)
	}
	pp.receipts = d.planReceipts(ixID, m, false)
	if pp.status == x402a2a.StatusFailed {
		pp.failNote = d.failureSnapshot(ixID, m)
	}
	return pp
}

// failureSnapshot is the pay_note of a payment the provider says did not
// settle (ux D3; failureNote), for ixID's payment as this node sent it:
// INVALID_SIGNATURE on an authorization signed under a key this node has
// rotated away since is the .rotated variant (KEL §5.3).
func (d *Daemon) failureSnapshot(ixID string, m map[string]any) []byte {
	reason, _ := m[x402a2a.KeyReason].(string)
	code, _ := m[x402a2a.KeyError].(string)
	var amount uint64
	var signedSeq uint64
	signed := false
	ix, err := d.ix.Get(ixID)
	if err != nil {
		return nil
	}
	if _, auth, _, err := payloadAuth(ix.PayPayload); err == nil && auth != nil {
		amount = auth.Amount
		if auth.Envelope != nil {
			signedSeq, signed = auth.Envelope.KeyStateSeq, true
		}
	}
	n := failureNote(reason, code, amount, ix.PeerAID, d.now())
	if n.Code == "x402:"+x402a2a.CodeInvalidSignature && signed && d.self != nil {
		if tip, err := kelTipSeq(d.self.KEL()); err == nil && signedSeq < tip {
			n.Code += ".rotated"
		}
	}
	b, err := json.Marshal(n)
	if err != nil {
		return nil
	}
	return b
}

// quoteOf is the quote a provider's status metadata carries: the
// x402.payment.required value, kept as the provider wrote it, since a local
// client copies an option out of it and the choice is compared with what
// was stored (§8.7). ok is false when there is none, when it names no way
// to pay, and when it does not read alike (jsonread.ReadsAlike): an option
// that spells a field in two cases would be shown to the client with one
// amount, payee or network and signed by this node with another
// (an internal note). raw is then what was there, if anything.
func quoteOf(meta []byte) (raw []byte, ok bool) {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(meta, &fields)
	raw = fields[x402a2a.KeyRequired]
	if len(raw) == 0 {
		return nil, false
	}
	var pr payment.PaymentRequired
	if json.Unmarshal(raw, &pr) != nil || len(pr.Accepts) == 0 || !jsonread.ReadsAlike(raw, &pr) {
		return raw, false
	}
	return raw, true
}

// release lets go of the receipts lock the plan holds (receiptPlan.release)
// when afterProviderPayment did not: a status that did not commit.
func (pp *providerPayment) release() {
	if pp != nil {
		pp.receipts.release()
	}
}

// applyTx writes the pay columns the status sets, inside its transaction.
func (pp *providerPayment) applyTx(tx *interactions.Tx, ixID string) error {
	prior, err := tx.Get(ixID)
	if err != nil {
		return err
	}
	pp.prior = prior.PayState
	switch pp.status {
	case x402a2a.StatusRequired:
		if pp.quote == nil {
			break
		}
		// A quote again after a payment was submitted means the provider
		// did not take it; the same authorization may be sent again (§8.3).
		expires := pp.expires
		if pp.quoted, err = tx.SetPayment(ixID, interactions.PayUpdate{
			From: []string{interactions.PayNone, interactions.PayRequired, interactions.PayFailed,
				interactions.PaySubmitted},
			State: interactions.PayState(interactions.PayRequired), Required: pp.quote, QuoteExpiresAt: &expires}); err != nil {
			return err
		}
	case x402a2a.StatusFailed:
		// The payment sent has a definite outcome, and it is not sent
		// again: pay_payload is cleared, so a quote repeated after this is
		// paid, if at all, with a new authorization and a decision (Q11).
		upd := interactions.PayUpdate{From: []string{interactions.PaySubmitted, interactions.PayRequired, interactions.PayFailed},
			State: interactions.PayState(interactions.PayFailed), Payload: []byte{}, Note: pp.failNote}
		if pp.quote != nil {
			expires := pp.expires
			upd.Required, upd.QuoteExpiresAt = pp.quote, &expires
		}
		if pp.failWrit, err = tx.SetPayment(ixID, upd); err != nil {
			return err
		}
	}
	return pp.receipts.applyTx(tx, ixID)
}

// triesAutoPay reports whether afterProviderPayment will try to pay pp's
// quote by itself: paid automatically once per quote, on the first one, or
// again with the same authorization after a re-quote. After a definite
// failure a person or an agent decides, so a provider cannot drain the
// automatic tier by quoting again and again. Read after the status's
// commit (applyTx sets prior and quoted).
func (d *Daemon) triesAutoPay(pp *providerPayment) bool {
	return pp != nil && pp.status == x402a2a.StatusRequired && pp.quoted &&
		(pp.prior == interactions.PayNone || pp.prior == interactions.PaySubmitted) && d.payer() != nil
}

// afterProviderPayment does what follows the commit of a status carrying
// a payment part. announce tells the task's watchers about the status (its
// message and the state); it is called once, when this is done.
//
// A quote this node tries to pay by itself is not announced before the
// attempt (an internal note F1): the task is stored input-required with the
// quote, and a waiter woken there — a blocking send, /tasks/wait, MCP
// wait_task, an A2A stream — answered its client input-required, "pay it",
// for a payment already being made (§8.3, §8.7: within the auto tier the
// node pays and the client is not asked). The status is announced after
// the decision (autoPay): on the working state its payment message sets, or
// on input-required with the decision's sentence when it leaves the quote
// to a person.
func (d *Daemon) afterProviderPayment(ctx context.Context, ixID string, pp *providerPayment, announce func()) {
	switch {
	case pp.status == x402a2a.StatusRequired && pp.quoted:
		if d.payer() == nil {
			d.noteNoPayer(ixID)
		}
		auto := d.triesAutoPay(pp)
		if !auto {
			d.publishState(ixID)
		}
		d.recordReceipts(pp.receipts)
		if pp.prior == interactions.PaySubmitted && d.carryOutRequestedCancel(ctx, ixID) {
			announce()
			return
		}
		if auto {
			d.autoPay(ctx, ixID, pp.prior == interactions.PaySubmitted, announce)
			return
		}
		announce()
		return
	case pp.status == x402a2a.StatusFailed:
		d.publishState(ixID)
	}
	d.recordReceipts(pp.receipts)
	if pp.status == x402a2a.StatusFailed {
		d.carryOutRequestedCancel(ctx, ixID)
	}
	announce()
}

// autoPay pays a stored quote at the automatic tier when decidePay says
// so, or leaves it to a person with the decision's sentence (HPP PAY-D2).
// With resend set (a quote again after a payment was submitted) it only
// sends the same authorization again: the one already sent has no
// definite outcome, and a new one is not signed until it has (§8.3
// [C13][C25]). announce is called once the decision is made.
//
// A decision that has to ask the payee for its signed price waits for the
// answer (at most 10 seconds), and the answer arrives on the same receive
// path that called this: that decision is taken off it, in the background.
// One the cache can answer is taken here, before the status is announced.
func (d *Daemon) autoPay(ctx context.Context, ixID string, resend bool, announce func()) {
	if d.payer() == nil {
		d.noteNoPayer(ixID)
		announce()
		return
	}
	if !resend && d.autoPayAsksForAPrice(ixID) {
		if d.goBackground(func() {
			d.autoPayNow(d.ctx, ixID, false)
			announce()
		}) {
			return
		}
	}
	d.autoPayNow(ctx, ixID, resend)
	announce()
}

// noteNoPayer keeps, for a quote this node cannot pay because it has no
// payment module, the sentence that says so (MOD-2): module_absent.payment
// names no `anet pay`, which would fail, and says to cancel the task if the
// price is not wanted. Nothing is declined, signed or sent to the payee:
// doing nothing is the failing-closed answer.
func (d *Daemon) noteNoPayer(ixID string) {
	ix, err := d.ix.Get(ixID)
	if err != nil || ix.Role != interactions.RoleOutbound || ix.IsTerminal() {
		return
	}
	n := a2ashape.PayNote{Code: "module_absent.payment", Peer: ix.PeerAID, At: d.now().UTC()}
	if ref := d.noPayments(); ref != nil {
		n.Why, _ = ref.Args["reason"].(string)
	}
	if pr := storedQuote(ix); pr != nil {
		n.Amount = quoteAmount(pr.Accepts[0])
	}
	d.storePayNote(ixID, n)
}

// autoPayNow is autoPay's decision, here and now.
func (d *Daemon) autoPayNow(ctx context.Context, ixID string, resend bool) {
	_, err := d.PayTask(ctx, PayRequest{TaskID: ixID, Decision: PayDecisionSubmit, Purpose: module.PurposeTaskAuto,
		resendOnly: resend})
	logAutoPay(ixID, err)
}

// logAutoPay notes in the daemon log why the automatic tier did not pay.
func logAutoPay(ixID string, err error) {
	if err == nil {
		return
	}
	var hold *PayHold
	if errors.As(err, &hold) {
		log.Printf("anet: %s: the quote waits for a decision (%s); pay it with `anet pay %s`", ixID, hold.Note.Code, ixID)
		return
	}
	if errors.Is(err, errAutoPaidOnce) || errors.Is(err, errNoResend) {
		log.Printf("anet: %s: %v; pay it with `anet pay %s`", ixID, err, ixID)
		return
	}
	log.Printf("anet: %s: not paid automatically: %v", ixID, err)
}

// autoPayAsksForAPrice reports whether the automatic decision on ixID's
// quote would ask the payee for its signed price (PAY-C10 case 2): the tier
// is on, the task calls a capability, and no fresh card this node keeps
// answers it — none, or one more than a minute old whose price is below
// the quote (signedPriceFor asks again then).
func (d *Daemon) autoPayAsksForAPrice(ixID string) bool {
	if d.config().Payments.limits().AutoMax == 0 {
		return false
	}
	ix, err := d.ix.Get(ixID)
	if err != nil {
		return false
	}
	capID := taskCapability(ix)
	if capID == "" {
		return false
	}
	pr := storedQuote(ix)
	if pr == nil || len(pr.Accepts) == 0 {
		return false
	}
	kel, _ := d.peerKEL(ix.PeerAID)
	now := d.nowMS()
	c, ok := d.cachedExtCard(ix.PeerAID, kel, now)
	if !ok {
		return true
	}
	quote := uint64(0)
	for _, o := range pr.Accepts {
		quote = max(quote, quoteAmount(o))
	}
	return quote > c.Prices[capID] && now-min(now, c.FetchedAt) > uint64(priceRefetchAge.Milliseconds())
}

// signedPriceArrived is PAY-D5's wake-up: aid's extended card was just
// kept, and each task of aid's that waits in input-required with a
// card_price_unavailable snapshot for want of an answer (timeout), its
// quote unexpired, has its payment decided again, once, at the tier that
// decided it, with the price the card gives. Off the receive path: a
// decision that pays sends a message.
func (d *Daemon) signedPriceArrived(aid string) {
	if d.ix == nil || d.payer() == nil {
		return
	}
	list, err := d.ix.ListPayState(interactions.RoleOutbound,
		[]string{interactions.PayRequired, interactions.PayFailed}, true)
	if err != nil {
		log.Printf("anet: the tasks waiting for %s's price: %v", aid, err)
		return
	}
	for _, ix := range list {
		if ix.PeerAID != aid || !lateCardWaits(ix, time.Now().UnixMilli()) {
			continue
		}
		id := ix.ID
		d.goBackground(func() { d.decideOnALateCard(id) })
	}
}

// lateCardWaits reports a task PAY-D5 decides again: input-required on a
// quote that has not lapsed, with a decision that had no signed price for
// want of an answer.
func lateCardWaits(ix *interactions.Interaction, nowMS int64) bool {
	if ix.State != interactions.StateInputRequired || (ix.QuoteExpiresAt > 0 && nowMS > ix.QuoteExpiresAt) {
		return false
	}
	n, ok := storedPayNote(ix)
	return ok && n.Code == "card_price_unavailable" && n.Why == priceWhyTimeout &&
		(n.Tier == a2ashape.PayTierAuto || n.Tier == a2ashape.PayTierAgent)
}

// decideOnALateCard is PAY-D5 for one task: the decision again, at the
// tier of the one it replaces, with the price of the card just kept (never
// asked for again: at most one more decision per card). PayTask checks
// under its lock that the task still waits for that card.
func (d *Daemon) decideOnALateCard(ixID string) {
	ix, err := d.ix.Get(ixID)
	if err != nil {
		return
	}
	n, ok := storedPayNote(ix)
	if !ok {
		return
	}
	kel, _ := d.peerKEL(ix.PeerAID)
	c, ok := d.cachedExtCard(ix.PeerAID, kel, d.nowMS())
	if !ok {
		return
	}
	price := cardPrice{Have: true, Price: c.Prices[taskCapability(ix)]}
	purpose := module.PurposeTaskAuto
	if n.Tier == a2ashape.PayTierAgent {
		purpose = module.PurposeTaskAgent
	}
	_, err = d.PayTask(d.ctx, PayRequest{TaskID: ixID, Decision: PayDecisionSubmit, Purpose: purpose,
		price: &price, lateCard: true})
	logAutoPay(ixID, err)
}

// PaymentReason is the anet.reason of an outbound task that waits on a
// payment decision, while a quote stands unpaid and the task is
// input-required: the code of the decision's sentence when it left the
// quote to a person (ux D1: needs_operator_approval, quote_above_card_price,
// card_price_unavailable), otherwise needs_operator_approval (no decision
// yet, or the last payment did not settle: ux D3 says why). "" otherwise.
func PaymentReason(ix *interactions.Interaction) string {
	if ix.Role == interactions.RoleOutbound && ix.State == interactions.StateInputRequired &&
		(ix.PayState == interactions.PayRequired || ix.PayState == interactions.PayFailed) {
		if n, ok := storedPayNote(ix); ok && a2ashape.PayNoteWaits(n.Code) {
			return a2ashape.ReasonCode(n.Code)
		}
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
	if ix.Role == interactions.RoleOutbound {
		// The provider's claims of settlement this node could not verify,
		// kept apart from what it states (a2ashape.SplitReceipts), also on
		// a task this node never paid for [redteam:F11].
		if _, un := a2ashape.SplitReceipts(storedReceipts(ix), true, ix.PayState); len(un) > 0 {
			out[x402a2a.KeyUnverifiedReceipts] = un
		}
	}
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
		required := json.RawMessage(ix.PayRequired)
		if ix.Role == interactions.RoleOutbound {
			// This node is the local client's signing service: the options
			// it can pay come first (an internal note item).
			required = payableFirst(ix.PayRequired, d.knownHomeNetwork())
		}
		out[x402a2a.KeyRequired] = required
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
	// client is that message's retry key (clientdedupe.go), stored with the
	// payment message in the same transaction; nil without a messageId.
	client *clientMark
	// resendOnly allows only the authorization sent last, on the same
	// terms; set for the automatic answer to a quote repeated after a
	// payment was submitted.
	resendOnly bool
	// price, when set, is the signed price to decide with instead of
	// asking the payee; lateCard says the decision is PAY-D5's, which goes
	// ahead only while the task still waits for a card (lateCardWaits).
	price    *cardPrice
	lateCard bool
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
	// SpendRefusal and Message are set on a decision left to a person
	// (PayHold): the first check that did not pass (ux D4's code for a
	// limit or the payee list, quote_above_card_price or
	// card_price_unavailable for the price; payCheck), and what happens
	// now, for a person or an agent to read: the decision's sentence.
	SpendRefusal string `json:"spend_refusal,omitempty"`
	Message      string `json:"message,omitempty"`
	// Note is the payment sentence of the decision taken (ux D1 or D2;
	// MCP submit_payment's note, HPP §4.6), the task's anet.payment_note.
	Note string `json:"note,omitempty"`
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

// errAutoPaidOnce refuses a second automatic authorization for a task: the
// automatic tier pays a task at most once (§8.3, an internal note item).
var errAutoPaidOnce = errors.New("anet: this task was already paid automatically once; another payment needs a decision")

// PayRefusal is a decision refused with an a2a-x402 outcome (§8.7): the
// caller is told payment-failed with Code and Reason, and nothing was
// signed or sent.
type PayRefusal struct {
	Outcome PayOutcome
}

func (e *PayRefusal) Error() string {
	return fmt.Sprintf("anet: payment refused: %s (%s)", e.Outcome.Error, e.Outcome.Reason)
}

// PayHold is a decision to pay that decidePay left to a person (HPP
// PAY-D2, PAY-D3): the tier's limit, its left today, the payee list or the
// payee's signed price did not let it through. Nothing was signed or sent,
// and the task goes on waiting, input-required with the decision's
// anet.reason and sentence, for the operator's `anet pay` on a terminal (or
// what the sentence says to run first). It is the answer, not a failure:
// /tasks/pay returns Outcome with 200 and the local A2A interface returns
// the task (a2ashape.PaymentHold); the automatic tier only logs it.
type PayHold struct {
	Outcome PayOutcome
	Note    a2ashape.PayNote
}

func (e *PayHold) Error() string {
	return fmt.Sprintf("anet: payment left to a person: %s", e.Note.Code)
}

// operatorDecides reports a spending-policy refusal that an operator can
// overrule: a limit or the payee list. A quote for nothing (zero_amount)
// is refused at every tier, so it is not held for anyone.
func operatorDecides(code string) bool {
	switch code {
	case SpendOverSingle, SpendOverAgentDaily, SpendOverDaily, SpendPayeeNotAllowed, SpendPayeesUnread:
		return true
	}
	return false
}

// holdPayment keeps a decision that left ix's quote to a person — its
// snapshot on the task — and is its answer (PayHold). check is the first
// check that failed (payCheck).
func (d *Daemon) holdPayment(ix *interactions.Interaction, n a2ashape.PayNote, check string) (PayOutcome, error) {
	d.storePayNote(ix.ID, n)
	n.Task = ix.ID
	text := a2ashape.PaymentNote(n, a2ashape.Lang())
	out := PayOutcome{TaskID: ix.ID, State: string(ix.State), PayState: ix.PayState,
		Reason: a2ashape.ReasonCode(n.Code), SpendRefusal: check, Message: text, Note: text}
	if cur, err := d.ix.Get(ix.ID); err == nil {
		out.State, out.PayState = string(cur.State), cur.PayState
		out.Status, _ = d.PaymentStatusMeta(cur)[x402a2a.KeyStatus].(string)
	}
	if out.Status == "" {
		out.Status = x402a2a.StatusRequired
	}
	return out, &PayHold{Outcome: out, Note: n}
}

// refusalNote is the sentence of a refusal AdmitSpend gave after decidePay
// had said to sign — the limits or the list moved in between — for the
// tier that asked: the decision again on what they are now, or, should
// that still say to sign, the D1 sentence of the refusal itself.
func (d *Daemon) refusalNote(ctx context.Context, ix *interactions.Interaction, amount uint64, payTo, purpose string,
	price cardPrice, r *SpendRefusal) (a2ashape.PayNote, string) {
	in := d.payInput(ctx, ix, amount, payTo, purpose, &price)
	n := decidePay(in)
	if !payNoteSigns(n) && n.Code != "" {
		return n, payCheck(in)
	}
	n = a2ashape.PayNote{Amount: amount, Tier: tierName(purpose), Single: in.Single, Left: in.Left, Peer: payTo,
		At: in.At.UTC()}
	switch r.Code {
	case SpendPayeeNotAllowed:
		n.Code = "needs_operator_approval.payee_not_allowed"
	case SpendPayeesUnread:
		n.Code = "needs_operator_approval.payees_unreadable"
	default:
		n.Code = "needs_operator_approval.over_limit"
	}
	return n, r.Code
}

// plainText is peer-written s without control or bidirectional
// formatting characters, cut to max bytes.
func plainText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) || pathsafe.IsBidiFormat(r) {
			return -1
		}
		return r
	}, s)
	return pathsafe.TruncateUTF8(s, max)
}

// PayTask carries out a decision on a task's quote: submit signs (or, on
// the same terms, reuses) an authorization bound to the task and sends
// payment-submitted; reject sends payment-rejected and cancels the task
// here. The purpose decides the spending tier; every signature passes the
// spending policy (§8.6). The control plane (/tasks/pay, /tasks/pay-manual)
// and the local A2A interface call this.
func (d *Daemon) PayTask(ctx context.Context, req PayRequest) (PayOutcome, error) {
	if d.payer() == nil {
		// Before anything of the task is read: a node without a payment
		// module answers module_absent whatever the task (MOD-2).
		return PayOutcome{TaskID: req.TaskID}, d.noPayments()
	}
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
		return d.rejectQuote(ctx, ix, req.ClientMsgID, req.client)
	case PayDecisionSubmit:
	default:
		return out, fmt.Errorf("anet: decision must be %q or %q", PayDecisionSubmit, PayDecisionReject)
	}
	if ix.QuoteExpiresAt > 0 && time.Now().UnixMilli() > ix.QuoteExpiresAt {
		return out, ErrQuoteExpired
	}
	p := d.payer()
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
	// an internal note item: an option on a ledger this node holds no credit on is not
	// one it can pay. Signed anyway, the authorization could only be
	// refused by that ledger's hub (unknown_payer, insufficient funds) —
	// or sit there as a signature over money this node does not have.
	// Refused before anything is signed, saying which options it can pay.
	//
	// So is any option while this node cannot learn which ledger its
	// credit is on (its hub has not answered since it started): it cannot
	// tell a payable option from another, and a receipt could not be
	// checked either. This used to sign whatever was chosen — the
	// provider's first option, when the client named none — and leave it
	// to that ledger's hub [redteam:Q28].
	if home := p.HomeNetwork(); home == "" || opt.Network != home {
		out.Message = railNotPayableText(opt.Network, home, pr.Accepts)
		return refuse(x402a2a.ReasonRailNotPayable)
	}
	if opt.PayTo != ix.PeerAID {
		// A receipt is checked against the peer as payee; a payment to
		// anyone else could never be shown to have paid for this task.
		return out, ErrPayeeNotPeer
	}
	if req.lateCard && !lateCardWaits(ix, time.Now().UnixMilli()) {
		// Decided since the card was asked for (PAY-D5 decides once).
		return out, ErrNoQuote
	}
	raw, authID, reused := d.reusablePayment(ix, *opt)
	if raw == nil && req.resendOnly {
		return out, errNoResend
	}
	if raw == nil && req.Purpose == module.PurposeTaskAuto && len(ix.PayAuthIDs) > 0 {
		// §8.3 (an internal note item): the automatic tier signs at most one
		// authorization per task. Once one was signed for it, a new one —
		// after a definite failure, or on new terms — is a person's or an
		// agent's decision; the task waits with needs_operator_approval.
		// Sending the same authorization again (reused above) is not a
		// second payment and stays automatic.
		return out, errAutoPaidOnce
	}
	var note a2ashape.PayNote
	if raw == nil {
		// A new authorization: the decision first (HPP PAY-D1). The
		// automatic and the agent tier sign only what decidePay says to;
		// anything else waits for a person, with its sentence. The manual
		// tier is decided by its limits alone (PAY-D4), which AdmitSpend
		// enforces as it records the payment.
		amount := quoteAmount(*opt)
		manual := req.Purpose == module.PurposeTaskManual
		var in PayInput
		if amount > 0 && (manual || req.Purpose == module.PurposeTaskAuto || req.Purpose == module.PurposeTaskAgent) {
			in = d.payInput(ctx, ix, amount, opt.PayTo, req.Purpose, req.price)
			note = decidePay(in)
			if !manual && !payNoteSigns(note) {
				return d.holdPayment(ix, note, payCheck(in))
			}
		}
		bind, err := x402a2a.PayBind(ix.ID, ix.TaskNonce)
		if err != nil {
			return out, err
		}
		signed, err := p.Authorize(*opt, ix.ID, bind, req.Purpose)
		if err != nil {
			if r, ok := isSpendRefusal(err); ok {
				logSpendRefusal(ix.ID, r)
				if !manual && note.Code != "" && operatorDecides(r.Code) {
					// The limits or the list moved since the decision:
					// decided again on what they are now.
					n, check := d.refusalNote(ctx, ix, amount, opt.PayTo, req.Purpose, in.Price, r)
					return d.holdPayment(ix, n, check)
				}
				return out, r
			}
			return out, err
		}
		raw = signed
		if _, _, authID, err = payloadAuth(raw); err != nil {
			return out, err
		}
		if payNoteSigns(note) {
			// What is left after this payment (ux D2), now that it is
			// recorded.
			st := d.SpendStatus()
			note.Left, note.At = st.tierLeft(req.Purpose), d.now().UTC()
		} else {
			note = a2ashape.PayNote{}
		}
	}
	meta, err := json.Marshal(map[string]any{x402a2a.KeyStatus: x402a2a.StatusSubmitted,
		x402a2a.KeyPayload: json.RawMessage(raw)})
	if err != nil {
		return out, err
	}
	if err := d.requesterPaymentMessage(ctx, ix, meta, req.ClientMsgID, req.client, interactions.StateWorking, func(tx *interactions.Tx) error {
		cur, err := tx.Get(ix.ID)
		if err != nil {
			return err
		}
		if cur.PayState == interactions.PaySubmitted {
			return ErrPaymentPending
		}
		upd := interactions.PayUpdate{
			From:  []string{interactions.PayRequired, interactions.PayFailed},
			State: interactions.PayState(interactions.PaySubmitted), AddAuthID: authID, Payload: raw}
		if note.Code != "" {
			b, err := json.Marshal(note)
			if err != nil {
				return err
			}
			upd.Note = b
		}
		applied, err := tx.SetPayment(ix.ID, upd)
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
	if note.Code != "" {
		note.Task = ix.ID
		out.Note = a2ashape.PaymentNote(note, a2ashape.Lang())
	}
	return out, nil
}

// rejectQuote declines a task's quote: payment-rejected to the provider,
// the task canceled here (§8.3).
func (d *Daemon) rejectQuote(ctx context.Context, ix *interactions.Interaction, clientMsgID string, cm *clientMark) (PayOutcome, error) {
	meta, _ := json.Marshal(map[string]any{x402a2a.KeyStatus: x402a2a.StatusRejected})
	err := d.requesterPaymentMessage(ctx, ix, meta, clientMsgID, cm, interactions.StateCanceled, func(tx *interactions.Tx) error {
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
// client's messageId, when given, is added to the stored copy only, with its
// retry key cm (clientdedupe.go).
func (d *Daemon) requesterPaymentMessage(ctx context.Context, ix *interactions.Interaction, meta []byte,
	clientMsgID string, cm *clientMark, state interactions.State, write func(tx *interactions.Tx) error) error {
	// The message id is the envelope's inner id (an internal note item), the same on
	// both sides.
	mid, msgID := newWireMID()
	stored := meta
	if clientMsgID != "" {
		m := decodeMeta(meta)
		m[a2ashape.KeyMessageID] = clientMsgID
		var err error
		if stored, err = json.Marshal(m); err != nil {
			return err
		}
	}
	payload, err := (&delegation.ChatMsg{Kind: delegation.ChatText, MsgID: msgID, Metadata: meta}).Marshal()
	if err != nil {
		return err
	}
	var seq int64
	id, err := d.queueSendAs(ctx, wireSend{to: ix.PeerAID, typ: seal.TypeMessage, ix: ix.ID, body: payload, mid: mid}, func(tx *interactions.Tx) error {
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
		rec := interactions.MessageRecord{InteractionID: ix.ID, SenderAID: d.AID(),
			Kind: interactions.MsgPayment, MsgID: msgID, Metadata: stored}
		if err := cm.stamp(tx, ix.ID, &rec); err != nil {
			return err
		}
		if seq, _, err = tx.AddMessageRecord(rec); err != nil {
			return err
		}
		_, err = tx.SetState(ix.ID, state, interactions.CauseLocalEnded.When(state))
		return err
	})
	if err != nil {
		return err
	}
	d.publishMessage(ix.ID, seq, interactions.MsgPayment)
	d.publishState(ix.ID)
	d.recordMessageSent(ix, msgID, interactions.MsgPayment, payload, 0)
	if err := d.deliverQueued(ctx, id); err != nil {
		log.Printf("anet: %s: payment message queued for delivery (%v)", ix.ID, err)
	}
	return nil
}

// railNotPayableText says why an option on network chosen cannot be paid
// here and which of the quoted options can: those on home, the ledger this
// node's credit is on. The options are the provider's, so they are made
// plain before they become text a model reads.
func railNotPayableText(chosen, home string, accepts []payment.PaymentOption) string {
	if home == "" {
		return fmt.Sprintf("this node could not learn from its hub which ledger its credit is on, so it cannot "+
			"tell whether it can pay on %s. Nothing was signed or sent; pay again once its hub answers.",
			plainText(chosen, 256))
	}
	var can []string
	for _, o := range accepts {
		if o.Scheme == payment.SchemeCredit && o.Network == home {
			can = append(can, fmt.Sprintf("%s %s to %s on %s", plainText(o.Amount, 40), plainText(o.Asset, 40),
				plainText(o.PayTo, 256), plainText(o.Network, 256)))
		}
	}
	s := fmt.Sprintf("the chosen option settles on %s, where this node holds no credit; it pays on %s",
		plainText(chosen, 256), plainText(home, 256))
	if len(can) == 0 {
		return s + ", and none of the quoted options is on it, so this node cannot pay this quote. Nothing was signed or sent."
	}
	return s + ". Options it can pay (first in x402.payment.required.accepts): " + strings.Join(can, "; ") +
		". Nothing was signed or sent."
}

// payableFirst is a stored PaymentRequired with its accepts in the order
// this node can pay them (an internal note item): the options on home, the ledger its
// credit is on, first, the rest after, each in its quoted order. Every
// option is as the provider quoted it — a local client copies one out and
// PayTask compares it with the stored quote field for field — and nothing
// else changes. Unchanged when home is unknown or the quote unreadable.
func payableFirst(required []byte, home string) json.RawMessage {
	if home == "" {
		return required
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(required, &top) != nil {
		return required
	}
	var accepts []json.RawMessage
	if json.Unmarshal(top["accepts"], &accepts) != nil || len(accepts) < 2 {
		return required
	}
	var pay, rest []json.RawMessage
	for _, a := range accepts {
		var o struct {
			Scheme  string `json:"scheme"`
			Network string `json:"network"`
		}
		if json.Unmarshal(a, &o) == nil && o.Scheme == payment.SchemeCredit && o.Network == home {
			pay = append(pay, a)
		} else {
			rest = append(rest, a)
		}
	}
	if len(pay) == 0 || len(rest) == 0 {
		return required
	}
	b, err := json.Marshal(append(pay, rest...))
	if err != nil {
		return required
	}
	top["accepts"] = b
	out, err := json.Marshal(top)
	if err != nil {
		return required
	}
	return out
}

// knownHomeNetwork is the ledger this node's credit is on when its hub's
// identity is already known here, "" otherwise. It never asks the hub: a
// task view must not wait on the network.
func (d *Daemon) knownHomeNetwork() string {
	if d.payer() == nil {
		return ""
	}
	key := strings.TrimRight(strings.TrimSpace(d.config().HubURL), "/")
	if key == "" {
		return ""
	}
	d.hubIDMu.Lock()
	h, ok := d.hubIDs[key]
	d.hubIDMu.Unlock()
	if !ok || h.aid == "" {
		return ""
	}
	return payment.CreditNetwork(h.aid)
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
	if bind, err := x402a2a.PayBind(ix.ID, ix.TaskNonce); err != nil || !bytes.Equal(a, b) || auth.InteractionID != bind {
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

// maxPeerReceipts bounds the receipt list taken from one provider message.
const maxPeerReceipts = 128

// notePaymentReceipts verifies and records the receipts a provider sent
// on a status or a result (§8.3; §4.2: also when the task has ended here).
//
// Each successful receipt is checked: signed by this node's hub for a
// payment by this node, for one of the authorizations this node signed for
// the task, paying the task's provider, for that authorization's amount.
// It is recorded once (by transaction) as anet.payment.settled, verified or
// not; a second verified settlement for one task is recorded and marked.
// Only a verified one moves pay_state to completed. One that cannot be
// checked yet, because this node cannot learn its hub's identity right
// now, is not recorded, so that its next delivery is checked.
//
// The list is the provider's whole history for the task (§8.2), failures
// included, and is stored as pay_receipts after the check, each success
// with this node's verdict in its extensions (x402a2a.ExtSettlementVerified)
// so whoever renders the task states only what this node verified
// (a2ashape.SplitReceipts). It was stored before any check and rendered as
// the task's settlement: a provider's made-up receipts on a free call read
// "Payment completed." [redteam:F11].
//
// The receive path plans the receipts before its step-10 transaction,
// writes the pay columns inside it and records the evidence after it
// commits (planReceipts, receiptPlan.applyTx, recordReceipts;
// [redteam:F28]); this is the three in a row, for a caller outside one.
// The plan holds the task's receipts lock (planReceipts) until the
// evidence is recorded or the plan is let go (release).
func (d *Daemon) notePaymentReceipts(ixID string, m map[string]any, afterTerminal bool) {
	rp := d.planReceipts(ixID, m, afterTerminal)
	if rp == nil {
		return
	}
	defer rp.release()
	if err := d.ix.Update(func(tx *interactions.Tx) error { return rp.applyTx(tx, ixID) }); err != nil {
		log.Printf("anet: %s: store the receipts: %v", ixID, err)
		return
	}
	d.recordReceipts(rp)
}

// receiptPlan is what a provider's receipts change: the list to store as
// pay_receipts with this node's verdicts, whether a verified settlement
// completes the payment, and the evidence entries to record once that is
// stored. It holds the task's receipts lock from planReceipts until
// recordReceipts or release.
type receiptPlan struct {
	self      string                  // this node's AID, the payer a verified receipt states
	list      []any                   // the provider's list, bounded to maxPeerReceipts
	verified  map[string]settledFacts // settlements this node verified, by transaction
	completed bool
	entries   []map[string]any
	unlock    func()
}

// release lets go of the task's receipts lock. It is safe on a nil plan
// and more than once, so a caller defers it whatever becomes of its
// transaction (a duplicate or a store error records nothing).
func (rp *receiptPlan) release() {
	if rp == nil || rp.unlock == nil {
		return
	}
	rp.unlock()
	rp.unlock = nil
}

// planReceipts checks the receipts in a provider's metadata and returns
// what they change, or nil when there are none to look at. It writes
// nothing. A non-nil plan holds the task's receipts lock: the caller
// releases it, or recordReceipts does.
func (d *Daemon) planReceipts(ixID string, m map[string]any, afterTerminal bool) *receiptPlan {
	list, ok := m[x402a2a.KeyReceipts].([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	if len(list) > maxPeerReceipts {
		// The newest: a provider keeps 64 (interactions.maxPayReceipts), so
		// a longer list is not an honest history, and each item may cost a
		// signature check and an evidence record here.
		list = list[len(list)-maxPeerReceipts:]
	}
	// One list at a time per task. Read the ledger, check, record and
	// store is not atomic, and two messages of the provider's carrying
	// the same receipt, delivered at once (p2p and hub, §3.6), each found
	// it unrecorded and recorded it verified: two settlements for one
	// payment, which audit reports as a task paid twice [redteam:F11].
	unlock := d.outboxLocks.lock("receipts:" + ixID)
	// Held past this function, through the store and the evidence record
	// (receiptPlan.release): the receive path stores inside its step-10
	// transaction and records after it commits [redteam:F28].
	rp := &receiptPlan{self: d.AID(), list: list, unlock: unlock}
	ix, err := d.ix.Get(ixID)
	if err != nil || ix.Role != interactions.RoleOutbound {
		rp.release()
		return nil
	}
	// What this node already recorded for the task, and which of those
	// settlements it verified, with what its hub's receipt states.
	recorded, verifiedTx, verifiedBefore := map[string]bool{}, map[string]settledFacts{}, 0
	d.ledger.scan(EvPaymentSettled, 0, func(_ int64, p map[string]any) {
		if p["interaction_id"] != ixID {
			return
		}
		tx, _ := p["transaction"].(string)
		if tx != "" {
			recorded[tx] = true
		}
		if p["verified"] == true {
			verifiedBefore++
			if tx != "" {
				f := settledFacts{}
				f.amount, _ = p["amount"].(string)
				f.network, _ = p["network"].(string)
				f.receipt, _ = p["receipt"].(string)
				verifiedTx[tx] = f
			}
		}
	})
	rp.verified = verifiedTx
	p := d.payer()
	// A hub receipt is checked against this node's hub's key history.
	// While that cannot be had — restarted, its hub not answering, the
	// provider's message come over p2p (§3.6) — a settlement cannot be
	// checked, which is not the same as not checking out: it is left
	// unrecorded, stored unverified (storePeerReceiptsTx), and checked when
	// it comes again. Recorded as refused, every later delivery of it was
	// skipped as already recorded, and a task paid and settled stayed
	// payment-submitted for good [redteam:F11].
	uncheckable := p != nil && p.HomeNetwork() == ""
	for _, item := range list {
		rb, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var sr payment.SettlementResponse
		if json.Unmarshal(rb, &sr) != nil || !sr.Success || sr.Transaction == "" || recorded[sr.Transaction] {
			continue
		}
		enc, _ := sr.Extensions[payment.ExtReceipt].(string)
		if uncheckable && enc != "" {
			log.Printf("anet: %s: settlement %q not checked yet: this node cannot learn its hub's identity now; "+
				"it is checked when the provider sends it again", ixID, plainText(sr.Transaction, 128))
			continue
		}
		recorded[sr.Transaction] = true
		entry := map[string]any{"interaction_id": ixID, "transaction": sr.Transaction,
			"amount": sr.Amount, "network": sr.Network}
		if afterTerminal || ix.IsTerminal() {
			entry["after_terminal"] = true
		}
		verified := false
		switch {
		case p == nil:
			entry["refused"] = moduleAbsent + ".payment: no payment module to check the receipt with"
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
			case sr.Transaction != facts.AuthID || sr.Network != facts.Network:
				// The anet-credit scheme: transaction = auth_id. The hub's
				// receipt restated under another transaction (or network)
				// is not a second settlement: it was recorded as one, a
				// "second_receipt" for a task paid once, stated as two
				// settlements, and reported by reconcile as missing from
				// the hub under a transaction the hub never issued
				// [redteam:F11].
				entry["refused"] = "the settlement names a transaction or network other than the hub receipt's"
			default:
				verified = true
				entry["amount"] = payment.Amount(facts.Amount)
			}
		}
		entry["verified"] = verified
		if verified {
			// entry holds the hub receipt's amount; the network is the
			// receipt's too, checked equal above.
			verifiedTx[sr.Transaction] = settledFacts{amount: entry["amount"].(string),
				network: sr.Network, receipt: enc}
			if verifiedBefore > 0 {
				// §8.3 [m]: a second settlement for one task. Recorded, and
				// marked for audit to show.
				entry["second_receipt"] = true
			}
			verifiedBefore++
			rp.completed = true
		}
		rp.entries = append(rp.entries, entry)
	}
	return rp
}

// applyTx writes the pay columns the receipts change, inside the
// transaction of the message that carried them: the list with this node's
// verdicts (storePeerReceiptsTx) and, after a verified settlement,
// pay_state completed.
func (rp *receiptPlan) applyTx(tx *interactions.Tx, ixID string) error {
	if rp == nil {
		return nil
	}
	if err := storePeerReceiptsTx(tx, ixID, rp.list, rp.verified, rp.self); err != nil {
		return err
	}
	if rp.completed {
		if _, err := tx.SetPayment(ixID, interactions.PayUpdate{
			State: interactions.PayState(interactions.PayCompleted)}); err != nil {
			return err
		}
	}
	return nil
}

// recordReceipts records the settlement evidence once the pay columns are
// stored, and lets go of the plan's receipts lock. The evidence ledger is
// a store of its own and cannot join the transaction.
func (d *Daemon) recordReceipts(rp *receiptPlan) {
	if rp == nil {
		return
	}
	defer rp.release()
	for _, entry := range rp.entries {
		if entry["verified"] != true {
			log.Printf("anet: %s: settlement receipt did not check out: %v", entry["interaction_id"], entry["refused"])
		}
		if _, lerr := d.ledger.Append(EvPaymentSettled, entry); lerr != nil {
			log.Printf("anet: settlement evidence: %v", lerr)
		}
	}
}

// storePeerReceipts is storePeerReceiptsTx in a transaction of its own.
func (d *Daemon) storePeerReceipts(ixID string, list []any, verified map[string]settledFacts) {
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		return storePeerReceiptsTx(tx, ixID, list, verified, d.AID())
	}); err != nil {
		log.Printf("anet: %s: store the receipts: %v", ixID, err)
	}
}

// storePeerReceiptsTx stores a provider's receipt list as pay_receipts
// once planReceipts has checked it, inside tx: normalized (failures with
// transaction "", Q18), and each success carrying this node's verdict,
// VerdictVerified for a transaction in verified and VerdictUnverified for
// any other, whatever the provider wrote there. A verified one states the
// amount the hub's receipt states (verified maps a transaction to it) and
// this node (self) as payer, not the figures the provider wrote beside it.
//
// The provider's list replaces the stored one when it is at least as
// long (it is the whole history, §8.2), except that a settlement this node
// verified stays even when the new list leaves it out or restates it
// unverified: the provider cannot take back what this node checked. A
// shorter list only adds the settlements the stored one does not have.
//
// Verified here is what the stored list carries this node's verified
// mark on, as well as what verified names. verified is the ledger as
// planReceipts read it before checking, and a concurrent delivery
// (p2p and hub, §3.6) may have verified and stored a settlement since:
// read only from the snapshot, that settlement was dropped from the list
// by the next longer one [redteam:F11].
//
// At most maxPeerReceipts are kept, every verified settlement among them.
// The shorter-list path appended without limit, so a provider could grow
// the row by a few made-up transactions per status [redteam:F11].
func storePeerReceiptsTx(tx *interactions.Tx, ixID string, list []any, verified map[string]settledFacts, self string) error {
	items := make([]json.RawMessage, 0, len(list))
	for _, item := range list {
		if b, err := json.Marshal(item); err == nil {
			items = append(items, withVerdict(normalizeReceipt(b), verified, self))
		}
	}
	cur, err := tx.Get(ixID)
	if err != nil {
		return err
	}
	old := storedReceipts(cur)
	var merged []json.RawMessage
	if len(items) >= len(old) {
		restated := map[string]bool{}
		for _, n := range items {
			if tx := verifiedTx(n); tx != "" {
				restated[tx] = true
			}
		}
		kept := map[string]bool{}
		for _, o := range old {
			tx := successTx(o)
			if _, ok := verified[tx]; tx != "" && (ok || verifiedTx(o) != "") && !restated[tx] && !kept[tx] {
				kept[tx] = true
				merged = append(merged, o)
			}
		}
		for _, n := range items {
			if tx := successTx(n); tx != "" && kept[tx] {
				continue // an unverified restatement of a verified one
			}
			merged = append(merged, n)
		}
	} else {
		merged = old
		have := successTxs(old)
		added := false
		for _, n := range items {
			if tx := successTx(n); tx != "" && !have[tx] {
				merged, added = append(merged, n), true
			}
		}
		if !added {
			return nil
		}
	}
	all, err := json.Marshal(boundReceipts(merged, maxPeerReceipts))
	if err != nil {
		return err
	}
	_, err = tx.SetPayment(ixID, interactions.PayUpdate{Receipts: all})
	return err
}

// boundReceipts is list cut to max entries: every settlement this node
// verified, wherever it stands, and the newest of the rest.
func boundReceipts(list []json.RawMessage, max int) []json.RawMessage {
	if len(list) <= max {
		return list
	}
	nVerified := 0
	for _, r := range list {
		if verifiedTx(r) != "" {
			nVerified++
		}
	}
	drop := len(list) - max
	if others := len(list) - nVerified; drop > others {
		drop = others
	}
	out := make([]json.RawMessage, 0, len(list)-drop)
	for _, r := range list {
		if drop > 0 && verifiedTx(r) == "" {
			drop--
			continue
		}
		out = append(out, r)
	}
	return out
}

// verifiedTx is the transaction of a stored settlement response this node
// marked verified (withVerdict), "" for anything else.
func verifiedTx(raw json.RawMessage) string {
	var r struct {
		Success     bool           `json:"success"`
		Transaction string         `json:"transaction"`
		Extensions  map[string]any `json:"extensions"`
	}
	if json.Unmarshal(raw, &r) != nil || !r.Success || r.Extensions[x402a2a.ExtSettlementVerified] != x402a2a.VerdictVerified {
		return ""
	}
	return r.Transaction
}

// settledFacts is what this node recorded of a settlement it verified
// (anet.payment.settled): the amount and network of the hub's receipt, and
// the receipt itself.
type settledFacts struct{ amount, network, receipt string }

// withVerdict is a settlement response with this node's verdict in its
// extensions when it claims success. A verified one is restated whole from
// what this node recorded of the hub's receipt — transaction, network,
// amount, this node (payer) as payer, the receipt — and keeps nothing the
// provider wrote beside it: re-sent with another network or other receipt
// bytes, it was stored verified as the provider rewrote it. Anything else
// keeps no verdict of the provider's writing: a failure's is removed, and
// an entry whose success is not a boolean is marked unverified; the
// verdict key is this node's [redteam:F11].
func withVerdict(raw json.RawMessage, verified map[string]settledFacts, payer string) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil || m == nil {
		return raw
	}
	ext, _ := m["extensions"].(map[string]any)
	switch m["success"] {
	case true:
	case false:
		if _, set := ext[x402a2a.ExtSettlementVerified]; !set {
			return raw
		}
		delete(ext, x402a2a.ExtSettlementVerified)
		return remarshal(m, raw)
	default:
		if ext == nil {
			ext = map[string]any{}
		}
		ext[x402a2a.ExtSettlementVerified] = x402a2a.VerdictUnverified
		m["extensions"] = ext
		return remarshal(m, raw)
	}
	tx, _ := m["transaction"].(string)
	if f, ok := verified[tx]; ok && tx != "" {
		out := map[string]any{"success": true, "transaction": tx, "payer": payer}
		for k, v := range map[string]string{"amount": f.amount, "network": f.network} {
			if v == "" {
				v, _ = m[k].(string) // recorded before this node kept it
			}
			if v != "" {
				out[k] = v
			}
		}
		oext := map[string]any{x402a2a.ExtSettlementVerified: x402a2a.VerdictVerified}
		if rc := f.receipt; rc != "" {
			oext[payment.ExtReceipt] = rc
		} else if rc, _ := ext[payment.ExtReceipt].(string); rc != "" {
			oext[payment.ExtReceipt] = rc
		}
		out["extensions"] = oext
		return remarshal(out, raw)
	}
	if ext == nil {
		ext = map[string]any{}
	}
	ext[x402a2a.ExtSettlementVerified] = x402a2a.VerdictUnverified
	m["extensions"] = ext
	return remarshal(m, raw)
}

// remarshal is m encoded, or fallback when it cannot be.
func remarshal(m map[string]any, fallback json.RawMessage) json.RawMessage {
	b, err := json.Marshal(m)
	if err != nil {
		return fallback
	}
	return b
}

// successTx is the transaction of a settlement response that claims
// success, "" for anything else.
func successTx(raw json.RawMessage) string {
	var r struct {
		Success     bool   `json:"success"`
		Transaction string `json:"transaction"`
	}
	if json.Unmarshal(raw, &r) != nil || !r.Success {
		return ""
	}
	return r.Transaction
}

// successTxs is the set of successTx over a list.
func successTxs(list []json.RawMessage) map[string]bool {
	out := map[string]bool{}
	for _, r := range list {
		if tx := successTx(r); tx != "" {
			out[tx] = true
		}
	}
	return out
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

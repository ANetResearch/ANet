package daemon

// paynote.go is the payment decision (HPP PAY-D1 to PAY-D5, §5.3): for a
// quote on a task this node started, whether to sign now or leave it to a
// person, and the one sentence that says which and why.
//
// decidePay is a pure function of what it is given — the tier, the quote,
// the capability the task calls, the price the payee signed for it, the
// tier's limit and what is left of it today, the manual tier's, and the
// payee list — and gives a snapshot (a2ashape.PayNote): "sign" (paid.*) or
// one code of ux D1. The snapshot is stored on the task as it was taken
// (pay_note) and every surface renders it as it is (PaymentNote): the
// projection's anet.payment_note and status.message, the CLI's payment
// line, MCP's note. No surface works a limit out again (PAY-U6).
//
// The order of the automatic and the agent tier (PAY-D2) is fixed, and so
// is the override: when the manual tier would refuse the quote as well, the
// sentence is needs_operator_approval.manual_limit, which names the setting
// that lets `anet pay` through, so the command a sentence gives always
// works when run as it says (P-PAY-6 ②).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// PayInput is what one payment decision reads (HPP PAY-D1).
type PayInput struct {
	// Purpose is the tier: module.PurposeTaskAuto, PurposeTaskAgent or
	// PurposeTaskManual.
	Purpose string
	// Amount is the quoted amount of the option to be paid.
	Amount uint64
	// CapID is the capability the task calls, from this node's own record
	// of what it asked (never the quote's resource); "" for a text task.
	CapID string
	// Price is the price the payee signed for CapID (PAY-C10 case 2). It
	// is read only when the tier's limit is above 0 and CapID is set.
	Price cardPrice
	// Single and Left are the tier's per-payment limit and what the tier
	// may still pay today (HPP §0.3).
	Single, Left uint64
	// ManualSingle and ManualLeft are the manual tier's: explicit_max and
	// daily_max − spent_today. SpentToday is what daily_max counts today,
	// for the daily_max that would let the payment through.
	ManualSingle, ManualLeft, SpentToday uint64
	// PayeesReadable says the payee list could be read; PayeeListed that
	// the payee is on it.
	PayeesReadable, PayeeListed bool
	// Peer is the payee's AID.
	Peer string
	At   time.Time
}

// tierName is the {tier} of a purpose (ux D1).
func tierName(purpose string) string {
	switch purpose {
	case module.PurposeTaskAuto:
		return a2ashape.PayTierAuto
	case module.PurposeTaskAgent:
		return a2ashape.PayTierAgent
	}
	return a2ashape.PayTierManual
}

// payNoteSigns reports a snapshot that says to sign (ux D2).
func payNoteSigns(n a2ashape.PayNote) bool {
	switch n.Code {
	case "paid.auto", "paid.agent", "paid.manual":
		return true
	}
	return false
}

// decidePay is the decision (see the file comment). A quote of 0 credits
// is no payment any tier makes (zero_amount): the snapshot is empty.
//
// For the manual tier (PAY-D4) there is no price check and no payee list:
// paid.manual within explicit_max and today's daily_max, otherwise the code
// of ux D4 that AdmitSpend refuses it with (not a snapshot that is stored).
func decidePay(in PayInput) a2ashape.PayNote {
	n := a2ashape.PayNote{Amount: in.Amount, Tier: tierName(in.Purpose), Single: in.Single, Left: in.Left,
		Peer: in.Peer, At: in.At.UTC()}
	if in.Amount == 0 {
		return a2ashape.PayNote{}
	}
	if in.Purpose == module.PurposeTaskManual {
		n.Single, n.Left = in.ManualSingle, in.ManualLeft
		switch {
		case in.Amount > in.ManualSingle:
			n.Code = SpendOverSingle
		case in.Amount > in.ManualLeft:
			n.Code = SpendOverDaily
		default:
			n.Code, n.Left = "paid.manual", in.ManualLeft-in.Amount
		}
		return n
	}
	if in.Price.Have && in.Single > 0 && in.CapID != "" {
		price := in.Price.Price
		n.Price = &price
	}
	switch check := payCheck(in); {
	case in.Single == 0: // 1: the tier is off; no price is asked for
		n.Code = "needs_operator_approval.off"
	case in.CapID == "": // 2: a text task has no signed price
		n.Code, n.Why = "card_price_unavailable", priceWhyText
	case check == "card_price_unavailable": // 3
		n.Code, n.Why = check, in.Price.Why
		if n.Why == "" {
			n.Why = priceWhyHubUnreachable
		}
	case check == "quote_above_card_price": // 4
		n.Code = check
	case check == SpendPayeesUnread: // 5
		n.Code = "needs_operator_approval.payees_unreadable"
	case check == SpendPayeeNotAllowed:
		n.Code = "needs_operator_approval.payee_not_allowed"
	case check != "": // 6
		n.Code = "needs_operator_approval.over_limit"
	default: // 7
		n.Code = "paid." + n.Tier
		n.Left = in.Left - in.Amount
		return n
	}
	// The override: a quote the manual tier would refuse too says which
	// setting lets `anet pay` through, with exactly the value that does.
	overSingle, overDaily := in.Amount > in.ManualSingle, in.Amount > in.ManualLeft
	if overSingle || overDaily {
		n.Code, n.Single, n.Left = "needs_operator_approval.manual_limit", in.ManualSingle, in.ManualLeft
		need := spendSum(in.SpentToday, in.Amount)
		amount := in.Amount
		switch {
		case overSingle && overDaily:
			// Both block it: one command sets both (anet payments set
			// takes several key=value), so the next step still works in
			// one go.
			n.Key, n.Value = fmt.Sprintf("explicit_max=%d daily_max", in.Amount), &need
		case overSingle:
			n.Key, n.Value = "explicit_max", &amount
		default:
			n.Key, n.Value = "daily_max", &need
		}
	}
	return n
}

// payCheck is the first check of PAY-D2 a non-manual payment does not pass,
// "" when it passes them all: the code of ux D4 for a limit or the payee
// list (over_single_limit also for a tier that is off), or the D1 code of
// a price that is missing or below the quote. It is the machine-readable
// half of a decision that leaves the quote to a person — /tasks/pay's
// spend_refusal — and the first check that failed stays its answer when
// the sentence is manual_limit's (HPP PAY-D2).
func payCheck(in PayInput) string {
	switch {
	case in.Single == 0:
		return SpendOverSingle
	case in.CapID == "" || !in.Price.Have:
		return "card_price_unavailable"
	case in.Amount > in.Price.Price:
		return "quote_above_card_price"
	case !in.PayeesReadable:
		return SpendPayeesUnread
	case !in.PayeeListed:
		return SpendPayeeNotAllowed
	case in.Amount > in.Single:
		return SpendOverSingle
	case in.Amount > in.Left:
		if countsAgentDaily(in.Purpose) && in.Amount <= in.ManualLeft {
			return SpendOverAgentDaily
		}
		return SpendOverDaily
	}
	return ""
}

// priceWhyText is card_price_unavailable's {why} for a text task (ux D1).
const priceWhyText = "text_task"

// payInput gathers what decidePay reads for paying opt on ix at purpose,
// asking the payee for its signed price only when the order needs it (a
// non-manual tier whose limit is above 0, on a capability task): from the
// cache, or within 10 seconds (PAY-C10 case 2). price, when not nil, is the
// signed price to use instead of asking (PAY-D5: a card that just arrived).
func (d *Daemon) payInput(ctx context.Context, ix *interactions.Interaction, amount uint64, payTo, purpose string,
	price *cardPrice) PayInput {
	st := d.SpendStatus()
	in := PayInput{Purpose: purpose, Amount: amount, Peer: payTo, At: d.now(),
		ManualSingle: st.ExplicitMax, ManualLeft: st.LeftToday, SpentToday: st.SpentToday, Left: st.tierLeft(purpose)}
	lim := SpendLimits{AutoMax: st.AutoMax, AgentMax: st.AgentMax, ExplicitMax: st.ExplicitMax}
	in.Single, _, _ = lim.single(purpose)
	in.CapID = taskCapability(ix)
	if purpose == module.PurposeTaskManual {
		return in
	}
	payees, err := d.readPayees()
	in.PayeesReadable = err == nil
	in.PayeeListed = err == nil && payees[payTo]
	if in.Single > 0 && in.CapID != "" {
		switch {
		case price != nil:
			in.Price = *price
		default:
			in.Price = d.signedPriceFor(ctx, ix.PeerAID, in.CapID, amount)
		}
	}
	return in
}

// taskCapability is the capability ix calls, from the request this node
// stored; "" for a text task or a request that does not read.
func taskCapability(ix *interactions.Interaction) string {
	if ix == nil || !ix.IsCapability {
		return ""
	}
	td, err := decodeTaskDoc(ix.RequestDoc)
	if err != nil {
		return ""
	}
	capID, _, ok := capabilityCall(td)
	if !ok {
		return ""
	}
	return capID
}

// putPayNote stores n as ix's payment decision, in tx.
func putPayNote(tx *interactions.Tx, ixID string, n a2ashape.PayNote) error {
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	_, err = tx.SetPayment(ixID, interactions.PayUpdate{Note: b})
	return err
}

// storePayNote stores n as ixID's payment decision and tells the task's
// watchers: its sentence changed.
func (d *Daemon) storePayNote(ixID string, n a2ashape.PayNote) {
	if n.Code == "" {
		return
	}
	if err := d.ix.Update(func(tx *interactions.Tx) error { return putPayNote(tx, ixID, n) }); err != nil {
		log.Printf("anet: %s: keep the payment decision: %v", ixID, err)
		return
	}
	d.publishState(ixID)
}

// storedPayNote is ix's stored payment decision; ok false when there is
// none (or it does not read).
func storedPayNote(ix *interactions.Interaction) (a2ashape.PayNote, bool) {
	if ix == nil || len(ix.PayNote) == 0 {
		return a2ashape.PayNote{}, false
	}
	n, err := a2ashape.ParsePayNote(ix.PayNote)
	if err != nil {
		return a2ashape.PayNote{}, false
	}
	n.Task = ix.ID
	return n, true
}

// failureNote is the snapshot of a payment that did not settle (HPP
// PAY-U5, ux D3): the row of the anet reason the provider gave, when it is
// one of D3's, else the a2a-x402 code's row, else SETTLEMENT_FAILED's. The
// provider's words only choose a row of this node's table; none of them is
// shown.
func failureNote(reason, code string, amount uint64, peer string, at time.Time) a2ashape.PayNote {
	n := a2ashape.PayNote{Amount: amount, Peer: peer, At: at.UTC()}
	switch {
	case reason != "" && a2ashape.PayNoteCode(reason) && !a2ashape.PayNoteWaits(reason) && !strings.HasPrefix(reason, "paid."):
		n.Code = reason
	case code != "" && a2ashape.PayNoteCode("x402:"+code):
		n.Code = "x402:" + code
	default:
		n.Code = "x402:" + x402a2a.CodeSettlementFailed
	}
	return n
}

// quoteAmount is the amount of a quoted option, 0 when it does not read.
func quoteAmount(opt payment.PaymentOption) uint64 {
	n, err := payment.ParseAmount(opt.Amount)
	if err != nil {
		return 0
	}
	return n
}

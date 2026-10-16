package daemon

// spend_check.go is anet.spend_check (CUX-S3): on a task this node started
// and that waits on a quote, who can pay it now — the agent (the node's
// agent tier signs it), the operator (`anet pay`) or nobody — and the tier
// whose first failing check stands in the way. It is a forecast, read-only:
// the signed price comes from the cached extended card (never fetched here),
// the limits from CheckSpend's snapshot; the verdict on a payment is the
// answer of the payment itself (CUX-S7). The order of the checks is
// decidePay's and payCheck's, which this file only calls (CUX-S2).

import (
	"context"
	"strconv"

	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// Reasons of anet.spend_check that are not a code of D4 or D1.
const (
	spendReasonQuoteExpired   = "quote_expired"
	spendReasonRailNotPayable = "rail_not_payable"
	spendReasonAutoFailed     = "auto_failed"
)

// SpendCheckInput is what spendCheck reads.
type SpendCheckInput struct {
	// Amount is the quoted amount of the option that would be paid.
	Amount uint64
	// Expired: the quote has lapsed. NotPayable: the option settles where
	// this node holds no credit (or pays someone other than the peer).
	Expired, NotPayable bool
	// Agent and Auto are the decisions' inputs for the two tiers that sign
	// on their own, with the same price and the same snapshot of the day.
	Agent, Auto PayInput
	// Manual is CheckSpend's answer for the manual tier: nil when `anet
	// pay` would pass.
	Manual *SpendRefusal
}

// spendCheck is the forecast (see the file comment). It is a pure function.
func spendCheck(in SpendCheckInput) a2ashape.SpendCheck {
	out := a2ashape.SpendCheck{Who: a2ashape.SpendWhoNobody, Amount: strconv.FormatUint(in.Amount, 10)}
	switch {
	case in.Expired:
		out.Reason = spendReasonQuoteExpired
		return out
	case in.NotPayable:
		out.Reason = spendReasonRailNotPayable
		return out
	}
	if payNoteSigns(decidePay(in.Agent)) {
		out.Who, out.ReasonTier = a2ashape.SpendWhoAgent, a2ashape.PayTierAuto
		if out.Reason = payCheck(in.Auto); out.Reason == "" {
			out.Reason = spendReasonAutoFailed
		}
		out.ReasonLimit = limitOf(in.Auto, out.Reason)
		return out
	}
	if in.Manual == nil {
		out.Who, out.ReasonTier = a2ashape.SpendWhoOperator, a2ashape.PayTierAgent
		if out.Reason = payCheck(in.Agent); out.Reason == "" {
			out.Reason = spendReasonAutoFailed
		}
		out.ReasonLimit = limitOf(in.Agent, out.Reason)
		return out
	}
	out.ReasonTier, out.Reason = a2ashape.PayTierManual, in.Manual.Code
	switch in.Manual.Code {
	case SpendOverSingle:
		out.ReasonLimit = decimal(in.Manual.Single)
	case SpendOverDaily:
		out.ReasonLimit = decimal(leftOf(in.Manual.Daily, in.Manual.Spent))
	}
	return out
}

// limitOf is the bound of a limit reason for the tier in: the per-payment
// limit, what daily_max leaves today, or what agent_daily_max leaves. Other
// reasons have none. It is below the amount whenever the reason is that.
func limitOf(in PayInput, reason string) *string {
	switch reason {
	case SpendOverSingle:
		return decimal(in.Single)
	case SpendOverDaily:
		return decimal(in.ManualLeft)
	case SpendOverAgentDaily:
		return decimal(in.Left)
	}
	return nil
}

func decimal(n uint64) *string {
	s := strconv.FormatUint(n, 10)
	return &s
}

// spendCheckFor is anet.spend_check of ix, nil when the task does not have
// one: not a task this node started, no quote waiting, no payment module,
// or a quote that reads as nothing to pay.
func (d *Daemon) spendCheckFor(ix *interactions.Interaction) *a2ashape.SpendCheck {
	if ix == nil || ix.Role != interactions.RoleOutbound || ix.PayState != interactions.PayRequired || ix.IsTerminal() {
		return nil
	}
	p := d.payer()
	pr := storedQuote(ix)
	if p == nil || pr == nil {
		return nil
	}
	opt := &pr.Accepts[0]
	if len(pr.Accepts) > 1 {
		if picked := pickRail(p, pr.Accepts); picked != nil {
			opt = picked
		}
	}
	amount := quoteAmount(*opt)
	if amount == 0 {
		return nil
	}
	home := p.HomeNetwork()
	in := SpendCheckInput{Amount: amount,
		Expired:    ix.QuoteExpiresAt > 0 && int64(d.nowMS()) > ix.QuoteExpiresAt,
		NotPayable: opt.Scheme != payment.SchemeCredit || home == "" || opt.Network != home || opt.PayTo != ix.PeerAID}
	if !in.Expired && !in.NotPayable {
		var price cardPrice
		if capID := taskCapability(ix); capID != "" {
			kel, _ := d.peerKEL(ix.PeerAID)
			if c, ok := d.cachedExtCard(ix.PeerAID, kel, d.nowMS()); ok {
				price = cardPrice{Have: true, Price: c.Prices[capID]}
			}
		}
		in.Agent = d.payInput(context.Background(), ix, amount, opt.PayTo, module.PurposeTaskAgent, &price)
		in.Auto = d.payInput(context.Background(), ix, amount, opt.PayTo, module.PurposeTaskAuto, &price)
		in.Manual = d.CheckSpend(opt.PayTo, amount, module.PurposeTaskManual)
	}
	c := spendCheck(in)
	return &c
}

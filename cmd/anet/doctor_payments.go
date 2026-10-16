package main

import (
	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// paymentsCardCheck is payments.card (ux §5.2, HPP §5.6): when this node
// has a priced capability, public or for the allow and trust lists only,
// its extended cards are made, verify under its own KEL and state exactly
// the prices of their skills; its public card says an extended card may be
// asked for; and every price is between 1 and 2^63-1. A node whose priced
// capabilities are all allow_only, with nothing public, has no public card
// and so no extended card: warned, payments.card_no_public. Only the
// running daemon can make the cards, so it answers from GET /status
// (UX-K2): absent there when nothing is priced, and unknown while the
// daemon is down.
func paymentsCardCheck(say func(id, status, key string, args a2ashape.Args),
	add func(id, status, detail, hint string), rep *doctorReport, env doctorEnv, layout daemon.Layout) {
	st, ok := runtimeStatus(env, rep, say, "payments.card", layout)
	if !ok {
		return
	}
	c, _ := st["payments_card"].(map[string]any)
	if c == nil {
		return // no priced skill: nothing to check
	}
	switch c["status"] {
	case stOK:
		add("payments.card", stOK, "", "")
	case stWarn:
		if c["reason"] == "payments.card_no_public" {
			say("payments.card", stWarn, "payments.card_no_public", nil)
			return
		}
		fallthrough // no other warning: told as a failure to make the cards
	default:
		reason, _ := c["reason"].(string)
		if reason != "payments.price_invalid" {
			reason = "payments.card_failed"
		}
		args := a2ashape.Args{}
		if m, ok := c["args"].(map[string]any); ok {
			for k, v := range m {
				if s, ok := v.(string); ok {
					args[k] = s
				}
			}
		}
		say("payments.card", stFail, reason, args)
	}
}

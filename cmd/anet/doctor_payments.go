package main

import (
	"encoding/json"
	"fmt"
	"time"

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

// paymentsAuditCheck is payments.audit (HPP §5.6): the newest row of the
// chain of balance proofs the daemon keeps. Passed: ok. Periods that could
// not be checked in time: warn. Any other finding: fail, in the words of its
// sentence. Nothing before the first check.
func paymentsAuditCheck(say func(id, status, key string, args a2ashape.Args),
	add func(id, status, detail, hint string), rep *doctorReport, env doctorEnv, layout daemon.Layout) {
	st, ok := runtimeStatus(env, rep, say, "payments.audit", layout)
	if !ok {
		return
	}
	a, _ := st["payments_audit"].(map[string]any)
	if a == nil {
		return
	}
	switch result, _ := a["result"].(string); result {
	case "ok":
		say("payments.audit", stOK, "payments.audit_ok", a2ashape.Args{"time": jsonTime(a["checked_at"])})
	case "":
	default:
		key, args := "proof_invalid", a2ashape.Args{"code": "proof", "time": jsonTime(a["close"])}
		if f, _ := a["finding"].(map[string]any); f != nil {
			if c, _ := f["code"].(string); c != "" {
				key = c
			}
			if m, _ := f["args"].(map[string]any); m != nil {
				args = findingArgs(m)
			}
		}
		level := stFail
		if key == "proof_expired" || result == "proof_expired" {
			level, key = stWarn, "proof_expired"
			if _, ok := args["n"]; !ok {
				args["n"] = "1"
			}
		}
		say("payments.audit", level, key, args)
	}
}

// jsonTime reads a unix-millisecond time out of a decoded JSON number.
func jsonTime(v any) time.Time {
	switch x := v.(type) {
	case float64:
		return time.UnixMilli(int64(x)).UTC()
	case json.Number:
		if ms, err := x.Int64(); err == nil {
			return time.UnixMilli(ms).UTC()
		}
	}
	return time.Time{}
}

// hubProbationCheck is hub.probation (HPP §5.6): info while the account is on
// probation or its trial credits are still to come.
func hubProbationCheck(say func(id, status, key string, args a2ashape.Args),
	add func(id, status, detail, hint string), rep *doctorReport, env doctorEnv, layout daemon.Layout) {
	st, ok := runtimeStatus(env, rep, say, "hub.probation", layout)
	if !ok {
		return
	}
	acct, _ := st["hub_account"].(map[string]any)
	if acct == nil {
		return
	}
	hub, _ := st["hub_url"].(string)
	until := jsonTime(acct["probation_until"])
	switch trial, _ := acct["trial"].(string); {
	case !until.IsZero() && until.After(time.Now()):
		say("hub.probation", stInfo, "registered.probation", a2ashape.Args{"hub": hub, "time": until})
	case trial == "pending":
		say("hub.probation", stInfo, "balance.trial_pending", nil)
	}
}

// clockRetriesCheck is clock.ts_retries (HPP PAY-R11): more than ten
// signatures in the last hour that the hub found behind its floor. Only the
// warning is told here; the rest of the clock check is the identity area's.
func clockRetriesCheck(say func(id, status, key string, args a2ashape.Args),
	add func(id, status, detail, hint string), rep *doctorReport, env doctorEnv, layout daemon.Layout) {
	if !rep.Control.Running || env.status == nil {
		return
	}
	st, err := env.status(rep.Control.Addr, layout.ControlTokenPath())
	if err != nil {
		return
	}
	c, _ := st["clock"].(map[string]any)
	if n, _ := c["ts_retries"].(float64); n > 10 {
		say("clock", stWarn, "clock.ts_retries", a2ashape.Args{"n": fmt.Sprintf("%.0f", n)})
	}
}

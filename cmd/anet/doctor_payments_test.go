package main

import (
	"reflect"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// payments.card (ux §5.2): the daemon's finding from GET /status?doctor=1
// told in doctor's language, with the hint of its sentence; nothing when
// the daemon reports none (nothing priced); unknown while the daemon is
// down (UX-K2).
func TestPaymentsCardDoctorCheck(t *testing.T) {
	l := freshInit(t)
	run := func(running bool, st map[string]any) []doctorCheck {
		env := testDoctorEnv(t)
		rep := &doctorReport{}
		rep.Control.Running = running
		env.status = func(addr, token string) (map[string]any, error) { return st, nil }
		var out []doctorCheck
		say := func(id, status, key string, args a2ashape.Args) {
			out = append(out, sayCheck(id, status, key, args, a2ashape.English))
		}
		add := func(id, status, detail, hint string) {
			out = append(out, doctorCheck{ID: id, Status: status, Detail: detail, Hint: hint})
		}
		paymentsCardCheck(say, add, rep, env, l)
		return out
	}
	if got := run(false, nil); len(got) != 1 || got[0].ID != "payments.card" || got[0].Status != stUnknown ||
		got[0].Reason != doctorDaemonDown {
		t.Errorf("daemon down: %+v", got)
	}
	if got := run(true, map[string]any{"aid": "x"}); len(got) != 0 {
		t.Errorf("nothing priced: %+v", got)
	}
	if got := run(true, map[string]any{"payments_card": map[string]any{"status": "ok"}}); !reflect.DeepEqual(got,
		[]doctorCheck{{ID: "payments.card", Status: stOK}}) {
		t.Errorf("ok: %+v", got)
	}
	got := run(true, map[string]any{"payments_card": map[string]any{"status": "fail", "reason": "payments.price_invalid",
		"args": map[string]any{"name": "vision.describe"}}})
	want := sayCheck("payments.card", stFail, "payments.price_invalid", a2ashape.Args{"name": "vision.describe"}, a2ashape.English)
	if len(got) != 1 || got[0] != want || want.Hint != noHint {
		t.Errorf("price_invalid: %+v, want %+v", got, want)
	}
	got = run(true, map[string]any{"payments_card": map[string]any{"status": "fail", "reason": "payments.card_failed",
		"args": map[string]any{"code": "list_verify"}}})
	want = sayCheck("payments.card", stFail, "payments.card_failed", a2ashape.Args{"code": "list_verify"}, a2ashape.English)
	if len(got) != 1 || got[0] != want || want.Hint != "anet doctor --json" {
		t.Errorf("card_failed: %+v, want %+v", got, want)
	}
	// A reason this doctor does not know is told as a failure to make the
	// cards, not passed through as a sentence key.
	got = run(true, map[string]any{"payments_card": map[string]any{"status": "fail", "reason": "something.else"}})
	if len(got) != 1 || got[0].Reason != "payments.card_failed" || got[0].Status != stFail {
		t.Errorf("unknown reason: %+v", got)
	}
}

package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
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
	// Priced allow_only capabilities and nothing public: no extended card,
	// a warning in its own words (the owner's 2026-10-10 ruling, UX-4).
	got = run(true, map[string]any{"payments_card": map[string]any{"status": "warn", "reason": "payments.card_no_public"}})
	want = sayCheck("payments.card", stWarn, "payments.card_no_public", nil, a2ashape.English)
	if len(got) != 1 || got[0] != want || want.Hint != noHint || !strings.Contains(want.Detail, "make at least one capability public") {
		t.Errorf("card_no_public: %+v, want %+v", got, want)
	}
	// Another warning is not passed through either.
	got = run(true, map[string]any{"payments_card": map[string]any{"status": "warn", "reason": "something.else"}})
	if len(got) != 1 || got[0].Reason != "payments.card_failed" || got[0].Status != stFail {
		t.Errorf("unknown warning: %+v", got)
	}
	// A reason this doctor does not know is told as a failure to make the
	// cards, not passed through as a sentence key.
	got = run(true, map[string]any{"payments_card": map[string]any{"status": "fail", "reason": "something.else"}})
	if len(got) != 1 || got[0].Reason != "payments.card_failed" || got[0].Status != stFail {
		t.Errorf("unknown reason: %+v", got)
	}
}

// payments.unchargeable (MOD-2): priced capabilities on a node without the
// payment module are warned, with the count and how the module is absent;
// nothing when the module is available, nothing priced, or the daemon is
// down (the other checks of the daemon's findings say that).
func TestDoctorUnchargeable(t *testing.T) {
	l := freshInit(t)
	run := func(without []string, running bool, st map[string]any) []doctorCheck {
		env := testDoctorEnv(t)
		rep := &doctorReport{}
		rep.Control.Running = running
		env.status = func(addr, token string) (map[string]any, error) { return st, nil }
		av, err := module.NewAvailability(without)
		if err != nil {
			t.Fatal(err)
		}
		var out []doctorCheck
		say := func(id, status, key string, args a2ashape.Args) {
			out = append(out, sayCheck(id, status, key, args, a2ashape.English))
		}
		paymentsUnchargeableCheck(say, rep, env, l, av)
		return out
	}
	priced := map[string]any{"payments_unchargeable": map[string]any{"n": float64(2)}}
	got := run([]string{"x402"}, true, priced)
	want := sayCheck("payments.unchargeable", stWarn, "payments.unchargeable",
		a2ashape.Args{"n": 2, "m": "x402", "reason": absentReason()}, a2ashape.English)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("without: %+v, want %+v", got, want)
	}
	for _, frag := range []string{"priced capabilities (2)", "payment module (x402", "remove the prices"} {
		if !strings.Contains(want.Detail, frag) {
			t.Errorf("sentence %q lacks %q", want.Detail, frag)
		}
	}
	if got := run([]string{"x402"}, true, map[string]any{"aid": "x"}); len(got) != 0 {
		t.Errorf("nothing priced: %+v", got)
	}
	if got := run([]string{"x402"}, false, priced); len(got) != 0 {
		t.Errorf("daemon down: %+v", got)
	}
	if module.Compiled() != nil && (module.Availability{}).Available("x402") {
		if got := run(nil, true, priced); len(got) != 0 {
			t.Errorf("payment module available: %+v", got)
		}
	}
}

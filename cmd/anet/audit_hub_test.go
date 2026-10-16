package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

func decodeAudit(t *testing.T, s string) auditHubAnswer {
	t.Helper()
	var a auditHubAnswer
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// HPP §5.2 text F: the chain line, the proof line, and the closing line.
func TestAuditHubTextF(t *testing.T) {
	a := decodeAudit(t, `{"epochs":12,"chain_ok":true,"proofs_ok":11,"latest":{"to_day":20000,"balance":37},"findings":[],"verified":true}`)
	got := auditHubLines(a, a2ashape.English, time.Now())
	if len(got) != 3 || !strings.HasPrefix(got[0], "The hub's issuance chain: 12 periods") ||
		!strings.Contains(got[1], "11 periods checked") || !strings.Contains(got[1], "balance 37 credits") ||
		got[2] != "No problems found." {
		t.Fatalf("lines: %q", got)
	}
}

// Each finding is one line of its own sentence, with its command; the
// closing line is not there.
func TestAuditHubTellsEachFinding(t *testing.T) {
	a := decodeAudit(t, `{"epochs":3,"chain_ok":true,"proofs_ok":1,"latest":{"to_day":20000,"balance":37},"findings":[
	  {"code":"balance_mismatch","level":"fail","args":{"time":1760000000000,"amount":135,"need":"130"}},
	  {"code":"omitted_from_ledger","level":"fail","args":{"amount":12}},
	  {"code":"proof_expired","level":"warn","args":{"n":2,"time":1760000000000}}],"verified":false}`)
	got := strings.Join(auditHubLines(a, a2ashape.English, time.Now()), "\n")
	for _, want := range []string{"reported your balance as 135", "say 130", "12-credit entry", "2 periods could not be checked"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "No problems found.") {
		t.Errorf("findings, and the closing line says none:\n%s", got)
	}
}

// Text G: the balance, then the account's state if it came in on probation.
func TestBalanceTextG(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ms := func(d time.Duration) string { return json.Number(itoa(now.Add(d).UnixMilli())).String() }
	probation := `{"hub":"https://hub.example","balance":20,"probation_until":` + ms(48*time.Hour) + `,"trial":"pending"}`
	got := balanceLines([]byte(probation), a2ashape.English, now)
	if len(got) != 2 || got[0] != "Balance 20 credits (held at the hub https://hub.example)." || !strings.HasPrefix(got[1], "The new account is on probation until") {
		t.Fatalf("probation: %q", got)
	}
	pending := `{"hub":"h","balance":20,"probation_until":` + ms(-time.Hour) + `,"trial":"pending"}`
	if got := balanceLines([]byte(pending), a2ashape.English, now); len(got) != 2 || !strings.HasPrefix(got[1], "Probation is over; the trial credits are still to come") {
		t.Fatalf("pending: %q", got)
	}
	granted := `{"hub":"h","balance":40,"probation_until":` + ms(-time.Hour) + `,"trial":"granted","entries":[{"delta":20,"reason":"trial credit","at":"2026-10-14T09:05:00.000000000Z"}]}`
	if got := balanceLines([]byte(granted), a2ashape.English, now); len(got) != 2 || !strings.Contains(got[1], "20 trial credits arrived") {
		t.Fatalf("granted: %q", got)
	}
	// An account the operator admitted has no second line; nor does one
	// whose payment module is absent have a balance line.
	if got := balanceLines([]byte(`{"hub":"h","balance":20}`), a2ashape.English, now); len(got) != 1 {
		t.Fatalf("admitted: %q", got)
	}
	if got := balanceLines([]byte(`{"hub":"h","balance":"module_absent","trial":"pending","probation_until":`+ms(time.Hour)+`}`), a2ashape.English, now); len(got) != 1 ||
		!strings.Contains(got[0], "probation") {
		t.Fatalf("module absent: %q", got)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// payments.audit, hub.probation and clock.ts_retries from /status?doctor=1.
func TestPaymentsAuditDoctorCheck(t *testing.T) {
	l := freshInit(t)
	run := func(st map[string]any, f func(say func(id, status, key string, args a2ashape.Args), add func(id, status, detail, hint string), rep *doctorReport, env doctorEnv, layout daemon.Layout)) []doctorCheck {
		env := testDoctorEnv(t)
		rep := &doctorReport{}
		rep.Control.Running = true
		env.status = func(addr, token string) (map[string]any, error) { return st, nil }
		var out []doctorCheck
		say := func(id, status, key string, args a2ashape.Args) {
			out = append(out, sayCheck(id, status, key, args, a2ashape.English))
		}
		add := func(id, status, detail, hint string) {
			out = append(out, doctorCheck{ID: id, Status: status, Detail: detail, Hint: hint})
		}
		f(say, add, rep, env, l)
		return out
	}
	if got := run(map[string]any{}, paymentsAuditCheck); len(got) != 0 {
		t.Errorf("before the first check: %+v", got)
	}
	got := run(map[string]any{"payments_audit": map[string]any{"result": "ok", "checked_at": float64(1760000000000)}}, paymentsAuditCheck)
	if len(got) != 1 || got[0].Status != stOK || got[0].Reason != "payments.audit_ok" {
		t.Errorf("ok: %+v", got)
	}
	got = run(map[string]any{"payments_audit": map[string]any{"result": "balance_mismatch", "close": float64(1760000000000),
		"finding": map[string]any{"code": "balance_mismatch", "args": map[string]any{"time": float64(1760000000000), "amount": float64(135), "need": "130"}}}}, paymentsAuditCheck)
	if len(got) != 1 || got[0].Status != stFail || got[0].Reason != "balance_mismatch" || !strings.Contains(got[0].Detail, "135") {
		t.Errorf("fail: %+v", got)
	}
	got = run(map[string]any{"payments_audit": map[string]any{"result": "proof_expired"}}, paymentsAuditCheck)
	if len(got) != 1 || got[0].Status != stWarn || got[0].Reason != "proof_expired" {
		t.Errorf("warn: %+v", got)
	}
	got = run(map[string]any{"hub_url": "https://hub.example", "hub_account": map[string]any{
		"probation_until": float64(time.Now().Add(time.Hour).UnixMilli()), "trial": "pending"}}, hubProbationCheck)
	if len(got) != 1 || got[0].Status != stInfo || got[0].Reason != "registered.probation" {
		t.Errorf("probation: %+v", got)
	}
	got = run(map[string]any{"hub_account": map[string]any{"probation_until": nil, "trial": "pending"}}, hubProbationCheck)
	if len(got) != 1 || got[0].Reason != "balance.trial_pending" {
		t.Errorf("trial pending: %+v", got)
	}
	if got := run(map[string]any{"hub_account": map[string]any{"probation_until": nil, "trial": ""}}, hubProbationCheck); len(got) != 0 {
		t.Errorf("admitted account: %+v", got)
	}
	if got := run(map[string]any{"clock": map[string]any{"ts_retries": float64(10)}}, clockRetriesCheck); len(got) != 0 {
		t.Errorf("ten retries: %+v", got)
	}
	got = run(map[string]any{"clock": map[string]any{"ts_retries": float64(11)}}, clockRetriesCheck)
	if len(got) != 1 || got[0].Status != stWarn || got[0].Reason != "clock.ts_retries" || !strings.Contains(got[0].Detail, "11 times") {
		t.Errorf("eleven retries: %+v", got)
	}
}

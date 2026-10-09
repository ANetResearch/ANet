package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// budgetIntRe is a JSON integer literal: no fraction, no exponent.
var budgetIntRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// FuzzBudgetValue (LEC §3.2): whatever config.json holds as a run budget reads without error, and it
// counts as a budget exactly when it is a JSON integer from 0 to 1000 — the same value after being
// written back and read again.
func FuzzBudgetValue(f *testing.F) {
	for _, v := range []string{`0`, `20`, `1000`, `1001`, `-1`, `-0`, `2.5`, `1e2`, `20.0`, `"5"`, `true`, `null`,
		`[1]`, `{}`, `0005`, `9223372036854775808`, `-9223372036854775809`} {
		f.Add([]byte(`{"runs_per_day":` + v + `,"runs_per_peer_per_day":` + v + `}`))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var c AutoReplySandboxConfig
		if json.Unmarshal(data, &c) != nil {
			return
		}
		for _, v := range []*BudgetValue{c.RunsPerDay, c.RunsPerPeerPerDay} {
			if v == nil {
				continue
			}
			n, ok := v.Int()
			raw := strings.TrimSpace(string(v.raw))
			want := false
			if budgetIntRe.MatchString(raw) {
				i, err := strconv.Atoi(raw)
				want = err == nil && i >= 0 && i <= runBudgetMax
				if want && ok && i != n {
					t.Fatalf("%s reads as %d", raw, n)
				}
			}
			if ok != want {
				t.Fatalf("%s: a budget %v, want %v", raw, ok, want)
			}
		}
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("written back: %v", err)
		}
		var again AutoReplySandboxConfig
		if err := json.Unmarshal(b, &again); err != nil {
			t.Fatalf("read again %s: %v", b, err)
		}
		cfg := AutoReplyConfig{Sandbox: &c}
		cfg2 := AutoReplyConfig{Sandbox: &again}
		d1, p1 := cfg.runBudgets()
		d2, p2 := cfg2.runBudgets()
		if d1 != d2 || p1 != p2 || (checkRunBudgets(&cfg) == nil) != (checkRunBudgets(&cfg2) == nil) {
			t.Fatalf("%s and %s differ", data, b)
		}
		if checkRunBudgets(&cfg) == nil && (d1 < 0 || d1 > runBudgetMax || p1 < 0 || p1 > d1) {
			t.Fatalf("%s passes the check with budgets %d/%d", data, d1, p1)
		}
	})
}

// FuzzBudgetRunOf (BU-2): any payload on the chain under anet.autoreply.invoked is read without
// panicking, and counts only as an untrusted peer's sandboxed or openai run that was not turned away
// before it started, started duration_ms before its record.
func FuzzBudgetRunOf(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("..", "evtypes", "testdata", "autoreply_invoked_*.json"))
	for _, file := range files {
		if b, err := os.ReadFile(file); err == nil {
			f.Add(time.Now().UnixMilli(), b)
		}
	}
	f.Add(int64(0), []byte(`{"mode":"sandbox","exit":"ok","duration_ms":-5,"trusted":"yes"}`))
	f.Add(int64(-1), []byte(`{"mode":"openai","exit":"error","duration_ms":1e300,"peer_aid":7}`))
	f.Fuzz(func(t *testing.T, at int64, data []byte) {
		var p map[string]any
		if json.Unmarshal(data, &p) != nil {
			return
		}
		r, ok := budgetRunOf(at, p)
		if !ok {
			return
		}
		if trusted, _ := p["trusted"].(bool); trusted {
			t.Fatalf("a trusted peer's run counts: %s", data)
		}
		if m := p["mode"]; m != modeSandbox && m != modeOpenAI {
			t.Fatalf("mode %v counts: %s", m, data)
		}
		if e := p["exit"]; e == exitBudget || e == exitSandboxUnavailable {
			t.Fatalf("exit %v counts: %s", e, data)
		}
		if peer, _ := p["peer_aid"].(string); r.peer != peer {
			t.Fatalf("peer %q, payload %s", r.peer, data)
		}
		if r.start.UnixMilli() != at-int64Of(p["duration_ms"]) {
			t.Fatalf("start %d, record at %d with %v", r.start.UnixMilli(), at, p["duration_ms"])
		}
	})
}

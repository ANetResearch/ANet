package main

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// sandboxDoctorLayout is a fresh data directory with the auto_reply block given.
func sandboxDoctorLayout(t *testing.T, autoReply map[string]any) daemon.Layout {
	t.Helper()
	l := freshInit(t)
	b, err := os.ReadFile(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if autoReply != nil {
		m["auto_reply"] = autoReply
	}
	b, _ = json.Marshal(m)
	if err := os.WriteFile(l.ConfigPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

// sandboxFinding is doctor's sandbox finding, nil when it shows none.
func sandboxFinding(t *testing.T, l daemon.Layout, env doctorEnv) (*doctorCheck, *doctorReport) {
	t.Helper()
	rep, err := collectDoctor(l, env)
	if err != nil {
		t.Fatal(err)
	}
	var found *doctorCheck
	for i, c := range rep.Checks {
		if c.ID == "sandbox" {
			if found != nil {
				t.Fatalf("two sandbox findings: %+v", rep.Checks)
			}
			found = &rep.Checks[i]
		}
	}
	return found, rep
}

// LEC §5.5, ux §5.2 sandbox, UX-K6: doctor shows the sandbox only when untrusted or trusted peers run in
// it, and then says whether it holds: ok sandbox.ready (sandbox.ready_egress with the allowed hosts), or
// fail with the self-check's reason, its sentence and next step (UX-K4, K7); what the self-check saw is
// in the --json report.
func TestDoctorSandbox(t *testing.T) {
	lang := a2ashape.LangFrom(func(string) string { return "" })
	calls := 0
	check := func(c daemon.SandboxCheck) func() daemon.SandboxCheck {
		return func() daemon.SandboxCheck { calls++; return c }
	}
	env := testDoctorEnv(t)
	env.sandbox = check(daemon.SandboxCheck{Kind: "seatbelt"})

	for name, ar := range map[string]map[string]any{
		"no auto_reply":  nil,
		"untrusted off":  {"backend": "exec", "agent": "claude"},
		"trusted direct": {"backend": "exec", "agent": "claude", "trusted": "direct"},
	} {
		if f, _ := sandboxFinding(t, sandboxDoctorLayout(t, ar), env); f != nil {
			t.Errorf("%s: doctor shows the sandbox: %+v", name, f)
		}
	}
	if calls != 0 {
		t.Errorf("the self-check ran %d times with no sandbox in use", calls)
	}

	sandboxed := map[string]any{"backend": "exec", "agent": "claude", "untrusted": "sandbox"}
	f, rep := sandboxFinding(t, sandboxDoctorLayout(t, sandboxed), env)
	if f == nil || f.Status != stOK || f.Reason != "sandbox.ready" || f.Detail != a2ashape.Sentence("sandbox.ready", lang, nil) {
		t.Errorf("a sandbox that holds: %+v", f)
	}
	if rep.Sandbox == nil || rep.Sandbox.Kind != "seatbelt" || calls != 1 {
		t.Errorf("report %+v after %d self-checks", rep.Sandbox, calls)
	}
	trusted := map[string]any{"backend": "exec", "agent": "claude", "trusted": "sandbox",
		"sandbox": map[string]any{"egress_allow": []any{"pypi.org", "files.pythonhosted.org"}}}
	f, _ = sandboxFinding(t, sandboxDoctorLayout(t, trusted), env)
	want := a2ashape.Sentence("sandbox.ready_egress", lang, a2ashape.Args{"n": 2, "hosts": "pypi.org, files.pythonhosted.org"})
	if f == nil || f.Status != stOK || f.Reason != "sandbox.ready_egress" || f.Detail != want || !strings.Contains(f.Detail, "pypi.org") {
		t.Errorf("trusted peers in the sandbox, two hosts allowed: %+v", f)
	}

	for _, reason := range []string{"no_sandbox_exec", "fs_isolation_failed", "netns_failed", "platform_unsupported", "userns_blocked"} {
		env.sandbox = check(daemon.SandboxCheck{Kind: "seatbelt", Reason: reason, Detail: "1 home (listed the real home)"})
		f, rep := sandboxFinding(t, sandboxDoctorLayout(t, sandboxed), env)
		if f == nil || f.Status != stFail || f.Reason != reason || f.Detail != a2ashape.Sentence(reason, lang, nil) {
			t.Errorf("%s: %+v", reason, f)
			continue
		}
		if next := a2ashape.Next(reason, lang, nil); next != "" && f.Hint != next || next == "" && f.Hint != noHint {
			t.Errorf("%s: hint %q, want %q", reason, f.Hint, next)
		}
		if rep.OK || rep.Sandbox == nil || rep.Sandbox.Detail != "1 home (listed the real home)" {
			t.Errorf("%s: ok %v, report %+v", reason, rep.OK, rep.Sandbox)
		}
		if !doctorIDKnown(f.ID) || !slices.Contains(doctorIDKeys("sandbox"), reason) {
			t.Errorf("%s: not registered for the sandbox item", reason)
		}
	}
}

// doctorIDKeys are the sentence keys registered for a doctor id.
func doctorIDKeys(id string) []string {
	for _, d := range doctorIDs {
		if d.id == id {
			return d.keys
		}
	}
	return nil
}

// LEC §5.5: while the daemon runs, doctor takes the sandbox's state from GET /status when the daemon
// reports one of this item's states, and runs the self-check itself only otherwise.
func TestDoctorSandboxFromTheRunningDaemon(t *testing.T) {
	calls := 0
	env := testDoctorEnv(t)
	env.sandbox = func() daemon.SandboxCheck { calls++; return daemon.SandboxCheck{Kind: "seatbelt", Reason: "netns_failed"} }
	status := map[string]any{}
	env.running = func(string, string) bool { return true }
	env.status = func(string, string) (map[string]any, error) { return status, nil }
	l := sandboxDoctorLayout(t, map[string]any{"backend": "exec", "agent": "claude", "untrusted": "sandbox"})
	for _, tc := range []struct {
		sandbox map[string]any
		reason  string
		calls   int
	}{
		{map[string]any{"state": "ready", "kind": "seatbelt"}, "sandbox.ready", 0},
		{map[string]any{"state": "unavailable", "reason": "fs_isolation_failed", "kind": "seatbelt"}, "fs_isolation_failed", 0},
		// Not this item's reason, or no state at all: doctor checks for itself.
		{map[string]any{"state": "unavailable", "reason": "no_api_key", "kind": "seatbelt"}, "netns_failed", 1},
		{nil, "netns_failed", 1},
	} {
		calls = 0
		status["auto_reply"] = map[string]any{"sandbox": tc.sandbox}
		f, _ := sandboxFinding(t, l, env)
		if f == nil || f.Reason != tc.reason || calls != tc.calls {
			t.Errorf("status %v: %+v after %d self-checks, want %s after %d", tc.sandbox, f, calls, tc.reason, tc.calls)
		}
	}
	if defaultDoctorEnv().sandbox == nil {
		t.Error("doctor's own environment runs no self-check")
	}
}

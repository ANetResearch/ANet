package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

	for _, reason := range []string{"no_sandbox_exec", "fs_isolation_failed", "netns_failed", "seccomp_failed", "platform_unsupported",
		"userns_blocked", "sandbox_start_failed"} {
		env.sandbox = check(daemon.SandboxCheck{Kind: "seatbelt", Reason: reason, Detail: "1 home (listed the real home)"})
		f, rep := sandboxFinding(t, sandboxDoctorLayout(t, sandboxed), env)
		// In doctor the three self-check reasons say what the self-check saw (their .doctor variants), and
		// sandbox_start_failed says bubblewrap's first line; the others are the sentence everywhere shows.
		key, args := reason, a2ashape.Args(nil)
		switch reason {
		case "fs_isolation_failed", "netns_failed", "seccomp_failed":
			key, args = reason+".doctor", a2ashape.Args{"detail": "1 home (listed the real home)"}
		case "sandbox_start_failed":
			args = a2ashape.Args{"error": "1 home (listed the real home)"}
		}
		if f == nil || f.Status != stFail || f.Reason != key || f.Detail != a2ashape.Sentence(key, lang, args) {
			t.Errorf("%s: %+v", reason, f)
			continue
		}
		if next := a2ashape.Next(key, lang, args); next != "" && f.Hint != next || next == "" && f.Hint != noHint {
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
	env.sandbox = func() daemon.SandboxCheck {
		calls++
		return daemon.SandboxCheck{Kind: "seatbelt", Reason: "netns_failed"}
	}
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

// ux §5.2: doctor shows the items after sandbox when it is shown, and the agent and the model service are
// told apart: an agent that cannot run is sandbox.agent, a missing key sandbox.upstream; a list of hosts
// the daemon would refuse is sandbox.egress; a proxy variable warns.
func TestDoctorSandboxDetailItems(t *testing.T) {
	env := testDoctorEnv(t)
	env.sandbox = func() daemon.SandboxCheck { return daemon.SandboxCheck{Kind: "bwrap"} }
	env.getenv = func(k string) string {
		if k == "HTTPS_PROXY" {
			return "http://proxy.invalid:3128"
		}
		return ""
	}
	ids := func(ar map[string]any) map[string][]doctorCheck {
		rep, err := collectDoctor(sandboxDoctorLayout(t, ar), env)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string][]doctorCheck{}
		for _, c := range rep.Checks {
			if strings.HasPrefix(c.ID, "sandbox") || c.ID == "auto_reply" {
				m[c.ID] = append(m[c.ID], c)
			}
		}
		return m
	}
	m := ids(map[string]any{"backend": "exec", "agent": "claude", "untrusted": "sandbox"})
	if c := m["sandbox.upstream"]; len(c) != 1 || c[0].Status != stFail || c[0].Reason != "no_api_key" || m["sandbox.agent"][0].Status != stOK {
		t.Errorf("no key: %+v", m)
	}
	if c := m["sandbox.egress"]; len(c) != 2 || c[0].Status != stOK || c[1].Status != stWarn || c[1].Reason != "sandbox.proxy_env" {
		t.Errorf("egress: %+v", c)
	}
	if c := m["sandbox.budget"]; len(c) != 1 || c[0].Status != stUnknown || c[0].Reason != doctorDaemonDown {
		t.Errorf("budget without the daemon: %+v", c)
	}
	if c := m["auto_reply"]; len(c) != 1 || c[0].Status != stOK || !strings.Contains(c[0].Detail, "exec / claude") {
		t.Errorf("auto_reply: %+v", c)
	}

	m = ids(map[string]any{"backend": "exec", "agent": "cursor", "untrusted": "sandbox", "api_key": "k"})
	if c := m["sandbox.agent"]; len(c) != 1 || c[0].Status != stFail || c[0].Reason != "agent_unsupported" || len(m["sandbox.upstream"]) != 0 {
		t.Errorf("cursor: %+v", m)
	}
	m = ids(map[string]any{"backend": "exec", "agent": "codex", "untrusted": "sandbox", "api_key": "k", "api_base": "https://example.org/v1", "model": "m"})
	if c := m["sandbox.agent"]; len(c) != 1 || c[0].Reason != "agent_unsupported.combo" {
		t.Errorf("combo: %+v", c)
	}
	m = ids(map[string]any{"backend": "exec", "agent": "claude", "untrusted": "sandbox", "api_key": "k",
		"sandbox": map[string]any{"egress_allow": []string{"api.anthropic.com"}}})
	if c := m["sandbox.egress"]; len(c) < 1 || c[0].Status != stFail || c[0].Reason != "egress_allow_model_host" {
		t.Errorf("model host in egress_allow: %+v", c)
	}

	// Off: none of the sandbox items, and the auto-reply item says off.
	m = ids(map[string]any{"backend": "exec", "agent": "claude"})
	if len(m["sandbox"])+len(m["sandbox.agent"])+len(m["sandbox.budget"]) != 0 {
		t.Errorf("off: %+v", m)
	}
	if c := ids(nil)["auto_reply"]; len(c) != 1 || c[0].Reason != "autoreply.off" {
		t.Errorf("none: %+v", c)
	}
}

// ux §5.2 hub: the finding is the join state of hub_join.json in the words of /status.hub, graded by the
// state, whatever config.json says.
func TestDoctorHubReadsTheJoinRecord(t *testing.T) {
	for _, tc := range []struct {
		state, reason, status string
	}{
		{"pending_admission", "hub_pending_admission", stWarn},
		{"retrying", "hub_retrying", stWarn},
		{"refused", "hub_refused", stFail},
		{"registered", "registered", stOK},
		{"registered", "registered.probation", stOK},
	} {
		l := freshInit(t)
		rec := `{"v":1,"hub":"https://hub.example","state":"` + tc.state + `","reason":"` + tc.reason + `","since":1760000000000,"probation_until":9999999999999}`
		if err := os.WriteFile(filepath.Join(l.Root, "hub_join.json"), []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
		rep, err := collectDoctor(l, testDoctorEnv(t))
		if err != nil {
			t.Fatal(err)
		}
		var hub, probation *doctorCheck
		for i, c := range rep.Checks {
			switch c.ID {
			case "hub":
				hub = &rep.Checks[i]
			case "hub.probation":
				if rep.Checks[i].Status != stUnknown { // unknown: the runtime item, daemon not running
					probation = &rep.Checks[i]
				}
			}
		}
		if hub == nil || hub.Status != tc.status || (tc.state != "registered" && hub.Reason != tc.reason) {
			t.Errorf("%s: %+v", tc.reason, hub)
		}
		if (probation != nil) != (tc.reason == "registered.probation") {
			t.Errorf("%s: hub.probation %+v", tc.reason, probation)
		}
		if tc.state == "registered" && (!rep.Hub.Registered || rep.Hub.URL != "https://hub.example") {
			t.Errorf("%s: report %+v", tc.reason, rep.Hub)
		}
	}
}

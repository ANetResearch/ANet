//go:build darwin

package autoreply

// The supported agents in the real macOS sandbox (LEC §8 EXE-9, J8's subset; MOD-V3): run by the CI
// seatbelt job on macos-14 and macos-15, where a skip is a failure. The agent CLIs are the job's own
// installs; nothing here reaches a real model service.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seatbeltAgentCLI is the agent CLI the test runs: $ANET_TEST_<NAME> or the one on PATH. Missing, the test
// skips saying so (the seatbelt job installs it and treats a skip as a failure).
func seatbeltAgentCLI(t *testing.T, name string) string {
	t.Helper()
	if p := os.Getenv("ANET_TEST_" + strings.ToUpper(name)); p != "" {
		return p
	}
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("needs the %s CLI on PATH or in ANET_TEST_%s: %v", name, strings.ToUpper(name), err)
	}
	return p
}

// J8 (subset), MS-1 to MS-3, EG-3, UP-1 to UP-4 on a Mac: the real claude, in the real Seatbelt sandbox,
// reaches its model only through the run's keyed injection route — the scripted model service sees the
// operator's key and the pinned model, never the placeholder — runs the tool the model asks for, writes
// its result file in the work dir and replies; the run leaves no run directory.
func TestSeatbeltClaudeCompletesATask(t *testing.T) {
	claude := seatbeltAgentCLI(t, "claude")
	if pr := realSeatbeltProbe(t); pr.Err != nil {
		t.Fatalf("the sandbox failed its self-check: %v", pr.Err)
	}
	m := newScriptedAnthropic(t)
	old := egressBuiltinBase[egressUpstreamAnthropic]
	egressBuiltinBase[egressUpstreamAnthropic] = m.srv.URL
	t.Cleanup(func() { egressBuiltinBase[egressUpstreamAnthropic] = old })
	cfg := AutoReplyConfig{Backend: "exec", Agent: agentClaude, Command: claude, APIKey: testOperatorKey}
	up, err := resolveEgressUpstream(cfg)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := lookupAgent(agentClaude)
	plan := &sandboxPlan{AID: "EaidSeatbeltClaudeJ8", NeverBind: []string{filepath.Join(t.TempDir(), "never")}}
	if err := plan.prepare(); err != nil {
		t.Fatal(err)
	}
	plan.Egress, plan.AgentHome = up, spec.sandbox.homeFor(up)
	work := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	reply, err := InvokeAgent(ctx, execInvokeOpts{AgentID: agentClaude, Command: claude, Model: up.Model,
		Instructions: "You are in a sandbox. Do what the user asks with your tools, then answer briefly.",
		Conversation: "Write the answer to answer.txt in the working directory.",
		WorkDir:      work, Timeout: 4 * time.Minute, Sandbox: plan})
	reqs := m.requests()
	t.Logf("reply %q, %d model requests", reply, len(reqs))
	if err != nil || !strings.Contains(reply, j8Done) {
		t.Fatalf("claude in the sandbox: %q %v", reply, err)
	}
	if b, err := os.ReadFile(filepath.Join(work, "answer.txt")); err != nil || strings.TrimSpace(string(b)) != j8Answer {
		t.Errorf("the result file: %q %v", b, err)
	}
	if len(reqs) < 2 {
		t.Fatalf("the model service saw %d requests", len(reqs))
	}
	for _, r := range reqs {
		if r.Header.Get("x-api-key") != testOperatorKey || r.Header.Get("Authorization") != "" {
			t.Errorf("%s: credentials %q / %q", r.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"))
		}
		if mdl, ok := r.Body["model"]; ok && mdl != up.Model {
			t.Errorf("%s: model %v, want the pinned %s", r.Path, mdl, up.Model)
		}
	}
	if s := plan.egressSummary(); s == nil || s.ModelRequests != len(reqs) {
		t.Errorf("the run's egress counts %+v, the service saw %d", s, len(reqs))
	}
	if left := leftRunDirs(plan.AID); len(left) != 0 {
		t.Errorf("run directories left: %v", left)
	}
}

// MS-2, AV-4: a Python agent's installation is listed for the profile unless it lies where the agent may
// already read and execute — /bin, /usr/bin, the command line tools — since a framework build's python
// re-executes the interpreter in its app bundle under the prefix: a prefix the agent could only read (the
// system frameworks) or not at all (/Library/Frameworks, Homebrew, uv, pyenv) is listed.
func TestSeatbeltPythonRuntimeOutsideTheExecutableSystem(t *testing.T) {
	for p, want := range map[string]bool{
		"/usr/bin": true, "/bin": true,
		"/Library/Developer/CommandLineTools/Library/Frameworks/Python3.framework/Versions/3.9": true,
		"/System/Library/Frameworks/Python.framework/Versions/2.7":                              false,
		"/Library/Frameworks/Python.framework/Versions/3.12":                                    false,
		"/opt/homebrew/Cellar/python@3.12/3.12.4/Frameworks/Python.framework/Versions/3.12":     false,
		"/usr/lib": false, "/usr": false,
	} {
		if got := underSystemDir(p); got != want {
			t.Errorf("underSystemDir(%s) = %v, want %v", p, got, want)
		}
	}
	// A framework-style installation outside the system: its prefix (holding lib/python3.*) is listed.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(root, "Python.framework", "Versions", "3.12")
	for _, d := range []string{filepath.Join(prefix, "bin"), filepath.Join(prefix, "lib", "python3.12")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	py := filepath.Join(prefix, "bin", "python3.12")
	if err := os.WriteFile(py, []byte("\xcf\xfa\xed\xfe"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := pythonRuntime(py, py)
	if len(got) == 0 {
		t.Errorf("pythonRuntime lists nothing, want %s", prefix)
	}
	for _, d := range got {
		if d != prefix {
			t.Errorf("pythonRuntime lists %v, want only %s", got, prefix)
		}
	}
}

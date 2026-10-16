//go:build linux

package daemon

import (
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// sandboxOf is the sandbox object of d's /status.auto_reply.
func sandboxOf(t *testing.T, d *Daemon) map[string]any {
	t.Helper()
	st, _ := statusOf(t, d)
	ar, _ := st["auto_reply"].(map[string]any)
	sb, _ := ar["sandbox"].(map[string]any)
	if sb == nil {
		t.Fatalf("no sandbox object in /status: %v", st)
	}
	return sb
}

// LEC §2.3.3: /status.auto_reply.sandbox says off, unavailable with the reason and its sentence, or ready;
// a budget of 0 is off; exec shows api_base and trusted; the answer to /autoreply carries the same line.
func TestStatusSandboxStates(t *testing.T) {
	lang := a2ashape.Lang()
	defer func(f func(string) (string, error)) { bwrapLookPath = f }(bwrapLookPath)
	bwrapLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	d := newTestDaemon(t, "", false)

	// openai starts no process: nothing is sandboxed.
	postAutoReply(t, d, `{"backend":"openai","api_base":"http://127.0.0.1:9/v1","model":"m"}`)
	if sb := sandboxOf(t, d); sb["state"] != "off" || sb["reason"] != "sandbox.off" {
		t.Errorf("openai: %v", sb)
	}

	// exec, untrusted peers in the sandbox, and no bubblewrap: unavailable, with the sentence and its fix.
	code, out, _ := postAutoReply(t, d, `{"backend":"exec","agent":"claude","untrusted":"sandbox","api_key":"k"}`)
	if code != 200 {
		t.Fatalf("set: %d %v", code, out)
	}
	sb := sandboxOf(t, d)
	if sb["state"] != "unavailable" || sb["reason"] != "no_bwrap" || sb["message"] != a2ashape.Sentence("no_bwrap", lang, nil) ||
		sb["kind"] != "bwrap" || sb["limits"] == "" {
		t.Errorf("no bwrap: %v", sb)
	}
	if out["message"] != sb["message"] {
		t.Errorf("/autoreply message %v, /status %v", out["message"], sb["message"])
	}
	if b, _ := sb["budget"].(map[string]any); b == nil || b["runs_per_day"] != float64(20) || b["runs_per_peer_per_day"] != float64(5) {
		t.Errorf("budget: %v", sb["budget"])
	}
	st, _ := statusOf(t, d)
	if ar, _ := st["auto_reply"].(map[string]any); ar["trusted"] != "direct" || ar["api_key_set"] != true {
		t.Errorf("auto_reply: %v", ar)
	}

	// A budget of 0 is untrusted=off: the sandbox is off, whatever the host has.
	postAutoReply(t, d, `{"sandbox":{"runs_per_day":0,"runs_per_peer_per_day":0}}`)
	if sb := sandboxOf(t, d); sb["state"] != "off" || sb["reason"] != "sandbox.off" {
		t.Errorf("budget 0: %v", sb)
	}
	postAutoReply(t, d, `{"sandbox":{"runs_per_day":null,"runs_per_peer_per_day":null}}`)

	// Trusted peers in the sandbox count too, and an agent the sandbox cannot run is said before the host.
	postAutoReply(t, d, `{"untrusted":"off","trusted":"sandbox","agent":"cursor"}`)
	if sb := sandboxOf(t, d); sb["state"] != "unavailable" || sb["reason"] != "no_bwrap" {
		t.Errorf("trusted sandbox: %v", sb)
	}

	// The host is fine (a probe that passes is not available here, so the upstream check is read directly):
	// an agent that cannot use the model service address says why.
	cfg := AutoReplyConfig{Backend: "exec", Agent: "codex", APIBase: "https://example.org/v1", Model: "m", APIKey: "k"}
	key, args := cfg.SandboxUpstreamProblem()
	if key != "agent_unsupported.combo" || !strings.Contains(a2ashape.Sentence(key, a2ashape.English, args), "codex works only with api.openai.com") {
		t.Errorf("combo: %q %v", key, args)
	}
	if key, _ := (AutoReplyConfig{Backend: "exec", Agent: "claude"}).SandboxUpstreamProblem(); key != "no_api_key" {
		t.Errorf("no key: %q", key)
	}
	if key, _ := (AutoReplyConfig{Backend: "exec", Agent: "cursor", APIKey: "k"}).SandboxUpstreamProblem(); key != "agent_unsupported" {
		t.Errorf("cursor: %q", key)
	}
}

// LEC §2.3.5: /autoreply-test runs direct first and adds sandbox when anything is sandboxed; a sandbox
// that cannot run fails that path with the reason's sentence and not the whole request.
func TestAutoReplyTestPaths(t *testing.T) {
	defer func(f func(string) (string, error)) { bwrapLookPath = f }(bwrapLookPath)
	bwrapLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	d := newTestDaemon(t, "", false)
	if _, err := d.TestAutoReplyPaths(t.Context(), ""); err == nil {
		t.Fatal("no auto-reply is not a result")
	}
	api := &fakeOpenAI{reply: "an answer"}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	postAutoReply(t, d, `{"backend":"openai","api_base":"`+srv.URL+`/v1","model":"m"}`)
	res, err := d.TestAutoReplyPaths(t.Context(), "")
	if err != nil || len(res) != 1 || res[0].Path != "direct" || !res[0].OK || res[0].ReplyChars != len("an answer") ||
		res[0].Message != a2ashape.Sentence("autoreply_test.pass_direct", a2ashape.Lang(), a2ashape.Args{"n": len("an answer")}) {
		t.Fatalf("openai: %+v %v", res, err)
	}
	postAutoReply(t, d, `{"backend":"exec","agent":"cursor","untrusted":"sandbox","api_key":"k"}`)
	res, _ = d.TestAutoReplyPaths(t.Context(), "")
	if len(res) != 2 || res[1].Path != "sandbox" || res[1].OK || res[1].Reason != "no_bwrap" ||
		!strings.Contains(res[1].Message, a2ashape.Sentence("no_bwrap", a2ashape.Lang(), nil)) {
		t.Fatalf("exec without bwrap: %+v", res)
	}
}

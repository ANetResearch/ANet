package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// G-APPARMOR (LEC §7.1): `anet doctor --apparmor-profile` prints the profile of deploy/apparmor, byte for
// byte, and writes nothing. The profile gives bubblewrap the user-namespace permission and puts what it
// starts under a profile with no capability.
func TestDoctorPrintsTheAppArmorProfile(t *testing.T) {
	want, err := os.ReadFile("../../deploy/apparmor/anet-bwrap")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	cliStdout = &buf
	defer func() { cliStdout = nil }()
	l := daemon.NewLayout(t.TempDir())
	if err := runDoctor(l, []string{"--apparmor-profile"}); err != nil {
		t.Fatal(err)
	}
	if buf.String() != string(want) {
		t.Errorf("the output is not the profile file:\n%s", buf.String())
	}
	p := buf.String()
	for _, must := range []string{"allow userns,", "px /** -> anet-bwrap//&anet-bwrap-unpriv,", "audit deny capability,"} {
		if !strings.Contains(p, must) {
			t.Errorf("the profile lacks %q", must)
		}
	}
	if strings.Contains(p, "-> &") {
		t.Error("a stacked profile name cannot begin with & (the kernel answers profile transition not found)")
	}
	if entries, _ := os.ReadDir(l.Root); len(entries) != 0 {
		t.Errorf("doctor --apparmor-profile wrote %d files", len(entries))
	}
}

// LEC §5.2: `anet autoreply show` is one aligned row per fact, each text a sentence of the table.
func TestAutoReplyShowRows(t *testing.T) {
	view := map[string]any{"backend": "exec", "agent": "claude", "trusted": "direct", "untrusted": "sandbox", "api_key_set": true,
		"sandbox": map[string]any{"state": "ready", "message": a2ashape.Sentence("sandbox.ready", a2ashape.Chinese, nil),
			"budget": map[string]any{"today": float64(3), "runs_per_day": float64(20), "runs_per_peer_per_day": float64(5)}}}
	var b bytes.Buffer
	printAutoReplyShow(&b, view, a2ashape.Chinese)
	want := "自动回复  exec / claude\n" +
		"信任对端  信任的对端直接以你的用户身份运行本机 agent，等同授予你的用户权限。\n" +
		"未信任    未信任对端的任务在沙箱中运行：只能经本节点访问模型，不能联网。\n" +
		"API key   已设置。\n" +
		"预算      今天已为未信任对端运行 3/20 次（每个对端至多 5 次）。\n"
	if b.String() != want {
		t.Errorf("zh:\n%s\nwant:\n%s", b.String(), want)
	}
	b.Reset()
	printAutoReplyShow(&b, nil, a2ashape.English)
	if b.String() != "Auto-reply is off.\n" {
		t.Errorf("off: %q", b.String())
	}
	// A missing key is said with its fix; an unavailable sandbox with its next step.
	view["api_key_set"] = false
	view["sandbox"] = map[string]any{"state": "unavailable", "reason": "no_api_key",
		"message": a2ashape.Sentence("no_api_key", a2ashape.English, nil), "next": "anet autoreply set --api-key-stdin"}
	b.Reset()
	printAutoReplyShow(&b, view, a2ashape.English)
	if strings.Count(b.String(), "needs an API key") != 1 || !strings.Contains(b.String(), "API key     In the sandbox this node calls the model") ||
		!strings.Contains(b.String(), nextPrefix+"anet autoreply set --api-key-stdin") {
		t.Errorf("no key:\n%s", b.String())
	}
}

// CL-1: after a set, the line that the setting is saved (with the key mentioned when the key was set),
// then the sandbox's line from the answer, then its next step.
func TestAutoReplySetPrintsTheSandboxLine(t *testing.T) {
	var out, errb bytes.Buffer
	cliStdout, cliStderr = &out, &errb
	defer func() { cliStdout, cliStderr = nil, nil }()
	body := `{"status":"updated","auto_reply":{"backend":"exec","agent":"claude"},"message":"m",` +
		`"sandbox":{"state":"unavailable","reason":"no_bwrap","message":"bubblewrap is not installed.","next":"apt install bubblewrap"}}`
	printAutoReplySet([]byte(body), true)
	want := a2ashape.Sentence("autoreply.updated_key", a2ashape.Lang(), nil) + "\n" +
		a2ashape.Label("status.sandbox", a2ashape.Lang()) + ": bubblewrap is not installed.\n" + nextPrefix + "apt install bubblewrap\n"
	if errb.String() != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", errb.String(), want)
	}
	if !strings.Contains(out.String(), `"status": "updated"`) {
		t.Errorf("stdout: %s", out.String())
	}
}

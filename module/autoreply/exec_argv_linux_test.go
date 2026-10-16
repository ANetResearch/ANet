package autoreply

// The Linux sandbox's half of exec_argv_test.go (EXE-5): what a sandboxed agent starts with.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SI-56, the environment and command line half (TestSandboxHoldsNoSecret): a sandboxed turn prepared
// the way the auto-reply loop prepares it — the replier's environment, then the sandbox launch — puts no
// model key (the operator's or the daemon's), proxy password, control token or local A2A token in what
// the agent starts with (the spec on fd 3), and no conversation in any argv (bwrap's or the agent's);
// the conversation is on fd 4. Needs no bubblewrap: nothing is started.
func TestSandboxHoldsNoSecret(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", canaryEnvKey)
	t.Setenv("HTTPS_PROXY", "http://user:"+canaryProxy+"@proxy.example:3128")
	// A directory of the daemon's search path that the sandbox does not show (an operator's tool dir).
	hidden := t.TempDir()
	t.Setenv("PATH", "/usr/bin:"+hidden+":/bin")
	d := newBareDaemon(t)
	ctlTok, err := newControlToken(d.layout)
	if err != nil {
		t.Fatal(err)
	}
	a2aTok := "a2a-" + strings.Repeat("7", 60)
	if err := os.MkdirAll(filepath.Join(d.layout.Root, "modules", "a2a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.layout.Root, "modules", "a2a", "a2a_token.txt"), []byte(a2aTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFakeModelService(t)
	up := testUpstream(t, egressUpstreamAnthropic, f)
	up.Key = canaryKey
	agent := writeStub(t, "#!/bin/sh\necho hi\n")
	plan := &sandboxPlan{Bwrap: "/usr/bin/bwrap", AID: d.AID(), Home: filepath.Join(t.TempDir(), "home"),
		NeverBind: d.sandboxNeverBind(), Egress: up}
	r := &execReplier{cfg: AutoReplyConfig{Backend: "exec", Agent: agentClaude, Command: agent, APIKey: canaryKey},
		dataDir: d.layout.Root, sandbox: plan, aid: d.AID()}
	o, err := r.baseOpts(replyContext{Role: "inbound", Goal: canaryGoal, InteractionID: canaryIX})
	if err != nil {
		t.Fatal(err)
	}
	conv := execConversation(canaryGoal, "Requester: "+canaryTurn)
	l, err := plan.command(o.WorkDir, []string{agent, "-p", "--append-system-prompt", "fixed"}, o.Env, []byte(conv))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	raw, err := io.ReadAll(l.Files[0])
	if err != nil {
		t.Fatal(err)
	}
	var spec sandboxInitSpec
	if err := decodeSandboxSpec(raw, &spec); err != nil {
		t.Fatal(err)
	}
	env := strings.Join(spec.Env, "\n")
	for _, c := range []string{canaryKey, canaryEnvKey, canaryProxy, ctlTok, a2aTok} {
		if strings.Contains(env, c) {
			t.Errorf("the sandboxed agent's environment holds %q", c)
		}
	}
	// SB-4: the search path names only what the sandbox shows — system directories and the agent's
	// runtime — not the daemon's other directories.
	for _, kv := range spec.Env {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok && (strings.Contains(p, hidden) || !strings.Contains(p, "/usr/bin")) {
			t.Errorf("the sandboxed agent's PATH is %q", p)
		}
	}
	argv := strings.Join(append(append([]string{}, l.Args...), spec.Argv...), " ")
	for _, c := range []string{canaryGoal, canaryTurn, canaryKey, canaryEnvKey, canaryIX, ctlTok, a2aTok} {
		if strings.Contains(argv, c) {
			t.Errorf("a command line holds %q: %s", c, argv)
		}
	}
	if spec.Stdin != "fd4" || l.Files[1] == nil {
		t.Fatalf("the conversation is not on fd 4: stdin %q, fd 4 %v", spec.Stdin, l.Files[1])
	}
	in, err := io.ReadAll(l.Files[1])
	if err != nil || string(in) != conv {
		t.Fatalf("fd 4 carries %d bytes (%v), want the conversation", len(in), err)
	}
}

// fd 4 carries stdin of any size the conversation bound allows (the writer does not block the launch),
// and nothing is open on fd 4 when there is no stdin.
func TestSandboxCommandPassesStdinOnFd4(t *testing.T) {
	agent := writeStub(t, "#!/bin/sh\necho hi\n")
	plan := &sandboxPlan{Bwrap: "/usr/bin/bwrap", AID: "EaidStdinOnFd40000001", Home: filepath.Join(t.TempDir(), "home")}
	big := []byte(strings.Repeat("c", conversationMaxBytes))
	l, err := plan.command(t.TempDir(), []string{agent}, []string{"PATH=/usr/bin"}, big)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(l.Files[1])
		done <- b
	}()
	select {
	case b := <-done:
		if len(b) != len(big) {
			t.Fatalf("fd 4 carried %d bytes, want %d", len(b), len(big))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fd 4 did not deliver its bytes")
	}
	l.Close()
	// An agent that never reads its stdin does not hold Close.
	l, err = plan.command(t.TempDir(), []string{agent}, []string{"PATH=/usr/bin"}, big)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { l.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close waited for a reader that never came")
	}
}

// A sandboxed agent's search path names only what the sandbox shows: system directories and the
// agent's runtime directories (SB-4).
func TestSandboxPathNamesOnlyWhatTheSandboxShows(t *testing.T) {
	runtimeDir := t.TempDir()
	env := envWithSandboxPath([]string{"PATH=/usr/bin:/home/u/.cargo/bin:" + runtimeDir + "/bin:/bin:relative", "A=1"},
		[]string{runtimeDir})
	want := []string{"PATH=/usr/bin:" + runtimeDir + "/bin:/bin", "A=1"}
	if strings.Join(env, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", env, want)
	}
}

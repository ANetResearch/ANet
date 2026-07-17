package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAutoReplyExecRoundTrip drives the exec backend with a stub agent binary.
func TestAutoReplyExecRoundTrip(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "stub.sh")
	script := `#!/bin/sh
echo "stub received prompt" >&2
echo "EXEC-OK: processed delegation"
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANET_EXEC_COMMAND", stub)

	f := newAutoReplyFixture(t, AutoReplyConfig{
		Backend: "exec",
		Agent:   "cursor",
	}, nil)

	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "say hello via exec", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	got := f.lastPeerMsg(t, id)
	if got.Body != "EXEC-OK: processed delegation" {
		t.Fatalf("reply = %q, want exec stub output", got.Body)
	}
}

func TestExecReplierBuildPromptWithImage(t *testing.T) {
	layout := NewLayout(t.TempDir())
	_ = layout.EnsureRoot()
	r := &execReplier{cfg: AutoReplyConfig{}, layout: layout, dataDir: layout.Root}
	turns := []chatTurn{{
		Role: "user", Text: "describe",
		Images: []chatImage{{Mime: "image/png", Data: []byte("fakepng")}},
	}}
	prompt, cleanup, err := r.buildPrompt(turns)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if prompt == "" || !contains(prompt, "describe") || !contains(prompt, ".png") {
		t.Fatalf("prompt = %q", prompt)
	}
}

// TestExecReplierCursorSession verifies the cursor native-session path: the first turn creates a chat
// and seeds it with the system prompt + full opening, and later turns RESUME that chat sending ONLY the
// new message (no transcript replay, no re-seeded system prompt).
func TestExecReplierCursorSession(t *testing.T) {
	dir := t.TempDir()
	logf := filepath.Join(dir, "calls.log")
	// Stub: `create-chat` prints a uuid-like id; otherwise it echoes the LAST arg (the prompt) so the
	// test can inspect exactly what was sent. Every invocation is appended to the log.
	stub := filepath.Join(dir, "stub.sh")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logf + `"
for a in "$@"; do
  if [ "$a" = "create-chat" ]; then
    printf 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee'
    exit 0
  fi
  last="$a"
done
printf '%s' "$last"
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := NewLayout(t.TempDir())
	_ = layout.EnsureRoot()
	r := &execReplier{
		cfg:      AutoReplyConfig{Backend: "exec", Agent: "cursor", Command: stub},
		layout:   layout,
		dataDir:  layout.Root,
		sessions: sharedExecSessionStore(layout.Root),
	}
	rc := replyContext{Role: "inbound", Goal: "greet", InteractionID: "int-session-1"}

	// Turn 1: fresh interaction → create-chat + seed.
	out1, err := r.Reply(context.Background(), rc, []chatTurn{{Role: "user", Text: "hello-alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out1, "hello-alpha") || !strings.Contains(out1, "AgentNetwork autopilot") {
		t.Fatalf("turn1 prompt should seed system + opening, got: %q", out1)
	}

	// Turn 2: same interaction, new message → resume, delta only, no re-seed, no old text.
	out2, err := r.Reply(context.Background(), rc, []chatTurn{
		{Role: "user", Text: "hello-alpha"},
		{Role: "assistant", Text: "hi there"},
		{Role: "user", Text: "hello-beta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "hello-beta") {
		t.Fatalf("turn2 should carry the new message, got: %q", out2)
	}
	if strings.Contains(out2, "hello-alpha") || strings.Contains(out2, "AgentNetwork autopilot") {
		t.Fatalf("turn2 must NOT replay history or re-seed system prompt, got: %q", out2)
	}

	log, _ := os.ReadFile(logf)
	if got := strings.Count(string(log), "create-chat"); got != 1 {
		t.Fatalf("create-chat should run exactly once, ran %d times\nlog:\n%s", got, log)
	}
	if got := strings.Count(string(log), "--resume aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"); got != 2 {
		t.Fatalf("both turns should --resume the same chat id, saw %d\nlog:\n%s", got, log)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && stringIndex(s, sub) >= 0)
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestExecReplierReplyUsesStub(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "stub.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho reply-text\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := NewLayout(t.TempDir())
	_ = layout.EnsureRoot()
	r := &execReplier{
		cfg:     AutoReplyConfig{Backend: "exec", Agent: "cursor", Command: stub},
		layout:  layout,
		dataDir: layout.Root,
	}
	out, err := r.Reply(context.Background(), replyContext{Role: "inbound", Goal: "hi"}, []chatTurn{{Role: "user", Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if out != "reply-text" {
		t.Fatalf("got %q", out)
	}
}

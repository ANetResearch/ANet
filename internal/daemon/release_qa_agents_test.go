package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// argvStub is an agent binary that reports the argv it was called with,
// so a test can assert on the command line each agent gets rather than on
// a reply the stub invented.
func argvStub(t *testing.T) string {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "argv.sh")
	// codex reads its reply from the -o file when one is present, so write
	// the argv there too — otherwise the codex case would assert on a path
	// the real binary does not take.
	script := `#!/bin/sh
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
if [ -n "$out" ]; then printf '%s\n' "$*" > "$out"; fi
printf '%s\n' "$*"
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

// Every agent in the registry must actually run and come back with the
// prompt in its command line. A registry entry whose invoke builds a
// command the binary rejects is an agent that is listed and broken, and
// the listing is what an operator picks from.
func TestEveryRegisteredAgentInvokesWithThePrompt(t *testing.T) {
	stub := argvStub(t)
	t.Setenv("ANET_EXEC_COMMAND", stub)
	for _, id := range SupportedExecAgents() {
		t.Run(id, func(t *testing.T) {
			got, err := InvokeAgent(context.Background(), execInvokeOpts{
				AgentID: id,
				Prompt:  "SENTINEL-PROMPT",
				Timeout: 10 * time.Second,
			})
			if err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			if !strings.Contains(got, "SENTINEL-PROMPT") {
				t.Errorf("%s never passed the prompt to the binary: %q", id, got)
			}
		})
	}
}

// The work directory is how a fleet operator points a node at the repo it
// should act on. An agent that silently ignores it runs in whatever
// directory the daemon happens to be in.
func TestEveryRegisteredAgentRunsInTheRequestedWorkDir(t *testing.T) {
	stub := argvStub(t)
	t.Setenv("ANET_EXEC_COMMAND", stub)
	work := t.TempDir()
	pwdStub := filepath.Join(t.TempDir(), "pwd.sh")
	if err := os.WriteFile(pwdStub, []byte("#!/bin/sh\npwd\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range SupportedExecAgents() {
		t.Run(id, func(t *testing.T) {
			// codex writes its reply to -o, so read the cwd off stdout by
			// using a stub that ignores -o entirely.
			t.Setenv("ANET_EXEC_COMMAND", pwdStub)
			got, err := InvokeAgent(context.Background(), execInvokeOpts{
				AgentID: id, Prompt: "p", WorkDir: work, Timeout: 10 * time.Second,
			})
			if err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			// t.TempDir can sit under a symlinked /tmp; compare the leaf.
			if !strings.HasSuffix(strings.TrimSpace(got), filepath.Base(work)) {
				t.Errorf("%s ran in %q, want %q", id, strings.TrimSpace(got), work)
			}
		})
	}
}

// An agent that exits non-zero must surface as an error, not as an empty
// reply that the delegation path would send back as if it were an answer.
// This is the honest-status rule at the point it costs most: a requester
// receiving "" cannot tell a silent agent from a working one with nothing
// to say.
func TestAFailingAgentIsAnErrorNotAnEmptyReply(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "fail.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 'boom' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANET_EXEC_COMMAND", stub)
	for _, id := range SupportedExecAgents() {
		t.Run(id, func(t *testing.T) {
			reply, err := InvokeAgent(context.Background(), execInvokeOpts{
				AgentID: id, Prompt: "p", Timeout: 10 * time.Second,
			})
			if err == nil {
				t.Fatalf("%s: a failing agent returned reply %q and no error", id, reply)
			}
			if !strings.Contains(err.Error(), "boom") {
				t.Errorf("%s: the agent's own stderr did not survive: %v", id, err)
			}
		})
	}
}

// An agent that returns nothing at all is also an error. Silence is not
// an answer, and a delegation answered with "" is worse than one answered
// with a failure: the requester has no way to tell it apart from success.
func TestAnAgentThatSaysNothingIsAnError(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "silent.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANET_EXEC_COMMAND", stub)
	for _, id := range SupportedExecAgents() {
		t.Run(id, func(t *testing.T) {
			reply, err := InvokeAgent(context.Background(), execInvokeOpts{
				AgentID: id, Prompt: "p", Timeout: 10 * time.Second,
			})
			if err == nil {
				t.Errorf("%s: an agent that printed nothing returned %q and no error", id, reply)
			}
		})
	}
}

// A hung agent must be cut off at the configured timeout. Without this a
// fleet operator's node stops answering anything after one stuck task,
// because the auto-reply loop is what would be blocked.
func TestAHungAgentIsCutOffAtTheTimeout(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "hang.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANET_EXEC_COMMAND", stub)
	start := time.Now()
	_, err := InvokeAgent(context.Background(), execInvokeOpts{
		AgentID: "claude", Prompt: "p", Timeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("a hung agent returned successfully")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("the timeout took %s to fire", elapsed)
	}
}

// An unknown agent id must be refused by name, listing what does work —
// an operator typing "opencodex" should be told, not left with a node
// that quietly answers nothing.
func TestAnUnknownAgentIsRefusedByName(t *testing.T) {
	_, err := InvokeAgent(context.Background(), execInvokeOpts{
		AgentID: "opencodex", Prompt: "p", Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("an unknown agent id was accepted")
	}
	if !strings.Contains(err.Error(), "opencodex") {
		t.Errorf("the refusal does not name what was asked for: %v", err)
	}
	for _, id := range SupportedExecAgents() {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("the refusal does not offer %q as an alternative: %v", id, err)
		}
	}
}

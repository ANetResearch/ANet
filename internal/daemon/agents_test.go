package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInvokeAgentStub(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "stub.sh")
	script := `#!/bin/sh
echo "EXEC-STUB-REPLY: $*" >&2
echo "hello from stub agent"
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANET_EXEC_COMMAND", stub)

	reply, err := InvokeAgent(context.Background(), execInvokeOpts{
		AgentID: "cursor",
		Prompt:  "test prompt",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "hello from stub agent" {
		t.Fatalf("reply = %q", reply)
	}
}

// An unattended provider should default to the CHEAPEST model when the operator gave none: cursor→auto.
func TestInvokeAgentDefaultsToCheapModel(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "echoargs.sh")
	// Echo argv to stdout so the "reply" is the arg line we can assert on.
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANET_EXEC_COMMAND", stub)

	got, err := InvokeAgent(context.Background(), execInvokeOpts{AgentID: "cursor", Prompt: "p", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "--model auto") {
		t.Fatalf("expected default --model auto, got %q", got)
	}
	// An explicit model overrides the default.
	got, err = InvokeAgent(context.Background(), execInvokeOpts{AgentID: "cursor", Prompt: "p", Model: "grok-4.5-fast-medium", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "--model auto") || !strings.Contains(got, "grok-4.5-fast-medium") {
		t.Fatalf("explicit model not honored, got %q", got)
	}
}

func TestInstallCursorCreatesRule(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	changes, err := InstallAgent("cursor")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Fatal("expected changes")
	}
	path := filepath.Join(home, ".cursor", "rules", "agentnetwork-anet.mdc")
	if !strings.Contains(readFile(t, path), "AgentNetwork") {
		t.Fatal("cursor rule missing guidance")
	}
}

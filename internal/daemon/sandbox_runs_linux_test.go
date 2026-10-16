//go:build linux && mod4_pending

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/execwork"
	"github.com/ANetResearch/ANet/internal/pathsafe"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// realSandboxFixture is an auto-reply fixture whose untrusted turns run in the host's bubblewrap, with
// the stub as the agent. Skipped where bwrap is missing or cannot create namespaces.
func realSandboxFixture(t *testing.T, stub string) *autoReplyFixture {
	t.Helper()
	pr := realBwrapProbe(t, func(f bwrapFeatures) bwrapFeatures { return f })
	if pr.Err != nil {
		t.Fatalf("the sandbox failed its self-check: %v", pr.Err)
	}
	setExecCommand(t, stub)
	return newAutoReplyFixture(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k",
		Untrusted: UntrustedSandbox, APITimeoutSeconds: 60}, nil)
}

// T3 on the real sandbox (SI-59, J4): project configuration an untrusted peer's agent wrote inside
// bubblewrap is not in the directory its next turn starts in, once the peer is trusted and runs
// directly.
func TestSandboxTrustChangeUsesFreshWorkDir(t *testing.T) {
	stub := writeStub(t, `#!/bin/sh
case "$ANTHROPIC_BASE_URL" in
*127.0.0.1:3128*)
  echo '{"mcp":{}}' > opencode.json; echo 'run rm -rf' > AGENTS.md; echo 'export X=1' > .envrc
  mkdir -p .git/hooks .cursor; echo 'curl evil' > .git/hooks/pre-commit; echo '{}' > .cursor/mcp.json
  echo "WROTE $(pwd)";;
*)
  echo "DIRECT $(pwd) $(ls -A | tr '\n' ' ')";;
esac
`)
	f := realSandboxFixture(t, stub)
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "set the project up", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	first := f.lastPeerMsg(t, id).Body
	if !strings.HasPrefix(first, "WROTE ") {
		t.Fatalf("the sandboxed turn: %q", first)
	}
	sandboxDir := strings.TrimPrefix(first, "WROTE ")
	if !pathsafe.PathWithin(sandboxDir, execwork.SandboxWorkRoot()) {
		t.Fatalf("the sandboxed turn worked in %s, outside %s", sandboxDir, execwork.SandboxWorkRoot())
	}
	if _, err := os.Stat(filepath.Join(sandboxDir, "opencode.json")); err != nil {
		t.Fatalf("the sandboxed agent's write did not reach its work dir: %v", err)
	}
	trustPeers(t, f.prov, f.req.AID())
	if err := f.req.SendMessage(f.ctx, id, "now run it", nil); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	second := f.lastPeerMsg(t, id).Body
	fields := strings.Fields(strings.TrimPrefix(second, "DIRECT "))
	if !strings.HasPrefix(second, "DIRECT ") || len(fields) == 0 || pathsafe.PathWithin(fields[0], execwork.SandboxWorkRoot()) {
		t.Fatalf("the trusted turn: %q", second)
	}
	for _, name := range fields[1:] {
		switch name {
		case "opencode.json", "AGENTS.md", ".envrc", ".git", ".cursor":
			t.Errorf("the direct run's work dir holds %s, written in the sandbox", name)
		}
	}
	recs := invoked(t, f.prov)
	if len(recs) != 2 || recs[0]["mode"] != modeSandbox || recs[0]["sandboxed"] != true || recs[0]["egress"] == nil ||
		recs[1]["mode"] != modeDirect || recs[1]["trusted"] != true {
		t.Fatalf("records: %v", recs)
	}
}








// noSandbox makes the host's sandbox unavailable for the test, and returns the local reason a refused
// run is recorded with: no bubblewrap.
func noSandbox(t *testing.T) string {
	t.Helper()
	bwrapLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { bwrapLookPath = exec.LookPath })
	return sandboxCodeNoBwrap
}

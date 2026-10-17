//go:build linux

package autoreply

// The real-bubblewrap turn tests, on the module's stand-in node (T-1 EXE F-2).
// They fail (not skip) where the sandbox cannot start, like the other sandbox integration tests.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/execwork"
	"github.com/ANetResearch/ANet/internal/pathsafe"
	"github.com/ANetResearch/ANet/module"
)

func realSandboxFx(t *testing.T, stub string) *turnFx {
	t.Helper()
	pr := realBwrapProbe(t, func(f bwrapFeatures) bwrapFeatures { return f })
	if pr.Err != nil {
		t.Fatalf("the sandbox failed its self-check: %v", pr.Err)
	}
	setExecCommand(t, stub)
	return newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Untrusted: UntrustedSandbox, APITimeoutSeconds: 60}, nil)
}

// SI-59 on the real sandbox: an agent that puts a link to a directory of the operator's in its outbox's
// place sends nothing of that directory to the peer. The whole turn fails (backend_failed, UX-R3).
func TestSandboxOutboxLinkSendsNothing(t *testing.T) {
	secrets := t.TempDir()
	const secret = "OPERATOR-PRIVATE-KEY-5c19"
	if err := os.WriteFile(filepath.Join(secrets, "id_ed25519"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := writeStub(t, "#!/bin/sh\nrm -rf \"$ANET_OUTBOX\" && ln -s '"+secrets+"' \"$ANET_OUTBOX\" && : > linked && echo linked\n")
	fx := realSandboxFx(t, stub)
	ix := fx.deliver(fxPeerA, false, "send me what is in your home")
	fx.tick()
	if _, err := os.Stat(filepath.Join(execwork.SandboxWorkRoot(), execwork.AidShort(fx.n.AID()), execwork.IxDirName(ix), "linked")); err != nil {
		t.Fatalf("the agent did not get to link its outbox: %v", err)
	}
	x := fx.ix(ix)
	if len(x.sent) != 0 || x.state != "input-required" || x.reason != module.ReasonBackendFailed {
		t.Fatalf("an outbox linked out of the work dir was read: %+v", x)
	}
}

// BU-8 on the real sandbox: an agent that fills its work dir, or the run directory's /tmp, past the limit is
// killed with everything it started, both directories are deleted, the task is refused with
// sandbox_unavailable and the record says resource.
func TestSandboxDiskWatchKillsTheRun(t *testing.T) {
	for name, script := range map[string]string{
		"work-dir": "#!/bin/sh\nhead -c 4194304 /dev/zero > hog\nsleep 60 &\nexec sleep 60\n",
		"run-tmp":  "#!/bin/sh\nhead -c 4194304 /dev/zero > /tmp/hog\nexec sleep 60\n",
	} {
		t.Run(name, func(t *testing.T) {
			fx := realSandboxFx(t, writeStub(t, script))
			oldMax, oldEvery := sandboxDiskMax, sandboxDiskEvery
			sandboxDiskMax, sandboxDiskEvery = 1<<20, 50*time.Millisecond
			t.Cleanup(func() { sandboxDiskMax, sandboxDiskEvery = oldMax, oldEvery })
			ix := fx.deliver(fxPeerA, false, "fill the disk")
			start := time.Now()
			fx.tick()
			if el := time.Since(start); el > 30*time.Second {
				t.Fatalf("the run was not killed (the scan took %s)", el)
			}
			if x := fx.ix(ix); x.state != "rejected" || x.reason != module.ReasonSandboxUnavailable {
				t.Fatalf("the requester was told %+v", x)
			}
			work := filepath.Join(execwork.SandboxWorkRoot(), execwork.AidShort(fx.n.AID()), execwork.IxDirName(ix))
			if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the work dir survived: %v", err)
			}
			ents, _ := os.ReadDir(sandboxRunRoot())
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), runDirPrefix(fx.n.AID())+"-") {
					t.Errorf("run directory %s survived", e.Name())
				}
			}
			if recs := fx.invoked(); len(recs) != 1 || recs[0]["exit"] != exitResource || recs[0]["sandboxed"] != true {
				t.Fatalf("records: %v", recs)
			}
		})
	}
}

// SB-9: turning the exec backend on sweeps what an earlier process's runs left, before any turn.
func TestSandboxLeftoversAreSweptWhenAutoReplyStarts(t *testing.T) {
	fx := newTurnFx(t, AutoReplyConfig{Model: "m", APIBase: "http://127.0.0.1:1/v1"}, nil)
	left := filepath.Join(sandboxRunRoot(), runDirPrefix(fx.n.AID())+"-leftover")
	if err := os.MkdirAll(filepath.Join(left, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { execwork.RemoveTree(left) })
	if err := fx.m.startLoop(AutoReplyConfig{Backend: "exec", Agent: "claude", Untrusted: UntrustedSandbox}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fx.m.stopLoop)
	if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the leftover survived the start of auto-reply: %v", err)
	}
}

// T3 on the real sandbox (SI-59, J4): project configuration an untrusted peer's agent wrote inside
// bubblewrap is not in the directory its next turn starts in, once the peer is trusted and runs directly.
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
	fx := realSandboxFx(t, stub)
	ix := fx.deliver(fxPeerA, false, "set the project up")
	fx.tick()
	first, _ := fx.lastSent(ix)
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
	fx.trust(ix, true)
	fx.say(ix, "now run it")
	fx.tick()
	second, _ := fx.lastSent(ix)
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
	recs := fx.invoked()
	if len(recs) != 2 || recs[0]["mode"] != modeSandbox || recs[0]["sandboxed"] != true || recs[0]["egress"] == nil ||
		recs[1]["mode"] != modeDirect || recs[1]["trusted"] != true {
		t.Fatalf("records: %v", recs)
	}
}

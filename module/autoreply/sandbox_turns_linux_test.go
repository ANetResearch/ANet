//go:build linux

package autoreply

// The real-bubblewrap turn tests of MOD-4's mod4_pending set, on the module's stand-in node (T-1 EXE F-2).
// They fail (not skip) where the sandbox cannot start, like the other sandbox integration tests.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/execwork"
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

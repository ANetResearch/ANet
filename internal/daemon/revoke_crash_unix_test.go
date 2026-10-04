//go:build unix

package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// crashChildEnv names the data directory of TestListWriteCrashReconciles'
// child process.
const crashChildEnv = "SS8_LIST_CRASH_ROOT"

// The control route writes the list file, then withdraws; a kill -9 between
// the two leaves the peer off the list with its credential still given. The
// next start withdraws it (SSR-D29). The child process is this test binary:
// it takes the peer off the list as the route does and kills itself before
// the withdrawal.
func TestListWriteCrashReconciles(t *testing.T) {
	const g = "did:anet:crash-peer-0001"
	if root := os.Getenv(crashChildEnv); root != "" {
		d, err := New(NewLayout(root))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		if _, err := d.RemovePeer(g); err != nil { // the route's list write
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL) // before withdrawAfterListEdit
		os.Exit(5)
	}
	keepRevokeLoopOut(t)
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0"})
	d := startAt(t, root)
	allowPeers(t, d, g)
	peerCap := issueTo(t, d, g, "peer", "", seal.CapClassContact, 1)
	oneShot := issueTo(t, d, g, "ix", "ix-1", seal.CapClassContact, 2)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestListWriteCrashReconciles$", "-test.count=1")
	cmd.Env = append(os.Environ(), crashChildEnv+"="+root)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the child was not killed: %v\n%s", err, out)
	}
	allowed, _ := os.ReadFile(filepath.Join(root, "peers.allow"))
	if bytes.Contains(allowed, []byte(g)) {
		t.Fatalf("the child did not write the list:\n%s", allowed)
	}
	s, err := interactions.Open(NewLayout(root).InteractionsDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.IssuedCap(peerCap.CapID)
	s.Close()
	if err != nil || r.RevokedAt != 0 {
		t.Fatalf("the child withdrew before it died: %+v %v", r, err)
	}

	d2 := startAt(t, root)
	if got := issuedState(t, d2, peerCap, oneShot); got != "withdrawn,given" {
		t.Fatalf("after the restart: %s", got)
	}
}

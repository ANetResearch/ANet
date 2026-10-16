//go:build mod4_pending

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/execwork"
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
	setExecCommand(t, stub)

	f := newAutoReplyFixture(t, AutoReplyConfig{
		Backend: "exec",
		Agent:   "cursor",
	}, nil)
	// The exec backend runs the local agent only for a peer on the trust list (A2A-DESIGN §6).
	trustPeers(t, f.prov, f.req.AID())

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

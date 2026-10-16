package daemon

import (
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// openSandboxTask records ix as an open inbound task, as the auto-reply loop has one before it runs
// a sandboxed turn for it. A test that drives runSandboxed directly needs it: the maintenance that
// New starts removes the exec work directory of every interaction that is not open (FS-L11), and its
// first pass at start-up would otherwise take the directory the turn has just made.
func openSandboxTask(t testing.TB, d *Daemon, ix string) {
	t.Helper()
	if err := d.ix.Create(interactions.New{ID: ix, Role: interactions.RoleInbound, PeerAID: "did:anet:sandbox-peer", Goal: "g"}); err != nil {
		t.Fatalf("record the task %s: %v", ix, err)
	}
}

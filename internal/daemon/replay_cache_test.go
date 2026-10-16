package daemon

import (
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
)

// A daemon replays each key history once (T-1 RT-F3): New leaves a ReplayCache installed.
func TestNewInstallsTheReplayCache(t *testing.T) {
	_ = newTestDaemon(t, silentHub, true)
	prev := identity.SetReplayCache(nil)
	identity.SetReplayCache(prev)
	if prev == nil {
		t.Fatal("no identity.ReplayCache after New")
	}
}

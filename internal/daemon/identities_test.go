package daemon_test

import (
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// Setting ANET_HOME must pin the identity, not merely move the container.
//
// It was absent from the pinning list, so a command run with it set fell
// through to the uid pointer and operated on whichever daemon was
// running. `ANET_HOME=/tmp/x anet hub-register <hub> --name throwaway`
// re-registered the LIVE node under that name and capability list on the
// real hub — the exact failure the strict path exists to prevent, from
// the one variable that was left out of the check. Found by using a
// throwaway identity to write a production check.
func TestSettingTheHomeContainerPinsTheIdentity(t *testing.T) {
	for _, env := range []string{"ANET_ID", "ANET_DATA_DIR", "ANET_HOME"} {
		t.Setenv("ANET_ID", "")
		t.Setenv("ANET_DATA_DIR", "")
		t.Setenv("ANET_HOME", "")
		t.Setenv(env, t.TempDir())
		if !daemon.SelectionIsExplicit("") {
			t.Errorf("%s set and the selection was not treated as pinned — "+
				"the CLI would fall back to whichever daemon is running", env)
		}
	}
	// And with none of them set, the fallback is still allowed: a single
	// daemon on a machine should stay reachable without ceremony.
	t.Setenv("ANET_ID", "")
	t.Setenv("ANET_DATA_DIR", "")
	t.Setenv("ANET_HOME", "")
	if daemon.CurrentIdentity() == "default" && daemon.SelectionIsExplicit("") {
		t.Error("nothing was pinned and the selection was treated as explicit")
	}
}

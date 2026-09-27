package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain isolates the exec work dirs of this package's tests and hosts the sandbox probe.
//
// Exec auto-reply runs create per-interaction work dirs under the user cache dir
// (execInteractionDir). XDG_CACHE_HOME is pointed at a temporary directory for the whole run so tests
// never write into the real ~/.cache.
//
// When the test binary is started by the sandbox integration test as the "agent" (its argument list
// then contains sandboxProbeMarker), it runs the in-sandbox checks instead of the tests and prints
// their results as the agent's reply.
func TestMain(m *testing.M) {
	for _, a := range os.Args[1:] {
		if i := strings.Index(a, sandboxProbeMarker); i >= 0 {
			os.Exit(runSandboxProbe(a[i+len(sandboxProbeMarker):]))
		}
	}
	cache, err := os.MkdirTemp("", "anet-test-cache-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_ = os.Setenv("XDG_CACHE_HOME", filepath.Join(cache, "cache"))
	code := m.Run()
	_ = os.RemoveAll(cache)
	os.Exit(code)
}

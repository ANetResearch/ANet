package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testMainHooks are the roles the test binary can be started in instead of running tests, by its first
// argument: anet's internal sandbox commands (sandbox_init_test.go), helper agents and the seccomp
// self-test (seccomp_linux_test.go). A test file registers its hooks in init().
var testMainHooks = map[string]func(args []string) int{}

// TestMain isolates the exec work dirs of this package's tests and hosts the sandbox probe.
//
// Exec auto-reply runs create per-interaction work dirs under the user cache dir
// (execInteractionDir). XDG_CACHE_HOME is pointed at a temporary directory for the whole run so tests
// never write into the real ~/.cache.
//
// When the test binary is started by the sandbox integration test as the "agent" (its argument list
// then contains sandboxProbeMarker), it runs the in-sandbox checks instead of the tests and prints
// their results as the agent's reply. Started as /run/anet/init (the sandbox binds os.Executable(),
// which in a test is this binary), it is `anet __sandbox-init` or `anet __sandbox-probe`.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		if hook, ok := testMainHooks[os.Args[1]]; ok {
			os.Exit(hook(os.Args[2:]))
		}
	}
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

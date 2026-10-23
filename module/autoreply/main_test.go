package autoreply

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testMainHooks are the roles the test binary can be started in instead of running tests, by its first
// argument: anet's internal sandbox commands (sandbox_init_test.go), helper agents and the seccomp
// self-test (seccomp_linux_test.go). A test file registers its hooks in init().
var testMainHooks = map[string]func(args []string) int{}

// testRealHome is the home the test binary was started with, where TestMain moved HOME (macOS).
var testRealHome string

// testGoCache is the go command's build cache of the user who runs the tests, read before TestMain moves
// the cache dir: a test that builds a helper program (fakeAgent) uses it rather than a cold one.
var testGoCache string

// testCleanups run after the tests, for what a helper made once for the whole run.
var testCleanups []func()

// TestMain isolates the exec work dirs of this package's tests and hosts the sandbox probe.
//
// Exec auto-reply runs create per-interaction work dirs under the user cache dir
// (execInteractionDir). XDG_CACHE_HOME is pointed at a temporary directory for the whole run so tests
// never write into the real ~/.cache; on macOS, whose cache dir is under the home, so is HOME.
//
// When the test binary is started by the sandbox integration test as the claude "agent" (its stdin then
// carries sandboxProbeMarker), it runs the in-sandbox checks instead of the tests and prints their
// results as the agent's reply. Started as /run/anet/init (the sandbox binds os.Executable(),
// which in a test is this binary), it is `anet __sandbox-init` or `anet __sandbox-probe`.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		if hook, ok := testMainHooks[os.Args[1]]; ok {
			os.Exit(hook(os.Args[2:]))
		}
	}
	// Started as the claude agent (invokeClaude: `-p --output-format …`), the probe request is in the
	// conversation, which claude reads on stdin (LEC AV-2).
	if len(os.Args) > 2 && os.Args[1] == "-p" && os.Args[2] == "--output-format" {
		in, _ := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
		probeStdinBytes = len(in)
		if i := strings.Index(string(in), sandboxProbeMarker); i >= 0 {
			os.Exit(runSandboxProbe(string(in[i+len(sandboxProbeMarker):])))
		}
		fmt.Fprintln(os.Stderr, "started as an agent without a probe request on stdin")
		os.Exit(2)
	}
	testGoCache = os.Getenv("GOCACHE")
	if uc, err := os.UserCacheDir(); err == nil && testGoCache == "" {
		testGoCache = filepath.Join(uc, "go-build")
	}
	cache, err := os.MkdirTemp("", "anet-test-cache-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_ = os.Setenv("XDG_CACHE_HOME", filepath.Join(cache, "cache"))
	if runtime.GOOS == "darwin" {
		// macOS's cache dir is ~/Library/Caches whatever XDG_CACHE_HOME says: a home of the run's own keeps
		// the work and run directories there too. A test that runs the go command gives it the real one.
		testRealHome = os.Getenv("HOME")
		home := filepath.Join(cache, "home")
		_ = os.MkdirAll(filepath.Join(home, "Library", "Caches"), 0o700)
		_ = os.Setenv("HOME", home)
	}
	if runtime.GOOS == "windows" {
		// Windows's cache dir is %LocalAppData% whatever XDG_CACHE_HOME says: one of the run's own keeps
		// the work dirs out of the user's profile.
		_ = os.Setenv("LocalAppData", filepath.Join(cache, "localappdata"))
	}
	code := m.Run()
	for _, f := range testCleanups {
		f()
	}
	_ = os.RemoveAll(cache)
	os.Exit(code)
}

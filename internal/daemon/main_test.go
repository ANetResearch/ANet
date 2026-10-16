package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// testMainHooks are the roles the test binary can be started in instead of running tests, by its first
// argument. A test file registers its hooks in init(). (The sandbox's own roles moved with the
// auto-reply module: module/autoreply/main_test.go.)
var testMainHooks = map[string]func(args []string) int{}

// testRealHome is the home the test binary was started with, where TestMain moved HOME (macOS).
var testRealHome string

// TestMain isolates the exec work dirs of this package's tests.
//
// Exec auto-reply runs create per-interaction work dirs under the user cache dir
// (execInteractionDir). XDG_CACHE_HOME is pointed at a temporary directory for the whole run so tests
// never write into the real ~/.cache; on macOS, whose cache dir is under the home, so is HOME.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		if hook, ok := testMainHooks[os.Args[1]]; ok {
			os.Exit(hook(os.Args[2:]))
		}
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
	code := m.Run()
	_ = os.RemoveAll(cache)
	os.Exit(code)
}

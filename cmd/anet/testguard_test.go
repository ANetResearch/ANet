package main

// testguard_test.go keeps the tests of this package away from the real user's anet. A test that
// runs the anet command must give it a temporary home, anet home and runtime directory, and select
// its identity explicitly: without an explicit identity the command resolves the control plane
// through the uid-wide daemon pointer (/run/user/<uid>/anet/daemon.json), which names the daemon of
// the real user of the machine. A test once ran `anet stop` that way and stopped that daemon.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// realUserHome is the home directory of the user who runs the tests, read when the test binary
// starts, before a test changes HOME.
var realUserHome = func() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(h); err == nil {
		return r
	}
	return h
}()

// tempRoot is the temporary directory, with its links resolved.
func tempRoot() string {
	d := os.TempDir()
	if r, err := filepath.EvalSymlinks(d); err == nil {
		return r
	}
	return d
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// temporaryDir reports whether p is a temporary directory and not one of the real user's.
func temporaryDir(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	} else if r, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		abs = filepath.Join(r, filepath.Base(abs))
	}
	if !within(abs, tempRoot()) {
		return false
	}
	return realUserHome == "" || within(realUserHome, tempRoot()) || !within(abs, realUserHome)
}

// guardCLIRun fails the test unless the anet command with env and args runs with temporary
// directories only and with an explicitly selected identity (checkCLIRun).
func guardCLIRun(t *testing.T, env, args []string) {
	t.Helper()
	if err := checkCLIRun(env, args); err != nil {
		t.Fatal(err)
	}
}

// checkCLIRun is why the anet command with env and args could reach the real user's anet, or nil.
func checkCLIRun(env, args []string) error {
	val := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		val[k] = v // the last value wins, as for the process
	}
	for _, k := range []string{"HOME", "XDG_RUNTIME_DIR"} {
		if val[k] == "" {
			return fmt.Errorf("testguard: anet %v runs without %s; give it a temporary one", args, k)
		}
	}
	for _, k := range []string{"HOME", "ANET_HOME", "ANET_DATA_DIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "TMPDIR",
		"USERPROFILE", "LOCALAPPDATA"} {
		if v := val[k]; v != "" && !temporaryDir(v) {
			return fmt.Errorf("testguard: anet %v runs with %s=%s, which is not a temporary directory", args, k, v)
		}
	}
	explicit := val["ANET_ID"] != "" || val["ANET_DATA_DIR"] != "" || val["ANET_HOME"] != ""
	for _, a := range args {
		if a == "--id" || strings.HasPrefix(a, "--id=") {
			explicit = true
		}
	}
	if !explicit {
		return fmt.Errorf("testguard: anet %v selects no identity explicitly (--id, ANET_ID, ANET_HOME or ANET_DATA_DIR); "+
			"it would resolve through the daemon pointer of the real user", args)
	}
	return nil
}

// The guard refuses the run that stopped the real user's daemon (no explicit identity), a real
// home and a missing runtime directory, and lets a run of a cliHome through.
func TestTestGuard(t *testing.T) {
	h := newCLIHome(t, "en")
	if err := checkCLIRun(h.env(), []string{"status"}); err != nil {
		t.Errorf("a cliHome run: %v", err)
	}
	if err := checkCLIRun(h.env("ANET_HOME=", "ANET_DATA_DIR="), []string{"--id", "default", "stop"}); err != nil {
		t.Errorf("--id default: %v", err)
	}
	for name, c := range map[string]struct{ env, args []string }{
		"no explicit identity": {h.env("ANET_HOME=", "ANET_DATA_DIR="), []string{"stop"}},
		"the real home":        {h.env("HOME=" + realUserHome), []string{"status"}},
		"no runtime directory": {h.env("XDG_RUNTIME_DIR="), []string{"status"}},
		"a data dir elsewhere": {h.env("ANET_DATA_DIR=/var/lib/anet"), []string{"status"}},
	} {
		if realUserHome == "" && name == "the real home" {
			continue
		}
		if err := checkCLIRun(c.env, c.args); err == nil {
			t.Errorf("%s: let through", name)
		}
	}
}

// guardInProcess fails the test unless the environment of this process names temporary
// directories for the home, the anet home and the runtime directory, for a test that runs
// the code of `anet mcp` in the test process.
func guardInProcess(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HOME", "XDG_RUNTIME_DIR"} {
		if v := os.Getenv(k); v == "" || !temporaryDir(v) {
			t.Fatalf("testguard: %s=%q is not a temporary directory", k, v)
		}
	}
	for _, k := range []string{"ANET_HOME", "ANET_DATA_DIR"} {
		if v := os.Getenv(k); v != "" && !temporaryDir(v) {
			t.Fatalf("testguard: %s=%q is not a temporary directory", k, v)
		}
	}
}

// daemonHolding is the process ID of the daemon that holds the lock file lock, found through
// /proc (Linux), among the processes of the test binary only; 0 when there is none.
func daemonHolding(t *testing.T, lock string) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, _ = filepath.EvalSymlinks(exe)
	lock, _ = filepath.EvalSymlinks(lock)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if p, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe")); err != nil || p != exe {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", e.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if p, err := os.Readlink(filepath.Join("/proc", e.Name(), "fd", fd.Name())); err == nil && p == lock {
				return pid
			}
		}
	}
	return 0
}

// stopRecorded ends the daemon whose process ID the test recorded: SIGTERM, and SIGKILL when it
// is still there after 10 seconds. No other process is signalled.
func stopRecorded(t *testing.T, pid int) {
	t.Helper()
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = p.Kill()
}

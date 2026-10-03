package main

// The CLI as a person runs it: this test binary re-executes itself as `anet`
// (TestMain runs main when ANET_TEST_RUN_MAIN is set), so a test can check
// what a command prints, its exit status, and what it touched — the data
// directory, the control plane — without building a second binary.

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("ANET_TEST_RUN_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// cliHome is an isolated home for one CLI run: its own HOME, data
// directory, runtime directory and language.
type cliHome struct {
	root, home, data, runtime string
	lang                      string // LANG for the run
}

func newCLIHome(t *testing.T, lang string) *cliHome {
	t.Helper()
	root := t.TempDir()
	h := &cliHome{root: root, home: filepath.Join(root, "home"), data: filepath.Join(root, "data"),
		runtime: filepath.Join(root, "run"), lang: lang}
	for _, d := range []string{h.home, h.data, h.runtime} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// env is the environment of a run: nothing of the test's own but PATH.
func (h *cliHome) env(extra ...string) []string {
	return append([]string{
		"ANET_TEST_RUN_MAIN=1", "PATH=" + os.Getenv("PATH"), "HOME=" + h.home, "ANET_HOME=" + h.home,
		"ANET_DATA_DIR=" + h.data, "XDG_RUNTIME_DIR=" + h.runtime, "XDG_CONFIG_HOME=" + filepath.Join(h.home, ".config"),
		"LC_ALL=", "LC_MESSAGES=", "LANG=" + h.lang,
	}, extra...)
}

// runCLI runs `anet args…` in h and returns its output and exit status.
func runCLI(t *testing.T, h *cliHome, args []string, extraEnv ...string) (stdout, stderr string, code int) {
	t.Helper()
	return runCLIWithin(t, h, 60*time.Second, args, extraEnv...)
}

// runCLIWithin is runCLI with a bound on how long the command may run: a
// command that should have refused at once and is still running has failed.
func runCLIWithin(t *testing.T, h *cliHome, within time.Duration, args []string, extraEnv ...string) (stdout, stderr string, code int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = h.env(extraEnv...)
	cmd.Dir = h.root
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(within):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("anet %s did not end within %v", strings.Join(args, " "), within)
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

// fakeControl is a control plane that counts the connections it is offered:
// a config.json and a token in h's data directory point the CLI at it.
type fakeControl struct {
	ln    net.Listener
	conns atomic.Int64
}

func newFakeControl(t *testing.T, h *cliHome) *fakeControl {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeControl{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fc.conns.Add(1)
			// An answer the CLI can read: an empty JSON object.
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 4096)
			_, _ = c.Read(buf)
			_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"))
			_ = c.Close()
		}
	}()
	cfg := `{"control_addr": "` + ln.Addr().String() + `"}`
	if err := os.WriteFile(filepath.Join(h.data, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.data, "control_token.txt"), []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return fc
}

// snapshotDir is every file under dir with its mode and bytes.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if fi.IsDir() {
			out[rel+"/"] = fi.Mode().String()
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = fi.Mode().String() + "\x00" + string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameSnapshot(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

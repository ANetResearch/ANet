//go:build unix

package procgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitForPID reads the pid a test command writes to file.
func waitForPID(t *testing.T, file string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(file); err == nil && strings.HasSuffix(string(b), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", file)
	return 0
}

// gone reports whether pid no longer runs (absent, or a zombie waiting for its new parent).
func gone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && strings.HasPrefix(s[i+1:], " Z")
}

func TestCancelKillsTheGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `sleep 60 & echo $! > "$PIDFILE"; wait`)
	cmd.Env = append(os.Environ(), "PIDFILE="+pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	attr := cmd.SysProcAttr
	g, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr != attr || !attr.Setpgid {
		t.Errorf("SysProcAttr: %+v (same struct: %v)", cmd.SysProcAttr, cmd.SysProcAttr == attr)
	}
	grandchild := waitForPID(t, pidFile)
	cancel()
	if err := g.Wait(); err == nil {
		t.Error("Wait after cancel returned nil")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !gone(grandchild) {
		if time.Now().After(deadline) {
			syscall.Kill(grandchild, syscall.SIGKILL)
			t.Fatalf("grandchild %d outlived the cancelled group", grandchild)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRun(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "exit 0")
	if err := Run(cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.Cancel == nil {
		t.Errorf("Start did not set the group up: %+v", cmd.SysProcAttr)
	}
	cmd = exec.CommandContext(context.Background(), "/bin/sh", "-c", "exit 3")
	var ee *exec.ExitError
	if err := Run(cmd); !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Errorf("Run(exit 3) = %v", err)
	}
	if err := Run(exec.CommandContext(context.Background(), "procgroup-test-no-such-command")); err == nil {
		t.Error("Run of a missing command returned nil")
	}
}

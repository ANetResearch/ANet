//go:build windows

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

	"golang.org/x/sys/windows"
)

// winPowerShell is Windows PowerShell 5.1, which every supported Windows has.
func winPowerShell(t *testing.T) string {
	t.Helper()
	dir, err := windows.GetSystemWindowsDirectory()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

// grandchildScript starts a grandchild PowerShell that writes its pid to $env:PROCGROUP_PIDFILE
// and sleeps; startArgs are the extra Start-Process parameters.
func grandchildScript(startArgs, then string) string {
	return `Start-Process ` + startArgs + ` -FilePath (Join-Path $PSHOME 'powershell.exe') ` +
		`-ArgumentList '-NoProfile','-NonInteractive','-Command','$PID | Out-File -Encoding ascii -FilePath $env:PROCGROUP_PIDFILE; Start-Sleep 120'; ` + then
}

func winWaitForPID(t *testing.T, file string) int {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(file); err == nil && strings.HasSuffix(string(b), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatalf("pid file %q: %v", b, err)
			}
			return pid
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", file)
	return 0
}

// winExits reports whether pid has exited, or exits within d.
func winExits(t *testing.T, pid int, d time.Duration) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return true // no such process
	}
	if err != nil {
		t.Fatalf("open process %d: %v", pid, err)
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(d/time.Millisecond))
	if err != nil {
		t.Fatalf("wait for process %d: %v", pid, err)
	}
	return ev == windows.WAIT_OBJECT_0
}

func winTerminate(pid int) {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	windows.TerminateProcess(h, 1)
	windows.CloseHandle(h)
}

func TestWindowsCancelKillsTheTree(t *testing.T) {
	jobs := openJobs.Load()
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, winPowerShell(t), "-NoProfile", "-NonInteractive", "-Command",
		grandchildScript("-NoNewWindow", "Start-Sleep 120"))
	cmd.Env = append(os.Environ(), "PROCGROUP_PIDFILE="+pidFile)
	g, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	grandchild := winWaitForPID(t, pidFile)
	cancel()
	if err := g.Wait(); err == nil {
		t.Error("Wait after cancel returned nil")
	}
	if !winExits(t, grandchild, 5*time.Second) {
		winTerminate(grandchild)
		t.Fatalf("grandchild %d outlived the cancelled tree", grandchild)
	}
	if n := openJobs.Load(); n != jobs {
		t.Errorf("%d job handles left open", n-jobs)
	}
}

func TestWindowsWaitReleasesTheJob(t *testing.T) {
	jobs := openJobs.Load()
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.CommandContext(context.Background(), winPowerShell(t), "-NoProfile", "-NonInteractive", "-Command",
		grandchildScript("-WindowStyle Hidden", "exit 0"))
	cmd.Env = append(os.Environ(), "PROCGROUP_PIDFILE="+pidFile)
	if err := Run(cmd); err != nil {
		t.Fatal(err)
	}
	grandchild := winWaitForPID(t, pidFile)
	t.Cleanup(func() { winTerminate(grandchild) })
	if winExits(t, grandchild, time.Second) {
		t.Fatalf("grandchild %d died with the command it outlived", grandchild)
	}
	if n := openJobs.Load(); n != jobs {
		t.Errorf("%d job handles left open", n-jobs)
	}
}

func TestWindowsMissingCommand(t *testing.T) {
	jobs := openJobs.Load()
	g, err := Start(exec.CommandContext(context.Background(), "procgroup-test-no-such-command"))
	if err == nil || g != nil || !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("Start = %v, %v; want the exec.ErrNotFound of cmd.Start", g, err)
	}
	if n := openJobs.Load(); n != jobs {
		t.Errorf("%d job handles left open", n-jobs)
	}
}

func TestWindowsStartKeepsSysProcAttr(t *testing.T) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	const belowNormalPriority = 0x00004000
	cmd := exec.CommandContext(context.Background(), filepath.Join(sys, "cmd.exe"), "/c", "exit 7")
	attr := &syscall.SysProcAttr{HideWindow: true, CreationFlags: belowNormalPriority}
	cmd.SysProcAttr = attr
	var ee *exec.ExitError
	if err := Run(cmd); !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Errorf("Run(exit 7) = %v", err)
	}
	want := uint32(belowNormalPriority | createSuspended | createNewProcessGroup | createNoWindow)
	if cmd.SysProcAttr != attr || !attr.HideWindow || attr.CreationFlags != want {
		t.Errorf("SysProcAttr: %+v, want CreationFlags %#x", cmd.SysProcAttr, want)
	}
	if cmd.Cancel == nil {
		t.Error("Start did not set cmd.Cancel")
	}
}

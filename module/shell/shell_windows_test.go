//go:build shell && windows

package shell

// shell_windows_test.go: the shell module on Windows, where commands run in Windows PowerShell by default.
// Every test is self-contained (t.TempDir(), no network, no repository files) so that it runs as a
// cross-compiled test binary (`shell.test.exe -test.run Windows -test.v`) and on a Windows CI runner.

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/ANetResearch/ANetCore/effect"
)

// psQuote is s as a PowerShell single-quoted string.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func systemDir(t *testing.T) string {
	t.Helper()
	sys, err := windows.GetSystemWindowsDirectory()
	if err != nil {
		t.Fatal(err)
	}
	return sys
}

// processGone waits up to d for process pid to end and reports whether it has.
func processGone(t *testing.T, pid int, d time.Duration) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return true // no such process
		}
		t.Fatalf("OpenProcess(%d): %v", pid, err)
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(d.Milliseconds()))
	if err != nil {
		t.Fatalf("WaitForSingleObject(%d): %v", pid, err)
	}
	return ev == windows.WAIT_OBJECT_0
}

// terminatePID ends process pid, which this test started.
func terminatePID(pid int) {
	if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid)); err == nil {
		_ = windows.TerminateProcess(h, 1)
		_ = windows.CloseHandle(h)
	}
}

// readPID reads the process id a command wrote into path.
func readPID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the command wrote no process id: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff")))
	if err != nil || pid <= 0 {
		t.Fatalf("process id file holds %q", b)
	}
	return pid
}

// startPing is a PowerShell line that starts ping.exe for about n seconds in the background and writes
// its process id into pidFile.
func startPing(t *testing.T, n int, pidFile string) string {
	ping := filepath.Join(systemDir(t), `System32\PING.EXE`)
	return "$p = Start-Process -FilePath " + psQuote(ping) + " -ArgumentList '-n','" + strconv.Itoa(n) +
		"','127.0.0.1' -WindowStyle Hidden -PassThru; [System.IO.File]::WriteAllText(" + psQuote(pidFile) +
		", [string]$p.Id)"
}

// The default interpreter, and the "powershell" keyword in any case, is Windows PowerShell in the system
// directory, never a program found on PATH; "cmd" is cmd.exe there.
func TestWindowsDefaultInterpreterIsSystemPowerShell(t *testing.T) {
	sys := systemDir(t)
	for _, kw := range []string{"", "powershell", "PowerShell"} {
		ip, err := resolveInterpreter(kw)
		if err != nil {
			t.Fatalf("%q: %v", kw, err)
		}
		want := filepath.Join(sys, `System32\WindowsPowerShell\v1.0\powershell.exe`)
		if !strings.EqualFold(ip.path, want) || ip.family != familyPowerShell {
			t.Fatalf("%q resolved to %+v, want %s", kw, ip, want)
		}
	}
	ip, err := resolveInterpreter("CMD")
	if err != nil || !strings.EqualFold(ip.path, filepath.Join(sys, `System32\cmd.exe`)) || ip.family != familyCmd {
		t.Fatalf("cmd resolved to %+v %v", ip, err)
	}
	m, err := newModule([]byte(`{"allow_arbitrary":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if ip := m.(*Module).ip; ip == nil || !strings.HasPrefix(strings.ToLower(ip.path), strings.ToLower(sys)+`\`) {
		t.Fatalf("the module's interpreter is %+v, not under %s", ip, sys)
	}
}

// An interpreter that is not one of the supported ones is a configuration error, so the module is not
// created: a program name (searched on PATH), a relative path, or an absolute path to another program.
func TestWindowsUnsupportedInterpreterFailsTheModule(t *testing.T) {
	for _, sh := range []string{"notepad.exe", `C:\Windows\notepad.exe`, "powershell.exe", `.\powershell.exe`,
		`bin\bash.exe`, "/bin/sh", "sh"} {
		raw := `{"allow_arbitrary":true,"shell":` + strconv.Quote(sh) + `}`
		if m, err := newModule([]byte(raw)); err == nil {
			t.Errorf("interpreter %q was accepted: %+v", sh, m)
		} else if !strings.Contains(err.Error(), "not supported on Windows") {
			t.Errorf("interpreter %q: %v", sh, err)
		}
	}
	for _, sh := range []string{`C:\Program Files\Git\bin\bash.exe`, `C:\Tools\PWSH.EXE`, `D:\x\cmd.exe`} {
		if _, err := newModule([]byte(`{"allow_arbitrary":true,"shell":` + strconv.Quote(sh) + `}`)); err != nil {
			t.Errorf("interpreter %q was refused: %v", sh, err)
		}
	}
}

func TestWindowsWriteOutput(t *testing.T) {
	cfg := Config{AllowLocal: true, AllowArbitrary: true}
	_, h := started(t, cfg)
	eff, err := invoke(t, h, CapExec, "", map[string]any{"command": "Write-Output hi"})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.OK || strings.TrimSpace(eff.Evidence.ObservedState) != "hi" {
		t.Fatalf("got %s %q", eff.Status, eff.Evidence.ObservedState)
	}
}

// Output is UTF-8 whatever the console code page is.
func TestWindowsOutputIsUTF8(t *testing.T) {
	_, h := started(t, Config{AllowLocal: true, AllowArbitrary: true})
	eff, err := invoke(t, h, CapExec, "", map[string]any{"command": "Write-Output ([string][char]0x00e9 + [char]0x4e2d)"})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.OK || strings.TrimSpace(eff.Evidence.ObservedState) != "\u00e9\u4e2d" {
		t.Fatalf("got %s %q (% x)", eff.Status, eff.Evidence.ObservedState, eff.Evidence.ObservedState)
	}
}

func TestWindowsNonZeroExitIsFailed(t *testing.T) {
	cfg := Config{AllowLocal: true, Allow: []string{"did:anet:friend"}, Commands: map[string]Command{
		"fail": {Run: "[Console]::Error.WriteLine('to-stderr'); exit 3"},
	}}
	_, h := started(t, cfg)
	eff, err := invoke(t, h, runPrefix+"fail", "did:anet:friend", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Failed || eff.Message != "exit 3" || eff.Record.Metrics["exit_code"] != 3 {
		t.Fatalf("got %s %q %+v", eff.Status, eff.Message, eff.Record.Metrics)
	}
	if !strings.Contains(eff.Evidence.ObservedState, "to-stderr") {
		t.Fatalf("stderr was not captured: %q", eff.Evidence.ObservedState)
	}
	runs := h.events(EvCommandRun)
	if len(runs) != 1 || runs[0]["caller"] != "did:anet:friend" || runs[0]["command"] != "fail" || runs[0]["exit_code"] != 3 {
		t.Fatalf("records: %+v", runs)
	}
}

// A command that times out is FAILED, killed, and so is what it started: a grandchild started with
// Start-Process does not outlive the timeout (procgroup's Job object).
func TestWindowsTimeoutKillsTheGrandchild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	cfg := Config{AllowLocal: true, Commands: map[string]Command{
		"slow": {Run: startPing(t, 120, pidFile) + "; Start-Sleep -Seconds 120", TimeoutS: 5},
	}}
	_, h := started(t, cfg)
	start := time.Now()
	eff, err := invoke(t, h, runPrefix+"slow", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Failed || !strings.Contains(eff.Message, "killed") {
		t.Fatalf("got %s %q", eff.Status, eff.Message)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("the timeout did not take effect: %s", d)
	}
	pid := readPID(t, pidFile)
	t.Cleanup(func() { terminatePID(pid) })
	if !processGone(t, pid, 5*time.Second) {
		t.Fatalf("the grandchild %d survived the timeout", pid)
	}
}

// A command that completes leaves what it started in the background running, as on unix.
func TestWindowsCompletionLeavesABackgroundProcess(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "background.pid")
	cfg := Config{AllowLocal: true, Commands: map[string]Command{
		"bg": {Run: startPing(t, 60, pidFile), TimeoutS: 60},
	}}
	_, h := started(t, cfg)
	eff, err := invoke(t, h, runPrefix+"bg", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.OK {
		t.Fatalf("got %s %q: %q", eff.Status, eff.Message, eff.Evidence.ObservedState)
	}
	pid := readPID(t, pidFile)
	defer terminatePID(pid)
	if processGone(t, pid, 500*time.Millisecond) {
		t.Fatalf("the background process %d ended with the command", pid)
	}
}

// Caller arguments are one literal each, whichever of PowerShell's five single-quote characters they
// carry: none of them ends the quoted string and lets the rest run as a command.
func TestWindowsCallerArgumentsCannotBecomeCommands(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AllowLocal: true, Commands: map[string]Command{"say": {Run: "Write-Output", Args: true}}}
	_, h := started(t, cfg)
	for i, q := range []string{"'", "\u2018", "\u2019", "\u201a", "\u201b"} {
		marker := filepath.Join(dir, "marker"+strconv.Itoa(i))
		arg := "x" + q + "; New-Item -ItemType File -Path " + psQuote(marker) + "; " + q
		eff, err := invoke(t, h, runPrefix+"say", "", map[string]any{"argv": []any{arg}})
		if err != nil {
			t.Fatalf("quote %U: %v", []rune(q)[0], err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("quote %U: an injected argument executed", []rune(q)[0])
		}
		if eff.Status != effect.OK || strings.TrimSpace(eff.Evidence.ObservedState) != arg {
			t.Fatalf("quote %U: the argument was not passed through as a literal: %s %q", []rune(q)[0], eff.Status, eff.Evidence.ObservedState)
		}
	}
	// Two arguments stay two, and $ is not expanded.
	eff, err := invoke(t, h, runPrefix+"say", "", map[string]any{"argv": []any{"a b", "$env:USERNAME"}})
	if err != nil || strings.ReplaceAll(strings.TrimSpace(eff.Evidence.ObservedState), "\r\n", "\n") != "a b\n$env:USERNAME" {
		t.Fatalf("got %v %q", err, eff.Evidence.ObservedState)
	}
	// A control character other than TAB is refused, and nothing runs.
	before := len(h.events(EvCommandRun))
	if _, err := invoke(t, h, runPrefix+"say", "", map[string]any{"argv": []any{"a\nb"}}); err == nil ||
		!strings.Contains(err.Error(), "control character") {
		t.Fatalf("a newline in an argument: %v", err)
	}
	if len(h.events(EvCommandRun)) != before {
		t.Fatal("a refused argument list ran")
	}
}

// cmd.exe has no quoting that holds for every argument, so a command with args:true is refused there.
func TestWindowsCmdRefusesArguments(t *testing.T) {
	cfg := Config{AllowLocal: true, Shell: "cmd", Commands: map[string]Command{
		"say": {Run: "echo", Args: true},
		"ver": {Run: "echo cmd-ok"},
	}}
	_, h := started(t, cfg)
	if _, err := invoke(t, h, runPrefix+"say", "", map[string]any{"argv": []any{"x & whoami"}}); err == nil ||
		!strings.Contains(err.Error(), "cmd.exe") {
		t.Fatalf("arguments to cmd.exe: %v", err)
	}
	if n := len(h.events(EvCommandRun)); n != 0 {
		t.Fatalf("%d commands ran", n)
	}
	eff, err := invoke(t, h, runPrefix+"ver", "", nil)
	if err != nil || eff.Status != effect.OK || strings.TrimSpace(eff.Evidence.ObservedState) != "cmd-ok" {
		t.Fatalf("a command without arguments under cmd.exe: %v %+v", err, eff)
	}
}

// A command line too long for -EncodedCommand is refused before anything starts.
func TestWindowsOverlongCommandIsRefused(t *testing.T) {
	_, h := started(t, Config{AllowLocal: true, AllowArbitrary: true})
	_, err := invoke(t, h, CapExec, "", map[string]any{"command": "Write-Output " + strings.Repeat("x", 12000)})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("got %v", err)
	}
	if n := len(h.events(EvCommandRun)); n != 0 {
		t.Fatalf("%d commands ran", n)
	}
}

// Only the allowed AID may call: a second AID is refused, and so is a call with no caller identity while
// allow_local is off; both refusals are on the chain.
func TestWindowsOnlyTheAllowedCallerRuns(t *testing.T) {
	cfg := Config{Allow: []string{"did:anet:controller"}, AllowArbitrary: true}
	_, h := started(t, cfg)
	for _, caller := range []string{"did:anet:other", ""} {
		eff, err := invoke(t, h, CapExec, caller, map[string]any{"command": "Write-Output ran"})
		if err != nil {
			t.Fatal(err)
		}
		if eff.Status != effect.Unavailable {
			t.Fatalf("caller %q: got %s", caller, eff.Status)
		}
	}
	refused := h.events(EvCommandRefused)
	if len(refused) != 2 || refused[0]["caller"] != "did:anet:other" || refused[1]["caller"] != "" {
		t.Fatalf("refusals: %+v", refused)
	}
	if n := len(h.events(EvCommandRun)); n != 0 {
		t.Fatalf("%d commands ran", n)
	}
	eff, err := invoke(t, h, CapExec, "did:anet:controller", map[string]any{"command": "Write-Output ran"})
	if err != nil || eff.Status != effect.OK || strings.TrimSpace(eff.Evidence.ObservedState) != "ran" {
		t.Fatalf("the allowed caller: %v %+v", err, eff)
	}
}

// An empty dir runs the command in the user's home directory.
func TestWindowsDefaultDirIsTheHomeDirectory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory: ", err)
	}
	_, h := started(t, Config{AllowLocal: true, AllowArbitrary: true})
	eff, err := invoke(t, h, CapExec, "", map[string]any{"command": "Write-Output (Get-Location).Path"})
	if err != nil || eff.Status != effect.OK || !strings.EqualFold(strings.TrimSpace(eff.Evidence.ObservedState), home) {
		t.Fatalf("got %v %q, want %s", err, eff.Evidence.ObservedState, home)
	}
}

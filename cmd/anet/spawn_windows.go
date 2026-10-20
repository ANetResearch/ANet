//go:build windows

package main

import (
	"errors"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Process creation flags of the detached daemon (design D10).
const (
	detachedProcess        = 0x00000008 // DETACHED_PROCESS: no console; it outlives the launching window
	createNewProcessGroup  = 0x00000200 // CREATE_NEW_PROCESS_GROUP: Ctrl+C in that window does not reach it
	createBreakawayFromJob = 0x01000000 // CREATE_BREAKAWAY_FROM_JOB: not ended with a job the shell runs in
)

// detachSysProcAttr starts the daemon without a console, in its own process group, and outside any job
// the launching shell belongs to, so it survives the PowerShell window (or an agent's tool call) that
// started it. See runDaemonDetached and startDetached.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup | createBreakawayFromJob,
		HideWindow:    true,
	}
}

// startDetached starts c and returns the command that runs. A job that does not allow breakaway
// refuses CREATE_BREAKAWAY_FROM_JOB with ERROR_ACCESS_DENIED; the start is then tried once more without
// that flag, as a new command with the same settings (an exec.Cmd cannot be started twice), and the
// daemon stays in the job (it then ends when the job does).
func startDetached(c *exec.Cmd) (*exec.Cmd, error) {
	return startDetachedWith(c, (*exec.Cmd).Start)
}

// startDetachedWith is startDetached with the start function given, for tests.
func startDetachedWith(c *exec.Cmd, start func(*exec.Cmd) error) (*exec.Cmd, error) {
	err := start(c)
	if err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		c.SysProcAttr == nil || c.SysProcAttr.CreationFlags&createBreakawayFromJob == 0 {
		return c, err
	}
	attr := *c.SysProcAttr
	attr.CreationFlags &^= createBreakawayFromJob
	retry := exec.Command(c.Path, c.Args[1:]...)
	retry.Env, retry.Dir = c.Env, c.Dir
	retry.Stdin, retry.Stdout, retry.Stderr = c.Stdin, c.Stdout, c.Stderr
	retry.ExtraFiles, retry.WaitDelay = c.ExtraFiles, c.WaitDelay
	retry.SysProcAttr = &attr
	return retry, start(retry)
}

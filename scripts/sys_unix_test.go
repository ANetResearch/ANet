//go:build !windows

package scripts

import (
	"os/exec"
	"syscall"
)

// killPID ends a process this test started, by its PID.
func killPID(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }

// newSession starts cmd in its own session, the way the scripts start theirs.
func newSession(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// mkfifo makes a named pipe.
func mkfifo(path string, mode uint32) error { return syscall.Mkfifo(path, mode) }

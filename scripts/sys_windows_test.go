//go:build windows

package scripts

// The tests that use these helpers drive bash and read /proc; they skip on Windows (needLinuxShell).

import (
	"errors"
	"os"
	"os/exec"
)

// killPID ends a process this test started, by its PID.
func killPID(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
		_ = p.Release()
	}
}

// newSession: Windows has no sessions in the unix sense; the process is started as it is.
func newSession(*exec.Cmd) {}

// mkfifo: Windows has no named pipes in the file system.
func mkfifo(string, uint32) error { return errors.ErrUnsupported }

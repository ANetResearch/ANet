//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// detachFromTerminal starts c in a session of its own, without a controlling terminal: a CLI run in a
// test is a run without a terminal, whatever terminal the test itself was started from (tty.go reads
// /dev/tty, which a test must not reach).
func detachFromTerminal(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

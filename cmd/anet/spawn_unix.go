//go:build !windows

package main

import "syscall"

// detachSysProcAttr puts the spawned daemon in its own session (setsid) so it is not a child of the
// launching shell's process group — it survives the shell (or an agent's tool-call) exiting. See
// runDaemonDetached.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

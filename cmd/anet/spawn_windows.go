//go:build windows

package main

import "syscall"

// detachSysProcAttr requests a new process group on Windows so the spawned daemon is not tied to the
// launching console. See runDaemonDetached.
func detachSysProcAttr() *syscall.SysProcAttr {
	const createNewProcessGroup = 0x00000200
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}

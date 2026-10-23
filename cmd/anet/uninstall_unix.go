//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

// platformUninstall adds what install.sh makes besides the binary: the
// links to it (linkSteps) and the PATH blocks of the shell profiles and the
// fish file (profileBlockSteps).
func platformUninstall(u *uninstaller, p *uninstallPlan) {
	linkSteps(p, u.exe, u.linkDirs, u.getenv("PATH"), u.goos)
	dir := filepath.Dir(u.exe)
	sh, fish := profileFiles(u.home, u.getenv)
	for _, f := range sh {
		profileBlockSteps(p, f, dir, u.home, false)
	}
	profileBlockSteps(p, fish, dir, u.home, true)
}

// removeInstalledExe removes the binary. A running program may be removed
// on this system: the processes that run it keep it until they end.
func removeInstalledExe(exe string) (string, error) { return "", os.Remove(exe) }

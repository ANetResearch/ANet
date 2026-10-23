//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// What install.ps1 writes to the registry of the user (Set-AnetAutostart,
// Set-AnetUserPath).
const (
	runKeyPath   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName = "AgentNetwork"
	envKeyPath   = `Environment`
)

// platformUninstall adds what install.ps1 makes besides the binary: the Run
// value AgentNetwork when it starts this binary, the install directory in
// the user's PATH, and the binaries an update moved aside or an
// installation that stopped left staged there (installLeftovers).
func platformUninstall(u *uninstaller, p *uninstallPlan) {
	dir := filepath.Dir(u.exe)
	if v, err := readUserString(runKeyPath, runValueName); err == nil {
		if exe := runValueExe(v); exe != "" && sameExe(exe, u.exe) {
			p.add(`the value AgentNetwork under HKCU\`+runKeyPath+`, which starts the node at logon`,
				func() error { return deleteUserValue(runKeyPath, runValueName) }, "")
		} else {
			p.kept(`the value AgentNetwork under HKCU\%s, which runs %s, not this anet`, runKeyPath, v)
		}
	}
	if raw, err := readUserString(envKeyPath, "Path"); err == nil {
		if rest, found := withoutPathEntry(raw, dir, expandEnv); found {
			p.add(dir+" in the user's PATH", func() error { return writeUserPath(rest) }, "")
		}
	}
	for _, f := range installLeftovers(dir) {
		p.add(f, func() error { return os.Remove(f) }, "")
	}
}

// removeInstalledExe removes anet.exe, which is running: it is this
// command. Windows does not delete a program that runs, and lets it be
// renamed. So, as Microsoft documents for a file in use, the binary is
// renamed aside first (<exe>.old-<milliseconds>, the name `anet update`
// and install.ps1 give a binary they move aside and remove later), and its
// deletion is then scheduled: with MoveFileEx(MOVEFILE_DELAY_UNTIL_REBOOT)
// at the next restart where Windows allows it (an elevated window), and in
// every case by a hidden powershell.exe that waits for this process to end,
// deletes the file, and removes the install directory when it is one
// install.ps1 chooses and nothing else is left in it.
func removeInstalledExe(exe string) (string, error) {
	aside := exe + ".old-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	if err := os.Rename(exe, aside); err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	atReboot := false
	if p, err := windows.UTF16PtrFromString(aside); err == nil {
		atReboot = windows.MoveFileEx(p, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT) == nil
	}
	removeDir := installerDir(dir)
	helperErr := startDeleteHelper(aside, dir, removeDir)
	switch {
	case helperErr == nil:
		note := fmt.Sprintf("%s is running (it is this command): it was renamed to %s, which is deleted once this command has ended", exe, aside)
		if removeDir {
			note += ", with " + dir + " when nothing else is left in it"
		}
		return note + ".", nil
	case atReboot:
		return fmt.Sprintf("%s is running (it is this command): it was renamed to %s, which is deleted at the next restart of Windows.", exe, aside), nil
	default:
		return fmt.Sprintf("%s is running (it is this command): it was renamed to %s; delete it once this command has ended: Remove-Item -LiteralPath '%s'",
			exe, aside, strings.ReplaceAll(aside, "'", "''")), nil
	}
}

// startDeleteHelper starts the hidden powershell.exe of removeInstalledExe.
func startDeleteHelper(aside, dir string, removeDir bool) error {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	ps := filepath.Join(sys, "WindowsPowerShell", "v1.0", "powershell.exe")
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := "$ErrorActionPreference = 'SilentlyContinue'; Wait-Process -Id " + strconv.Itoa(os.Getpid()) + " -Timeout 300; " +
		"for ($i = 0; $i -lt 30 -and (Test-Path -LiteralPath " + q(aside) + "); $i++) { Remove-Item -LiteralPath " + q(aside) +
		" -Force; if (Test-Path -LiteralPath " + q(aside) + ") { Start-Sleep -Seconds 1 } }"
	if removeDir {
		script += "; if (@(Get-ChildItem -LiteralPath " + q(dir) + " -Force).Count -eq 0) { Remove-Item -LiteralPath " + q(dir) + " }"
	}
	c := exec.Command(ps, "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	c.SysProcAttr = detachSysProcAttr()
	c, err = startDetached(c)
	if err != nil {
		return err
	}
	return c.Process.Release()
}

// installerDir reports whether dir is one of the install directories
// install.ps1 chooses without -Prefix (Select-AnetInstallDir), which it
// creates itself.
func installerDir(dir string) bool {
	local, profile := os.Getenv("LOCALAPPDATA"), os.Getenv("USERPROFILE")
	for _, d := range []string{filepath.Join(local, "Programs", "anet"), filepath.Join(local, "anet", "bin"), filepath.Join(profile, ".anet-bin")} {
		if local != "" && profile != "" && strings.EqualFold(filepath.Clean(d), filepath.Clean(dir)) {
			return true
		}
	}
	return false
}

// sameExe reports whether a and b name the same file.
func sameExe(a, b string) bool {
	if strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) {
		return true
	}
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}

// expandEnv expands %NAME% in a PATH entry, as Windows does for a new
// window.
func expandEnv(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	if v, err := registry.ExpandString(s); err == nil {
		return v
	}
	return s
}

// readUserString reads a string value of a key of HKEY_CURRENT_USER, without
// expanding it.
func readUserString(path, name string) (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	return v, err
}

// deleteUserValue deletes a value of a key of HKEY_CURRENT_USER.
func deleteUserValue(path, name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// writeUserPath writes the user's PATH, of the type it has (REG_EXPAND_SZ
// stays one), or deletes the value when nothing is left in it, and tells
// the windows that are open that the environment changed, as setting it in
// the System Properties does.
func writeUserPath(value string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, envKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	_, kind, err := k.GetStringValue("Path")
	if err != nil {
		return err
	}
	switch {
	case value == "":
		err = k.DeleteValue("Path")
	case kind == registry.EXPAND_SZ || strings.Contains(value, "%"):
		err = k.SetExpandStringValue("Path", value)
	default:
		err = k.SetStringValue("Path", value)
	}
	if err != nil {
		return err
	}
	broadcastEnvironment()
	return nil
}

// broadcastEnvironment sends WM_SETTINGCHANGE "Environment" to the top-level
// windows, waiting at most five seconds for each that hangs.
func broadcastEnvironment() {
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
	)
	env, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	var result uintptr
	proc := windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")
	_, _, _ = proc.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)), smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&result)))
}

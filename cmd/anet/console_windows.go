//go:build windows

package main

// console_windows.go adjusts the console this process runs in (design D10, D14). Nothing here changes
// the console's code page: Go writes UTF-16 to a console, so text reaches it correctly as it is.

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modKernel32               = windows.NewLazySystemDLL("kernel32.dll")
	modUser32                 = windows.NewLazySystemDLL("user32.dll")
	procGetConsoleProcessList = modKernel32.NewProc("GetConsoleProcessList")
	procGetConsoleWindow      = modKernel32.NewProc("GetConsoleWindow")
	procShowWindow            = modUser32.NewProc("ShowWindow")
)

// hideOwnConsoleIfAlone hides the console window when this process is the only one attached to it:
// a console Windows created for this process alone, as when the logon Run value starts `anet up`.
// A console shared with a shell (PowerShell, cmd) is left as it is.
func hideOwnConsoleIfAlone() {
	if procGetConsoleProcessList.Find() != nil || procGetConsoleWindow.Find() != nil || procShowWindow.Find() != nil {
		return
	}
	var pids [2]uint32
	// The count of attached processes, 0 without a console; a count above len(pids) is returned as is.
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n != 1 {
		return
	}
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd == 0 {
		return
	}
	_, _, _ = procShowWindow.Call(hwnd, windows.SW_HIDE)
}

// enableVT turns on ANSI escape sequence processing for f when f is a console, and returns a function
// that restores the previous mode. ok is false when f is not a console or the console does not
// support the mode; the caller then writes no escape sequences.
func enableVT(f *os.File) (restore func(), ok bool) {
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return func() {}, false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return func() {}, true
	}
	if err := windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return func() {}, false
	}
	return func() { _ = windows.SetConsoleMode(h, mode) }, true
}

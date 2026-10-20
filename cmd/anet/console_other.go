//go:build !windows

package main

import "os"

// hideOwnConsoleIfAlone is a Windows console adjustment (console_windows.go); elsewhere it does nothing.
func hideOwnConsoleIfAlone() {}

// enableVT: a unix terminal takes escape sequences as they are.
func enableVT(*os.File) (restore func(), ok bool) { return func() {}, true }

//go:build !windows

package main

import (
	"io"
	"os"
)

// openTTY opens the controlling terminal. Tests replace it.
var openTTY = func() (io.ReadWriteCloser, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }

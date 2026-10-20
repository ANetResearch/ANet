//go:build windows

package main

// tty_windows.go is the terminal of tty.go on Windows (design D14): the console this process is
// attached to, opened by name as CONIN$ for input and CONOUT$ for output, the way /dev/tty names the
// controlling terminal on unix. Standard input is never used in its place: a process that has no
// console (started with DETACHED_PROCESS, or by a service) cannot open CONIN$, or gets a handle on
// which GetConsoleMode fails, and is refused with tty_required.

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// openTTY opens the console. Tests replace it.
var openTTY = func() (io.ReadWriteCloser, error) {
	c, err := openConsole()
	if err != nil {
		return nil, err
	}
	return c, nil
}

// winConsole is the console as one ReadWriteCloser: reads from CONIN$, writes to CONOUT$.
type winConsole struct {
	in, out *os.File
}

// openConsole opens CONIN$ and CONOUT$ and checks that the input is a console. Both handles are
// closed on any failure.
func openConsole() (*winConsole, error) {
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var mode uint32
	if err := windows.GetConsoleMode(windows.Handle(in.Fd()), &mode); err != nil {
		_ = in.Close()
		return nil, fmt.Errorf("CONIN$ is not a console: %w", err)
	}
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	return &winConsole{in: in, out: out}, nil
}

func (c *winConsole) Read(p []byte) (int, error)  { return c.in.Read(p) }
func (c *winConsole) Write(p []byte) (int, error) { return c.out.Write(p) }

func (c *winConsole) Close() error {
	return errors.Join(c.in.Close(), c.out.Close())
}

// inputFd is the console input handle, for term.ReadPassword (readPassphrase).
func (c *winConsole) inputFd() uintptr { return c.in.Fd() }

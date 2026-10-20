//go:build windows

package main

// Windows behaviour of the CLI (design D10, D14, D2). Self-contained: temporary directories, no
// network, no repository files, no console required; run as `anet.test.exe -test.run Windows`.

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

// The console satisfies what tty.go needs of a terminal, and readPassphrase finds its input handle.
var (
	_ io.ReadWriteCloser = (*winConsole)(nil)
	_ consoleInput       = (*winConsole)(nil)
)

func TestWindowsDetachSysProcAttr(t *testing.T) {
	a := detachSysProcAttr()
	const want = 0x00000008 | 0x00000200 | 0x01000000
	if a.CreationFlags != want || !a.HideWindow {
		t.Fatalf("CreationFlags %#x HideWindow %v, want %#x true", a.CreationFlags, a.HideWindow, want)
	}
}

// detachedCmd is the command runDaemonDetached starts, without starting it.
func detachedCmd(t *testing.T) (*exec.Cmd, *os.File) {
	t.Helper()
	logf, err := os.Create(filepath.Join(t.TempDir(), "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logf.Close() })
	c := exec.Command(`C:\nonexistent\anet-test.exe`, "daemon")
	c.Env = []string{"ANET_DATA_DIR=" + t.TempDir()}
	c.Stdout, c.Stderr = logf, logf
	c.SysProcAttr = detachSysProcAttr()
	return c, logf
}

// A job that refuses breakaway answers ERROR_ACCESS_DENIED: the start is tried once more, as a new
// command with the same settings and without CREATE_BREAKAWAY_FROM_JOB.
func TestWindowsStartDetachedRetriesWithoutBreakaway(t *testing.T) {
	c, logf := detachedCmd(t)
	var flags []uint32
	var cmds []*exec.Cmd
	start := func(x *exec.Cmd) error {
		flags = append(flags, x.SysProcAttr.CreationFlags)
		cmds = append(cmds, x)
		if len(flags) == 1 {
			return &os.PathError{Op: "fork/exec", Path: x.Path, Err: windows.ERROR_ACCESS_DENIED}
		}
		return nil
	}
	got, err := startDetachedWith(c, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(flags) != 2 || flags[0] != 0x01000208 || flags[1] != 0x00000208 {
		t.Fatalf("flags %#x", flags)
	}
	if got != cmds[1] || got == c {
		t.Fatal("the command returned is not the one started second")
	}
	if got.Path != c.Path || strings.Join(got.Args, " ") != strings.Join(c.Args, " ") ||
		strings.Join(got.Env, ";") != strings.Join(c.Env, ";") || got.Stdout != logf || got.Stderr != logf ||
		!got.SysProcAttr.HideWindow {
		t.Fatalf("the retry lost a setting: %+v", got)
	}
	if c.SysProcAttr.CreationFlags != 0x01000208 {
		t.Fatalf("the first command's flags changed: %#x", c.SysProcAttr.CreationFlags)
	}
}

// Any other failure, or a start without the breakaway flag, is not tried again.
func TestWindowsStartDetachedDoesNotRetryOtherFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		breakaway bool
	}{
		{"not found", &os.PathError{Op: "fork/exec", Err: windows.ERROR_FILE_NOT_FOUND}, true},
		{"denied without breakaway", &os.PathError{Op: "fork/exec", Err: windows.ERROR_ACCESS_DENIED}, false},
	} {
		c, _ := detachedCmd(t)
		if !tc.breakaway {
			c.SysProcAttr.CreationFlags &^= 0x01000000
		}
		n := 0
		got, err := startDetachedWith(c, func(*exec.Cmd) error { n++; return tc.err })
		if n != 1 || !errors.Is(err, tc.err.(*os.PathError).Err) || got != c {
			t.Fatalf("%s: %d starts, err %v", tc.name, n, err)
		}
	}
}

// A real start of a program that does not exist fails once, with the path's error, not with
// "already started" from a second Start of the same command.
func TestWindowsStartDetachedMissingProgram(t *testing.T) {
	c, _ := detachedCmd(t)
	if _, err := startDetached(c); err == nil || strings.Contains(err.Error(), "already started") {
		t.Fatalf("err %v", err)
	}
}

// ttyConfirm on a console: a CRLF answer "yes" goes on, anything else is declined, and no console is
// tty_required.
func TestWindowsTTYConfirmAnswers(t *testing.T) {
	for _, tc := range []struct {
		answer string
		want   error
	}{
		{"yes\r\n", nil},
		{"YES\r\n", nil},
		{"no\r\n", errDeclined},
		{"", errDeclined},
	} {
		withTTY(t, "")
		openTTY = func() (io.ReadWriteCloser, error) { return &fakeTTY{r: strings.NewReader(tc.answer)}, nil }
		if err := confirmOnTTY("trust", "Trust the peer."); !errors.Is(err, tc.want) {
			t.Fatalf("%q: %v", tc.answer, err)
		}
	}
	withTTY(t, "")
	if err := confirmOnTTY("trust", "Trust the peer."); !errors.Is(err, errNoTTY) {
		t.Fatalf("no console: %v", err)
	}
}

// fakeConsole is a terminal with a separate input handle, as winConsole is.
type fakeConsole struct {
	fakeTTY
	fd uintptr
}

func (f *fakeConsole) inputFd() uintptr { return f.fd }

// readPassphrase reads a console's passphrase through its input handle (term.ReadPassword), not from
// the buffered stream: a handle that is not a console is an error, and the line stays unread.
func TestWindowsReadPassphraseUsesConsoleInput(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "not-a-console"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := &fakeConsole{fakeTTY: fakeTTY{r: strings.NewReader("secret\r\n")}, fd: f.Fd()}
	s := &ttySession{rw: c, br: bufio.NewReader(c)}
	if p, err := readPassphrase(s, "Passphrase:"); err == nil {
		t.Fatalf("read %q from a file handle", p)
	}
	if rest, _ := s.br.ReadString('\n'); rest != "secret\r\n" {
		t.Fatalf("the stream was read: %q left", rest)
	}
}

// Without a console, openConsole fails or returns a console whose input mode can be read; it never
// returns a non-console. (On a runner without a console it fails; in a console window it succeeds.)
func TestWindowsOpenConsoleIsAConsoleOrAnError(t *testing.T) {
	c, err := openConsole()
	if err != nil {
		return
	}
	defer c.Close()
	var mode uint32
	if err := windows.GetConsoleMode(windows.Handle(c.inputFd()), &mode); err != nil {
		t.Fatalf("openConsole returned a handle that is not a console: %v", err)
	}
}

func TestWindowsEnableVTOnAFile(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	restore, ok := enableVT(f)
	restore()
	if ok {
		t.Fatal("a file took VT mode")
	}
}

// A file written by writeFileDurable with 0600 is private, and statFile reports it so: mode 0600.
// The directory writeFileDurable creates is private too.
func TestWindowsStatFilePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new dir")
	p := filepath.Join(dir, "secret.anetrec")
	if err := writeFileDurable(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.CheckPrivate(dir); err != nil {
		t.Fatalf("directory: %v", err)
	}
	if err := fsperm.CheckPrivate(p); err != nil {
		t.Fatalf("file: %v", err)
	}
	fs := statFile(p)
	if !fs.Present || !fs.Private || fs.Mode != "0600" {
		t.Fatalf("statFile: %+v", fs)
	}
	if fs := statFile(filepath.Join(dir, "absent")); fs.Present || fs.Private {
		t.Fatalf("absent: %+v", fs)
	}
	// Written again over the existing file: still private.
	if err := writeFileDurable(p, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "y" {
		t.Fatalf("%q %v", b, err)
	}
	if fs := statFile(p); !fs.Private {
		t.Fatalf("rewritten: %+v", fs)
	}
}

// The commands printed for the user are valid in Windows PowerShell 5.1 (no &&) and name this user.
func TestWindowsHints(t *testing.T) {
	if strings.Contains(restartNodeCmds, "&&") || strings.Contains(restartNodeQuoted, "&&") {
		t.Fatalf("restart: %q %q", restartNodeCmds, restartNodeQuoted)
	}
	h := privateFileHint(`C:\Users\A B\.anet\config.json`)
	if !strings.HasPrefix(h, `icacls "C:\Users\A B\.anet\config.json" /inheritance:r /grant:r "*`+fsperm.CurrentUser()+`:F"`) {
		t.Fatalf("hint %q", h)
	}
}

// The detached daemon's flags are the ones syscall passes to CreateProcess.
func TestWindowsDetachFlagsAreProcessCreationFlags(t *testing.T) {
	if detachedProcess != windows.DETACHED_PROCESS || createNewProcessGroup != windows.CREATE_NEW_PROCESS_GROUP ||
		createBreakawayFromJob != windows.CREATE_BREAKAWAY_FROM_JOB {
		t.Fatal("flag values differ from x/sys/windows")
	}
}

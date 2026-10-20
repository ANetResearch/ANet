//go:build windows

package main

// Windows behaviour of the CLI (design D10, D14, D2). Self-contained: temporary directories, no
// network, no repository files, no console required; run as `anet.test.exe -test.run Windows`.

import (
	"bufio"
	"errors"
	"fmt"
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
	// A file whose access control list cannot be read: no /remove.
	h := privateFileHint(`C:\Users\A B\.anet\config.json`)
	want := `icacls "C:\Users\A B\.anet\config.json" /inheritance:r /grant:r "*` + fsperm.CurrentUser() +
		`:F" "*S-1-5-18:F" "*S-1-5-32-544:F"`
	if h != want {
		t.Fatalf("hint %q, want %q", h, want)
	}
}

// winSetFileDACL sets the DACL of path from sddl. Unless protect is set, the entries that the
// directory above passes on are added as inherited entries, as for any file created there.
func winSetFileDACL(t *testing.T, path, sddl string, protect bool) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("SDDL %q: %v", sddl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("SDDL %q: %v", sddl, err)
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if protect {
		info = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		t.Fatalf("set the DACL of %s: %v", path, err)
	}
}

// The command privateFileHint prints leaves exactly the private access control list, whichever shell
// runs it: inherited entries for another account and explicit allow and deny entries for other
// accounts are all removed, and the file has three entries, for this user, SYSTEM and Administrators.
func TestWindowsPrivateFileHintLeavesOnlyThePrivateACL(t *testing.T) {
	user := fsperm.CurrentUser()
	dir := t.TempDir()
	// The directory passes read access for Authenticated Users on to the file (an inherited entry).
	winSetFileDACL(t, dir, "D:P(A;OICI;FA;;;"+user+")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FR;;;AU)", true)
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Explicit entries: read for Everyone, a denial for Guests, and the user's own grant.
	messy := "D:(A;;FR;;;WD)(D;;FW;;;BG)(A;;FA;;;" + user + ")"

	type shell struct {
		name, ext, body string
		cmd             func(script string) *exec.Cmd
	}
	shells := []shell{
		{"cmd", ".cmd", "@echo off\r\n%s\r\nexit /b %%errorlevel%%\r\n", func(script string) *exec.Cmd {
			return exec.Command("cmd.exe", "/d", "/c", script)
		}},
		{"powershell", ".ps1", "%s\r\nexit $LASTEXITCODE\r\n", func(script string) *exec.Cmd {
			return exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
		}},
	}
	for _, sh := range shells {
		t.Run(sh.name, func(t *testing.T) {
			if err := sh.cmd("x").Err; err != nil {
				t.Skipf("%s is not available: %v", sh.name, err)
			}
			winSetFileDACL(t, p, messy, false)
			if info, err := fsperm.Inspect(p); err != nil || info.Private() {
				t.Fatalf("the prepared file is private (%v) or cannot be read: %v", info.Private(), err)
			}
			h := privateFileHint(p)
			for _, sid := range []string{"S-1-1-0", "S-1-5-32-546"} {
				if !strings.Contains(h, ` "*`+sid+`"`) {
					t.Errorf("hint %q does not remove %s", h, sid)
				}
			}
			if strings.Contains(h, "S-1-5-11") || strings.Count(h, user) != 1 {
				t.Errorf("hint %q names an inherited entry or removes this user", h)
			}
			script := filepath.Join(t.TempDir(), "hint"+sh.ext)
			if err := os.WriteFile(script, []byte(fmt.Sprintf(sh.body, h)), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := sh.cmd(script).CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", h, err, out)
			}
			info, err := fsperm.Inspect(p)
			if err != nil || !info.Private() {
				t.Fatalf("after the hint the file is not private: %+v %v", info, err)
			}
			sids, err := fsperm.ExplicitSIDs(p)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, s := range sids {
				got[s] = true
			}
			if len(sids) != 3 || !got[user] || !got["S-1-5-18"] || !got["S-1-5-32-544"] {
				t.Errorf("explicit entries after the hint: %v", sids)
			}
			sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal(err)
			}
			ctl, _, err := sd.Control()
			if err != nil {
				t.Fatal(err)
			}
			dacl, _, err := sd.DACL()
			if err != nil || dacl == nil {
				t.Fatalf("DACL: %v", err)
			}
			if ctl&windows.SE_DACL_PROTECTED == 0 || dacl.AceCount != 3 {
				t.Errorf("after the hint: protected %v, %d entries; want protected and 3",
					ctl&windows.SE_DACL_PROTECTED != 0, dacl.AceCount)
			}
		})
	}
}

// The detached daemon's flags are the ones syscall passes to CreateProcess.
func TestWindowsDetachFlagsAreProcessCreationFlags(t *testing.T) {
	if detachedProcess != windows.DETACHED_PROCESS || createNewProcessGroup != windows.CREATE_NEW_PROCESS_GROUP ||
		createBreakawayFromJob != windows.CREATE_BREAKAWAY_FROM_JOB {
		t.Fatal("flag values differ from x/sys/windows")
	}
}

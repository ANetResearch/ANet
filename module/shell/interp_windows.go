//go:build shell && windows

package shell

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// powerShellPrefix runs before every PowerShell command line: no progress records on the output, and
// output written as UTF-8 without a byte order mark, whatever the console code page is.
const powerShellPrefix = "$ProgressPreference='SilentlyContinue';[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new($false)"

// powerShellEncodedMax bounds the -EncodedCommand argument, within the 32767 characters of a Windows
// command line.
const powerShellEncodedMax = 30000

// resolveInterpreter maps the configured interpreter to an absolute path and a family. The keywords,
// without regard to case, are "" and "powershell" (Windows PowerShell in the system directory), "pwsh"
// (PowerShell 7 under the Program Files folder, which must be installed) and "cmd"; anything else must be
// an absolute path to powershell.exe, pwsh.exe, cmd.exe, sh.exe or bash.exe. PATH is never searched, so
// the interpreter cannot be replaced by a program planted in a directory on it.
func resolveInterpreter(configured string) (interp, error) {
	switch strings.ToLower(configured) {
	case "", "powershell":
		sys, err := windows.GetSystemWindowsDirectory()
		if err != nil {
			return interp{}, fmt.Errorf("shell: the Windows directory is unknown: %w", err)
		}
		return interp{path: filepath.Join(sys, `System32\WindowsPowerShell\v1.0\powershell.exe`), family: familyPowerShell}, nil
	case "pwsh":
		pf, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
		if err != nil {
			return interp{}, fmt.Errorf("shell: the Program Files folder is unknown: %w", err)
		}
		p := filepath.Join(pf, `PowerShell\7\pwsh.exe`)
		if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
			return interp{}, fmt.Errorf("shell: interpreter %q is not installed (%s does not exist)", configured, p)
		}
		return interp{path: p, family: familyPowerShell}, nil
	case "cmd":
		sys, err := windows.GetSystemWindowsDirectory()
		if err != nil {
			return interp{}, fmt.Errorf("shell: the Windows directory is unknown: %w", err)
		}
		return interp{path: filepath.Join(sys, `System32\cmd.exe`), family: familyCmd}, nil
	}
	if filepath.IsAbs(configured) {
		p := filepath.Clean(configured)
		switch strings.ToLower(filepath.Base(p)) {
		case "powershell.exe", "pwsh.exe":
			return interp{path: p, family: familyPowerShell}, nil
		case "cmd.exe":
			return interp{path: p, family: familyCmd}, nil
		case "sh.exe", "bash.exe":
			return interp{path: p, family: familyPosix}, nil
		}
	}
	return interp{}, fmt.Errorf("shell: interpreter %q is not supported on Windows (use powershell, pwsh, cmd "+
		"or an absolute path to one of powershell.exe, pwsh.exe, cmd.exe, sh.exe, bash.exe)", configured)
}

// command is the process that runs line. PowerShell receives the line, after powerShellPrefix, as
// -EncodedCommand (Base64 of UTF-16LE), so no character of it is subject to command-line quoting;
// cmd.exe receives `/d /s /c "<line>"` as its command line verbatim; a POSIX shell `-c <line>`.
func (i interp) command(ctx context.Context, line string) (*exec.Cmd, error) {
	switch i.family {
	case familyPowerShell:
		enc := encodePowerShell(powerShellPrefix + "\n" + line)
		if len(enc) > powerShellEncodedMax {
			return nil, fmt.Errorf("shell: the command is too long for powershell -EncodedCommand "+
				"(%d characters encoded; at most %d)", len(enc), powerShellEncodedMax)
		}
		return exec.CommandContext(ctx, i.path, "-NoLogo", "-NoProfile", "-NonInteractive",
			"-ExecutionPolicy", "Bypass", "-OutputFormat", "Text", "-EncodedCommand", enc), nil
	case familyCmd:
		cmd := exec.CommandContext(ctx, i.path)
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + i.path + `" /d /s /c "` + line + `"`}
		return cmd, nil
	}
	return exec.CommandContext(ctx, i.path, "-c", line), nil
}

// encodePowerShell is s as PowerShell's -EncodedCommand takes it: Base64 of its UTF-16LE encoding.
func encodePowerShell(s string) string {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for k, c := range u {
		binary.LittleEndian.PutUint16(b[2*k:], c)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// quote renders caller-supplied arguments for the command line. cmd.exe has no quoting that holds for
// every argument, so arguments are refused there. For every other family an argument that contains a
// character cmd.exe interprets is refused first (refuseCmdCharacters): the operator's command may start
// a batch file or a .cmd shim, which Windows runs through cmd.exe whatever the interpreter is. For
// PowerShell each remaining argument is one single-quoted string: every character PowerShell reads as a
// single quote (', U+2018, U+2019, U+201A, U+201B) is doubled, which is how a single-quoted string
// carries it, and control characters other than TAB are refused. A POSIX shell takes quoteArgv.
func (i interp) quote(argv any) (string, error) {
	if i.family == familyCmd {
		return "", fmt.Errorf("arguments cannot be passed safely to cmd.exe; use powershell, or a command without args")
	}
	if err := refuseCmdCharacters(argv); err != nil {
		return "", err
	}
	if i.family == familyPowerShell {
		return quotePowerShell(argv)
	}
	return quoteArgv(argv)
}

// cmdCharacters are the characters cmd.exe interprets in a command line, including in the arguments of
// a batch file it runs: the double quote, which ends quoting; % and !, which expand variables; ^, the
// escape character; & and |, which separate commands; < and >, which redirect; and the parentheses,
// which group commands. CR and LF end the command.
const cmdCharacters = "\"%!^&|<>()\r\n"

// refuseCmdCharacters refuses a caller argument that contains one of cmdCharacters. No quoting by the
// interpreter protects such an argument when the operator's command is a batch file: PowerShell and the
// POSIX shells pass the value to the program, and Windows starts a batch file through cmd.exe, which
// reads the value again with its own rules. A value that is not a list of strings is left to the
// family's own quoting, which reports it.
func refuseCmdCharacters(argv any) error {
	raw, ok := argv.([]any)
	if !ok {
		return nil
	}
	for i, x := range raw {
		s, ok := x.(string)
		if !ok {
			return nil
		}
		k := strings.IndexAny(s, cmdCharacters)
		if k < 0 {
			continue
		}
		if s[k] == '\r' || s[k] == '\n' {
			return fmt.Errorf("argv[%d] contains a control character (a line break), which cannot be passed "+
				"safely on Windows (a batch file or cmd.exe would interpret it)", i)
		}
		return fmt.Errorf("argv[%d] contains the character %c, which cannot be passed safely on Windows "+
			"(a batch file or cmd.exe would interpret it); the characters \" %% ! ^ & | < > ( ) are refused "+
			"in arguments", i, s[k])
	}
	return nil
}

// quotePowerShell is quote for the PowerShell family.
func quotePowerShell(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	raw, ok := v.([]any)
	if !ok {
		return "", fmt.Errorf("argv must be a list of strings")
	}
	var b strings.Builder
	for i, x := range raw {
		s, ok := x.(string)
		if !ok {
			return "", fmt.Errorf("argv[%d] is not a string", i)
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		for _, r := range s {
			switch {
			case r < 0x20 && r != '\t':
				return "", fmt.Errorf("argv[%d] contains a control character", i)
			case r == '\'' || r == '‘' || r == '’' || r == '‚' || r == '‛':
				b.WriteRune(r)
			}
			b.WriteRune(r)
		}
		b.WriteByte('\'')
	}
	return b.String(), nil
}

// commandDir is the working directory of a command: dir as configured; empty is the user's home
// directory (the daemon's own working directory on Windows is wherever it was started from, often the
// system directory).
func commandDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("shell: the user's home directory is unknown: %w", err)
	}
	return home, nil
}

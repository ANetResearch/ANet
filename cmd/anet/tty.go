package main

// tty.go is the terminal confirmation that granting commands require
// (A2A-DESIGN §5.3, §8.6; ux UX-R12, UX-C12): putting a peer on the allow or
// trust list, approving a held delegation, loosening the inbound policy,
// changing a spending limit, putting a payee on the list, paying a quote,
// redeeming credit, allowing a network host.
//
// Every confirmation has one shape: what it does, in lines of the reason
// table's M group (ux §1.4), then confirm.yes. Only "yes" goes on; any other
// answer is confirm.declined and exit status 1, with nothing changed.
//
// The confirmation is read from /dev/tty (on Windows, the console:
// tty_windows.go), not stdin, and a process without a
// controlling terminal is refused before anything else happens, with
// tty_required naming the command to run in a terminal. An agent that
// drives this CLI through a tool call has no terminal and cannot grant on
// its own. The check runs in this process: the daemon only sees the
// control token and cannot tell whether a caller went through a terminal,
// and anything that can read the token can call the routes directly (§21
// item 13). The refusal therefore does not tell an agent how to get around
// it; it says who has to act.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"golang.org/x/term"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// errNoTTY is returned by a granting command run without a terminal, after
// tty_required has been said on stderr: exit status 1, nothing more to print.
var errNoTTY = errors.New("")

// errDeclined is returned when the answer on the terminal was not yes, after
// confirm.declined has been said on stderr: exit status 1 (ux UX-C4).
var errDeclined = errors.New("")

// openTTY opens the controlling terminal (tty_unix.go, tty_windows.go). Tests replace it.

// ttyStderr is where tty_required and confirm.declined are said; tests
// replace it.
var ttyStderr io.Writer = os.Stderr

// confirmOnTTY is ttyConfirm with the explanation known before a terminal
// is open.
func confirmOnTTY(action, explanation string) error {
	return ttyConfirm(action, func() (string, error) { return explanation, nil })
}

// ttyConfirm asks on the terminal. action is the tty_required phrase of
// what is confirmed (ux §1.4 E2: trust, inbound_policy, approve, pay, …).
// The explanation is built only once a terminal is open: without one,
// summary is not called, so a refused command makes no request of the
// daemon; an error from summary (the thing to approve does not exist) ends
// the command without asking.
func ttyConfirm(action string, summary func() (string, error)) error {
	lang := a2ashape.Lang()
	tty, err := openTTY()
	if err != nil {
		return refuseNoTTY(action, lang)
	}
	defer tty.Close()
	explanation, err := summary()
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tty, "%s\n%s ", explanation, a2ashape.Sentence("confirm.yes", lang, nil)); err != nil {
		return refuseNoTTY(action, lang)
	}
	// The question is on the terminal now; an answer that ends without yes —
	// Ctrl+D, or the read failing — is not a missing terminal but a "no":
	// confirm.declined, not tty_required telling the person at that
	// terminal to go and find one.
	line, _ := bufio.NewReader(tty).ReadString('\n')
	if strings.TrimSpace(strings.ToLower(line)) != "yes" {
		fmt.Fprintln(ttyStderr, a2ashape.Sentence("confirm.declined", lang, nil))
		return errDeclined
	}
	return nil
}

// refuseNoTTY says tty_required for action, with the command as typed, and
// returns errNoTTY.
func refuseNoTTY(action string, lang a2ashape.Language) error {
	args := a2ashape.Args{"action": action, "command": typedCommand(commandLine())}
	sayRefusal(ttyStderr, "tty_required", lang, args)
	return errNoTTY
}

// printable makes text that came from elsewhere — a peer's self-declared
// name, a hub's directory entry — safe to put in a terminal prompt: control
// and bidirectional-override characters are dropped, so the text cannot
// move the cursor or rewrite what the prompt says, and it is cut to max
// runes.
func printable(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == '\u2028' || r == '\u2029' {
			continue
		}
		if n == max {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// consoleInput is a terminal whose input is a separate handle (a Windows console: CONIN$ for input,
// CONOUT$ for output), so echo can be turned off on the right one.
type consoleInput interface{ inputFd() uintptr }

// ttySession is the terminal a command with prompts talks to: one open /dev/tty, read through one
// buffer, so that several questions in a row do not lose the typed-ahead input between them.
type ttySession struct {
	rw io.ReadWriteCloser
	br *bufio.Reader
}

// openTTYSession opens the controlling terminal; err is non-nil when there is none.
func openTTYSession() (*ttySession, error) {
	t, err := openTTY()
	if err != nil {
		return nil, err
	}
	return &ttySession{rw: t, br: bufio.NewReader(t)}, nil
}

func (t *ttySession) Close() error { return t.rw.Close() }

// say writes a line on the terminal.
func (t *ttySession) say(s string) { fmt.Fprintln(t.rw, s) }

// line asks a question and reads the answer, echoed.
func (t *ttySession) line(prompt string) (string, error) {
	if _, err := fmt.Fprint(t.rw, prompt+" "); err != nil {
		return "", err
	}
	s, err := t.br.ReadString('\n')
	if err != nil && s == "" {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// readPassphrase asks for a passphrase on the terminal with echo off (KEL L-1). The bytes come back
// without the line end; the caller clears them when done. A passphrase is read from the terminal and
// from nowhere else: no argument, no environment variable (KEL L-1).
func readPassphrase(t *ttySession, prompt string) ([]byte, error) {
	if _, err := fmt.Fprint(t.rw, prompt+" "); err != nil {
		return nil, err
	}
	// A Windows console (tty_windows.go) reads the passphrase from its input handle.
	if c, ok := t.rw.(consoleInput); ok {
		p, err := term.ReadPassword(int(c.inputFd()))
		fmt.Fprintln(t.rw)
		return p, err
	}
	if f, ok := t.rw.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		p, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(t.rw)
		return p, err
	}
	// Not a terminal device (a test's pipe): read the line as it is.
	b, err := t.br.ReadBytes('\n')
	if err != nil && len(b) == 0 {
		return nil, err
	}
	return bytes.TrimRight(b, "\r\n"), nil
}

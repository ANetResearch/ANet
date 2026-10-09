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
// The confirmation is read from /dev/tty, not stdin, and a process without a
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
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// errNoTTY is returned by a granting command run without a terminal, after
// tty_required has been said on stderr: exit status 1, nothing more to print.
var errNoTTY = errors.New("")

// errDeclined is returned when the answer on the terminal was not yes, after
// confirm.declined has been said on stderr: exit status 1 (ux UX-C4).
var errDeclined = errors.New("")

// openTTY opens the controlling terminal. Tests replace it.
var openTTY = func() (io.ReadWriteCloser, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }

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
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && line == "" {
		return refuseNoTTY(action, lang)
	}
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
	say(ttyStderr, a2ashape.Sentence("tty_required", lang, args), a2ashape.Next("tty_required", lang, args))
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

package main

// tty.go is the terminal confirmation that granting commands require
// (A2A-DESIGN §5.3, §8.6): putting a peer on the allow or trust list,
// approving a held delegation, loosening the inbound policy, changing a
// spending limit — and `anet pay`, which is to call ttyConfirm the same way.
//
// The confirmation is read from /dev/tty, not stdin, and a process without a
// controlling terminal is refused before anything else happens. An agent
// that drives this CLI through a tool call has no terminal and cannot grant
// on its own. The check runs in this process: the daemon only sees the
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
)

// errNoTTY is returned by a granting command run without a terminal.
var errNoTTY = errors.New("this command needs a confirmation typed on a terminal (/dev/tty), " +
	"and this process has none. Ask the person operating this node to run it in their terminal")

// openTTY opens the controlling terminal. Tests replace it.
var openTTY = func() (io.ReadWriteCloser, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }

// confirmOnTTY shows prompt on the terminal and reads one line from it. It
// returns nil only when the answer is "yes".
func confirmOnTTY(prompt string) error {
	return ttyConfirm(func() (string, error) { return prompt, nil })
}

// ttyConfirm is confirmOnTTY with the prompt built only once a terminal is
// open. Without a terminal, summary is not called, so a refused command
// makes no request of the daemon; an error from summary (the thing to
// approve does not exist) ends the command without asking.
func ttyConfirm(summary func() (string, error)) error {
	tty, err := openTTY()
	if err != nil {
		return errNoTTY
	}
	defer tty.Close()
	prompt, err := summary()
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tty, "%s\nType yes to confirm: ", prompt); err != nil {
		return errNoTTY
	}
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && line == "" {
		return errNoTTY
	}
	if strings.TrimSpace(strings.ToLower(line)) != "yes" {
		return fmt.Errorf("not confirmed; nothing changed")
	}
	return nil
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
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == ' ' || r == ' ' {
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

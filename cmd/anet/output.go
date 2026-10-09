package main

// output.go is how a command that calls the control plane says what came
// back (ux §4.1):
//
//   - UX-C1: the answer's JSON goes to stdout and nothing else does, so a
//     pipe carries only JSON; every line written for a person goes to
//     stderr.
//   - UX-C2: after a task, at most five human lines in a fixed order, each
//     "label: sentence", only those with a value — the reason of a failed or
//     rejected task, its delivery, its payment, how long it is kept, and the
//     answer's note — then, when there is one, the next step on a "  → "
//     line.
//   - UX-C3: a refusal (any answer other than 2xx) prints only the
//     daemon's sentence and its next step on stderr, no JSON, exit status 1.
//
// The CLI words none of these sentences (UX-C5, UX-C7): each comes from the
// control plane's answer — the task projection, the error body — or, for
// the label and the command of the next step, from the reason table.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/localpeer"
)

// cliStdout and cliStderr are where answers and human lines go; tests
// replace them. nil means os.Stdout and os.Stderr as they are when written.
var cliStdout, cliStderr io.Writer

func outW() io.Writer {
	if cliStdout != nil {
		return cliStdout
	}
	return os.Stdout
}

func errW() io.Writer {
	if cliStderr != nil {
		return cliStderr
	}
	return os.Stderr
}

// do calls a control route and prints its answer: the JSON on stdout and
// the answer's note on stderr (UX-C1, UX-C2 line 5), or a refusal (UX-C3).
func (c *client) do(path string, body any) error {
	out, code, err := c.fetch(path, body)
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	return printAnswer(out, code, false)
}

// doTask is do for a route that answers with one task: the human lines
// that follow the JSON are the task's (UX-C2).
func (c *client) doTask(path string, body any) error {
	out, code, err := c.fetch(path, body)
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	return printAnswer(out, code, true)
}

// printAnswer prints one control-plane answer (see the file comment).
func printAnswer(out []byte, code int, task bool) error {
	if code < 200 || code >= 300 {
		return answerError(code, out)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, out, "", "  ") == nil {
		fmt.Fprintln(outW(), strings.TrimRight(pretty.String(), "\n"))
	} else {
		fmt.Fprintln(outW(), strings.TrimRight(string(out), "\n"))
	}
	for _, l := range answerLines(out, task, a2ashape.Lang(), time.Now()) {
		fmt.Fprintln(errW(), l)
	}
	return nil
}

// answerError prints a refusal (ux UX-C3): the error body's sentence and,
// when it has one, its command on a "  → " line, on stderr, and returns
// errExit1. An answer that is not an error body (a route this daemon does
// not have, a proxy in the way) is shown by its status and first line.
func answerError(code int, out []byte) error {
	var e a2ashape.ErrorBody
	if json.Unmarshal(out, &e) == nil && strings.TrimSpace(e.Error) != "" {
		say(errW(), oneLine(e.Error, maxLineRunes), oneLine(e.Next, maxLineRunes))
		return errExit1
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	fmt.Fprintf(errW(), "error: the daemon answered %d %s: %s\n", code, http.StatusText(code), oneLine(first, 200))
	return errExit1
}

// maxLineRunes bounds one human line: a sentence of the reason table is far
// shorter, and a provider's own words on a failed task are shown in full in
// the JSON.
const maxLineRunes = 1000

// answerLines are the human lines of an answer, in the UX-C2 order. task
// says the answer is one task.
func answerLines(out []byte, task bool, lang a2ashape.Language, now time.Time) []string {
	var top map[string]any
	if json.Unmarshal(out, &top) != nil {
		return nil
	}
	var lines []string
	add := func(label, text string) {
		if text = oneLine(text, maxLineRunes); text != "" {
			lines = append(lines, a2ashape.Label(label, lang)+": "+text)
		}
	}
	next := ""
	if task {
		var t a2ashape.Task
		if json.Unmarshal(out, &t) != nil {
			return nil
		}
		reasonLine := ""
		if t.Status.State == a2ashape.TaskStateFailed || t.Status.State == a2ashape.TaskStateRejected {
			reasonLine = messageText(t.Status.Message)
		}
		// v030-todo(RC-5): line 1 is also the sentence of one message the other side refused while
		// the task goes on (ux §1.1 class M: that message's anet.reason, its placeholder text), once
		// the Message projection carries it.
		add("task.reason", reasonLine)
		if s, ok := deliveryLine(t, lang, now); ok {
			add("task.delivery", s)
		}
		note, _ := t.Metadata[keyPaymentNote].(string)
		add("task.payment", note)
		if s, ok := keptLine(t, lang, now); ok {
			add("task.kept", s)
		}
		if reasonLine != "" || note != "" {
			next = reasonNext(t, lang)
		}
	}
	warning, _ := top["warning"].(string)
	message, _ := top["message"].(string)
	if warning != "" {
		add("task.note", warning)
	} else {
		add("task.note", message)
	}
	if n, _ := top["next"].(string); strings.TrimSpace(n) != "" {
		next = n
	}
	if next = oneLine(next, maxLineRunes); next != "" && len(lines) > 0 {
		lines = append(lines, nextPrefix+next)
	}
	return lines
}

// keyPaymentNote is the task's payment sentence (core §2.13, HPP): the
// payment line of UX-C2.
const keyPaymentNote = "anet.payment_note"

// deliveryLine is line 2 of UX-C2, the delivery of the latest message this
// node sent on the task, when it is pending or unconfirmed.
//
// v030-todo(RC-5): a2ashape.DeliveryLine(task, lang, nowMS) (DLV-L-2), once
// the projection carries anet.delivery.
var deliveryLine = func(t a2ashape.Task, lang a2ashape.Language, now time.Time) (string, bool) { return "", false }

// keptLine is line 4 of UX-C2, task.kept_until of an ended task.
//
// v030-todo(FS-11): the projection's anet.content_expires_at and the
// retention, task.kept_until.
var keptLine = func(t a2ashape.Task, lang a2ashape.Language, now time.Time) (string, bool) { return "", false }

// reasonNext is the next step of a task's reason (a quote's payment is one:
// needs_operator_approval says to run anet pay): the reason's command, with
// the task and its peer filled in. A code whose sentences differ in their
// command by variant (needs_operator_approval with manual_limit: first
// anet payments set) has none here, since the task does not say which
// variant its sentence is; nor has a command that needs a value the task
// does not give.
//
// v030-todo(PAY-12): the variant of the payment sentence (the pay_note
// snapshot) for the payment line's command.
func reasonNext(t a2ashape.Task, lang a2ashape.Language) string {
	code, _ := t.Metadata[a2ashape.KeyReason].(string)
	if code == "" {
		return ""
	}
	args := a2ashape.Args{"task": t.ID}
	if p, _ := t.Metadata[a2ashape.KeyPeerAID].(string); p != "" {
		args["peer"] = p
	}
	key, template := "", ""
	for _, r := range a2ashape.Rows() {
		if r.Code != code {
			continue
		}
		switch {
		case key == "":
			key, template = r.Key(), r.Next
		case r.Next != template:
			return ""
		}
	}
	if key == "" || template == "" {
		return ""
	}
	return a2ashape.Next(key, lang, args)
}

// messageText is the text of a message's text parts, one after another.
func messageText(m *a2ashape.Message) string {
	if m == nil {
		return ""
	}
	var parts []string
	for _, p := range m.Parts {
		if p.Kind == a2ashape.PartText && strings.TrimSpace(p.Text) != "" {
			parts = append(parts, strings.TrimSpace(p.Text))
		}
	}
	return strings.Join(parts, " ")
}

// oneLine makes text safe to print as one line on a terminal: a line
// break or a tab becomes a space, other control and bidirectional-override
// characters are dropped, and the result is cut to max runes. The text may
// be a peer's words, which must not move the cursor or rewrite what the
// line says; a sentence of the reason table passes unchanged.
func oneLine(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || r == '\u2028' || r == '\u2029':
			r = ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r):
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

// diagnoseNoDaemon is what a command says when the control plane cannot be
// reached: daemon_not_running with the data directory and its command
// (ux §1.4 G), and, when another identity's daemon is running on this
// machine, the same command for it. A listener that is not this user's
// daemon is said in its own words: the token was not sent there.
func diagnoseNoDaemon(base, dataDir string, cause error) error {
	if errors.Is(cause, localpeer.ErrNotOurs) {
		return fmt.Errorf("the control address %s is held by a process that is not your anet daemon, so the token "+
			"was not sent (%v); stop that process, or start your daemon with anet up, which moves to a free port",
			base, cause)
	}
	lang := a2ashape.Lang()
	args := a2ashape.Args{"path": dataDir}
	say(errW(), a2ashape.Sentence("daemon_not_running", lang, args), a2ashape.Next("daemon_not_running", lang, args))
	typed := commandLine()
	for _, e := range runningElsewhere(base) {
		fmt.Fprintln(errW(), nextPrefix+elsewhereCommand(e, typed))
	}
	return errExit1
}

// elsewhereCommand is the command as typed, for the identity name: an --id
// the user gave is taken out first, since the last --id on a command line
// is the one that counts and would point the command back here.
func elsewhereCommand(name string, typed []string) string {
	_, rest := extractGlobalID(typed)
	return typedCommand(append([]string{"--id", name}, rest...))
}

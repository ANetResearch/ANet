package main

// task_watch.go is `anet task watch <id> [--reasoning]` and `anet delegate
// … --watch` (correctness-ux §4.5, ux UX-C4, UX-C8): the control plane's
// /tasks/events read as lines for a person.
//
// On stderr: a line "[tag] sentence" at each change of the task's state, the
// delivery sentence of each delivery event, and the preview, which a
// terminal shows in place and a pipe shows when it changes. On stdout, when
// the task has ended or waits for the user, its JSON. The exit status is 0
// for completed, 1 for failed, rejected and canceled, and 3 for
// input-required (the task waits for a payment or an answer).

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/localpeer"
)

// errInputRequired ends `task watch` at an input-required task, after the
// task was printed: exit status 3 (UX-C4).
var errInputRequired = errors.New("")

// previewLines is how many lines of the end of a preview are shown.
const previewLines = 20

// maxSSELine bounds one line of the event stream: a task event is held to
// 24 KiB by the daemon, so this is well above any line it writes.
const maxSSELine = 1 << 20

// extractBool takes the flag --name out of args (--name, --name=true or
// --name=false), whatever its place: a boolean flag would otherwise take
// the word after it as its value.
func extractBool(args []string, name string) (on bool, rest []string) {
	rest = make([]string, 0, len(args))
	for _, a := range args {
		switch a {
		case "--" + name, "--" + name + "=true":
			on = true
		case "--" + name + "=false":
			on = false
		default:
			rest = append(rest, a)
		}
	}
	return on, rest
}

// taskWatchCommand is `anet task watch <id> [--reasoning]`.
func taskWatchCommand(c *client, rest []string) error {
	const use = "anet task watch <id> [--reasoning]"
	reasoning, rest := extractBool(rest, "reasoning")
	pos, flags := splitFlags(rest)
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" || len(flags) > 0 {
		return usageError{use}
	}
	return watchTask(c, pos[0], reasoning)
}

// watchDelegate is `anet delegate … --watch`: the send asks for previews and
// returns at once, and the task is then watched. Only the watch prints.
func watchDelegate(c *client, body map[string]any) error {
	if err := requireModule(daemon.Layout{Root: c.dataDir}, "preview"); err != nil {
		// Without the preview module the watch waits for the final answer
		// alone; one line says so (11-modularity §5.4).
		fmt.Fprintln(os.Stderr, err.Error())
	} else {
		body["preview"] = "on"
	}
	out, code, err := c.fetch("/tasks/send", body)
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if code < 200 || code >= 300 {
		return answerError(code, out)
	}
	var t a2ashape.Task
	if json.Unmarshal(out, &t) != nil || t.ID == "" {
		return printAnswer(out, code, true)
	}
	return watchTask(c, t.ID, false)
}

func watchTask(c *client, id string, reasoning bool) error {
	var buf strings.Builder
	if err := json.NewEncoder(&buf).Encode(map[string]string{"task_id": id}); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+"/tasks/events", strings.NewReader(buf.String()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "text/event-stream")
	// No overall timeout: the stream lasts as long as the task.
	resp, err := localpeer.Client(c.token, 0).Do(req)
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return answerError(resp.StatusCode, b)
	}
	w := &watcher{out: errW(), reasoning: reasoning, lang: a2ashape.Lang()}
	if f, ok := w.out.(*os.File); ok {
		w.tty = term.IsTerminal(int(f.Fd()))
		if w.tty {
			// A Windows console takes the escape sequences preview writes only in VT mode (console_*.go).
			restore, vt := enableVT(f)
			defer restore()
			w.tty = vt
		}
	}
	r := bufio.NewReaderSize(resp.Body, 64<<10)
	var name string
	var data []byte
	for {
		line, err := readSSELine(r)
		if err != nil {
			return fmt.Errorf("watch: the event stream ended before the task did; read it with anet task get %s", id)
		}
		switch {
		case line == "":
			if name != "" && len(data) > 0 {
				if done, err := w.handle(name, data); done {
					return err
				}
			}
			name, data = "", nil
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")...)
		}
	}
}

// readSSELine reads one line, without its line ending, refusing one longer
// than maxSSELine.
func readSSELine(r *bufio.Reader) (string, error) {
	var b []byte
	for {
		part, isPrefix, err := r.ReadLine()
		if err != nil {
			return "", err
		}
		b = append(b, part...)
		if len(b) > maxSSELine {
			return "", errors.New("event line too long")
		}
		if !isPrefix {
			return string(b), nil
		}
	}
}

// watcher turns the events into lines.
type watcher struct {
	out       io.Writer
	tty       bool
	reasoning bool
	lang      a2ashape.Language

	state    a2ashape.TaskState
	sentence string
	shown    int    // lines of a preview on the terminal to take back
	lastText string // the preview last printed (not a terminal)
	lastReas string
}

// handle reads one event. done is true when the command ends, with its
// result.
func (w *watcher) handle(name string, data []byte) (done bool, err error) {
	switch name {
	case "task":
		var t a2ashape.Task
		if json.Unmarshal(data, &t) != nil {
			return false, nil
		}
		return w.task(t, data)
	case "preview":
		var p struct {
			At               uint64 `json:"at"`
			Text             string `json:"text"`
			Omitted          uint64 `json:"omitted"`
			Reasoning        string `json:"reasoning"`
			ReasoningOmitted uint64 `json:"reasoning_omitted"`
		}
		if json.Unmarshal(data, &p) != nil {
			return false, nil
		}
		w.preview(p.At, p.Text, p.Omitted, p.Reasoning, p.ReasoningOmitted)
	case "delivery":
		var e struct {
			Delivery string `json:"delivery"`
			Text     string `json:"text"`
		}
		if json.Unmarshal(data, &e) != nil || strings.TrimSpace(e.Text) == "" {
			return false, nil
		}
		w.settle()
		if e.Delivery == a2ashape.DeliveryConfirmed {
			fmt.Fprintf(w.out, "[%s] %s\n", a2ashape.Label("watch.delivered", w.lang), oneLine(e.Text, maxLineRunes))
		} else {
			fmt.Fprintf(w.out, "%s: %s\n", a2ashape.Label("task.delivery", w.lang), oneLine(e.Text, maxLineRunes))
		}
	}
	return false, nil
}

// settle leaves the preview on the terminal as it is, so that the next line
// is written under it.
func (w *watcher) settle() { w.shown = 0 }

var watchTags = map[a2ashape.TaskState]string{
	a2ashape.TaskStateSubmitted:     "watch.submitted",
	a2ashape.TaskStateWorking:       "watch.working",
	a2ashape.TaskStateInputRequired: "watch.input_required",
	a2ashape.TaskStateCompleted:     "watch.completed",
	a2ashape.TaskStateFailed:        "watch.failed",
	a2ashape.TaskStateRejected:      "watch.rejected",
	a2ashape.TaskStateCanceled:      "watch.canceled",
}

// task prints a task event's state line when the state or its sentence
// changed, and ends the command at a state that ends or waits.
func (w *watcher) task(t a2ashape.Task, raw []byte) (bool, error) {
	state := t.Status.State
	sentence := ""
	if state != a2ashape.TaskStateCompleted {
		sentence = oneLine(messageText(t.Status.Message), maxLineRunes)
	}
	if state != w.state || sentence != w.sentence {
		w.settle()
		tag := a2ashape.Label(watchTags[state], w.lang)
		if tag == "" {
			tag = string(state)
		}
		if sentence != "" {
			fmt.Fprintf(w.out, "[%s] %s\n", tag, sentence)
		} else {
			fmt.Fprintf(w.out, "[%s]\n", tag)
		}
		w.state, w.sentence = state, sentence
	}
	switch state {
	case a2ashape.TaskStateCompleted, a2ashape.TaskStateFailed, a2ashape.TaskStateRejected,
		a2ashape.TaskStateCanceled, a2ashape.TaskStateInputRequired:
	default:
		return false, nil
	}
	if state == a2ashape.TaskStateCompleted {
		w.reply(t)
	}
	if err := printAnswer(raw, http.StatusOK, true); err != nil {
		return true, err
	}
	switch state {
	case a2ashape.TaskStateCompleted:
		return true, nil
	case a2ashape.TaskStateInputRequired:
		return true, errInputRequired
	}
	return true, errExit1
}

// reply prints the text of the task's anet.reply, line by line.
func (w *watcher) reply(t a2ashape.Task) {
	for _, a := range t.Artifacts {
		if a.ID != a2ashape.ArtifactReply {
			continue
		}
		for _, p := range a.Parts {
			if p.Kind != a2ashape.PartText {
				continue
			}
			for _, l := range strings.Split(p.Text, "\n") {
				fmt.Fprintln(w.out, oneLine(l, maxLineRunes))
			}
		}
		return
	}
}

// preview shows the end of the text so far, at most previewLines lines,
// and with --reasoning the end of the reasoning as well. On a terminal it
// takes back the lines of the last preview first; elsewhere it prints only
// what changed.
func (w *watcher) preview(at uint64, text string, omitted uint64, reasoning string, reasoningOmitted uint64) {
	if !w.reasoning {
		reasoning = ""
	}
	if !w.tty && text == w.lastText && reasoning == w.lastReas {
		return
	}
	w.lastText, w.lastReas = text, reasoning
	var lines []string
	block := func(key, body string, omitted uint64) {
		if body == "" {
			return
		}
		tm := time.UnixMilli(int64(at)).Format("15:04:05")
		head := strings.NewReplacer("{time}", tm, "{n}", fmt.Sprint(omitted)).Replace(a2ashape.Label(key, w.lang))
		lines = append(lines, head+":")
		for _, l := range tailLines(body, previewLines) {
			lines = append(lines, "  "+oneLine(l, maxLineRunes))
		}
	}
	block("watch.reasoning", reasoning, reasoningOmitted)
	block("watch.preview", text, omitted)
	if len(lines) == 0 {
		return
	}
	if w.tty && w.shown > 0 {
		fmt.Fprintf(w.out, "\x1b[%dA\x1b[J", w.shown)
	}
	for _, l := range lines {
		fmt.Fprintln(w.out, l)
	}
	w.shown = len(lines)
}

// tailLines is the last n lines of s.
func tailLines(s string, n int) []string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return ls
}

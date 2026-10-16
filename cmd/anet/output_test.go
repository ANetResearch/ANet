package main

// UX-C1–C3: what a command that calls the control plane prints, where.

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/ANetResearch/ANet/internal/a2ashape/stored"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// captureCLI runs fn with the CLI's stdout and stderr (cliStdout, cliStderr) captured.
func captureCLI(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	prevOut, prevErr := cliStdout, cliStderr
	cliStdout, cliStderr = &out, &errb
	defer func() { cliStdout, cliStderr = prevOut, prevErr }()
	fn()
	return out.String(), errb.String()
}

// isUsage reports a command used wrongly: exit status 2 (ux UX-C4).
func isUsage(err error) bool {
	var ue usageError
	return errors.As(err, &ue)
}

// oneJSONValue reports whether s is exactly one JSON value and white space.
func oneJSONValue(s string) bool {
	dec := json.NewDecoder(strings.NewReader(s))
	var v any
	if dec.Decode(&v) != nil {
		return false
	}
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}

// TestCLIJSONOnStdout (UX-C1, UX-C2, UX-C3): a task command run with its output in a pipe writes the
// task's JSON to stdout and nothing else; the human lines — the reason of a failed task, the payment
// sentence of a quote, the answer's note, and the next step after them — go to stderr, in their order;
// a refusal writes only the daemon's sentence and its next step, to stderr, and nothing to stdout.
// [mut] a human line on stdout → red.
func TestCLIJSONOnStdout(t *testing.T) {
	en := a2ashape.English
	failed := `{"id":"ix_f","contextId":"c","status":{"state":"TASK_STATE_FAILED","message":{"messageId":"m","role":"ROLE_AGENT",` +
		`"parts":[{"text":"The other side's node never confirmed it."}]}},"metadata":{"anet.role":"requester","anet.reason":"busy"}}`
	quoted := `{"id":"ix_q","contextId":"c","status":{"state":"TASK_STATE_INPUT_REQUIRED"},"metadata":{"anet.role":"requester",` +
		`"anet.reason":"needs_operator_approval","anet.payment_note":"Quoted 3 credits; automatic payment is off, so nothing was paid."}}`
	// A reason whose command is the same in every sentence of its code (no_local_task: anet audit
	// --interaction {task}) gets its "  → " line.
	rejected := `{"id":"ix_r","contextId":"c","status":{"state":"TASK_STATE_REJECTED","message":{"messageId":"m","role":"ROLE_AGENT",` +
		`"parts":[{"text":"You are not on their list."}]}},"metadata":{"anet.role":"requester","anet.reason":"no_local_task",` +
		`"anet.next":"anet audit --interaction ix_r"}}`
	submitted := `{"id":"ix_s","contextId":"c","status":{"state":"TASK_STATE_SUBMITTED"},"metadata":{"anet.role":"requester"}}`
	refusal, _ := json.Marshal(a2ashape.Refuse("hub_pending_admission", a2ashape.Args{"hub": "https://hub.example", "aid": wholeAID}).Body(en))
	var ref a2ashape.ErrorBody
	_ = json.Unmarshal(refusal, &ref)
	for _, tc := range []struct {
		name         string
		args         []string
		code         int
		answer       string
		exit         int
		stdoutIsJSON bool
		stderr       string
	}{
		{"a failed task", []string{"task", "get", "ix_f"}, 200, failed, 0, true,
			"reason: The other side's node never confirmed it.\n"},
		{"a quote waiting for the operator", []string{"task", "get", "ix_q"}, 200, quoted, 0, true,
			"payment: Quoted 3 credits; automatic payment is off, so nothing was paid.\n"},
		{"a refusal with a command", []string{"task", "get", "ix_r"}, 200, rejected, 0, true,
			"reason: You are not on their list.\n" + nextPrefix + "anet audit --interaction ix_r\n"},
		{"a task sent", []string{"delegate", wholeAID, "translate", "this"}, 200, submitted, 0, true, ""},
		{"a refusal", []string{"delegate", wholeAID, "translate", "this"}, 503, string(refusal), 1, false,
			ref.Error + "\n" + nextPrefix + ref.Next + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCLIHome(t, "C")
			newScriptedControl(t, h, func(string, map[string]any) (int, string) { return tc.code, tc.answer })
			stdout, stderr, code := runCLIWithin(t, h, 30*time.Second, tc.args)
			if code != tc.exit {
				t.Fatalf("exit %d, want %d (stderr %q)", code, tc.exit, stderr)
			}
			if tc.stdoutIsJSON {
				var got, want any
				if !oneJSONValue(stdout) || json.Unmarshal([]byte(stdout), &got) != nil ||
					json.Unmarshal([]byte(tc.answer), &want) != nil || !jsonEqual(got, want) {
					t.Errorf("stdout is not the answer and only the answer:\n%s", stdout)
				}
			} else if stdout != "" {
				t.Errorf("stdout %q, want nothing", stdout)
			}
			if stderr != tc.stderr {
				t.Errorf("stderr %q, want %q", stderr, tc.stderr)
			}
		})
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// UX-C2: the five human lines in their order, each "label: sentence" in the language of the
// process, only those with a value, and the next step last — read from a real projection: the
// reason's sentence, the payment note and anet.next as the daemon gives them (core §2.13 revision
// 7: the CLI works no command out itself). The delivery and kept lines come from the work packages
// that give the projection their keys (deliveryLine, keptLine); here they are stood in for.
func TestTaskLinesOrder(t *testing.T) {
	prevD, prevK := deliveryLine, keptLine
	t.Cleanup(func() { deliveryLine, keptLine = prevD, prevK })
	deliveryLine = func(a2ashape.Task, a2ashape.Language, time.Time) (string, bool) { return "D sentence.", true }
	keptLine = func(a2ashape.Task, a2ashape.Language, time.Time) (string, bool) { return "K sentence.", true }
	note, _ := json.Marshal(a2ashape.PayNote{Code: "paid.auto", Amount: 3, Tier: "auto", Single: 5, Left: 44,
		At: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)})
	ix := &interactions.Interaction{ID: "ix_1", Role: interactions.RoleOutbound, PeerAID: wholeAID,
		State: interactions.StateRejected, StateSeq: 3, ContextID: "c", PayState: interactions.PayCompleted,
		ResultMeta: `{"anet.reason":"no_local_task"}`, PayNote: note}
	task := stored.Project(stored.Source{Interaction: ix, Payment: map[string]any{}}, stored.Options{})
	b, _ := json.Marshal(task)
	var top map[string]any
	_ = json.Unmarshal(b, &top)
	top["warning"] = "N sentence."
	answer, _ := json.Marshal(top)
	reason, payment := messageText(task.Status.Message), task.Metadata[a2ashape.KeyPaymentNote].(string)
	next, _ := task.Metadata[a2ashape.KeyNext].(string)
	if reason == "" || payment != a2ashape.PaymentNote(a2ashape.PayNote{Code: "paid.auto", Amount: 3, Tier: "auto",
		Single: 5, Left: 44}, a2ashape.Lang()) || next != "anet audit --interaction ix_1" {
		t.Fatalf("the projection: reason %q, payment %q, next %q", reason, payment, next)
	}
	for _, lang := range []a2ashape.Language{a2ashape.English, a2ashape.Chinese} {
		got := answerLines(answer, true, lang, time.Now())
		label := func(k string) string { return a2ashape.Label(k, lang) + ": " }
		want := []string{label("task.reason") + reason, label("task.delivery") + "D sentence.",
			label("task.payment") + payment, label("task.kept") + "K sentence.", label("task.note") + "N sentence.",
			nextPrefix + next}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%v:\n%s\nwant\n%s", lang, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	deliveryLine, keptLine = prevD, prevK
	// A task waiting on manual_limit: the command is the variant's (anet payments set first), not
	// the anet pay the code's other variants give.
	twelve := uint64(12)
	note, _ = json.Marshal(a2ashape.PayNote{Code: "needs_operator_approval.manual_limit", Amount: 12, Tier: "auto",
		Single: 10, Left: 50, Key: "explicit_max", Value: &twelve, At: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)})
	quote := []byte(`{"x402Version":2,"accepts":[{"scheme":"credit","network":"n","amount":"12","asset":"credit","payTo":"` + wholeAID + `"}]}`)
	waiting := &interactions.Interaction{ID: "ix_2", Role: interactions.RoleOutbound, PeerAID: wholeAID,
		State: interactions.StateInputRequired, StateSeq: 2, ContextID: "c", PayState: interactions.PayRequired,
		PayRequired: quote, IsCapability: true, PayNote: note}
	wt := stored.Project(stored.Source{Interaction: waiting, Payment: map[string]any{"anet.reason": "needs_operator_approval"}},
		stored.Options{})
	b, _ = json.Marshal(wt)
	if ml := answerLines(b, true, a2ashape.English, time.Now()); len(ml) != 2 || ml[1] != nextPrefix+"anet payments set explicit_max=12" {
		t.Errorf("manual_limit: %q", ml)
	}
	// Only the lines with a value: a completed task has none; a working one with a message only that.
	if got := answerLines([]byte(`{"id":"ix_1","status":{"state":"TASK_STATE_COMPLETED"}}`), true, a2ashape.English, time.Now()); len(got) != 0 {
		t.Errorf("a completed task: %q", got)
	}
	// A task's own words cannot move the cursor: control characters out, one line.
	got := answerLines([]byte(`{"id":"ix_1","status":{"state":"TASK_STATE_FAILED","message":{"messageId":"m","role":"ROLE_AGENT",`+
		`"parts":[{"text":"evil\u001b[2J\nline\u202etwo"}]}}}`), true, a2ashape.English, time.Now())
	if len(got) != 1 || got[0] != "reason: evil[2J linetwo" {
		t.Errorf("a peer's words: %q", got)
	}
}

// UX-T1, the CLI's place (ux UX-G3, UX-C3, UX-C7): for every refusal the control plane can give — each
// row of the reason table it answers with — the CLI prints the control plane's sentence and command,
// exactly, in either language, and nothing on stdout. The CLI renders no sentence again. [mut] the CLI
// changes one character of a sentence → red (ux mutation 1).
func TestCLISaysTheControlPlanesSentence(t *testing.T) {
	n := 0
	for _, r := range a2ashape.Rows() {
		if r.Class&(a2ashape.ClassC|a2ashape.ClassE) == 0 || r.NoSentence {
			continue
		}
		for _, lang := range []a2ashape.Language{a2ashape.English, a2ashape.Chinese} {
			args := a2ashape.Args{"task": "ix_1", "hub": "https://hub.example", "aid": wholeAID, "peer": wholeAID,
				"path": "/srv/u/.anet", "value": "x", "key": "k", "rule": "r", "what": "anet x", "instead": "accept"}
			body := a2ashape.Refuse(r.Key(), args).Body(lang)
			b, _ := json.Marshal(body)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write(b)
			}))
			c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second}
			var err error
			stdout, stderr := captureCLI(t, func() { err = c.doTask("/tasks/send", map[string]any{}) })
			srv.Close()
			want := body.Error + "\n"
			if body.Next != "" {
				want += nextPrefix + body.Next + "\n"
			}
			if err != errExit1 || stdout != "" || stderr != want {
				t.Errorf("%s (%v): exit %v, stdout %q, stderr %q, want %q", r.Key(), lang, err, stdout, stderr, want)
			}
			n++
		}
	}
	if n < 100 {
		t.Fatalf("only %d refusals checked", n)
	}
}

// daemon_not_running's command for another identity's running daemon is the command as typed with
// that identity's --id, and none other: an --id the user gave, in either spelling, is taken out, or
// the command would point back at the daemon that is not running.
func TestElsewhereCommandNamesOneIdentity(t *testing.T) {
	for _, tc := range []struct {
		typed []string
		want  string
	}{
		{[]string{"status"}, "anet --id coder status"},
		{[]string{"--id", "gone", "task", "get", "ix_1"}, "anet --id coder task get ix_1"},
		{[]string{"delegate", wholeAID, "hello world", "--id=gone"}, "anet --id coder delegate " + wholeAID + " 'hello world'"},
	} {
		if got := elsewhereCommand("coder", tc.typed); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.typed, got, tc.want)
		}
	}
}

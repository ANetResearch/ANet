package main

import (
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/module"
)

// UX-10: `anet task watch` and `anet delegate --watch` read /tasks/events;
// the exit status follows the task's end (UX-C4).

func sse(events ...[2]string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: " + e[0] + "\ndata: " + e[1] + "\n\n")
	}
	return b.String()
}

func watchTaskJSON(state, msg, reply string) string {
	t := map[string]any{"id": "ix_1", "contextId": "c", "status": map[string]any{"state": state},
		"metadata": map[string]any{"anet.role": "requester"}}
	if msg != "" {
		t["status"] = map[string]any{"state": state, "message": map[string]any{"messageId": "m", "role": "ROLE_AGENT",
			"parts": []any{map[string]any{"text": msg}}}}
	}
	if reply != "" {
		t["artifacts"] = []any{map[string]any{"artifactId": "anet.reply", "name": "anet.reply",
			"parts": []any{map[string]any{"text": reply}}}}
	}
	b, _ := json.Marshal(t)
	return string(b)
}

func watchAnswer(body string) func(string, map[string]any) (int, string) {
	return func(path string, _ map[string]any) (int, string) {
		if path != "/tasks/events" {
			return 404, `{"error":"no such route"}`
		}
		return 200, body
	}
}

func TestTaskWatchExitStatus(t *testing.T) {
	prev := `{"seq":1,"at":1700000000000,"text":"Here is\nthe start\u001b[31m red","omitted":3}`
	for _, tc := range []struct {
		name   string
		body   string
		code   int
		stdout string
		stderr []string
	}{
		{"completed", sse(
			[2]string{"task", watchTaskJSON("TASK_STATE_WORKING", "", "")},
			[2]string{"delivery", `{"delivery":"confirmed","detail":"sent","wait":"","text":"Delivered: the other node confirmed."}`},
			[2]string{"preview", prev},
			[2]string{"task", watchTaskJSON("TASK_STATE_COMPLETED", "", "The English version")}),
			0, "TASK_STATE_COMPLETED", []string{"[working]", "[delivered] Delivered", "preview (", "3 bytes left out", "  Here is", "[completed]", "The English version"}},
		{"failed", sse([2]string{"task", watchTaskJSON("TASK_STATE_FAILED", "The provider refused.", "")}),
			1, "TASK_STATE_FAILED", []string{"[failed] The provider refused."}},
		{"rejected", sse([2]string{"task", watchTaskJSON("TASK_STATE_REJECTED", "", "")}), 1, "TASK_STATE_REJECTED", []string{"[rejected]"}},
		{"canceled", sse([2]string{"task", watchTaskJSON("TASK_STATE_CANCELED", "", "")}), 1, "TASK_STATE_CANCELED", []string{"[canceled]"}},
		{"input required", sse([2]string{"task", watchTaskJSON("TASK_STATE_INPUT_REQUIRED", "Which language?", "")}),
			3, "TASK_STATE_INPUT_REQUIRED", []string{"[input required] Which language?"}},
		{"the stream ends early", sse([2]string{"task", watchTaskJSON("TASK_STATE_WORKING", "", "")}), 1, "", []string{"anet task get ix_1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCLIHome(t, "C")
			newScriptedControl(t, h, watchAnswer(tc.body))
			stdout, stderr, code := runCLIWithin(t, h, 30*time.Second, []string{"task", "watch", "ix_1"}, "ANET_LANG=en")
			if code != tc.code {
				t.Fatalf("exit %d, want %d (stdout %q, stderr %q)", code, tc.code, stdout, stderr)
			}
			if !strings.Contains(stdout, tc.stdout) {
				t.Errorf("stdout %q lacks %q", stdout, tc.stdout)
			}
			for _, want := range tc.stderr {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr)
				}
			}
			if strings.ContainsRune(stderr, 0x1b) {
				t.Errorf("a peer's escape sequence reached the terminal: %q", stderr)
			}
		})
	}
}

func TestTaskWatchReasoningOnlyWhenAsked(t *testing.T) {
	body := sse([2]string{"preview", `{"seq":1,"at":1700000000000,"text":"answer so far","omitted":0,"reasoning":"private thoughts","reasoning_omitted":0}`},
		[2]string{"task", watchTaskJSON("TASK_STATE_COMPLETED", "", "done")})
	for _, tc := range []struct {
		args []string
		want bool
	}{{[]string{"task", "watch", "ix_1"}, false}, {[]string{"task", "watch", "ix_1", "--reasoning"}, true}, {[]string{"task", "watch", "--reasoning", "ix_1"}, true}} {
		h := newCLIHome(t, "C")
		newScriptedControl(t, h, watchAnswer(body))
		_, stderr, code := runCLIWithin(t, h, 30*time.Second, tc.args, "ANET_LANG=en")
		if code != 0 || strings.Contains(stderr, "private thoughts") != tc.want {
			t.Errorf("%v: exit %d, reasoning shown %v, stderr %q", tc.args, code, !tc.want, stderr)
		}
	}
}

func TestTaskWatchRefusals(t *testing.T) {
	h := newCLIHome(t, "C")
	sc := newScriptedControl(t, h, func(string, map[string]any) (int, string) {
		return 404, `{"error":"No task ix_9 on this node.","reason":"no_local_task"}`
	})
	_, stderr, code := runCLIWithin(t, h, 30*time.Second, []string{"task", "watch", "ix_9"}, "ANET_LANG=en")
	if code != 1 || !strings.Contains(stderr, "No task ix_9") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, args := range [][]string{{"task", "watch"}, {"task", "watch", "a", "b"}} {
		before := len(sc.sent())
		if _, _, code := runCLIWithin(t, h, 30*time.Second, args); code != 2 || len(sc.sent()) != before {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestDelegateWatchSendsThenWatches(t *testing.T) {
	if !slices.Contains(module.Compiled(), "preview") {
		t.Skip("the preview module is not in this build (-tags no_preview): --watch asks for no preview")
	}
	h := newCLIHome(t, "C")
	sc := newScriptedControl(t, h, func(path string, _ map[string]any) (int, string) {
		switch path {
		case "/tasks/send":
			return 200, watchTaskJSON("TASK_STATE_WORKING", "", "")
		case "/tasks/events":
			return 200, sse([2]string{"task", watchTaskJSON("TASK_STATE_COMPLETED", "", "hello")})
		}
		return 404, `{"error":"no such route"}`
	})
	stdout, stderr, code := runCLIWithin(t, h, 30*time.Second, []string{"delegate", wholeAID, "--watch", "say", "hello"}, "ANET_LANG=en")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if got := sc.sent(); len(got) != 2 || got[0] != "/tasks/send" || got[1] != "/tasks/events" {
		t.Fatalf("calls %v", got)
	}
	b := sc.lastBody("/tasks/send")
	if b["preview"] != "on" || b["return_immediately"] != true || b["text"] != "say hello" {
		t.Fatalf("send body %v", b)
	}
	if strings.Count(stdout, "TASK_STATE_") != 1 || !strings.Contains(stdout, "TASK_STATE_COMPLETED") {
		t.Errorf("stdout is the final task alone: %q", stdout)
	}
	// Without --watch the send is not asked for a preview.
	h2 := newCLIHome(t, "C")
	sc2 := newScriptedControl(t, h2, func(string, map[string]any) (int, string) { return 200, watchTaskJSON("TASK_STATE_WORKING", "", "") })
	if _, _, code := runCLIWithin(t, h2, 30*time.Second, []string{"delegate", wholeAID, "hi"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, ok := sc2.lastBody("/tasks/send")["preview"]; ok {
		t.Error("a plain delegate asked for previews")
	}
}

// MOD-6, -tags no_preview: the watch asks for no preview, waits for the final
// answer as ever, and says once on stderr that there is no preview.
func TestDelegateWatchWithoutPreviewModule(t *testing.T) {
	if slices.Contains(module.Compiled(), "preview") {
		t.Skip("the preview module is in this build")
	}
	h := newCLIHome(t, "C")
	sc := newScriptedControl(t, h, func(path string, _ map[string]any) (int, string) {
		switch path {
		case "/tasks/send":
			return 200, watchTaskJSON("TASK_STATE_WORKING", "", "")
		case "/tasks/events":
			return 200, sse([2]string{"task", watchTaskJSON("TASK_STATE_COMPLETED", "", "hello")})
		}
		return 404, `{"error":"no such route"}`
	})
	stdout, stderr, code := runCLIWithin(t, h, 30*time.Second, []string{"delegate", wholeAID, "--watch", "say", "hello"}, "ANET_LANG=en")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if _, ok := sc.lastBody("/tasks/send")["preview"]; ok {
		t.Error("the send asked for a preview of a build without previews")
	}
	if strings.Count(stderr, "does not include the preview module") != 1 {
		t.Errorf("stderr should say once that there is no preview module, got %q", stderr)
	}
	if !strings.Contains(stdout, "TASK_STATE_COMPLETED") {
		t.Errorf("the final answer was not printed: %q", stdout)
	}
}

// FuzzWatchEvents: whatever the daemon's stream carries, handling an event
// neither panics nor writes a control character other than a line end.
func FuzzWatchEvents(f *testing.F) {
	f.Add("task", []byte(watchTaskJSON("TASK_STATE_COMPLETED", "", "x")))
	f.Add("preview", []byte(`{"at":1,"text":"a\u001b[2Jb","omitted":1,"reasoning":"r"}`))
	f.Add("delivery", []byte(`{"delivery":"pending","text":"t\u0007"}`))
	f.Add("task", []byte(`{"status":{"state":5}}`))
	f.Fuzz(func(t *testing.T, name string, data []byte) {
		var out strings.Builder
		w := &watcher{out: &out, reasoning: true}
		so, se := cliStdout, cliStderr
		cliStdout, cliStderr = io.Discard, io.Discard
		defer func() { cliStdout, cliStderr = so, se }()
		w.handle(name, data)
		for _, r := range out.String() {
			if r < 0x20 && r != '\n' || r == 0x7f {
				t.Fatalf("control character %q in %q", r, out.String())
			}
		}
	})
}

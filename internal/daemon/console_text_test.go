package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// consoleThread is one thread of /threads or /thread, as the console reads it.
type consoleThread struct {
	InteractionID string `json:"interaction_id"`
	State         string `json:"state"`
	StatusText    string `json:"status_text"`
}

// ux UX-W1 (and UX-C6): the console takes a task's reason, payment and delivery sentences from the
// control plane's task projection and words none of its own. The data: a failed task's thread, as a
// console session reads it from /threads and /thread, carries status_text, the very text of the
// status.message /tasks/get projects for it; a working task carries none. The page: it shows
// status_text as it is, a refusal's error and next, and holds no sentence of the reason table and no
// reading of anet.reason, so it cannot say a reason of its own. [mut] status_text built from anything
// but the projection → the first half is red; a reason sentence written into console.html → the second.
func TestConsoleUsesProjectionText(t *testing.T) {
	p := newPlane(t)
	const failed, working = "ix_w1_failed", "ix_w1_working"
	peer := p.d.AID() // any well-formed AID; the task is never sent
	for _, id := range []string{failed, working} {
		if err := p.d.ix.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: peer, Goal: "g"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.d.ix.SetState(working, interactions.StateWorking, interactions.CauseNotTerminal); err != nil {
		t.Fatal(err)
	}
	if err := p.d.ix.Finish(failed, interactions.Finish{State: interactions.StateFailed, Verified: interactions.VerificationUnknown,
		Meta: []byte(`{"anet.reason":"undeliverable"}`)}, interactions.CausePeerEnded); err != nil {
		t.Fatal(err)
	}
	resp, b := p.req(t, "POST", "/tasks/get", `{"task_id":"`+failed+`"}`, p.bearer)
	if resp.StatusCode != 200 {
		t.Fatalf("/tasks/get = %d %s", resp.StatusCode, b)
	}
	var task a2ashape.Task
	if err := json.Unmarshal(b, &task); err != nil || task.Status.Message == nil {
		t.Fatalf("/tasks/get: %v %s", err, b)
	}
	var want []string
	for _, part := range task.Status.Message.Parts {
		if part.Kind == a2ashape.PartText {
			want = append(want, strings.TrimSpace(part.Text))
		}
	}
	if len(want) == 0 || strings.Join(want, " ") == "" {
		t.Fatalf("the failed task's projection has no status text: %s", b)
	}

	s := p.session(t)
	resp, b = p.req(t, "POST", "/threads", "{}", s.as(p))
	if resp.StatusCode != 200 {
		t.Fatalf("/threads = %d %s", resp.StatusCode, b)
	}
	var threads struct {
		Threads []consoleThread `json:"threads"`
	}
	if err := json.Unmarshal(b, &threads); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, th := range threads.Threads {
		switch th.InteractionID {
		case failed:
			seen++
			if th.StatusText != strings.Join(want, " ") {
				t.Errorf("/threads status_text %q, want the projection's %q", th.StatusText, strings.Join(want, " "))
			}
		case working:
			seen++
			if th.StatusText != "" {
				t.Errorf("a working task has status_text %q", th.StatusText)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("/threads listed %d of the two tasks: %s", seen, b)
	}
	resp, b = p.req(t, "POST", "/thread", `{"interaction_id":"`+failed+`"}`, s.as(p))
	var one struct {
		Thread consoleThread `json:"thread"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(b, &one) != nil || one.Thread.StatusText != strings.Join(want, " ") {
		t.Errorf("/thread = %d %s, want status_text %q", resp.StatusCode, b, strings.Join(want, " "))
	}

	page, err := ConsoleHTML()
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	for _, must := range []string{
		"t.status_text",                     // shows the projection's sentence
		"throw ctlError(j, text, r.status)", // a refusal keeps its sentence and next step (UX-W4)
		`e.next ? "  → " + e.next`,          // and shows them together
	} {
		if !strings.Contains(html, must) {
			t.Errorf("console.html lacks %q", must)
		}
	}
	for _, mustNot := range []string{`"anet.reason"`, `anet.reason"]`, "anet.payment_note", "anet.delivery"} {
		if strings.Contains(html, mustNot) {
			t.Errorf("console.html reads %s itself", mustNot)
		}
	}
	// No sentence of the reason table, in either language: a template's text up to its first
	// placeholder, where that is long enough to be a phrase and not a word.
	for _, r := range a2ashape.Rows() {
		for _, tpl := range []string{r.Zh, r.En} {
			fixed, _, _ := strings.Cut(tpl, "{")
			fixed = strings.TrimSpace(fixed)
			if utf8.RuneCountInString(fixed) < 12 {
				continue
			}
			if strings.Contains(html, fixed) {
				t.Errorf("console.html carries the reason table's sentence of %s: %q", r.Key(), fixed)
			}
		}
	}
}

// ux UX-W3: the hub's /graph lost its review edges (HPP), and the console must show as well without
// them. It draws no graph at all: it never asks a hub for /graph and reads no edges, so a hub that
// sends none changes nothing. [mut] the page reads a graph's edges again → red.
func TestConsoleRendersWithoutEdges(t *testing.T) {
	page, err := ConsoleHTML()
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	for _, mustNot := range []string{"/graph", ".edges", `"edges"`, "edges.", "edges["} {
		if strings.Contains(html, mustNot) {
			t.Errorf("console.html still reads %q", mustNot)
		}
	}
	// What the home page does read from the hub is the counters of /stats, each with a default when
	// it is absent.
	for _, must := range []string{`HUB + "/stats"`, "s.agents ?? 0", "s.tasks_completed ?? 0", "s.reviews ?? 0"} {
		if !strings.Contains(html, must) {
			t.Errorf("console.html lacks %q", must)
		}
	}
}

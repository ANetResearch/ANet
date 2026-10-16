package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

func watchPair(t *testing.T, seam *DaemonTaskSeam, ctx context.Context, peer, id string, preview bool) (a2ashape.Task, <-chan a2ashape.TaskEvent, error) {
	t.Helper()
	return seam.Watch(ctx, peer, id, module.WatchOpts{Preview: preview})
}

// UX-10: POST /tasks/events (CUX §2.7, §4.8) and the preview artifact
// updates of a stream (CUX-A2, A3).

type sseEvent struct {
	name string
	data string
}

// pipeWriter is a streaming ResponseWriter: what the handler writes can be
// read while it runs.
type pipeWriter struct {
	hdr    http.Header
	status int
	pw     *io.PipeWriter
}

func (w *pipeWriter) Header() http.Header         { return w.hdr }
func (w *pipeWriter) WriteHeader(code int)        { w.status = code }
func (w *pipeWriter) Write(b []byte) (int, error) { return w.pw.Write(b) }
func (w *pipeWriter) Flush()                      {}

// openEvents calls /tasks/events for task on d; the returned channel carries
// its events, and closes when the handler returns.
func openEvents(t *testing.T, d *Daemon, task string) (<-chan sseEvent, *pipeWriter, context.CancelFunc) {
	t.Helper()
	pr, pw := io.Pipe()
	w := &pipeWriter{hdr: http.Header{}, pw: pw}
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/tasks/events", strings.NewReader(`{"task_id":"`+task+`"}`)).WithContext(ctx)
	go func() {
		d.hTasksEvents(w, r)
		_ = pw.Close()
	}()
	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(pr)
		sc.Buffer(nil, 4<<20)
		var cur sseEvent
		for sc.Scan() {
			switch l := sc.Text(); {
			case strings.HasPrefix(l, "event: "):
				cur.name = strings.TrimPrefix(l, "event: ")
			case strings.HasPrefix(l, "data: "):
				cur.data = strings.TrimPrefix(l, "data: ")
			case l == "" && cur.name != "":
				out <- cur
				cur = sseEvent{}
			}
		}
	}()
	t.Cleanup(cancel)
	return out, w, cancel
}

func nextEvent(t *testing.T, ch <-chan sseEvent, name string) sseEvent {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatalf("the stream ended; wanted a %s event", name)
		}
		if name != "" && e.name != name {
			t.Fatalf("event %s %s; wanted %s", e.name, e.data, name)
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s event", name)
	}
	return sseEvent{}
}

func TestTasksEventsStreamsTaskPreviewAndDelivery(t *testing.T) {
	_, req, prov := quietPair(t)
	clk := frozenClock(req)
	id := previewDelegation(t, req, prov, previewModeWithReasoning)
	answers := lastOwnText(t, req, id)
	req.setOutboxHold(true)

	ch, w, _ := openEvents(t, req, id)
	first := nextEvent(t, ch, "task")
	var task a2ashape.Task
	if err := json.Unmarshal([]byte(first.data), &task); err != nil || task.ID != id {
		t.Fatalf("first task event: %v %s", err, first.data)
	}
	if len(first.data) > maxEventTaskBytes {
		t.Fatalf("a task event of %d bytes", len(first.data))
	}
	if ct := w.hdr.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}

	// A frame taken is a preview event, with the reasoning of a round that asked for it.
	cm := previewFrameMsg(answers, clk.now(), "draft one")
	cm.ReasoningBody, cm.ReasoningStreamAtMS = "thinking", int64(clk.now())
	if r := receive(t, req, craftPreview(t, prov, req, id, cm, nil)); !taken(r) {
		t.Fatalf("frame: %+v", r)
	}
	var pv PreviewEvent
	if err := json.Unmarshal([]byte(nextEvent(t, ch, "preview").data), &pv); err != nil ||
		pv.Text != "draft one" || pv.Reasoning != "thinking" || pv.Seq == 0 {
		t.Fatalf("preview event %+v %v", pv, err)
	}

	// A delivery event carries its fields, and moves no state.
	req.publishDelivery(id, DeliveryEvent{Delivery: "confirmed", Detail: "sent", Wait: "", Text: "Delivered."})
	var dv map[string]string
	if err := json.Unmarshal([]byte(nextEvent(t, ch, "delivery").data), &dv); err != nil ||
		dv["delivery"] != "confirmed" || dv["detail"] != "sent" || dv["wait"] != "" || dv["text"] != "Delivered." {
		t.Fatalf("delivery event %v %v", dv, err)
	}
	select {
	case e := <-ch:
		t.Fatalf("a delivery event moved the task: %s", e.name)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestTasksEventsEndsAfterTheTerminalTask(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	task, err := req.TaskSeam().Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("hi", "", "m-1"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	ch, _, _ := openEvents(t, req, task.ID)
	nextEvent(t, ch, "task")
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, task.ID, "the answer", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.CompleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var last a2ashape.Task
	for {
		e, ok := <-ch
		if !ok {
			break
		}
		if e.name == "delivery" {
			continue // the confirmation of the delegation (RC-5): it moves no state
		}
		if e.name != "task" {
			t.Fatalf("event %s", e.name)
		}
		_ = json.Unmarshal([]byte(e.data), &last)
	}
	if last.Status.State != a2ashape.TaskStateCompleted || len(last.Artifacts) != 1 || last.Artifacts[0].ID != "anet.reply" {
		t.Fatalf("the last task event: %+v", last)
	}
}

func TestTasksEventsUnknownTask(t *testing.T) {
	_, req, _ := registeredPair(t)
	p := newPlaneFor(t, req, "tok")
	resp, body := p.req(t, "POST", "/tasks/events", `{"task_id":"ix_nope"}`, p.bearer)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "no_local_task") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	resp, body = p.req(t, "POST", "/tasks/events", `{}`, p.bearer)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no task id: %d %s", resp.StatusCode, body)
	}
}

// A task event stays within its bound however long the reply is.
func TestTasksEventsTaskIsBounded(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	task, err := req.TaskSeam().Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("hi", "", "m-1"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, task.ID, strings.Repeat("long ", 40000), nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ch, _, _ := openEvents(t, req, task.ID)
	e := nextEvent(t, ch, "task")
	if len(e.data) > maxEventTaskBytes+512 {
		t.Fatalf("a task event of %d bytes", len(e.data))
	}
}

// The route takes the control token only: not a console session, not MCP.
func TestTasksEventsNotInSessionOrMCPRoutes(t *testing.T) {
	if _, ok := sessionRoutes["POST /tasks/events"]; ok {
		t.Fatal("/tasks/events is on the console session allowlist")
	}
}

// CUX-A3, mutation M24: a watcher that did not opt in gets no preview; one
// that did gets the frame as an anet.reply update that replaces the
// artifact and is not final; one that comes late is given the current
// preview after the snapshot; reasoning is never in it.
func TestWatchPreviewIsForOptedInWatchersOnly(t *testing.T) {
	_, req, prov := quietPair(t)
	clk := frozenClock(req)
	id := previewDelegation(t, req, prov, previewModeWithReasoning)
	answers := lastOwnText(t, req, id)
	req.setOutboxHold(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seam := req.TaskSeam()

	_, with, err := watchPair(t, seam, ctx, prov.AID(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	_, without, err := watchPair(t, seam, ctx, prov.AID(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	cm := previewFrameMsg(answers, clk.now(), "so far")
	cm.ReasoningBody, cm.ReasoningStreamAtMS = "secret reasoning", int64(clk.now())
	if r := receive(t, req, craftPreview(t, prov, req, id, cm, nil)); !taken(r) {
		t.Fatalf("frame: %+v", r)
	}
	select {
	case e := <-with:
		u := e.ArtifactUpdate
		if u == nil || u.Artifact.ID != "anet.reply" || u.Append || u.LastChunk || u.Metadata["anet.preview"] != true ||
			u.Artifact.Metadata["anet.preview"] != true || u.Artifact.Parts[0].Text != "so far" {
			t.Fatalf("preview update %+v", e)
		}
		if b, _ := json.Marshal(e); strings.Contains(string(b), "secret reasoning") {
			t.Fatal("reasoning on the A2A stream")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no preview update")
	}
	select {
	case e := <-without:
		t.Fatalf("a watcher that did not opt in got %+v", e)
	case <-time.After(150 * time.Millisecond):
	}

	// A late subscriber gets the current preview first, replacing.
	_, late, err := watchPair(t, seam, ctx, prov.AID(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-late:
		if e.ArtifactUpdate == nil || e.ArtifactUpdate.Append || e.ArtifactUpdate.Artifact.Parts[0].Text != "so far" {
			t.Fatalf("replay %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no replay of the current preview")
	}
	// A watcher without the extension gets none.
	_, plain, err := watchPair(t, seam, ctx, prov.AID(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-plain:
		t.Fatalf("replay to a watcher that did not opt in: %+v", e)
	case <-time.After(150 * time.Millisecond):
	}
}

// A frame that omitted its head starts with an ellipsis.
func TestPreviewUpdateOmittedHead(t *testing.T) {
	u := previewUpdate(&interactions.Interaction{ID: "ix"}, "c", previewFrame{previewContent: previewContent{Text: "tail", Omitted: 9}})
	if u.Artifact.Parts[0].Text != "…tail" {
		t.Fatalf("%q", u.Artifact.Parts[0].Text)
	}
}

// CUX-A2: a new provider message sends anet.reply before the status update
// that announces it, and a second note that does not change the reply does
// not send it again.
func TestWatchSendsReplyArtifactBeforeStatus(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seam := req.TaskSeam()
	task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("hi", "", "m-1"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	_, events, err := seam.Watch(ctx, prov.AID(), task.ID, module.WatchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, task.ID, "which language?", nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for len(kinds) < 2 {
		select {
		case e := <-events:
			switch {
			case e.ArtifactUpdate != nil:
				if !e.ArtifactUpdate.LastChunk || e.ArtifactUpdate.Append || e.ArtifactUpdate.Artifact.Parts[0].Text != "which language?" {
					t.Fatalf("artifact update %+v", e.ArtifactUpdate)
				}
				kinds = append(kinds, "artifact")
			case e.StatusUpdate != nil:
				kinds = append(kinds, "status:"+string(e.StatusUpdate.Status.State))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("events so far: %v", kinds)
		}
	}
	if kinds[0] != "artifact" || kinds[1] != "status:TASK_STATE_INPUT_REQUIRED" {
		t.Fatalf("order %v", kinds)
	}
	got, err := seam.Get(ctx, prov.AID(), task.ID, nil)
	if err != nil || len(got.Artifacts) != 1 || got.Artifacts[0].ID != "anet.reply" {
		t.Fatalf("GetTask at input-required: %+v %v", got.Artifacts, err)
	}
}

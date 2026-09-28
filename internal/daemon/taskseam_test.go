package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/a2acard"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// textMsg is a client message with one text part.
func textMsg(text, contextID, messageID string) a2ashape.Message {
	return a2ashape.Message{ID: messageID, ContextID: contextID, Role: a2ashape.RoleUser,
		Parts: []a2ashape.Part{a2ashape.TextPart(text)}}
}

// followUp is a client message on an existing task.
func followUp(taskID, text, messageID string) a2ashape.Message {
	m := textMsg(text, "", messageID)
	m.TaskID = taskID
	return m
}

// outboundCount is how many outbound interactions d holds.
func outboundCount(t *testing.T, d *Daemon) int {
	t.Helper()
	n, err := d.ix.Count(interactions.ListFilter{Role: interactions.RoleOutbound})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A client scoped to one agent reaches only the outbound tasks it sent that
// agent. Another agent's task, an inbound task, an unknown id and another
// agent's context are all TaskNotFound (A2A-DESIGN §11.1 [C17]).
func TestTheTaskSeamIsScopedToOneAgent(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	other := newTestDaemon(t, srv.URL, true)
	if err := other.RegisterWithHub(ctx, srv.URL, "Other", nil, ""); err != nil {
		t.Fatal(err)
	}
	seam := req.TaskSeam()
	task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("hello", "ctx-a", "m-1"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	// An inbound task on the requester's side, from the same peer.
	if err := req.ix.Create(interactions.New{ID: "ix_inbound", Role: interactions.RoleInbound, PeerAID: prov.AID(),
		Goal: "for you", ContextID: "ctx-in"}); err != nil {
		t.Fatal(err)
	}

	notFound := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, a2ashape.ErrTaskNotFound) {
			t.Errorf("%s: %v, want TaskNotFound", what, err)
		}
	}
	_, err = seam.Get(ctx, other.AID(), task.ID, nil)
	notFound("get another agent's task", err)
	_, err = seam.Cancel(ctx, other.AID(), task.ID)
	notFound("cancel another agent's task", err)
	_, _, err = seam.Watch(ctx, other.AID(), task.ID)
	notFound("watch another agent's task", err)
	_, err = seam.Send(ctx, other.AID(), module.TaskSend{Message: followUp(task.ID, "more", "m-2"), ReturnImmediately: true})
	notFound("follow up another agent's task", err)
	_, err = seam.Get(ctx, prov.AID(), "ix_inbound", nil)
	notFound("get an inbound task", err)
	_, err = seam.Send(ctx, prov.AID(), module.TaskSend{Message: followUp("ix_inbound", "more", "m-3"), ReturnImmediately: true})
	notFound("follow up an inbound task", err)
	_, err = seam.Get(ctx, prov.AID(), "ix_does_not_exist", nil)
	notFound("get an unknown task", err)
	_, err = seam.Send(ctx, prov.AID(), module.TaskSend{Message: followUp("ix_chosen_by_client", "new?", "m-4"), ReturnImmediately: true})
	notFound("a client-chosen id for a new task", err)
	_, err = seam.Get(ctx, "", task.ID, nil)
	notFound("no agent named", err)

	// Another agent's context: refused, and no task is created.
	before := outboundCount(t, req)
	_, err = seam.Send(ctx, other.AID(), module.TaskSend{Message: textMsg("sneak in", "ctx-a", "m-5"), ReturnImmediately: true})
	notFound("a new task in another agent's context", err)
	if outboundCount(t, req) != before {
		t.Fatal("a task was created in another agent's context")
	}

	// Listing shows each agent only its own tasks.
	page, err := seam.List(ctx, other.AID(), module.TaskFilter{})
	if err != nil || len(page.Tasks) != 0 || page.TotalSize != 0 {
		t.Fatalf("the other agent lists %+v (%v)", page, err)
	}
	page, err = seam.List(ctx, prov.AID(), module.TaskFilter{})
	if err != nil || len(page.Tasks) != 1 || page.Tasks[0].ID != task.ID {
		t.Fatalf("the agent lists %+v (%v), want only %s", page, err, task.ID)
	}
	page, _ = seam.List(ctx, prov.AID(), module.TaskFilter{ContextID: "ctx-in"})
	if len(page.Tasks) != 0 {
		t.Fatal("an inbound task's context is listed through the seam")
	}

	// The right agent in the same context is a second task there.
	second, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("again", "ctx-a", "m-6"), ReturnImmediately: true})
	if err != nil || second.ContextID != "ctx-a" || second.ID == task.ID {
		t.Fatalf("second task in the context: %+v %v", second, err)
	}
}

// A contextId the client chose is kept as it is and finds the task again
// (C22); a retry with the same (contextId, messageId) returns the task the
// first attempt made and sends nothing (A2A-DESIGN §11.5). The client's
// message id is metadata, not the envelope's message id.
func TestClientContextIsKeptAndRetriesAreDeduplicated(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	send := module.TaskSend{Message: textMsg("summarize", "client-ctx-1", "client-msg-1"), ReturnImmediately: true}
	task, err := seam.Send(ctx, prov.AID(), send)
	if err != nil {
		t.Fatal(err)
	}
	if task.ContextID != "client-ctx-1" {
		t.Fatalf("contextId = %q, want the client's", task.ContextID)
	}
	retry, err := seam.Send(ctx, prov.AID(), send)
	if err != nil || retry.ID != task.ID {
		t.Fatalf("retry = %s (%v), want %s", retry.ID, err, task.ID)
	}
	if n := outboundCount(t, req); n != 1 {
		t.Fatalf("%d tasks after a retry", n)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("%d delegations sent for one client message", n)
	}
	page, err := seam.List(ctx, prov.AID(), module.TaskFilter{ContextID: "client-ctx-1"})
	if err != nil || len(page.Tasks) != 1 || page.Tasks[0].ID != task.ID {
		t.Fatalf("list by the client's context = %+v (%v)", page, err)
	}
	msgs, _ := req.ix.Messages(task.ID)
	if len(msgs) != 1 || msgs[0].MsgID == "client-msg-1" || !strings.Contains(msgs[0].Metadata, `"a2a.messageId":"client-msg-1"`) {
		t.Fatalf("first message = %+v; the client id belongs in metadata, not in msg_id", msgs)
	}
	if len(task.History) != 1 || task.History[0].ID != "client-msg-1" || task.History[0].Role != a2ashape.RoleUser {
		t.Fatalf("history = %+v", task.History)
	}
	// The provider receives the client's metadata with the goal.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	pm, _ := prov.ix.Messages(task.ID)
	if len(pm) != 1 || !strings.Contains(pm[0].Metadata, "client-msg-1") {
		t.Fatalf("provider's first message = %+v", pm)
	}
	// Without a contextId the daemon mints one.
	minted, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("other", "", "client-msg-2"), ReturnImmediately: true})
	if err != nil || minted.ContextID == "" || minted.ContextID == "client-ctx-1" {
		t.Fatalf("minted context = %q (%v)", minted.ContextID, err)
	}
	// The control plane finds it by context too (MCP list_tasks context_id).
	cp, err := req.listTasks(controlScope, taskListReq{TaskFilter: module.TaskFilter{ContextID: "client-ctx-1"}})
	if err != nil || len(cp.Tasks) != 1 || cp.Tasks[0].ID != task.ID {
		t.Fatalf("control plane list by context = %+v (%v)", cp, err)
	}
}

// A blocking follow-up on an input-required task does not return the
// question it answers: it waits for the provider to speak again (C35).
func TestABlockingFollowUpWaitsForTheNextReply(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()

	type result struct {
		task a2ashape.Task
		err  error
	}
	// A blocking new task returns once the provider asks its question.
	first := make(chan result, 1)
	go func() {
		task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("plan a trip", "", "m-1")})
		first <- result{task, err}
	}()
	var id string
	waitUntil(t, "the delegation to reach the provider", func() bool {
		_ = prov.pollOnce(ctx)
		list, _ := prov.ix.ListAll(interactions.ListFilter{Role: interactions.RoleInbound})
		if len(list) == 1 {
			id = list[0].ID
			return true
		}
		return false
	})
	select {
	case r := <-first:
		t.Fatalf("the blocking send returned before the provider answered: %+v %v", r.task.Status, r.err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := prov.SendMessage(ctx, id, "where to?", nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var r result
	select {
	case r = <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocking send did not return after the provider's question")
	}
	if r.err != nil || r.task.Status.State != a2ashape.TaskStateInputRequired || statusText(r.task) != "where to?" {
		t.Fatalf("first answer: %+v %v", r.task.Status, r.err)
	}

	// The follow-up.
	second := make(chan result, 1)
	go func() {
		task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: followUp(id, "Kyoto", "m-2")})
		second <- result{task, err}
	}()
	waitUntil(t, "the follow-up to be stored", func() bool { return countMsgs(t, req, id) == 3 })
	select {
	case r := <-second:
		t.Fatalf("the follow-up returned the old question: %+v %v", r.task.Status, r.err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, id, "which month?", nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r = <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("the follow-up did not return after the provider's next reply")
	}
	if r.err != nil || r.task.Status.State != a2ashape.TaskStateInputRequired || statusText(r.task) != "which month?" {
		t.Fatalf("follow-up answer: %+v %v", r.task.Status, r.err)
	}
}

// statusText is the text of a task's status message.
func statusText(t a2ashape.Task) string {
	if t.Status.Message == nil {
		return ""
	}
	var parts []string
	for _, p := range t.Status.Message.Parts {
		if p.Kind == a2ashape.PartText {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "")
}

// Input to a task in a terminal state is UnsupportedOperation (§4.2, C35).
func TestSendingToAFailedTaskIsUnsupported(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	if err := req.ix.Create(interactions.New{ID: "ix_failed", Role: interactions.RoleOutbound, PeerAID: prov.AID(),
		Goal: "g", ContextID: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := req.ix.SetFailed("ix_failed", []byte("broken")); err != nil {
		t.Fatal(err)
	}
	_, err := req.TaskSeam().Send(ctx, prov.AID(), module.TaskSend{Message: followUp("ix_failed", "try again", "m-1"), ReturnImmediately: true})
	if !errors.Is(err, a2ashape.ErrUnsupportedOperation) {
		t.Fatalf("send to a failed task: %v, want UnsupportedOperation", err)
	}
	if countMsgs(t, req, "ix_failed") != 0 {
		t.Fatal("the message was stored")
	}
	if _, err := req.TaskSeam().Cancel(ctx, prov.AID(), "ix_failed"); !errors.Is(err, a2ashape.ErrTaskNotCancelable) {
		t.Fatalf("cancel a failed task: %v, want TaskNotCancelable", err)
	}
}

// A url part of any scheme is InvalidParams: the daemon fetches nothing
// and reads no local file (A2A-DESIGN §11.5 [C44]). Nothing is created or
// sent.
func TestURLPartsAreRefused(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	for _, u := range []string{"file:///etc/passwd", "https://example.com/a.pdf", "data:text/plain,hi", "/etc/hosts"} {
		m := textMsg("read this", "", "")
		m.Parts = append(m.Parts, a2ashape.URLPart(u, "x", ""))
		if _, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: m, ReturnImmediately: true}); !errors.Is(err, a2ashape.ErrInvalidParams) {
			t.Errorf("url %q: %v, want InvalidParams", u, err)
		}
	}
	if outboundCount(t, req) != 0 || len(queuedFor(t, srv, prov.AID())) != 0 {
		t.Fatal("a message with a url part created or sent a task")
	}
	// On a follow-up as well.
	task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("start", "", ""), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	m := followUp(task.ID, "", "")
	m.Parts = []a2ashape.Part{a2ashape.URLPart("file:///etc/shadow", "", "")}
	if _, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: m, ReturnImmediately: true}); !errors.Is(err, a2ashape.ErrInvalidParams) {
		t.Fatalf("url part on a follow-up: %v", err)
	}
	// A raw file part is an attachment; a DataPart {skill, args} is a
	// capability call.
	raw := textMsg("see file", "", "")
	raw.Parts = append(raw.Parts, a2ashape.RawPart([]byte("hello file"), "notes.txt", "text/plain"))
	ft, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: raw, ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	if atts, _ := req.ix.Attachments(ft.ID); len(atts) != 1 || atts[0].Name != "notes.txt" {
		t.Fatalf("attachments = %+v", atts)
	}
	capMsg := a2ashape.Message{ID: "m-cap", Parts: []a2ashape.Part{a2ashape.DataPart(map[string]any{"skill": "echo.say", "args": map[string]any{"x": 1}})}}
	ct, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: capMsg, ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	if ix, _ := req.ix.Get(ct.ID); !ix.IsCapability || ct.Metadata["anet.skill"] != "echo.say" {
		t.Fatalf("capability task: %+v %+v", ix, ct.Metadata)
	}
	mixed := capMsg
	mixed.Parts = append(mixed.Parts, a2ashape.TextPart("and also"))
	if _, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: mixed, ReturnImmediately: true}); !errors.Is(err, a2ashape.ErrInvalidParams) {
		t.Fatalf("capability call with text: %v", err)
	}
}

// ListTasks pages newest state change first with a token, reports the total
// before paging, and filters by state and time (A2A-DESIGN §11.5).
func TestListTasksPages(t *testing.T) {
	d := newTestDaemon(t, "", false)
	seam := d.TaskSeam()
	ctx := context.Background()
	clock := int64(1_000_000)
	d.ix.SetClock(func() int64 { return clock })
	var ids []string
	for i := 0; i < 7; i++ {
		clock += 1000
		id := fmt.Sprintf("ix_%02d", i)
		if err := d.ix.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: "peer-a", Goal: "g", ContextID: "c"}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for i := 0; i < 2; i++ {
		clock += 1000
		_ = d.ix.Create(interactions.New{ID: fmt.Sprintf("ix_b%d", i), Role: interactions.RoleOutbound, PeerAID: "peer-b", Goal: "g"})
	}
	var got []string
	token := ""
	for pages := 0; ; pages++ {
		page, err := seam.List(ctx, "peer-a", module.TaskFilter{PageSize: 3, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		if page.TotalSize != 7 || page.PageSize != 3 {
			t.Fatalf("page %d: total %d size %d", pages, page.TotalSize, page.PageSize)
		}
		for _, tk := range page.Tasks {
			got = append(got, tk.ID)
			if tk.Artifacts != nil {
				t.Fatal("artifacts listed without includeArtifacts")
			}
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
		if pages > 5 {
			t.Fatal("paging does not end")
		}
	}
	if len(got) != 7 {
		t.Fatalf("listed %v", got)
	}
	for i, id := range got {
		if id != ids[len(ids)-1-i] {
			t.Fatalf("order %v, want newest first", got)
		}
	}
	if _, err := seam.List(ctx, "peer-a", module.TaskFilter{PageSize: 101}); !errors.Is(err, a2ashape.ErrInvalidParams) {
		t.Fatalf("pageSize 101: %v", err)
	}
	if _, err := seam.List(ctx, "peer-a", module.TaskFilter{PageToken: "forged"}); !errors.Is(err, a2ashape.ErrInvalidParams) {
		t.Fatalf("forged token: %v", err)
	}
	clock += 1000
	if _, err := d.ix.SetState(ids[0], interactions.StateWorking); err != nil {
		t.Fatal(err)
	}
	page, err := seam.List(ctx, "peer-a", module.TaskFilter{State: string(a2ashape.TaskStateWorking)})
	if err != nil || len(page.Tasks) != 1 || page.Tasks[0].ID != ids[0] || page.TotalSize != 1 {
		t.Fatalf("status filter = %+v (%v)", page, err)
	}
	after := time.UnixMilli(clock - 1)
	page, err = seam.List(ctx, "peer-a", module.TaskFilter{UpdatedAfter: &after})
	if err != nil || len(page.Tasks) != 1 || page.Tasks[0].ID != ids[0] {
		t.Fatalf("statusTimestampAfter = %+v (%v)", page, err)
	}
	if _, err := seam.List(ctx, "peer-a", module.TaskFilter{State: "TASK_STATE_SLEEPING"}); !errors.Is(err, a2ashape.ErrInvalidParams) {
		t.Fatalf("unknown status: %v", err)
	}
}

// A watcher gets the provider's question, then the reply artifact before
// the terminal status, and the channel closes.
func TestWatchStreamsStatusThenArtifactsThenTheEnd(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seam := req.TaskSeam()
	task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("hi", "", "m-1"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	snap, events, err := seam.Watch(ctx, prov.AID(), task.ID)
	if err != nil || snap.Status.State != a2ashape.TaskStateSubmitted {
		t.Fatalf("snapshot %+v %v", snap.Status, err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, task.ID, "hello back", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.CompleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	timeout := time.After(5 * time.Second)
	for done := false; !done; {
		select {
		case e, ok := <-events:
			if !ok {
				done = true
				break
			}
			switch {
			case e.ArtifactUpdate != nil:
				kinds = append(kinds, "artifact:"+e.ArtifactUpdate.Artifact.ID)
			case e.StatusUpdate != nil:
				kinds = append(kinds, "status:"+string(e.StatusUpdate.Status.State))
			}
		case <-timeout:
			t.Fatalf("no end of stream; got %v", kinds)
		}
	}
	joined := strings.Join(kinds, " ")
	iReply := strings.Index(joined, "artifact:anet.reply")
	iDone := strings.Index(joined, "status:TASK_STATE_COMPLETED")
	if iReply < 0 || iDone < iReply || !strings.HasSuffix(joined, "status:TASK_STATE_COMPLETED") {
		t.Fatalf("events %v: want the anet.reply artifact before the completed status, which ends the stream", kinds)
	}
	final, err := seam.Get(ctx, prov.AID(), task.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if final.Metadata["anet.receipt_verified"] != "verified" || final.Artifacts[0].ID != "anet.reply" {
		t.Fatalf("final task metadata %v artifacts %+v", final.Metadata, final.Artifacts)
	}
}

// The /tasks/* routes answer with the projection and an A2A error name, and
// the provider answers through /tasks/reply.
func TestTaskRoutesOverTheControlPlane(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	planes := map[*Daemon]*plane{req: newPlaneFor(t, req, "tok"), prov: newPlaneFor(t, prov, "tok")}
	call := func(d *Daemon, path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		p := planes[d]
		resp, raw := p.req(t, "POST", path, string(b), p.bearer)
		out := map[string]any{}
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	code, out := call(req, "/tasks/get", map[string]any{"task_id": "ix_nope"})
	if code != http.StatusNotFound || out["code"] != "TaskNotFoundError" {
		t.Fatalf("get unknown: %d %v", code, out)
	}
	code, out = call(req, "/tasks/send", map[string]any{"to": prov.AID(), "text": "hi", "context_id": "cp-ctx", "return_immediately": true})
	if code != http.StatusOK || out["contextId"] != "cp-ctx" {
		t.Fatalf("send: %d %v", code, out)
	}
	id, _ := out["id"].(string)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// The provider replies and completes through its own control plane.
	code, out = call(prov, "/tasks/reply", map[string]any{"task_id": id, "text": "done it", "state": "completed"})
	if code != http.StatusOK || out["status"].(map[string]any)["state"] != string(a2ashape.TaskStateCompleted) {
		t.Fatalf("reply: %d %v", code, out)
	}
	code, out = call(prov, "/tasks/reply", map[string]any{"task_id": id, "text": "more"})
	if code != http.StatusConflict || out["code"] != "UnsupportedOperationError" {
		t.Fatalf("reply to a completed task: %d %v", code, out)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	code, out = call(req, "/tasks/wait", map[string]any{"task_id": id, "timeout_ms": 2000})
	if code != http.StatusOK || out["status"].(map[string]any)["state"] != string(a2ashape.TaskStateCompleted) {
		t.Fatalf("wait: %d %v", code, out)
	}
	arts, _ := out["artifacts"].([]any)
	if len(arts) == 0 {
		t.Fatalf("no artifacts: %v", out)
	}
	first := arts[0].(map[string]any)
	part := first["parts"].([]any)[0].(map[string]any)
	if first["artifactId"] != "anet.reply" || part["text"] != "done it" {
		t.Fatalf("reply artifact = %v", first)
	}
	code, out = call(req, "/tasks/list", map[string]any{"context_id": "cp-ctx", "role": "outbound"})
	if code != http.StatusOK || out["totalSize"].(float64) != 1 {
		t.Fatalf("list: %d %v", code, out)
	}
	code, out = call(req, "/tasks/send", map[string]any{"to": prov.AID(), "task_id": id, "text": "again"})
	if code != http.StatusConflict {
		t.Fatalf("follow-up on a completed task: %d %v", code, out)
	}
}

// A wait that times out answers the task as it is, marked timed_out.
func TestAWaitThatTimesOutAnswersTheTask(t *testing.T) {
	d := newTestDaemon(t, "", false)
	if err := d.ix.Create(interactions.New{ID: "ix_w", Role: interactions.RoleOutbound, PeerAID: "peer", Goal: "g", ContextID: "c"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ix, timedOut, err := d.waitTask(context.Background(), "ix_w", 0, 50*time.Millisecond)
	if err != nil || !timedOut || ix.State != interactions.StateSubmitted || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("wait = %+v %v %v", ix, timedOut, err)
	}
	// Already waiting for input, and nothing newer asked for: at once.
	if _, err := d.ix.SetState("ix_w", interactions.StateInputRequired); err != nil {
		t.Fatal(err)
	}
	ix, timedOut, err = d.waitTask(context.Background(), "ix_w", 0, time.Minute)
	if err != nil || timedOut || ix.State != interactions.StateInputRequired {
		t.Fatalf("wait on input-required = %+v %v %v", ix, timedOut, err)
	}
	// After that state_seq: waits.
	_, timedOut, _ = d.waitTask(context.Background(), "ix_w", ix.StateSeq, 30*time.Millisecond)
	if !timedOut {
		t.Fatal("a wait after the current state_seq returned without a newer state")
	}
}

// Two copies of one client message sent at once make one task.
func TestConcurrentCopiesOfOneMessageMakeOneTask(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	send := module.TaskSend{Message: textMsg("once", "ctx-dup", "m-dup"), ReturnImmediately: true}
	ids := make(chan string, 4)
	for i := 0; i < 4; i++ {
		go func() {
			task, err := seam.Send(ctx, prov.AID(), send)
			if err != nil {
				ids <- "error: " + err.Error()
				return
			}
			ids <- task.ID
		}()
	}
	first := <-ids
	for i := 1; i < 4; i++ {
		if got := <-ids; got != first {
			t.Fatalf("copies answered %s and %s", first, got)
		}
	}
	if n := outboundCount(t, req); n != 1 {
		t.Fatalf("%d tasks for one message", n)
	}
}

// The store's key for a client's message id and the projection's are one
// string: the dedupe query reads what the projection shows.
func TestTheClientMessageIDKeyIsTheProjections(t *testing.T) {
	if interactions.ClientMessageIDKey != a2ashape.KeyMessageID {
		t.Fatalf("interactions %q, a2ashape %q", interactions.ClientMessageIDKey, a2ashape.KeyMessageID)
	}
}

// A new task whose delegation the hub refuses for good (here 413) is
// recorded, failed with anet.reason=undeliverable and answered as it is
// (0017 Q5); the client's retry of the same message is a new attempt, not
// the failed task handed back as its duplicate. Without a contextId a retry
// is still recognised, by agent and messageId.
func TestARetryAfterAnUndeliveredTaskIsANewAttempt(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	fake := fakeHubAt(t, srv.URL)
	setHub := func(f func()) {
		fake.mu.Lock()
		f()
		fake.mu.Unlock()
	}
	setHub(func() { fake.maxEnvelope = 1 })
	send := module.TaskSend{Message: textMsg("hello", "ctx-u", "m-u"), ReturnImmediately: true}
	refused, err := seam.Send(ctx, prov.AID(), send)
	if err != nil || refused.Status.State != a2ashape.TaskStateFailed ||
		refused.Metadata[a2ashape.KeyReason] != a2ashape.ReasonUndeliverable {
		t.Fatalf("send with the hub refusing: %s %v (%v), want failed, undeliverable", refused.Status.State, refused.Metadata, err)
	}
	failed, err := req.ix.ListAll(interactions.ListFilter{Role: interactions.RoleOutbound})
	if err != nil || len(failed) != 1 || failed[0].State != interactions.StateFailed || failed[0].ID != refused.ID {
		t.Fatalf("after the failed send: %+v %v", failed, err)
	}
	setHub(func() { fake.maxEnvelope = 0 })
	retry, err := seam.Send(ctx, prov.AID(), send)
	if err != nil || retry.ID == failed[0].ID || retry.Status.State != a2ashape.TaskStateSubmitted {
		t.Fatalf("retry = %s %s (%v), want a new submitted task", retry.ID, retry.Status.State, err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("%d delegations delivered, want 1", n)
	}
	// The retry is the one a further copy finds.
	again, err := seam.Send(ctx, prov.AID(), send)
	if err != nil || again.ID != retry.ID {
		t.Fatalf("second retry = %s (%v), want %s", again.ID, err, retry.ID)
	}

	noCtx := module.TaskSend{Message: textMsg("no context", "", "m-nc"), ReturnImmediately: true}
	first, err := seam.Send(ctx, prov.AID(), noCtx)
	if err != nil {
		t.Fatal(err)
	}
	dup, err := seam.Send(ctx, prov.AID(), noCtx)
	if err != nil || dup.ID != first.ID || dup.ContextID != first.ContextID {
		t.Fatalf("retry without a context = %s/%s (%v), want %s/%s", dup.ID, dup.ContextID, err, first.ID, first.ContextID)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 2 {
		t.Fatalf("%d delegations delivered, want 2", n)
	}
	// The same message id sent to another agent is that agent's own task.
	other := newTestDaemon(t, srv.URL, true)
	if err := other.RegisterWithHub(ctx, srv.URL, "Other", nil, ""); err != nil {
		t.Fatal(err)
	}
	elsewhere, err := seam.Send(ctx, other.AID(), noCtx)
	if err != nil || elsewhere.ID == first.ID {
		t.Fatalf("same message id to another agent = %s (%v)", elsewhere.ID, err)
	}
}

// A follow-up sent while the hub is not carrying mail is recorded and
// queued, not refused: a retry of it finds it recorded, and it reaches the
// provider once the hub is back.
func TestAFollowUpIsQueuedWhileTheHubIsDown(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("plan", "", "m-1"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, task.ID, "where to?", nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	fake := fakeHubAt(t, srv.URL)
	fake.mu.Lock()
	fake.relayDown = true
	fake.mu.Unlock()
	follow := module.TaskSend{Message: followUp(task.ID, "Kyoto", "m-2"), ReturnImmediately: true}
	got, err := seam.Send(ctx, prov.AID(), follow)
	if err != nil || got.Status.State != a2ashape.TaskStateWorking {
		t.Fatalf("follow-up with the hub refusing = %s (%v), want working", got.Status.State, err)
	}
	stored := countMsgs(t, req, task.ID)
	if _, err := seam.Send(ctx, prov.AID(), follow); err != nil {
		t.Fatal(err)
	}
	if n := countMsgs(t, req, task.ID); n != stored {
		t.Fatalf("a retry stored the follow-up again (%d messages, was %d)", n, stored)
	}
	rows, err := req.ix.Outbox(task.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("outbox = %+v (%v), want the follow-up", rows, err)
	}
	fake.mu.Lock()
	fake.relayDown = false
	fake.mu.Unlock()
	if err := req.deliverQueued(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, _ := prov.ix.Messages(task.ID)
	if len(msgs) == 0 || msgs[len(msgs)-1].Body != "Kyoto" {
		t.Fatalf("provider's messages = %+v, want the follow-up last", msgs)
	}
}

// On a task delegated to this node, /tasks/wait waits for the requester:
// after the provider's question it returns when the requester answers, not
// at the provider's own input-required. A terminal task ends any wait.
func TestAProviderWaitsForTheRequestersAnswer(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	task, err := req.TaskSeam().Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("plan", "", "m-1"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// A new task is the provider's turn: nothing to wait for.
	if ix, timedOut, err := prov.waitTask(ctx, task.ID, 0, time.Second); err != nil || timedOut || ix.State != interactions.StateSubmitted {
		t.Fatalf("wait on a new inbound task = %+v %v %v", ix, timedOut, err)
	}
	// A reply given as a message speaks for the agent.
	asked, err := prov.replyTask(ctx, replyReq{TaskID: task.ID, Message: &a2ashape.Message{Role: a2ashape.RoleAgent,
		Parts: []a2ashape.Part{a2ashape.TextPart("where to?")}}})
	if err != nil {
		t.Fatal(err)
	}
	seq, _ := asked.Metadata[a2ashape.KeyStateSeq].(int64)
	if _, timedOut, _ := prov.waitTask(ctx, task.ID, seq, 50*time.Millisecond); !timedOut {
		t.Fatal("the provider's wait returned at its own question")
	}
	done := make(chan *interactions.Interaction, 1)
	go func() {
		ix, _, _ := prov.waitTask(ctx, task.ID, seq, 5*time.Second)
		done <- ix
	}()
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := req.TaskSeam().Send(ctx, prov.AID(), module.TaskSend{Message: followUp(task.ID, "Kyoto", "m-2"), ReturnImmediately: true}); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case ix := <-done:
		if ix == nil || ix.State != interactions.StateWorking || ix.StateSeq <= seq {
			t.Fatalf("the wait returned %+v", ix)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the provider's wait did not return when the requester answered")
	}
	// Terminal: returns at once, whatever after_seq says.
	if err := prov.CompleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ix, timedOut, err := prov.waitTask(ctx, task.ID, 1<<40, time.Minute)
	if err != nil || timedOut || ix.State != interactions.StateCompleted || time.Since(start) > time.Second {
		t.Fatalf("wait on a completed task = %+v %v %v", ix, timedOut, err)
	}
}

// A blocking wait whose context reaches its deadline answers the task as it
// is (the task runs on, §11.5); a canceled context is an error.
func TestAWaitAtTheCallersDeadlineAnswersTheTask(t *testing.T) {
	d := newTestDaemon(t, "", false)
	if err := d.ix.Create(interactions.New{ID: "ix_d", Role: interactions.RoleOutbound, PeerAID: "peer", Goal: "g", ContextID: "c"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	ix, timedOut, err := d.waitTask(ctx, "ix_d", 0, 0)
	if err != nil || !timedOut || ix == nil || ix.ID != "ix_d" {
		t.Fatalf("wait past the deadline = %+v %v %v", ix, timedOut, err)
	}
	gone, stop := context.WithCancel(context.Background())
	stop()
	if _, _, err := d.waitTask(gone, "ix_d", 0, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait with a canceled context: %v", err)
	}
}

// A status filter naming an A2A state no task here is ever in matches
// nothing rather than failing; statusTimestampAfter is "at or after", to the
// millisecond the store keeps.
func TestListTasksFilterEdges(t *testing.T) {
	d := newTestDaemon(t, "", false)
	seam := d.TaskSeam()
	ctx := context.Background()
	clock := int64(5_000)
	d.ix.SetClock(func() int64 { return clock })
	if err := d.ix.Create(interactions.New{ID: "ix_t", Role: interactions.RoleOutbound, PeerAID: "peer", Goal: "g", ContextID: "c"}); err != nil {
		t.Fatal(err)
	}
	page, err := seam.List(ctx, "peer", module.TaskFilter{State: string(a2ashape.TaskStateAuthRequired)})
	if err != nil || len(page.Tasks) != 0 || page.TotalSize != 0 {
		t.Fatalf("auth-required filter = %+v (%v), want an empty page", page, err)
	}
	for _, c := range []struct {
		after time.Time
		want  int
	}{
		{time.UnixMilli(5_000), 1},                             // equal: included
		{time.UnixMilli(4_999), 1},                             // before
		{time.UnixMilli(5_000).Add(500 * time.Microsecond), 0}, // after, within the millisecond
		{time.UnixMilli(5_001), 0},
	} {
		after := c.after
		page, err := seam.List(ctx, "peer", module.TaskFilter{UpdatedAfter: &after})
		if err != nil || len(page.Tasks) != c.want {
			t.Errorf("statusTimestampAfter %s: %d tasks (%v), want %d", after.Format(time.RFC3339Nano), len(page.Tasks), err, c.want)
		}
	}
	// A malformed agent id is refused before anything is written.
	for _, bad := range []string{"../x", "Peer", "did:anet:abc"} {
		if _, err := d.sendTask(ctx, controlScope, bad, module.TaskSend{Message: textMsg("hi", "", ""), ReturnImmediately: true}, 0); !errors.Is(err, a2ashape.ErrInvalidParams) {
			t.Errorf("send to %q: %v, want InvalidParams", bad, err)
		}
	}
	// The client speaks as the user.
	m := textMsg("hi", "", "")
	m.Role = a2ashape.RoleAgent
	if _, err := seam.Send(ctx, "peer", module.TaskSend{Message: m, ReturnImmediately: true}); !errors.Is(err, a2ashape.ErrInvalidParams) {
		t.Fatalf("a message in the agent's role: %v, want InvalidParams", err)
	}
}

// Two writers may publish their state events in the opposite order, or both
// publish the later state (publishState reads the row after its own
// commit). A watcher still sees state_seq strictly increase.
func TestStateEventsReachAWatcherInOrderAndOnce(t *testing.T) {
	d := newTestDaemon(t, "", false)
	if err := d.ix.Create(interactions.New{ID: "ix_o", Role: interactions.RoleOutbound, PeerAID: "peer", Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	snap, ch, cancel, err := d.Watch("ix_o")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	base := snap.StateSeq
	for _, seq := range []int64{base + 2, base + 1, base + 2, base + 3} {
		d.bus.publish(Event{IX: "ix_o", Kind: EventState, State: interactions.StateWorking, StateSeq: seq})
	}
	var got []int64
	for len(got) < 2 {
		select {
		case e := <-ch:
			got = append(got, e.StateSeq)
		case <-time.After(time.Second):
			t.Fatalf("events %v", got)
		}
	}
	select {
	case e := <-ch:
		t.Fatalf("extra event %+v after %v", e, got)
	default:
	}
	if got[0] != base+2 || got[1] != base+3 {
		t.Fatalf("state events %v, want [%d %d]", got, base+2, base+3)
	}
}

// Discovery admits a card only at or above the params.seq high water of the
// cards already admitted for that agent: an older card (a hub replaying
// one) and a second card under the same seq are refused (A2A-DESIGN §10.3).
func TestDiscoveryAppliesTheCardHighWater(t *testing.T) {
	self := "self-" + t.Name()
	card := func(seq uint64, body byte) *a2acard.Verified {
		return &a2acard.Verified{AID: "agent", Seq: seq, PayloadHash: [32]byte{body}}
	}
	if err := cardMarks.admit(self, card(5, 1)); err != nil {
		t.Fatalf("first card: %v", err)
	}
	if err := cardMarks.admit(self, card(5, 1)); err != nil {
		t.Fatalf("the same card again: %v", err)
	}
	if err := cardMarks.admit(self, card(4, 1)); !a2acard.IsCode(err, a2acard.CodeSeqRollback) {
		t.Fatalf("an older card: %v, want a rollback", err)
	}
	if err := cardMarks.admit(self, card(5, 2)); !a2acard.IsCode(err, a2acard.CodeSeqFork) {
		t.Fatalf("another card under the same seq: %v, want a fork", err)
	}
	if err := cardMarks.admit(self, card(6, 2)); err != nil {
		t.Fatalf("a newer card: %v", err)
	}
	if err := cardMarks.admit(self, card(5, 1)); err == nil {
		t.Fatal("the mark did not advance")
	}
	// Another node in the process keeps its own marks.
	if err := cardMarks.admit(self+"-other", card(4, 1)); err != nil {
		t.Fatalf("another node's first card: %v", err)
	}
}

// Discovery asks the hub by skill and tag only; the free text is matched
// here against the cards that come back and is never sent (A2A-DESIGN
// §10.5). The hub's word that a card verified is recorded, not taken: an
// unsigned card is UNVERIFIED, and neither returned nor searched (0017
// Q24). A verified card is returned as the bytes served.
func TestDiscoveryNeverSendsTheFreeText(t *testing.T) {
	kyoto := newCardAgent(t, "Kyoto guide", "trips", "plan")
	var mu sync.Mutex
	var asked []*url.URL
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL)
		mu.Unlock()
		w.Header().Set(hubapi.WireVersionHeader, strconv.Itoa(hubapi.WireVersion))
		switch r.URL.Path {
		case "/a2a/v1/agents":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"agents":[{"aid":%q,"card":%s,"cardVerification":"VERIFIED"},`+
				`{"aid":"agentb","card":{"name":"Kyoto tax helper","skills":[]},"cardVerification":"VERIFIED"}],"nextCursor":"c2"}`,
				kyoto.aid, kyoto.card)
		case "/a2a/v1/agents/" + kyoto.aid + "/card":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(kyoto.card)
		case "/a2a/v1/agents/agentb/card":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"name":"Kyoto tax helper","skills":[]}`)
		case "/agents/" + kyoto.aid + "/kel":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"aid":%q,"kel":%q}`, kyoto.aid, kyoto.kel)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	d := newTestDaemon(t, hub.URL, false)
	ctx := context.Background()
	agents, next, err := d.listAgents(ctx, module.AgentQuery{Skill: "plan", Query: "KYOTO"})
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].AID != kyoto.aid || next != "c2" {
		t.Fatalf("agents %+v next %q, want only the verified Kyoto guide", agents, next)
	}
	if a := agents[0]; a.Verification != cardVerified || a.HubVerification != "VERIFIED" {
		t.Fatalf("verification %q (hub %q)", a.Verification, a.HubVerification)
	}
	all, _, err := d.listAgents(ctx, module.AgentQuery{Skill: "plan"})
	if err != nil || len(all) != 2 || all[1].AID != "agentb" || all[1].Verification != cardUnverified ||
		all[1].HubVerification != "" || len(all[1].Card) != 0 {
		t.Fatalf("the unsigned card: %v %+v", err, all)
	}
	card, err := d.agentCard(ctx, kyoto.aid)
	if err != nil || string(card.Card) != string(kyoto.card) || card.Verification != cardVerified {
		t.Fatalf("card = %s %q (%v), want the bytes served, verified", card.Card, card.Verification, err)
	}
	if card, err := d.agentCard(ctx, "agentb"); err != nil || len(card.Card) != 0 || card.Verification != cardUnverified {
		t.Fatalf("unsigned card = %s %q (%v), want no bytes, unverified", card.Card, card.Verification, err)
	}
	if _, err := d.agentCard(ctx, "../agents"); !errors.Is(err, a2ashape.ErrInvalidParams) {
		t.Fatalf("a malformed agent id: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, u := range asked {
		if u.Query().Has("q") || strings.Contains(strings.ToLower(u.String()), "kyoto") {
			t.Fatalf("the free text reached the hub: %s", u)
		}
	}
	listed := false
	for _, u := range asked {
		listed = listed || (u.Path == "/a2a/v1/agents" && u.Query().Get("skill") == "plan")
	}
	if !listed {
		t.Fatalf("hub requests %v, want the registry asked with skill=plan", asked)
	}
}

// /tasks/list max_task_bytes holds each listed task to the bound (MCP
// list_tasks sets it): a peer's 3 MiB reply is listed as a notice of its
// size, not carried whole in every page that lists its task (docs/notes/0035:
// one such reply made every list_tasks page megabytes long). Without the
// bound the page is as it was.
func TestTaskListHoldsEachTaskToMaxTaskBytes(t *testing.T) {
	p := newPlane(t)
	d := p.d
	if err := d.ix.Create(interactions.New{ID: "ix_big", Role: interactions.RoleOutbound, PeerAID: "peer", Goal: "g",
		ContextID: "c-big"}); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("<", 3<<20)
	if _, err := d.ix.AddMessage("ix_big", "peer", interactions.MsgText, huge); err != nil {
		t.Fatal(err)
	}
	list := func(body string) (int, []byte) {
		resp, raw := p.req(t, "POST", "/tasks/list", body, p.bearer)
		return resp.StatusCode, raw
	}
	code, raw := list(`{"context_id":"c-big","history_length":1,"max_task_bytes":8192}`)
	var page struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &page); code != http.StatusOK || err != nil || len(page.Tasks) != 1 {
		t.Fatalf("list: %d %v %.200s", code, err, raw)
	}
	if n := len(page.Tasks[0]); n > 8192 {
		t.Fatalf("the listed task is %d bytes as JSON, over max_task_bytes 8192", n)
	}
	var task a2ashape.Task
	if err := json.Unmarshal(page.Tasks[0], &task); err != nil {
		t.Fatal(err)
	}
	if task.Metadata[a2ashape.KeyTruncated] != true || len(task.History) != 1 ||
		task.History[0].Metadata[a2ashape.KeyTruncated] != true {
		t.Fatalf("the cut is not marked: metadata %v history %+v", task.Metadata, task.History)
	}
	if code, raw := list(`{"context_id":"c-big","history_length":1}`); code != http.StatusOK || len(raw) < len(huge) {
		t.Fatalf("without max_task_bytes: %d, %d bytes", code, len(raw))
	}
	if code, _ := list(`{"max_task_bytes":-1}`); code != http.StatusBadRequest {
		t.Fatalf("max_task_bytes -1: %d, want 400", code)
	}
}

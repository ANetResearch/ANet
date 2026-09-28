package daemon

// 0017 Q30: a provider that ends a text task with a reply — /tasks/reply
// state=completed (MCP reply_task, the §11.6 backend), or an auto-reply
// that judges the task done — sends that reply and then the result as two
// envelopes. The requester used to enter input-required at the reply, and
// a blocking SendMessage returned there, without the answer or the receipt
// (a2a-tck DM-ART-001; Hermes read "needs more input"). The reply now
// carries anet.state=working with anet.final=true: the requester stays
// working until the result, and the reply is a conversation turn all the
// same (in the transcript the receipt covers, the history and anet.reply).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// replyArtifact is the text of a task's anet.reply artifact.
func replyArtifact(t a2ashape.Task) string {
	for _, a := range t.Artifacts {
		if a.ID != a2ashape.ArtifactReply {
			continue
		}
		for _, p := range a.Parts {
			if p.Kind == a2ashape.PartText {
				return p.Text
			}
		}
	}
	return ""
}

// lastOwnText is the metadata of the last text message d sent on id.
func lastOwnText(t *testing.T, d *Daemon, id string) map[string]any {
	t.Helper()
	msgs, err := d.ix.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].SenderAID == d.AID() && msgs[i].Kind == interactions.MsgText {
			return decodeMeta([]byte(msgs[i].Metadata))
		}
	}
	t.Fatalf("%s sent no text on %s", d.AID(), id)
	return nil
}

// The requester's side: a final reply keeps the task working, is shown as
// the status message and in the history, and the result that follows
// completes the task with it as the anet.reply artifact.
func TestAFinalReplyKeepsTheRequesterWorkingUntilTheResult(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "write a haiku", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov)

	const answer = "waves fold, salt remembers"
	if _, err := prov.sendMessage(ctx, id, answer, nil, a2ashape.FinalReplyMetadata()); err != nil {
		t.Fatal(err)
	}
	poll(t, req)
	ix := mustIX(t, req, id)
	if ix.State != interactions.StateWorking {
		t.Fatalf("at the final reply the requester's task is %s, want working (the result follows)", ix.State)
	}
	task, err := req.taskView(ix, viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	if m := task.Status.Message; m == nil || len(m.Parts) == 0 || m.Parts[0].Text != answer {
		t.Fatalf("status.message %+v", task.Status.Message)
	}
	if n := len(task.History); n == 0 || task.History[n-1].Parts[0].Text != answer {
		t.Fatalf("the final reply is not in the history: %+v", task.History)
	}

	if err := prov.CompleteTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	poll(t, req)
	ix = mustIX(t, req, id)
	task, err = req.taskView(ix, viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	if ix.State != interactions.StateCompleted || replyArtifact(task) != answer ||
		task.Metadata[a2ashape.KeyReceiptVerified] != a2ashape.ReceiptVerified {
		t.Fatalf("after the result: state %s, reply %q, receipt %v", ix.State, replyArtifact(task),
			task.Metadata[a2ashape.KeyReceiptVerified])
	}
}

// /tasks/reply state=completed sends the reply as a final reply, and a
// requester's blocking SendMessage returns the completed task with the
// answer, not the input-required moment between reply and result.
func TestReplyCompletedDoesNotEndABlockingSendEarly(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()

	type sent struct {
		task a2ashape.Task
		err  error
	}
	done := make(chan sent, 1)
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	go func() {
		t, err := seam.Send(sctx, prov.AID(), module.TaskSend{Message: textMsg("write a haiku", "ctx-q30", "m-q30")})
		done <- sent{t, err}
	}()

	var id string
	waitUntil(t, "the provider has the task", func() bool {
		_ = prov.pollOnce(ctx)
		items, err := prov.ix.ListAll(interactions.ListFilter{Role: interactions.RoleInbound})
		if err != nil || len(items) == 0 {
			return false
		}
		id = items[0].ID
		return true
	})
	const answer = "waves fold, salt remembers"
	if _, err := prov.replyTask(ctx, replyReq{TaskID: id, Text: answer, State: "completed"}); err != nil {
		t.Fatal(err)
	}
	if meta := lastOwnText(t, prov, id); !a2ashape.FinalReply(meta) {
		t.Fatalf("the completing reply's metadata %v, want a final reply (anet.state=working, anet.final)", meta)
	}

	var got sent
	waitUntil(t, "the blocking send returns", func() bool {
		_ = req.pollOnce(ctx)
		select {
		case got = <-done:
			return true
		default:
			return false
		}
	})
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.task.Status.State != a2ashape.TaskStateCompleted || replyArtifact(got.task) != answer {
		b, _ := json.Marshal(got.task.Status)
		t.Fatalf("the blocking send returned %s (reply %q); want completed with the answer", b, replyArtifact(got.task))
	}
}

// An auto-reply that judges the task done sends its reply as a final
// reply before it completes the task.
func TestAnAutoReplyThatCompletesSendsAFinalReply(t *testing.T) {
	api := &fakeOpenAI{reply: "here is your function: def f(): pass\n" + autoReplyDoneSentinel}
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api)
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "write f()", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if meta := lastOwnText(t, f.prov, id); !a2ashape.FinalReply(meta) {
		t.Fatalf("the completing auto-reply's metadata %v, want a final reply", meta)
	}
	if err := f.req.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	task, err := f.req.taskView(mustIX(t, f.req, id), viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != a2ashape.TaskStateCompleted || replyArtifact(task) != "here is your function: def f(): pass" {
		t.Fatalf("state %s, reply %q", task.Status.State, replyArtifact(task))
	}

	// A reply that does not complete the task is an ordinary turn.
	api2 := &fakeOpenAI{reply: "which language?"}
	g := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api2)
	id2, err := g.req.Delegate(g.ctx, g.prov.AID(), "write f()", nil)
	if err != nil {
		t.Fatal(err)
	}
	g.tick(t)
	if meta := lastOwnText(t, g.prov, id2); len(meta) != 0 {
		t.Fatalf("a question carries %v", meta)
	}
}

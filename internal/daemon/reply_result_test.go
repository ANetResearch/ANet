package daemon

// correctness-ux §4.3.3: a provider that completes a text task with a reply
// folds it into the result (completeWithReply, one transaction and one
// message); the requester checks it against the transcript the receipt
// covers (CUX-R2), checks the transcript's account of what it said (CUX-R6
// item 4), stores the reply with the result in every branch, and fails the
// task before it refuses a result (CUX-R7).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/transcript"
	"github.com/ANetResearch/ANet/module"
)

// pollEach collects each daemon's mailbox once (poll, which is in a file
// -tags no_x402 leaves out).
func pollEach(t *testing.T, ds ...*Daemon) {
	t.Helper()
	for _, d := range ds {
		if err := d.pollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

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

// sealFromAck is sealFrom that also returns the envelope's ack_key, to read
// the delivery token the receiver answers with (tokenOf).
func sealFromAck(t testing.TB, from, to *Daemon, typ, ix string, body []byte) ([]byte, seal.AckKey) {
	t.Helper()
	now := from.nowMS()
	sv, err := from.sealMessage(sealMsg{to: to.AID(), typ: typ, ix: ix, body: body, mid: seal.NewMID(), ts: now,
		exp: now + messageLifetimeMS}, keySetOf(t, to), seal.SealOpts{}, now)
	if err != nil {
		t.Fatal(err)
	}
	env, err := seal.SetAccess(sv.env, &seal.Access{Kind: seal.AccessKindSession})
	if err != nil {
		t.Fatal(err)
	}
	return env, sv.ackKey
}

// delegated is a text task req delegated to prov, which prov holds.
func delegated(t *testing.T, req, prov *Daemon, goal string) string {
	t.Helper()
	id, err := req.Delegate(context.Background(), prov.AID(), goal, nil)
	if err != nil {
		t.Fatal(err)
	}
	pollEach(t, prov)
	mustIX(t, prov, id)
	return id
}

// providerResult is the body of a done result prov signs for task id over
// a transcript of entries, carrying reply (nil for none): what a provider
// that says whatever it likes sends, under a receipt that verifies.
func providerResult(t *testing.T, prov *Daemon, id string, entries []transcript.Message, reply *delegation.ReplyMsg) []byte {
	t.Helper()
	ix := mustIX(t, prov, id)
	tr, err := encodeTranscript(ix, entries)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := prov.signResult(ix, tr)
	if err != nil {
		t.Fatal(err)
	}
	body, err := (&delegation.ResultResp{Status: delegation.StatusDone, Deliverable: signed.transcript,
		Receipt: signed.receipt, Metadata: completedMeta(), Reply: reply}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// signedOver is prov's receipt over a transcript of entries for ix.
func signedOver(t *testing.T, prov *Daemon, ix *interactions.Interaction, entries []transcript.Message) signedResult {
	t.Helper()
	tr, err := encodeTranscript(ix, entries)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := prov.signResult(ix, tr)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func textReply(body, msgID string) *delegation.ReplyMsg {
	return &delegation.ReplyMsg{Kind: delegation.ChatText, Body: body, MsgID: msgID}
}

// msgWithBody is the stored message of id whose body is body, or nil.
func msgWithBody(t *testing.T, d *Daemon, id, body string) *interactions.Message {
	t.Helper()
	msgs, err := d.ix.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	for i := range msgs {
		if msgs[i].Body == body {
			return &msgs[i]
		}
	}
	return nil
}

// ledgerHas reports whether d's evidence chain holds an event of kind about
// interaction id.
func ledgerHas(t *testing.T, d *Daemon, kind, id string) bool {
	t.Helper()
	b, err := os.ReadFile(d.layout.EvidenceLedgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rec, err := decodeRecord(line)
		if err != nil {
			t.Fatal(err)
		}
		if rec.EventType == kind && plainMap(rec.Payload)["interaction_id"] == id {
			return true
		}
	}
	return false
}

// faultDDL is a trigger that aborts the transaction of every write matching
// on, as a crash there would leave it: nothing of it commits (storeFault).
func faultDDL(name, on string) string {
	return `CREATE TRIGGER ` + name + ` BEFORE ` + on + ` BEGIN SELECT RAISE(ABORT, 'injected: ` + name + `'); END`
}

// TestCompleteWithReplyOneTx is CUX-R4 and M11: the reply, the result with
// its receipt and the completed state, and the queued result commit
// together. A crash at any of the writes — the reply row, its file, the
// queued result (after the transcript and the receipt are made), the
// terminal write — leaves none of them; with no fault all are there, and
// the requester gets the completion and the reply in one result.
func TestCompleteWithReplyOneTx(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id := delegated(t, req, prov, "draw me a sheep")
	const answer = "here is your sheep"
	file := delegation.Attachment{Name: "sheep.txt", Mime: "text/plain", Size: 5, Data: []byte("baaaa")}
	cid, err := anetcid.SumRaw(file.Data)
	if err != nil {
		t.Fatal(err)
	}
	file.CID = cid

	for _, f := range []struct{ name, on string }{
		{"reply_row", `INSERT ON message`},
		{"reply_file", `INSERT ON attachment`},
		{"queued_result", `INSERT ON outbox WHEN NEW.type = '` + seal.TypeResult + `'`},
		{"terminal_write", `UPDATE ON interaction WHEN NEW.state = 'completed'`},
	} {
		remove := storeFault(t, prov, "fault_"+f.name, faultDDL("fault_"+f.name, f.on))
		if err := prov.completeWithReply(ctx, id, answer, []delegation.Attachment{file}); err == nil {
			t.Fatalf("%s: completeWithReply succeeded through the fault", f.name)
		}
		remove()
		ix := mustIX(t, prov, id)
		atts, err := prov.ix.Attachments(id)
		if err != nil {
			t.Fatal(err)
		}
		if msgWithBody(t, prov, id, answer) != nil || len(atts) != 0 || ix.IsTerminal() || len(ix.Receipt) != 0 ||
			len(resultRows(t, prov, id)) != 0 {
			t.Fatalf("a crash at %s left part of the completion: reply %v, files %d, state %s, receipt %d, results queued %d",
				f.name, msgWithBody(t, prov, id, answer) != nil, len(atts), ix.State, len(ix.Receipt), len(resultRows(t, prov, id)))
		}
	}

	if err := prov.completeWithReply(ctx, id, answer, []delegation.Attachment{file}); err != nil {
		t.Fatal(err)
	}
	ix := mustIX(t, prov, id)
	if m := msgWithBody(t, prov, id, answer); m == nil || m.SenderAID != prov.AID() || m.Metadata != "" ||
		ix.State != interactions.StateCompleted || len(ix.Receipt) == 0 {
		t.Fatalf("after the completion: reply %+v, state %s, receipt %d", m, ix.State, len(ix.Receipt))
	}
	tr, err := transcript.Parse(ix.Result)
	if err != nil {
		t.Fatal(err)
	}
	last := tr.Messages[len(tr.Messages)-1]
	if last.From != transcriptProvider || last.Body != answer || len(last.Attachments) != 1 || last.Attachments[0].CID != file.CID {
		t.Fatalf("the transcript does not end with the reply: %+v", tr.Messages)
	}

	pollEach(t, req)
	rix := mustIX(t, req, id)
	task, err := req.taskView(rix, viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	if rix.State != interactions.StateCompleted || replyArtifact(task) != answer ||
		task.Metadata[a2ashape.KeyReceiptVerified] != a2ashape.ReceiptVerified {
		t.Fatalf("the requester: state %s, reply %q, receipt %v", rix.State, replyArtifact(task), task.Metadata[a2ashape.KeyReceiptVerified])
	}
	if m := msgWithBody(t, req, id, answer); m == nil || m.SenderAID != prov.AID() {
		t.Fatalf("the requester did not store the reply as the provider's message: %+v", m)
	}
	ratts, err := req.ix.Attachments(id)
	if err != nil || len(ratts) != 1 || ratts[0].CID != file.CID {
		t.Fatalf("the reply's file at the requester: %+v %v", ratts, err)
	}
	if !ledgerHas(t, req, EvResultAccepted, id) {
		t.Error("an accepted result is not on the requester's chain (C5)")
	}
}

// R-CUX4 (an internal note item scenario): /tasks/reply state=completed is one result,
// and a requester's blocking SendMessage returns completed with the answer.
func TestReplyCompletedEndsABlockingSendWithTheAnswer(t *testing.T) {
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
		t, err := seam.Send(sctx, prov.AID(), module.TaskSend{Message: textMsg("write a haiku", "ctx-r4", "m-r4")})
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
	rows, err := prov.ix.OutboxRows(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Type == seal.TypeMessage {
			t.Fatalf("the reply went as a message of its own beside the result: %+v", r)
		}
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
	noHistoryState(t, got.task)
}

// noHistoryState is M30: no message of the history carries anet.state —
// the reply that completes a task is a conversation turn, never marked.
func noHistoryState(t *testing.T, task a2ashape.Task) {
	t.Helper()
	for _, m := range task.History {
		if _, ok := m.Metadata[a2ashape.KeyState]; ok {
			t.Fatalf("a history message carries anet.state: %+v", m)
		}
		if _, ok := m.Metadata["anet."+"final"]; ok {
			t.Fatalf("a history message carries the retired final mark: %+v", m)
		}
	}
}

// M30 on both sides: after a completion with a reply, neither the
// provider's nor the requester's history has a message with anet.state, and
// the reply is the history's last message.
func TestNoHistoryMessageCarriesAState(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id := delegated(t, req, prov, "count to three")
	if _, err := prov.replyTask(ctx, replyReq{TaskID: id, Text: "one, two, three", State: "completed"}); err != nil {
		t.Fatal(err)
	}
	pollEach(t, req)
	for _, d := range []*Daemon{prov, req} {
		task, err := d.taskView(mustIX(t, d, id), viewOpts{artifacts: true})
		if err != nil {
			t.Fatal(err)
		}
		noHistoryState(t, task)
		if n := len(task.History); n == 0 || task.History[n-1].Parts[0].Text != "one, two, three" {
			t.Fatalf("the reply is not the last of the history: %+v", task.History)
		}
		if task.Status.State != a2ashape.TaskStateCompleted {
			t.Fatalf("state %s", task.Status.State)
		}
	}
}

// An auto-reply that judges the task done completes it with its reply in
// the result; a reply that does not complete the task is an ordinary turn.
func TestAnAutoReplyThatCompletesFoldsItsReplyIntoTheResult(t *testing.T) {
	api := &fakeOpenAI{reply: "here is your function: def f(): pass\n" + autoReplyDoneSentinel}
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api)
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "write f()", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	pix := mustIX(t, f.prov, id)
	if pix.State != interactions.StateCompleted {
		t.Fatalf("the provider's task is %s after the done reply", pix.State)
	}
	rows, err := f.prov.ix.OutboxRows(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Type == seal.TypeMessage {
			t.Fatalf("the done reply went as a message of its own: %+v", r)
		}
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
	noHistoryState(t, task)

	api2 := &fakeOpenAI{reply: "which language?"}
	g := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api2)
	id2, err := g.req.Delegate(g.ctx, g.prov.AID(), "write f()", nil)
	if err != nil {
		t.Fatal(err)
	}
	g.tick(t)
	if ix := mustIX(t, g.prov, id2); ix.IsTerminal() {
		t.Fatalf("a question completed the task: %s", ix.State)
	}
	if m := msgWithBody(t, g.prov, id2, "which language?"); m == nil || m.Metadata != "" {
		t.Fatalf("the question: %+v", m)
	}
}

// TestReplyMismatchFailsTaskBeforeToken is CUX-R2, R7, M12 and M39: a
// result whose reply is not the last provider entry of its transcript is
// refused — tok(2) invalid to the provider — and the requester's task is
// failed with result_refused first; nothing of the result is stored. When
// that terminal write cannot be stored, no token is made and the envelope
// is not acknowledged; its redelivery is refused once the store recovers.
func TestReplyMismatchFailsTaskBeforeToken(t *testing.T) {
	_, req, prov := registeredPair(t)
	id := delegated(t, req, prov, "what is two plus two")
	entries := []transcript.Message{{From: transcriptRequester, Body: "what is two plus two"}, {From: transcriptProvider, Body: "four"}}
	body := providerResult(t, prov, id, entries, textReply("five", "m-forged"))

	env, ack := sealFromAck(t, prov, req, seal.TypeResult, id, body)
	remove := storeFault(t, req, "fault_refused_fail", faultDDL("fault_refused_fail", `UPDATE ON interaction WHEN NEW.state = 'failed'`))
	if r := receive(t, req, env); r.class != rxTransient || r.dack != nil {
		t.Fatalf("a refusal whose terminal write failed: %+v, want not acknowledged and no token", r)
	}
	if ix := mustIX(t, req, id); ix.IsTerminal() {
		t.Fatalf("the task is %s", ix.State)
	}
	remove()

	r := receive(t, req, env)
	if r.class != rxDropped || r.reason != dropReplyMismatch {
		t.Fatalf("a reply that is not the transcript's: %+v, want %s", r, dropReplyMismatch)
	}
	wantToken(t, r, ack, seal.OutcomeRefused, seal.NoteInvalid)
	ix := mustIX(t, req, id)
	task, err := req.taskView(ix, viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	if ix.State != interactions.StateFailed || task.Metadata[a2ashape.KeyReason] != reasonResultRefused ||
		len(ix.Receipt) != 0 || ix.ResultCID != "" || msgWithBody(t, req, id, "five") != nil || msgWithBody(t, req, id, "four") != nil {
		t.Fatalf("after the refusal: state %s, reason %v, receipt %d, result cid %q", ix.State,
			task.Metadata[a2ashape.KeyReason], len(ix.Receipt), ix.ResultCID)
	}
	if ledgerHas(t, req, EvResultAccepted, id) {
		t.Error("a refused result is on the chain")
	}

	// The same result with the reply it should carry, after the refusal:
	// the task stays as the refusal left it.
	good := providerResult(t, prov, id, entries, textReply("four", "m-good"))
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeResult, id, good)); r.class == rxDropped {
		t.Logf("a result after the refusal: %+v", r)
	}
	if ix := mustIX(t, req, id); ix.State != interactions.StateFailed {
		t.Fatalf("a refused task changed to %s", ix.State)
	}
}

// M39 for result-refused: a result whose receipt does not verify fails the
// requester's task with result_refused (a capability task UNVERIFIED), and
// the provider is answered tok(2) invalid.
func TestARefusedReceiptFailsTheTask(t *testing.T) {
	_, req, prov := registeredPair(t)
	id := delegated(t, req, prov, "summarise this")
	ix := mustIX(t, prov, id)
	// A receipt over one transcript, delivered with another.
	signed := signedOver(t, prov, ix, []transcript.Message{{From: transcriptRequester, Body: "summarise this"},
		{From: transcriptProvider, Body: "the summary"}})
	other, err := encodeTranscript(ix, []transcript.Message{{From: transcriptRequester, Body: "summarise this"},
		{From: transcriptProvider, Body: "another summary"}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := (&delegation.ResultResp{Status: delegation.StatusDone, Deliverable: other, Receipt: signed.receipt}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	env, ack := sealFromAck(t, prov, req, seal.TypeResult, id, body)
	r := receive(t, req, env)
	if r.reason != dropResultRefused {
		t.Fatalf("%+v, want %s", r, dropResultRefused)
	}
	wantToken(t, r, ack, seal.OutcomeRefused, seal.NoteInvalid)
	got := mustIX(t, req, id)
	task, err := req.taskView(got, viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != interactions.StateFailed || task.Metadata[a2ashape.KeyReason] != reasonResultRefused || len(got.Receipt) != 0 {
		t.Fatalf("state %s, reason %v, receipt %d", got.State, task.Metadata[a2ashape.KeyReason], len(got.Receipt))
	}
}

// A capability task's refused result says its effect is UNVERIFIED (CUX-R7),
// and a capability result carrying a reply is bad-body (CUX-R1).
func TestARefusedCapabilityResultIsUnverified(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "LinkBox", nil); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A receipt over other bytes than the deliverable.
	stand := &interactions.Interaction{ID: id, PeerAID: req.AID(), RequestCID: mustIX(t, req, id).RequestCID,
		RequestDoc: mustIX(t, req, id).RequestDoc}
	signed, err := prov.signResult(stand, []byte(`{"status":"FAILED"}`))
	if err != nil {
		t.Fatal(err)
	}
	receipt := signed.receipt
	withReply, err := (&delegation.ResultResp{Status: delegation.StatusDone, Deliverable: []byte(`{"status":"OK"}`),
		Receipt: receipt, Reply: textReply("ok", "m-cap")}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeResult, id, withReply)); r.reason != dropBadBody {
		t.Fatalf("a capability result with a reply: %+v, want %s", r, dropBadBody)
	}
	if ix := mustIX(t, req, id); ix.IsTerminal() {
		t.Fatalf("bad-body ended the task: %s", ix.State)
	}
	payload, err := (&delegation.ResultResp{Status: delegation.StatusDone, Deliverable: []byte(`{"status":"OK"}`),
		Receipt: receipt}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeResult, id, payload)); r.reason != dropResultRefused {
		t.Fatalf("%+v", r)
	}
	ix := mustIX(t, req, id)
	task, err := req.taskView(ix, viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	if ix.State != interactions.StateFailed || task.Metadata[a2ashape.KeyReason] != reasonResultRefused ||
		task.Metadata[a2ashape.KeyEffectStatus] != "UNVERIFIED" {
		t.Fatalf("state %s, metadata %v", ix.State, task.Metadata)
	}
}

// TestLateAndAlreadyResultWriteReply is CUX-R6 item 3: the reply is stored
// with the result after the task ended here (late) and when the result was
// stored already (already), once — never a second row for one reply.
func TestLateAndAlreadyResultWriteReply(t *testing.T) {
	ctx := context.Background()

	// Late: the requester canceled before the result came.
	_, req, prov := registeredPair(t)
	id := delegated(t, req, prov, "late one")
	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := prov.completeWithReply(ctx, id, "too late, but here", nil); err != nil {
		t.Fatal(err)
	}
	pollEach(t, req)
	ix := mustIX(t, req, id)
	if ix.State != interactions.StateCanceled || len(ix.Receipt) == 0 {
		t.Fatalf("late: state %s, receipt %d", ix.State, len(ix.Receipt))
	}
	if m := msgWithBody(t, req, id, "too late, but here"); m == nil || m.SenderAID != prov.AID() {
		t.Fatalf("late: the reply is not stored: %+v", m)
	}

	// Already: a copy of the result without its reply was stored first.
	_, req2, prov2 := registeredPair(t)
	id2 := delegated(t, req2, prov2, "already one")
	entries := []transcript.Message{{From: transcriptRequester, Body: "already one"}, {From: transcriptProvider, Body: "done it"}}
	bare := providerResult(t, prov2, id2, entries, nil)
	if r := receive(t, req2, sealFrom(t, prov2, req2, seal.TypeResult, id2, bare)); r.class != rxAccepted {
		t.Fatalf("the bare result: %+v", r)
	}
	if mustIX(t, req2, id2).State != interactions.StateCompleted || msgWithBody(t, req2, id2, "done it") != nil {
		t.Fatal("the bare result")
	}
	n := countMsgs(t, req2, id2)
	full := providerResult(t, prov2, id2, entries, textReply("done it", "m-already"))
	for i := 0; i < 2; i++ {
		if r := receive(t, req2, sealFrom(t, prov2, req2, seal.TypeResult, id2, full)); r.class != rxAccepted {
			t.Fatalf("copy %d with the reply: %+v", i, r)
		}
	}
	if m := msgWithBody(t, req2, id2, "done it"); m == nil || m.MsgID != "m-already" || countMsgs(t, req2, id2) != n+1 {
		t.Fatalf("already: reply %+v, %d messages (want %d)", m, countMsgs(t, req2, id2), n+1)
	}

	// A copy that rides on another transcript than the stored one is not
	// the stored result: its reply was never checked against the receipt
	// this node holds, and is not stored.
	forgedEntries := []transcript.Message{{From: transcriptRequester, Body: "already one"}, {From: transcriptProvider, Body: "forged late word"}}
	forged := providerResult(t, prov2, id2, forgedEntries, textReply("forged late word", "m-forged"))
	if r := receive(t, req2, sealFrom(t, prov2, req2, seal.TypeResult, id2, forged)); r.class == rxAccepted {
		t.Fatalf("a copy on another transcript: %+v", r)
	}
	if msgWithBody(t, req2, id2, "forged late word") != nil || mustIX(t, req2, id2).State != interactions.StateCompleted {
		t.Fatal("a reply on another transcript was stored")
	}

	// A reply whose message id names another message is not that entry.
	_, req3, prov3 := registeredPair(t)
	id3 := delegated(t, req3, prov3, "collide")
	goal := msgWithBody(t, req3, id3, "collide")
	entries3 := []transcript.Message{{From: transcriptRequester, Body: "collide"}, {From: transcriptProvider, Body: "ok"}}
	if r := receive(t, req3, sealFrom(t, prov3, req3, seal.TypeResult, id3,
		providerResult(t, prov3, id3, entries3, textReply("ok", goal.MsgID)))); r.reason != dropReplyMismatch {
		t.Fatalf("a reply under the goal's message id: %+v", r)
	}
	if ix := mustIX(t, req3, id3); ix.State != interactions.StateFailed {
		t.Fatalf("collide: %s", ix.State)
	}
}

// M13 and CUX-R6 item 4: a transcript that changes what the requester said
// is stored as receipt_verified=mismatch, with no C5 and no review; one
// that only leaves a requester message out is verified.
func TestRequesterEntryMismatch(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id := delegated(t, req, prov, "translate: good morning")
	entries := []transcript.Message{{From: transcriptRequester, Body: "translate: good night"}, {From: transcriptProvider, Body: "bonne nuit"}}
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeResult, id, providerResult(t, prov, id, entries,
		textReply("bonne nuit", "m-r")))); r.class != rxAccepted {
		t.Fatalf("%+v", r)
	}
	ix := mustIX(t, req, id)
	task, err := req.taskView(ix, viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	if ix.State != interactions.StateCompleted || task.Metadata[a2ashape.KeyReceiptVerified] != a2ashape.ReceiptMismatch ||
		replyArtifact(task) != "bonne nuit" {
		t.Fatalf("state %s, receipt %v, reply %q", ix.State, task.Metadata[a2ashape.KeyReceiptVerified], replyArtifact(task))
	}
	if ledgerHas(t, req, EvResultAccepted, id) {
		t.Error("a result whose transcript changes the requester's words is on the chain (C5)")
	}
	_, err = req.SubmitReview(id, 5, "great")
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) || ref.Code() != reasonReceiptMismatch {
		t.Fatalf("a review of it: %v, want %s", err, reasonReceiptMismatch)
	}
	if ix := mustIX(t, req, id); len(ix.Review) != 0 {
		t.Fatal("a review was stored")
	}
	_ = ctx

	// Pure deletion is not a mismatch.
	_, req2, prov2 := registeredPair(t)
	id2 := delegated(t, req2, prov2, "translate: thank you")
	only := []transcript.Message{{From: transcriptProvider, Body: "merci"}}
	if r := receive(t, req2, sealFrom(t, prov2, req2, seal.TypeResult, id2, providerResult(t, prov2, id2, only,
		textReply("merci", "m-d")))); r.class != rxAccepted {
		t.Fatalf("%+v", r)
	}
	if v := mustIX(t, req2, id2).ReceiptVerified; v != interactions.VerificationVerified {
		t.Fatalf("a transcript that leaves the requester's words out: %s, want verified", v)
	}
	if !ledgerHas(t, req2, EvResultAccepted, id2) {
		t.Error("a verified result is not on the chain")
	}
}

// UX-T1 for the codes of this area: result_refused on the control plane
// and the local A2A interface (CLI and MCP render the control plane's) is
// the reason table's sentence under its code; receipt_mismatch is the
// control plane's refusal of a review, in the same words.
func TestResultRefusedAndReceiptMismatchSayOneSentence(t *testing.T) {
	_, req, prov := registeredPair(t)
	id := delegated(t, req, prov, "two plus two")
	entries := []transcript.Message{{From: transcriptRequester, Body: "two plus two"}, {From: transcriptProvider, Body: "four"}}
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeResult, id,
		providerResult(t, prov, id, entries, textReply("five", "m-x")))); r.reason != dropReplyMismatch {
		t.Fatalf("%+v", r)
	}
	want := a2ashape.Sentence(reasonResultRefused, a2ashape.Lang(), nil)
	for _, s := range taskSurfaces(t, req, prov.AID(), id) {
		if s.text != want || s.reason != reasonResultRefused {
			t.Errorf("%s: %q under %q, want %q under %q", s.name, s.text, s.reason, want, reasonResultRefused)
		}
	}

	_, req2, prov2 := registeredPair(t)
	id2 := delegated(t, req2, prov2, "hello")
	changed := []transcript.Message{{From: transcriptRequester, Body: "goodbye"}, {From: transcriptProvider, Body: "hi"}}
	if r := receive(t, req2, sealFrom(t, prov2, req2, seal.TypeResult, id2,
		providerResult(t, prov2, id2, changed, textReply("hi", "m-y")))); r.class != rxAccepted {
		t.Fatalf("%+v", r)
	}
	code, out := controlCall(t, req2.hReview, "/review", `{"interaction_id":"`+id2+`","rating":5}`)
	sentence := a2ashape.Sentence(reasonReceiptMismatch, a2ashape.Lang(), nil)
	if code != http.StatusConflict || out["error"] != sentence || out["reason"] != reasonReceiptMismatch {
		t.Fatalf("/review: %d %v, want 409 %q under %q", code, out, sentence, reasonReceiptMismatch)
	}
	for _, s := range taskSurfaces(t, req2, prov2.AID(), id2) {
		_ = s
	}
	ct, err := (&DaemonTaskSeam{d: req2}).Get(context.Background(), prov2.AID(), id2, nil)
	if err != nil || ct.Metadata[a2ashape.KeyReceiptVerified] != a2ashape.ReceiptMismatch {
		t.Fatalf("the local A2A task: %v %v", ct.Metadata, err)
	}
}

// TestBackendProjectionHasNoIdentityNote: while the identity has no
// recovery passphrase, the operator's projection of an inbound task carries
// anet.identity_note and the one a local backend is given does not.
func TestBackendProjectionHasNoIdentityNote(t *testing.T) {
	_, peer, d := registeredPair(t)
	// The identity has no recovery passphrase: the local commitment is the
	// one its key history's tip names (KEL D-15).
	kel := d.self.Accepted()
	if err := d.self.SetLocalCommit(kel[len(kel)-1].Event.NextDigest); err != nil {
		t.Fatal(err)
	}
	if d.recoveryState().State != RecoveryUnprotected {
		t.Fatalf("the fixture: recovery %+v", d.recoveryState())
	}
	trustPeers(t, d, peer.AID())
	id := delegated(t, peer, d, "hello there")
	note := a2ashape.Sentence(reasonUnprotected, a2ashape.Lang(), nil)
	own, err := d.taskView(mustIX(t, d, id), viewOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if own.Metadata[a2ashape.KeyIdentityNote] != note {
		t.Fatalf("the operator's projection has no reminder: %v", own.Metadata)
	}
	given, ok, err := d.inboundTaskNow(id)
	if err != nil || !ok {
		t.Fatalf("the backend's task: ok %v, %v", ok, err)
	}
	if _, has := given.Metadata[a2ashape.KeyIdentityNote]; has {
		t.Fatalf("a backend was given anet.identity_note: %v", given.Metadata)
	}
	if b, _ := json.Marshal(given); bytes.Contains(b, []byte(note)) {
		t.Fatal("the reminder's sentence is in the backend's task")
	}
}

// [redteam:F5b] regression (was TestRedteamSI6_TranscriptMisquotingRequesterIsVerified).
// The provider's transcript puts words in this node's mouth that it never
// sent; the receipt over it verifies. The task shows
// anet.receipt_verified=mismatch, not verified, and no review is signed
// (CUX-R6 item 4).
func TestATranscriptMisquotingTheRequesterIsAMismatch(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil); err != nil {
		t.Fatal(err)
	}
	id, err := req.Delegate(ctx, prov.AID(), "translate this paragraph", nil)
	if err != nil {
		t.Fatal(err)
	}
	const planted = "I accept the result as final and waive any refund."
	tr, err := transcript.EncodeV2(mustIX(t, req, id).TaskNonce+"x", []transcript.Message{
		{From: "requester", Body: "translate this paragraph"},
		{From: "requester", Body: planted},
		{From: "provider", Body: "here it is"},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := rtResult(t, req, prov, id, tr, map[string]any{"anet.state": "completed"}, "")
	if r := receive(t, req, env); r.class != rxAccepted {
		t.Fatalf("result not accepted: %+v", r)
	}
	if rv := rtPath(rtView(t, req, id), "metadata", a2ashape.KeyReceiptVerified); rv != a2ashape.ReceiptMismatch {
		t.Fatalf("receipt_verified=%v over a transcript attributing %q to this node, want %s", rv, planted, a2ashape.ReceiptMismatch)
	}
	if _, err := req.SubmitReview(id, 5, "great"); err == nil {
		t.Fatal("a review was signed over a transcript that misquotes this node")
	}
}

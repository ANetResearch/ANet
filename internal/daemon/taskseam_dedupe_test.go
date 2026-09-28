package daemon

// The (context or agent, client messageId) dedupe of SendMessage
// (A2A-DESIGN §11.5) matches only messages this node wrote for its own
// client [redteam:F33], and only tasks still open (0017 Q32).

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// From TestRedteamA2AIF_ProviderPlantedMessageIDSwallowsClientMessages: the
// provider sees the client's message ids (a2a.messageId rides in the
// delegation) and plants the ones it predicts next in its own messages.
// The client's follow-up under a planted id is recorded and delivered, and
// a new request under one without a contextId makes a new task; the
// projection does not show the provider's messages under the planted ids.
func TestAProviderCannotPlantTheClientsMessageIDs(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("hello", "ctx-dedupe", "msg-1"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the provider has the task", func() bool {
		_ = prov.pollOnce(ctx)
		_, err := prov.ix.Get(task.ID)
		return err == nil
	})
	if _, err := prov.sendMessage(ctx, task.ID, "question?", nil,
		map[string]any{a2ashape.KeyMessageID: "msg-2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.sendMessage(ctx, task.ID, "another", nil,
		map[string]any{a2ashape.KeyMessageID: "msg-3"}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the requester stored the provider's messages", func() bool {
		_ = req.pollOnce(ctx)
		return countMsgs(t, req, task.ID) >= 3
	})
	// However the provider's key got into a stored row, it is not the
	// client's.
	if _, _, err := req.ix.AddMessageRecord(interactions.MessageRecord{InteractionID: task.ID, SenderAID: prov.AID(),
		Kind: interactions.MsgText, Body: "legacy", Metadata: []byte(`{"a2a.messageId":"msg-4"}`)}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"msg-2", "msg-3", "msg-4"} {
		if _, err := req.ix.FindByClientMessage(interactions.ClientMessageQuery{Role: interactions.RoleOutbound,
			PeerAID: prov.AID(), ClientMsgID: id}); err == nil {
			t.Fatalf("the provider's %s matched the client's dedupe", id)
		}
	}
	got, err := seam.Get(ctx, prov.AID(), task.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range got.History {
		if h.Role == a2ashape.RoleAgent && (h.ID == "msg-2" || h.ID == "msg-3" || h.ID == "msg-4") {
			t.Fatalf("the projection shows the provider's message under planted id %s", h.ID)
		}
	}

	before := countMsgs(t, req, task.ID)
	f, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: followUp(task.ID, "the real answer", "msg-2"), ReturnImmediately: true})
	if err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	if f.ID != task.ID || countMsgs(t, req, task.ID) != before+1 {
		t.Fatalf("the follow-up was not recorded (%d → %d messages)", before, countMsgs(t, req, task.ID))
	}
	waitUntil(t, "the provider receives the follow-up", func() bool {
		_ = prov.pollOnce(ctx)
		pm, _ := prov.ix.Messages(task.ID)
		for _, m := range pm {
			if m.Body == "the real answer" {
				return true
			}
		}
		return false
	})

	nOut := outboundCount(t, req)
	nt, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("a brand new request", "", "msg-3"), ReturnImmediately: true})
	if err != nil {
		t.Fatalf("new task: %v", err)
	}
	if nt.ID == task.ID || outboundCount(t, req) != nOut+1 {
		t.Fatalf("the new request was answered with %s (tasks %d → %d)", nt.ID, nOut, outboundCount(t, req))
	}
}

// 0017 Q32: a client's retry (same messageId, no contextId) finds its task
// while that task is open; once the task has ended, the same messageId
// makes a new task, as a client that reuses message ids (the A2A TCK)
// expects.
func TestAMessageIDSeenOnAFinishedTaskStartsANewOne(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	send := module.TaskSend{Message: textMsg("hello", "", "m-q32"), ReturnImmediately: true}
	first, err := seam.Send(ctx, prov.AID(), send)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := seam.Send(ctx, prov.AID(), send)
	if err != nil || retry.ID != first.ID || outboundCount(t, req) != 1 {
		t.Fatalf("retry while open = %s (%v), %d tasks; want %s", retry.ID, err, outboundCount(t, req), first.ID)
	}
	if err := req.ix.Finish(first.ID, interactions.Finish{State: interactions.StateCompleted, Result: []byte("done"),
		Verified: interactions.VerificationUnknown}); err != nil {
		t.Fatal(err)
	}
	again, err := seam.Send(ctx, prov.AID(), send)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID == first.ID || outboundCount(t, req) != 2 {
		t.Fatalf("after the task completed the same messageId returned %s, %d tasks; want a new task", again.ID, outboundCount(t, req))
	}
	if again.Status.State != a2ashape.TaskStateSubmitted {
		t.Fatalf("new task state %s", again.Status.State)
	}
	// And the retry of that one finds it, not the finished task.
	if r, err := seam.Send(ctx, prov.AID(), send); err != nil || r.ID != again.ID {
		t.Fatalf("retry of the new task = %s (%v), want %s", r.ID, err, again.ID)
	}
	// A contextId the client repeats is keyed the same way.
	withCtx := module.TaskSend{Message: textMsg("hi", "ctx-q32", "m-q32c"), ReturnImmediately: true}
	c1, err := seam.Send(ctx, prov.AID(), withCtx)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.ix.Finish(c1.ID, interactions.Finish{State: interactions.StateFailed, Result: []byte("x"),
		Verified: interactions.VerificationUnknown}); err != nil {
		t.Fatal(err)
	}
	if c2, err := seam.Send(ctx, prov.AID(), withCtx); err != nil || c2.ID == c1.ID {
		t.Fatalf("same messageId and contextId after the task failed = %s (%v), want a new task", c2.ID, err)
	}
}

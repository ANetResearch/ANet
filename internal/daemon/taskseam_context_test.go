package daemon

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// 0017 Q22: on the A2A interface, a message with only a contextId answers
// the one input-required task of this endpoint in that context (Hermes'
// way of continuing); a retry of it returns that task; with two such tasks
// it is a new task, as the specification reads it; the control plane is not
// lenient.
func TestAContextOnlyMessageContinuesTheTaskWaitingForIt(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	first, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("plan a trip", "ctx-h", "h-1"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, first.ID, "where to?", nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if ix, _ := req.ix.Get(first.ID); ix.State != interactions.StateInputRequired {
		t.Fatalf("state %s, want input-required", ix.State)
	}

	got, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("Kyoto", "ctx-h", "h-2"), ReturnImmediately: true})
	if err != nil || got.ID != first.ID {
		t.Fatalf("contextId-only answer made task %q (%v), want %s continued", got.ID, err, first.ID)
	}
	if n := outboundCount(t, req); n != 1 {
		t.Fatalf("%d outbound tasks, want 1", n)
	}
	waitUntil(t, "the answer to reach the provider on the same task", func() bool {
		_ = prov.pollOnce(ctx)
		msgs, _ := prov.ix.Messages(first.ID)
		return len(msgs) > 0 && msgs[len(msgs)-1].Body == "Kyoto"
	})
	// A retry of the same message returns the task and stores nothing.
	stored := countMsgs(t, req, first.ID)
	again, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("Kyoto", "ctx-h", "h-2"), ReturnImmediately: true})
	if err != nil || again.ID != first.ID || countMsgs(t, req, first.ID) != stored || outboundCount(t, req) != 1 {
		t.Fatalf("retry: task %q (%v), %d messages (was %d), %d tasks", again.ID, err,
			countMsgs(t, req, first.ID), stored, outboundCount(t, req))
	}

	// Two tasks waiting in the context: the client must say which, so a
	// contextId-only message is a new task.
	for _, id := range []string{"ix_wait_a", "ix_wait_b"} {
		if err := req.ix.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: prov.AID(),
			Goal: "g", ContextID: "ctx-two"}); err != nil {
			t.Fatal(err)
		}
		if _, err := req.ix.SetState(id, interactions.StateInputRequired); err != nil {
			t.Fatal(err)
		}
	}
	before := outboundCount(t, req)
	fresh, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("which?", "ctx-two", "h-3"), ReturnImmediately: true})
	if err != nil || fresh.ID == "ix_wait_a" || fresh.ID == "ix_wait_b" || outboundCount(t, req) != before+1 {
		t.Fatalf("two waiting tasks: got %q (%v)", fresh.ID, err)
	}

	// The control plane names tasks: a contextId-only send is a new task.
	if _, err := req.ix.SetState("ix_wait_b", interactions.StateCanceled); err != nil {
		t.Fatal(err)
	}
	before = outboundCount(t, req)
	cp, err := req.sendTask(ctx, controlScope, prov.AID(), module.TaskSend{Message: textMsg("cp", "ctx-two", "h-4"),
		ReturnImmediately: true}, 0)
	if err != nil || cp.ID == "ix_wait_a" || outboundCount(t, req) != before+1 {
		t.Fatalf("control plane: got %q (%v)", cp.ID, err)
	}
	// While the same message on the A2A interface continues the one left.
	cont, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("this one", "ctx-two", "h-5"), ReturnImmediately: true})
	if err != nil || cont.ID != "ix_wait_a" {
		t.Fatalf("one waiting task left: got %q (%v)", cont.ID, err)
	}
}

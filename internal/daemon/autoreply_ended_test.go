package daemon

import (
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// An interaction that has ended gets no auto-reply turn, even though the
// peer had the last word: ActiveThreads leaves ended interactions out and
// the scan skips them again. Either filter alone keeps the backend from
// being asked, so this fails only when both are gone (supp c35-5). A task
// that has not ended, sent the same way, is answered: the test asks for
// the absence of a turn where there would otherwise be one.
func TestAutoReplyLeavesEndedTasksAlone(t *testing.T) {
	api := &fakeOpenAI{reply: "an answer"}
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "m"}, api)

	live, err := f.req.Delegate(f.ctx, f.prov.AID(), "a task to answer", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if n := api.calls.Load(); n != 1 {
		t.Fatalf("the backend was asked %d times about a live task, want 1", n)
	}
	f.lastPeerMsg(t, live)

	ended, err := f.req.Delegate(f.ctx, f.prov.AID(), "a task to cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.prov.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.req.CancelTask(f.ctx, ended); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if st := stateOf(t, f.prov, ended); st != interactions.StateCanceled {
		t.Fatalf("provider state after the cancel arrived = %s", st)
	}
	f.tick(t)
	if n := api.calls.Load(); n != 1 {
		t.Fatalf("the backend was asked %d times in all, want 1: a task that had ended got a turn", n)
	}
}

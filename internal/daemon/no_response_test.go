package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// failedAt runs the requester's no-response sweep as if it were at (unix
// ms) and reports what it failed.
func failedAt(d *Daemon, at int64) []string { return d.failUnanswered(at) }

// docs/notes/0035 §8.3: a provider's refusals past its notice rate limit
// are dropped without a word, and the requester's task stayed submitted
// for ever — 64 of 300 calls in a burst, every client waiting on them until
// its own timeout. A task nobody answers now fails with anet.reason=
// no_response and effect UNVERIFIED once no_response_after has passed; a
// result that comes after that is still verified and recorded, and does not
// reopen the task.
func TestAnUnansweredTaskFailsAsNoResponse(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	// The provider has not polled: from the requester's side it said
	// nothing at all.
	if got := failedAt(req, now+(DefaultNoResponseAfter-time.Minute).Milliseconds()); len(got) != 0 {
		t.Fatalf("failed before the deadline: %v", got)
	}
	if st, _, _ := taskViewMap(t, req, id); st != string(a2ashape.TaskStateSubmitted) {
		t.Fatalf("state %s before the deadline", st)
	}
	got := failedAt(req, now+(DefaultNoResponseAfter+time.Second).Milliseconds())
	if len(got) != 1 || got[0] != id {
		t.Fatalf("failed %v, want [%s]", got, id)
	}
	state, es, reason := taskViewMap(t, req, id)
	if state != string(a2ashape.TaskStateFailed) || es != "UNVERIFIED" || reason != a2ashape.ReasonNoResponse {
		t.Fatalf("requester: state=%s effect_status=%s reason=%s; want failed, UNVERIFIED, no_response", state, es, reason)
	}
	if ev := lastLedgerPayload(t, req, EvNoResponse); ev["interaction_id"] != id || ev["peer_aid"] != prov.AID() {
		t.Fatalf("evidence %v", ev)
	}
	if got := failedAt(req, now+(2*DefaultNoResponseAfter).Milliseconds()); len(got) != 0 {
		t.Fatalf("failed again: %v", got)
	}

	// The provider was only slow: it runs the call and answers. The
	// answer is recorded as a late result; the task stays failed.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(lamp.invoked) != 1 {
		t.Fatalf("provider ran the call %d times", len(lamp.invoked))
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	cur, err := req.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if cur.State != interactions.StateFailed || len(cur.Receipt) == 0 {
		t.Fatalf("after the late result: state %s, receipt %d bytes; want failed with the receipt recorded", cur.State, len(cur.Receipt))
	}
	if ev := lastLedgerPayload(t, req, EvResultAccepted); ev["after_terminal"] != true || ev["interaction_id"] != id {
		t.Fatalf("result evidence %v, want after_terminal", ev)
	}
}

// Only silence counts. A task whose peer said anything (a pending_approval
// notice is a status), whose delegation is still in the retry queue (it
// has not reached the provider: the queue fails it as undeliverable when it
// gives up), or that has moved on is left alone; and no_response_after "0"
// turns the deadline off.
func TestOnlyATaskThatHeardNothingFailsAsNoResponse(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	late := time.Now().Add(2 * DefaultNoResponseAfter).UnixMilli()

	heard, err := req.Delegate(ctx, prov.AID(), "held for approval", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := req.ix.AddMessageRecord(interactions.MessageRecord{InteractionID: heard, SenderAID: prov.AID(),
		Kind: interactions.MsgStatus, Metadata: []byte(`{"anet.inbound":"pending_approval"}`)}); err != nil {
		t.Fatal(err)
	}
	moved, err := req.Delegate(ctx, prov.AID(), "a conversation", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.SendMessage(ctx, moved, "and a follow-up", nil); err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
	queued, err := req.Delegate(ctx, prov.AID(), "not delivered yet", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows, _ := req.ix.Outbox(queued); len(rows) == 0 {
		t.Fatal("setup: the delegation is not queued")
	}
	if got := failedAt(req, late); len(got) != 0 {
		t.Fatalf("failed %v; none of these waits on silence", got)
	}
	for _, id := range []string{heard, queued} {
		if st, _, _ := taskViewMap(t, req, id); st != string(a2ashape.TaskStateSubmitted) {
			t.Fatalf("%s: %s", id, st)
		}
	}

	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = false })
	silent, err := req.Delegate(ctx, prov.AID(), "never answered", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.mu.Lock()
	req.cfg.NoResponseAfter = "0"
	req.mu.Unlock()
	if got := failedAt(req, late); len(got) != 0 {
		t.Fatalf("no_response_after 0 failed %v", got)
	}
	req.mu.Lock()
	req.cfg.NoResponseAfter = ""
	req.mu.Unlock()
	if got := failedAt(req, late); len(got) != 1 || got[0] != silent {
		t.Fatalf("failed %v, want only the silent one %s", got, silent)
	}
	// A text task has no effect status in its projection; the reason is
	// the same.
	if st, _, reason := taskViewMap(t, req, silent); st != string(a2ashape.TaskStateFailed) || reason != a2ashape.ReasonNoResponse {
		t.Fatalf("silent: %s %s", st, reason)
	}
}

// The deadline counts from when the delegation left this node, not from
// when the task was made: a delegation that waited out a hub outage in the
// retry queue gets the whole period after it is delivered.
func TestTheNoResponseDeadlineCountsFromDelivery(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
	id, err := req.Delegate(ctx, prov.AID(), "sent during an outage", nil)
	if err != nil {
		t.Fatal(err)
	}
	made, err := req.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond) // the outage
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = false })
	rows, err := req.ix.Outbox(id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("outbox %d %v", len(rows), err)
	}
	if err := req.deliverQueued(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	after := DefaultNoResponseAfter.Milliseconds()
	// Past the deadline counted from the task's making, inside it counted
	// from its delivery.
	if got := failedAt(req, made.StateAt+30+after); len(got) != 0 {
		t.Fatalf("failed %v inside the deadline counted from delivery", got)
	}
	delivered, _ := req.ix.Get(id)
	if got := failedAt(req, delivered.UpdatedAtTime().UnixMilli()+after+1); len(got) != 1 {
		t.Fatalf("failed %v past the deadline counted from delivery", got)
	}
}

// A provider now says working when a long call starts, so its requester
// does not take an hour of work for silence and fail the task while it
// runs.
func TestALongCallTellsItsRequesterItStarted(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	slow := newSlowProvider(time.Hour)
	if err := prov.Providers().Register(ctx, slow); err != nil {
		t.Fatal(err)
	}
	defer close(slow.release)
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the long call starts", func() bool { return slow.started.Load() == 1 })
	waitUntil(t, "the requester hears the call started", func() bool {
		if err := req.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		cur, err := req.ix.Get(id)
		return err == nil && cur.State == interactions.StateWorking
	})
	if got := failedAt(req, time.Now().Add(2*DefaultNoResponseAfter).UnixMilli()); len(got) != 0 {
		t.Fatalf("failed %v while the call runs", got)
	}
}

// no_response_after: a Go duration, 15m when left out (and written so by a
// fresh config), "0" off; anything else stops the daemon from starting.
func TestNoResponseAfterConfig(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"", DefaultNoResponseAfter, false},
		{defaultNoResponseAfterText, DefaultNoResponseAfter, false},
		{"0", 0, false},
		{"2h", 2 * time.Hour, false},
		{"-1m", 0, true},
		{"soon", 0, true},
	} {
		c := DefaultConfig()
		c.NoResponseAfter = tc.in
		got, err := c.noResponseAfter()
		if (err != nil) != tc.bad || (!tc.bad && got != tc.want) {
			t.Errorf("%q: %s %v", tc.in, got, err)
		}
		if verr := validatePolicy(c, false); (verr != nil) != tc.bad {
			t.Errorf("%q: validatePolicy %v", tc.in, verr)
		} else if tc.bad && !strings.Contains(verr.Error(), "no_response_after") {
			t.Errorf("%q: %v does not name the key", tc.in, verr)
		}
	}
	if DefaultConfig().NoResponseAfter != defaultNoResponseAfterText {
		t.Fatalf("a fresh config writes no_response_after %q", DefaultConfig().NoResponseAfter)
	}
}

// Tasks that wait on an answer without qualifying — here old ones whose
// peer said pending_approval — do not keep a newer unanswered task from
// being failed, however many of them there are: the sweep pages past them.
func TestTasksThatDoNotQualifyDoNotHideOnesThatDo(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	old := noResponseBatch
	noResponseBatch = 2
	t.Cleanup(func() { noResponseBatch = old })
	for i := 0; i < 5; i++ {
		id, err := req.Delegate(ctx, prov.AID(), "held", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := req.ix.AddMessageRecord(interactions.MessageRecord{InteractionID: id, SenderAID: prov.AID(),
			Kind: interactions.MsgStatus, Metadata: []byte(`{"anet.inbound":"pending_approval"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(5 * time.Millisecond)
	silent, err := req.Delegate(ctx, prov.AID(), "never answered", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := failedAt(req, time.Now().Add(2*DefaultNoResponseAfter).UnixMilli()); len(got) != 1 || got[0] != silent {
		t.Fatalf("failed %v, want [%s]", got, silent)
	}
}

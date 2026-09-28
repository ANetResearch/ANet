package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/provider"
)

// gateProvider is a long-running capability (LongRunning, 2 minutes): an
// invocation blocks until released or until its context ends, and records
// how it ended.
type gateProvider struct {
	mu       sync.Mutex
	started  chan struct{}
	release  chan struct{}
	canceled bool
	calls    int
}

const gateCap = "gate.work"

func newGateProvider() *gateProvider {
	return &gateProvider{started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (p *gateProvider) ID() string                                     { return "gate" }
func (p *gateProvider) Capabilities(context.Context) ([]string, error) { return []string{gateCap}, nil }
func (p *gateProvider) Describe(context.Context) (string, error)       { return "", nil }
func (p *gateProvider) Health(context.Context) error                   { return nil }
func (p *gateProvider) InvokeTimeout(string) (time.Duration, bool)     { return 2 * time.Minute, true }
func (p *gateProvider) Invoke(ctx context.Context, _ provider.Call) (effect.Effect, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	p.started <- struct{}{}
	select {
	case <-p.release:
		return effect.Effect{Status: effect.OK, Record: &tsir.EffectRecord{}}, nil
	case <-ctx.Done():
		p.mu.Lock()
		p.canceled = true
		p.mu.Unlock()
		return effect.Effect{}, ctx.Err()
	}
}

func (p *gateProvider) wasCanceled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.canceled
}

// stateOf reads an interaction's state.
func stateOf(t *testing.T, d *Daemon, ix string) interactions.State {
	t.Helper()
	got, err := d.ix.Get(ix)
	if err != nil {
		t.Fatal(err)
	}
	return got.State
}

// A requester's cancel ends a text task on both sides, the provider tells
// the requester status{canceled}, and the provider can no longer complete
// it: no receipt is signed, stored, recorded or sent (A2A-DESIGN §4.2).
func TestACanceledTaskGetsNoReceipt(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "a task to cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ix, err := req.CancelTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ix.State != interactions.StateCanceled {
		t.Fatalf("requester state after cancel = %s", ix.State)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, prov, id); st != interactions.StateCanceled {
		t.Fatalf("provider state after the cancel arrived = %s", st)
	}
	// The provider's status{canceled} reaches the requester and changes
	// nothing there (already canceled).
	if n := len(queuedFor(t, srv, req.AID())); n != 1 {
		t.Fatalf("%d messages queued for the requester, want the canceled status", n)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, req, id); st != interactions.StateCanceled {
		t.Fatalf("requester state = %s", st)
	}
	// Completing a canceled task is refused and leaves no receipt anywhere.
	if err := prov.CompleteTask(ctx, id); !errors.Is(err, ErrTaskTerminal) {
		t.Fatalf("CompleteTask on a canceled task: %v, want ErrTaskTerminal", err)
	}
	pix, _ := prov.ix.Get(id)
	if len(pix.Receipt) != 0 || len(pix.Result) != 0 {
		t.Fatalf("canceled interaction carries a receipt (%d bytes) or result", len(pix.Receipt))
	}
	if n := chainEvents(t, prov, EvReceipt); n != 0 {
		t.Fatalf("%d receipts on the provider chain for a canceled task", n)
	}
	if rows, _ := prov.ix.Outbox(id); len(rows) != 0 {
		t.Fatalf("a result is queued for a canceled task: %+v", rows)
	}
	if n := len(queuedFor(t, srv, req.AID())); n != 0 {
		t.Fatalf("%d messages reached the hub for the requester after the cancel", n)
	}
	// And new input is refused on both sides.
	if err := req.SendMessage(ctx, id, "anyone there?", nil); !errors.Is(err, ErrTaskTerminal) {
		t.Fatalf("message on a canceled task: %v", err)
	}
	if _, err := req.CancelTask(ctx, id); !errors.Is(err, ErrNotCancelable) {
		t.Fatalf("second cancel: %v", err)
	}
}

// A provider can cancel its own task; the requester learns it from
// status{canceled}.
func TestAProviderCancelReachesTheRequester(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.CancelTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, req, id); st != interactions.StateCanceled {
		t.Fatalf("requester state = %s, want canceled", st)
	}
}

// The state is written by the event that causes it (A2A-DESIGN §4.1): a
// provider message makes the task input-required, a message marked
// anet.state=working and a status message leave it working, and a
// requester message makes it working — on both sides.
func TestMessagesAndStatusWriteTheState(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, req, id); st != interactions.StateSubmitted {
		t.Fatalf("new task = %s, want submitted", st)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	both := func(want interactions.State, why string) {
		t.Helper()
		if err := req.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if a, b := stateOf(t, req, id), stateOf(t, prov, id); a != want || b != want {
			t.Fatalf("%s: requester %s, provider %s, want %s", why, a, b, want)
		}
	}
	if err := prov.SendMessage(ctx, id, "which format?", nil); err != nil {
		t.Fatal(err)
	}
	both(interactions.StateInputRequired, "provider question")
	if err := req.SendMessage(ctx, id, "markdown", nil); err != nil {
		t.Fatal(err)
	}
	both(interactions.StateWorking, "requester answer")
	if err := prov.SendMessage(ctx, id, "question two", nil); err != nil {
		t.Fatal(err)
	}
	both(interactions.StateInputRequired, "provider question two")
	if err := prov.SendMessageOpts(ctx, id, "50% done", nil, map[string]any{"anet.state": "working"}); err != nil {
		t.Fatal(err)
	}
	both(interactions.StateWorking, "provider progress")
	if err := prov.SendStatus(ctx, id, interactions.StateInputRequired, "need a file", nil); err != nil {
		t.Fatal(err)
	}
	both(interactions.StateInputRequired, "provider status")
	// A progress message is not a conversation turn and is not in the
	// transcript the receipt covers.
	if err := prov.CompleteTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	pix, _ := prov.ix.Get(id)
	if strings.Contains(string(pix.Result), "50% done") {
		t.Fatalf("a progress message is in the signed transcript: %s", pix.Result)
	}
	if !strings.Contains(string(pix.Result), "markdown") {
		t.Fatalf("the transcript lost a conversation turn: %s", pix.Result)
	}
	both(interactions.StateCompleted, "completion")
}

// A blocking follow-up waits for a state event newer than its own write
// (C35): after the requester answers an input-required task, the task is
// working and nothing wakes the waiter until the provider speaks again.
func TestAFollowUpWaitsForANewerStateEvent(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, id, "question", nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := req.SendMessage(ctx, id, "follow-up", nil); err != nil {
		t.Fatal(err)
	}
	snap, events, cancel, err := req.Watch(id)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if snap.State != interactions.StateWorking {
		t.Fatalf("after the follow-up the task is %s, want working (a stale input-required would end the wait)", snap.State)
	}
	// Nothing from the provider yet: no state event arrives.
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-events:
		if e.Kind == EventState {
			t.Fatalf("a state event arrived before the provider replied: %+v", e)
		}
	case <-time.After(100 * time.Millisecond):
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, id, "answer", nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatal("watch channel closed")
			}
			if e.Kind != EventState {
				continue
			}
			if e.State != interactions.StateInputRequired || e.StateSeq <= snap.StateSeq {
				t.Fatalf("state event %+v after snapshot seq %d", e, snap.StateSeq)
			}
			return
		case <-deadline:
			t.Fatal("no state event after the provider replied")
		}
	}
}

// Watch delivers each state change once: a change committed before the
// snapshot is not delivered again, cancel closes the channel, and a watcher
// that stops reading is dropped rather than blocking the writer.
func TestWatchDeliversEachStateChangeOnce(t *testing.T) {
	d := newTestDaemon(t, "", false)
	if err := d.ix.Put("ix_w", interactions.RoleOutbound, "did:anet:p", "g", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ix.SetState("ix_w", interactions.StateWorking); err != nil {
		t.Fatal(err)
	}
	snap, ch, cancel, err := d.Watch("ix_w")
	if err != nil {
		t.Fatal(err)
	}
	d.publishState("ix_w") // the event of the write already in the snapshot
	if _, err := d.ix.SetState("ix_w", interactions.StateInputRequired); err != nil {
		t.Fatal(err)
	}
	d.publishState("ix_w")
	e := <-ch
	if e.State != interactions.StateInputRequired || e.StateSeq != snap.StateSeq+1 {
		t.Fatalf("first event %+v, snapshot seq %d", e, snap.StateSeq)
	}
	select {
	case e := <-ch:
		t.Fatalf("unexpected second event %+v", e)
	default:
	}
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel open after cancel")
	}
	cancel() // idempotent
	// A subscriber that never reads is dropped once its buffer is full.
	_, slow, cancelSlow, _ := d.Watch("ix_w")
	defer cancelSlow()
	for i := 0; i < eventBufferSize+1; i++ {
		d.publishMessage("ix_w", int64(i), interactions.MsgText)
	}
	n := 0
	for range slow {
		n++
	}
	if n != eventBufferSize || d.bus.subscribers("ix_w") != 0 {
		t.Fatalf("slow watcher got %d events and %d subscriptions remain", n, d.bus.subscribers("ix_w"))
	}
}

// After a terminal state only new input is refused (A2A-DESIGN §4.2): a
// text message for a completed task is acknowledged and not stored, and a
// wire-1 end_accept is dropped.
func TestInputAfterTheEndIsRefused(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.CompleteTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	before := countMsgs(t, prov, id)
	late := sealFrom(t, req, prov, seal.TypeMessage, id, chatBody(t, "one more thing", "msg_late"))
	if r := receive(t, prov, late); !r.ack() {
		t.Fatalf("late input: %+v, want acknowledged", r)
	}
	if countMsgs(t, prov, id) != before || counter(prov, dropAfterTerminal) != 1 {
		t.Fatalf("late input was stored (%d → %d) or not counted", before, countMsgs(t, prov, id))
	}
	accept, _ := (&delegation.ChatMsg{Kind: delegation.ChatEndAccept, MsgID: "msg_ea"}).Marshal()
	if r := receive(t, prov, sealFrom(t, req, prov, seal.TypeMessage, id, accept)); !r.ack() || counter(prov, dropEndAccept) != 1 {
		t.Fatalf("end_accept: %+v, counted %d", r, counter(prov, dropEndAccept))
	}
	if st := stateOf(t, prov, id); st != interactions.StateCompleted {
		t.Fatalf("state changed to %s", st)
	}
}

// A capability call that has not started when a cancel or end_request
// arrives is canceled, with status{canceled} and no receipt (A2A-DESIGN
// §4.2).
func TestACapabilityCallCanceledBeforeItRuns(t *testing.T) {
	srv, req, prov := registeredPair(t)
	for _, kind := range []string{delegation.KindCancel, delegation.ChatEndRequest} {
		id := "ix_notstarted_" + kind
		if err := prov.ix.Create(interactions.New{ID: id, Role: interactions.RoleInbound, PeerAID: req.AID(),
			Goal: "invoke capability x", IsCapability: true, Trust: interactions.TrustPeer}); err != nil {
			t.Fatal(err)
		}
		if err := req.ix.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: prov.AID(),
			Goal: "invoke capability x", IsCapability: true}); err != nil {
			t.Fatal(err)
		}
		body, _ := (&delegation.ChatMsg{Kind: kind, MsgID: "msg_" + kind}).Marshal()
		if r := receive(t, prov, sealFrom(t, req, prov, seal.TypeMessage, id, body)); r.class != rxAccepted {
			t.Fatalf("%s: %+v", kind, r)
		}
		pix, _ := prov.ix.Get(id)
		if pix.State != interactions.StateCanceled || len(pix.Receipt) != 0 {
			t.Fatalf("%s before execution: state %s, receipt %d bytes", kind, pix.State, len(pix.Receipt))
		}
	}
	if n := len(queuedFor(t, srv, req.AID())); n != 2 {
		t.Fatalf("%d status messages for the requester, want 2", n)
	}
	if err := req.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{delegation.KindCancel, delegation.ChatEndRequest} {
		if st := stateOf(t, req, "ix_notstarted_"+kind); st != interactions.StateCanceled {
			t.Fatalf("requester %s: %s", kind, st)
		}
	}
}

// During a long capability call an end_request is ignored and a cancel
// stops the call's context; either way no transcript receipt is signed and
// the capability's own result is delivered (A2A-DESIGN §4.2 [C6][C29]). The
// requester, which canceled locally, records the result that arrives after
// its cancel without reopening the task (C34).
func TestALongCapabilityCallUnderEndRequestAndCancel(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	slow := newGateProvider()
	if err := prov.Providers().Register(ctx, slow); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), gateCap, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	<-slow.started
	if st := stateOf(t, prov, id); st != interactions.StateWorking {
		t.Fatalf("a running long call is %s, want working", st)
	}
	// The provider said working when the call started (announceLongCall);
	// the requester takes that in now, so what its mailbox holds later is
	// the result.
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, req, id); st != interactions.StateWorking {
		t.Fatalf("the requester sees %s once the long call started, want working", st)
	}
	if err := req.RequestEnd(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if pix, _ := prov.ix.Get(id); pix.State != interactions.StateWorking || len(pix.Receipt) != 0 {
		t.Fatalf("end_request during execution changed the task: %s, receipt %d", pix.State, len(pix.Receipt))
	}
	if slow.wasCanceled() {
		t.Fatal("end_request stopped the call")
	}
	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the call's context to be canceled and its result stored", func() bool {
		pix, _ := prov.ix.Get(id)
		return slow.wasCanceled() && len(pix.Receipt) > 0
	})
	pix, _ := prov.ix.Get(id)
	var res capabilityResult
	if err := json.Unmarshal(pix.Result, &res); err != nil || res.Capability != gateCap {
		t.Fatalf("the provider's result is not the capability result: %v %s", err, pix.Result)
	}
	if pix.State != interactions.StateFailed {
		t.Fatalf("a call stopped by cancel ended %s, want failed", pix.State)
	}
	waitUntil(t, "the result to reach the requester's mailbox", func() bool {
		return len(queuedFor(t, srv, req.AID())) > 0
	})
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	rix, _ := req.ix.Get(id)
	if rix.State != interactions.StateCanceled {
		t.Fatalf("the late result reopened the requester's task: %s", rix.State)
	}
	if len(rix.Receipt) == 0 {
		t.Fatal("the result that arrived after the local cancel was not recorded")
	}
	_, recs := req.ledger.Evidence(EvidenceQuery{EventType: EvResultAccepted})
	if len(recs) != 1 || !strings.Contains(string(mustJSON(t, recs[0])), `"after_terminal":true`) {
		t.Fatalf("result evidence = %+v", recs)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The outbound retry queue survives a restart (A2A-DESIGN §4.2): a result
// the hub refused to carry is kept with its envelope bytes and delivered by
// the next process.
func TestTheRetryQueueSurvivesARestart(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	fake := fakeHubAt(t, srv.URL)
	fake.mu.Lock()
	fake.sendsBy[prov.AID()] = 1 // every further send by the provider: 429
	fake.senderLimit = 1
	fake.mu.Unlock()
	if err := prov.CompleteTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	rows, _ := prov.ix.Outbox(id)
	if len(rows) != 1 || len(rows[0].Envelope) == 0 || rows[0].Attempts != 1 {
		t.Fatalf("outbox after a refused send = %+v", rows)
	}
	sealed := append([]byte(nil), rows[0].Envelope...)
	if n := len(queuedFor(t, srv, req.AID())); n != 0 {
		t.Fatalf("%d messages reached the hub while it refused sends", n)
	}
	layout := prov.layout
	if err := prov.Close(); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.senderLimit = 0
	fake.mu.Unlock()
	prov2, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	prov2.stopRelayLoop()
	t.Cleanup(func() { prov2.Close() })
	waitUntil(t, "the queued result to be delivered by the restarted daemon", func() bool {
		return len(queuedFor(t, srv, req.AID())) == 1
	})
	if got := queuedFor(t, srv, req.AID())[0]; string(got) != string(sealed) {
		t.Fatal("the retry sent different envelope bytes than the first attempt")
	}
	waitUntil(t, "the delivered row to leave the queue", func() bool {
		rows, _ := prov2.ix.Outbox(id)
		return len(rows) == 0
	})
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, req, id); st != interactions.StateCompleted {
		t.Fatalf("requester state = %s", st)
	}
}

// retryDelay doubles from the base and stops at 24 hours.
func TestRetryBackoffIsBounded(t *testing.T) {
	if retryDelay(1) != retryBaseDelay || retryDelay(2) != 2*retryBaseDelay || retryDelay(3) != 4*retryBaseDelay {
		t.Fatalf("backoff = %s %s %s", retryDelay(1), retryDelay(2), retryDelay(3))
	}
	if retryDelay(100) != retryMaxDelay {
		t.Fatalf("backoff at 100 attempts = %s, want %s", retryDelay(100), retryMaxDelay)
	}
}

// A long capability call that was running when the daemon stopped is
// reported at the next start as failed, effect UNVERIFIED, reason
// interrupted, and the requester is told (A2A-DESIGN §3.6 startup
// recovery). It is not run again.
func TestAnInterruptedLongCallIsReportedAtStart(t *testing.T) {
	srv, req, prov := registeredPair(t)
	const id = "ix_interrupted"
	if err := prov.ix.Create(interactions.New{ID: id, Role: interactions.RoleInbound, PeerAID: req.AID(),
		Goal: "invoke capability " + gateCap, IsCapability: true, Trust: interactions.TrustPeer,
		TaskNonce: "bm9uY2U"}); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.SetState(id, interactions.StateWorking); err != nil {
		t.Fatal(err)
	}
	if err := req.ix.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: prov.AID(),
		Goal: "invoke capability " + gateCap, IsCapability: true}); err != nil {
		t.Fatal(err)
	}
	layout := prov.layout
	if err := prov.Close(); err != nil {
		t.Fatal(err)
	}
	prov2, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	prov2.stopRelayLoop()
	t.Cleanup(func() { prov2.Close() })
	pix, _ := prov2.ix.Get(id)
	var res capabilityResult
	_ = json.Unmarshal(pix.Result, &res)
	if pix.State != interactions.StateFailed || res.Status != string(effect.Unverified) || len(pix.Receipt) == 0 {
		t.Fatalf("after restart: state %s, effect %q, receipt %d bytes", pix.State, res.Status, len(pix.Receipt))
	}
	waitUntil(t, "the interrupted result to reach the requester", func() bool {
		return len(queuedFor(t, srv, req.AID())) == 1
	})
	if err := req.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rix, _ := req.ix.Get(id)
	if rix.State != interactions.StateFailed {
		t.Fatalf("requester state = %s, want failed", rix.State)
	}
}

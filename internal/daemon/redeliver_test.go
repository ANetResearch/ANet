package daemon

// Tests of B3-01: re-sending answers for redelivered delegations (C33),
// the retry queue's refusals, deadlines and Retry-After, the late result in
// the receive transaction, requester sends through the retry queue (0017
// Q5) and message ids derived from the envelope (0017 Q9).

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// setFake changes the fake hub under its lock.
func setFake(t *testing.T, url string, f func(h *fakeHub)) {
	t.Helper()
	h := fakeHubAt(t, url)
	h.mu.Lock()
	f(h)
	h.mu.Unlock()
}

// resultMetaOf is the decoded result metadata of ix on d.
func resultMetaOf(t *testing.T, d *Daemon, ix string) map[string]any {
	t.Helper()
	cur, err := d.ix.Get(ix)
	if err != nil {
		t.Fatal(err)
	}
	return decodeMeta([]byte(cur.ResultMeta))
}

// A public capability call that has ended keeps no key for its requester
// (§3.8). A redelivered delegation still gets the answer, sealed to the key
// set the redelivery carried, exactly once (G4, C33).
func TestAnEndedPublicCallAnswersARedeliveredDelegateOnce(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: lampCap}}); err != nil {
		t.Fatal(err)
	}
	id, err := stranger.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	pix, err := prov.ix.Get(id)
	if err != nil || pix.Trust != interactions.TrustPublicCap || !pix.IsTerminal() || len(pix.PeerKeys) != 0 {
		t.Fatalf("the public call did not end with its keys cleared: %+v %v", pix, err)
	}
	clearMailbox(t, srv, stranger.AID()) // the answer is lost on the way

	injectEnvelope(t, srv, prov.AID(), env)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("the capability ran %d times", n)
	}
	if n := len(queuedFor(t, srv, stranger.AID())); n != 1 {
		t.Fatalf("%d answers re-sent for the redelivery, want 1", n)
	}
	if rows, _ := prov.ix.Outbox(id); len(rows) != 0 {
		t.Fatalf("the re-sent answer is still queued: %+v", rows)
	}
	if err := stranger.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := stranger.ix.Get(id)
	if err != nil || got.State != interactions.StateCompleted || len(got.Receipt) == 0 {
		t.Fatalf("the re-sent answer did not land: %+v %v", got, err)
	}
}

// An answer still in the retry queue is the answer: a redelivered
// delegation does not queue a second copy, and the requester gets one
// envelope once the hub carries mail again (C33).
func TestAQueuedAnswerIsNotQueuedAgainForARedelivery(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
	if r := receive(t, prov, env); r.class != rxAccepted {
		t.Fatalf("first delivery: %+v", r)
	}
	rows, err := prov.ix.Outbox(id)
	if err != nil || len(rows) != 1 || rows[0].Type != seal.TypeResult {
		t.Fatalf("outbox with the relay down = %+v (%v), want the answer", rows, err)
	}
	for i := 0; i < 3; i++ {
		if r := receive(t, prov, env); r.reason != dropDuplicate {
			t.Fatalf("redelivery %d: %+v", i, r)
		}
	}
	if again, _ := prov.ix.Outbox(id); len(again) != 1 || again[0].ID != rows[0].ID {
		t.Fatalf("outbox after redeliveries = %+v, want the one answer", again)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = false })
	if err := prov.deliverQueued(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	prov.flushOutboxAt(ctx, int64(prov.nowMS())+int64(retryMaxDelay.Milliseconds()))
	if n := len(queuedFor(t, srv, req.AID())); n != 1 {
		t.Fatalf("%d answers delivered, want 1", n)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("the capability ran %d times", n)
	}
}

// 400, 404 and 413 from the relay are answers about the message: the row is
// dropped at once with the reason on the chain, and never tried again. A
// requester's task whose delegation was refused is failed as undeliverable.
func TestARefusalForGoodIsNotRetried(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()

	// 413: the requester's own delegation.
	setFake(t, srv.URL, func(h *fakeHub) { h.maxEnvelope = 1 })
	_, err := req.Delegate(ctx, prov.AID(), "too big for this hub", nil)
	if !errors.Is(err, errUndeliverable) {
		t.Fatalf("delegate over the hub's cap: %v, want errUndeliverable", err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.maxEnvelope = 0 })
	list, err := req.ix.ListAll(interactions.ListFilter{Role: interactions.RoleOutbound})
	if err != nil || len(list) != 1 || list[0].State != interactions.StateFailed {
		t.Fatalf("after a refused delegation: %+v %v", list, err)
	}
	if m := resultMetaOf(t, req, list[0].ID); m[a2ashape.KeyReason] != a2ashape.ReasonUndeliverable {
		t.Fatalf("result meta %v, want anet.reason=undeliverable", m)
	}
	if ev := lastLedgerPayload(t, req, EvDeliveryExpired); ev["reason"] != undeliveredTooLarge {
		t.Fatalf("evidence %v, want reason %s", ev, undeliveredTooLarge)
	}
	if n, _ := req.ix.OutboxLen(); n != 0 {
		t.Fatalf("%d rows left to retry a refused message", n)
	}

	// 404: the requester left the hub before the provider answered.
	id, err := req.Delegate(ctx, prov.AID(), "answer me later", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := req.LeaveHub(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, id, "here you are", nil); err != nil {
		t.Fatalf("a provider message is recorded whatever the hub says: %v", err)
	}
	if n, _ := prov.ix.OutboxLen(); n != 0 {
		t.Fatalf("%d rows left to retry a message to an unknown recipient", n)
	}
	if ev := lastLedgerPayload(t, prov, EvDeliveryExpired); ev["reason"] != undeliveredUnknown || ev["interaction_id"] != id {
		t.Fatalf("evidence %v, want reason %s for %s", ev, undeliveredUnknown, id)
	}
	// The provider's own task is not failed by an undelivered reply.
	if st := stateOf(t, prov, id); st != interactions.StateInputRequired {
		t.Fatalf("provider state = %s", st)
	}
}

// A delegation to an agent the hub has no keys for is an error before
// anything is written (§3.5 step 1): no task, nothing queued.
func TestADelegationToAnUnknownAgentWritesNothing(t *testing.T) {
	_, req, _ := registeredPair(t)
	stranger := newStranger(t)
	if _, err := req.Delegate(context.Background(), stranger.aid, "hello?", nil); err == nil {
		t.Fatal("a delegation to an agent without keys was accepted")
	}
	if n := outboundCount(t, req); n != 0 {
		t.Fatalf("%d tasks written", n)
	}
	if n, _ := req.ix.OutboxLen(); n != 0 {
		t.Fatalf("%d rows queued", n)
	}
}

// A 429 is retried after the hub's Retry-After, not after the backoff.
func TestTheHubsRetryAfterIsKept(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) {
		h.sendsBy[prov.AID()] = 1
		h.senderLimit = 1
		h.retryAfter = "3600"
	})
	before := int64(prov.nowMS())
	if err := prov.CompleteTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	rows, _ := prov.ix.Outbox(id)
	if len(rows) != 1 || rows[0].Attempts != 1 {
		t.Fatalf("outbox after a 429 = %+v", rows)
	}
	if wait := rows[0].NextAt - before; wait < 3599_000 || wait > 3700_000 {
		t.Fatalf("next attempt in %d ms, want the hub's hour", wait)
	}
	if got, ok := retryAfter(&hubError{path: "/relay/send", code: 429, retryAfter: "0"}); !ok || got != retryAfterMin {
		t.Fatalf("Retry-After 0 = %s %v, want the floor", got, ok)
	}
	if got, ok := retryAfter(&hubError{path: "/relay/send", code: 429,
		retryAfter: time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)}); !ok || got < time.Hour || got > 2*time.Hour {
		t.Fatalf("Retry-After as a date = %s %v", got, ok)
	}
}

// Which hub answers end a row, and which are retried.
func TestPermanentRefusalsAreOnlyAnswersAboutTheMessage(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&hubError{path: "/relay/send", code: 400}, undeliveredBad},
		{&hubError{path: "/relay/send", code: 404}, undeliveredUnknown},
		{&hubError{path: "/relay/send", code: 413}, undeliveredTooLarge},
		{&hubError{path: "/agents/aid_x/keys", code: 404}, undeliveredUnknown},
		{&hubError{path: "/relay/send", code: 429}, ""},
		{&hubError{path: "/relay/send", code: 507}, ""},
		{&hubError{path: "/relay/send", code: 401}, ""},
		{&hubError{path: "/hub/identity", code: 404}, ""}, // a mistyped hub URL is not the recipient
		{errors.New("connection refused"), ""},
	} {
		got, ok := permanentRefusal(c.err)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%v: %q %v, want %q", c.err, got, ok, c.want)
		}
	}
}

// A row that was never sealed has a deadline too: once it passes, the row is
// dropped with the evidence, whether it carries its own exp or (a row from
// before) only the time it was queued.
func TestAnUnsealedRowExpires(t *testing.T) {
	_, req, _ := registeredPair(t)
	ctx := context.Background()
	stranger := newStranger(t)
	// No keys for the recipient: queued unsealed, with a deadline.
	id, err := req.queueSend(ctx, stranger.aid, seal.TypeStatus, "ix_unsealed", []byte("body"), nil)
	if err != nil {
		t.Fatal(err)
	}
	it, err := req.ix.GetOutbox(id)
	if err != nil || len(it.Envelope) != 0 || it.Exp == 0 || len(it.MID) != seal.MIDLen {
		t.Fatalf("unsealed row = %+v (%v), want a body, a deadline and its message id", it, err)
	}
	// Kept out of the background loop's way: this test attempts the rows.
	later := int64(math.MaxInt64 / 2)
	if err := req.ix.RescheduleOutbox(id, 0, later, ""); err != nil {
		t.Fatal(err)
	}
	legacy, err := req.ix.EnqueueOutbox(interactions.OutboxItem{IX: "ix_legacy", ToAID: stranger.aid,
		Type: seal.TypeStatus, Body: []byte("old"), NextAt: later})
	if err != nil {
		t.Fatal(err)
	}
	now := req.nowMS()
	req.clock = func() uint64 { return now + messageLifetimeMS + 60_000 }
	for _, row := range []int64{id, legacy} {
		if err := req.deliverQueued(ctx, row); !errors.Is(err, errUndeliverable) {
			t.Fatalf("row %d past its deadline: %v, want errUndeliverable", row, err)
		}
	}
	if n, _ := req.ix.OutboxLen(); n != 0 {
		t.Fatalf("%d expired rows kept", n)
	}
	if ev := lastLedgerPayload(t, req, EvDeliveryExpired); ev["reason"] != undeliveredExpired {
		t.Fatalf("evidence %v", ev)
	}
}

// A result that arrives after the task ended here is recorded in the same
// transaction as its replay row: a store failure leaves neither, and the
// redelivery records it.
func TestALateResultCommitsWithItsReplayRow(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	fail := true
	req.rxFault = func(typ string) error {
		if fail && typ == seal.TypeResult {
			return errors.New("injected store failure")
		}
		return nil
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, _ := req.ix.Get(id); len(cur.Receipt) != 0 {
		t.Fatal("a late result was stored although its transaction failed")
	}
	if n := len(queuedFor(t, srv, req.AID())); n != 1 {
		t.Fatalf("the failed result was acknowledged (%d left)", n)
	}
	fail = false
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	cur, err := req.ix.Get(id)
	if err != nil || cur.State != interactions.StateCanceled || len(cur.Receipt) == 0 || len(cur.Result) == 0 {
		t.Fatalf("the redelivered late result = %+v (%v), want it recorded on the canceled task", cur, err)
	}
	if ev := lastLedgerPayload(t, req, EvResultAccepted); ev["after_terminal"] != true {
		t.Fatalf("evidence %v, want after_terminal", ev)
	}
}

// 0017 Q5: a new task sent while the relay is down is recorded and
// answered submitted; the delegation is delivered once the hub carries mail
// again, and a retry of the client's message is the same task.
func TestASendWhileTheRelayIsDownIsSubmittedAndDelivered(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
	send := module.TaskSend{Message: textMsg("plan a trip", "ctx-q5", "m-q5"), ReturnImmediately: true}
	task, err := seam.Send(ctx, prov.AID(), send)
	if err != nil || task.Status.State != a2ashape.TaskStateSubmitted {
		t.Fatalf("send with the relay down = %s (%v), want submitted", task.Status.State, err)
	}
	rows, err := req.ix.Outbox(task.ID)
	if err != nil || len(rows) != 1 || rows[0].Type != seal.TypeDelegate || len(rows[0].Envelope) == 0 {
		t.Fatalf("outbox = %+v (%v), want the sealed delegation", rows, err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("%d delegations reached the hub while it was down", n)
	}
	again, err := seam.Send(ctx, prov.AID(), send)
	if err != nil || again.ID != task.ID {
		t.Fatalf("retry = %s (%v), want %s", again.ID, err, task.ID)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = false })
	if err := req.deliverQueued(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("%d delegations delivered, want 1", n)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := prov.ix.Get(task.ID); err != nil || got.Goal != "plan a trip" {
		t.Fatalf("provider's task = %+v (%v)", got, err)
	}

	// The same over the control plane: /tasks/send answers 200 submitted.
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
	p := newPlaneFor(t, req, "tok")
	resp, raw := p.req(t, "POST", "/tasks/send", `{"to":"`+prov.AID()+`","text":"hi","return_immediately":true}`, p.bearer)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), string(a2ashape.TaskStateSubmitted)) {
		t.Fatalf("/tasks/send with the relay down: %d %s", resp.StatusCode, raw)
	}
}

// 0017 Q5: a delegation the queue gives up on fails the task with
// anet.reason=undeliverable, and the A2A view says so.
func TestAnUndeliveredDelegationFailsTheTask(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
	task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("hello", "", "m-x"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := req.ix.Outbox(task.ID)
	if len(rows) != 1 {
		t.Fatalf("outbox = %+v", rows)
	}
	now := req.nowMS()
	req.clock = func() uint64 { return now + messageLifetimeMS + 60_000 }
	if err := req.deliverQueued(ctx, rows[0].ID); err != nil && !errors.Is(err, errUndeliverable) {
		t.Fatal(err)
	}
	got, err := seam.Get(ctx, prov.AID(), task.ID, nil)
	if err != nil || got.Status.State != a2ashape.TaskStateFailed || got.Metadata[a2ashape.KeyReason] != a2ashape.ReasonUndeliverable {
		t.Fatalf("task after its delegation expired = %s %v (%v)", got.Status.State, got.Metadata, err)
	}
	if ev := lastLedgerPayload(t, req, EvDeliveryExpired); ev["reason"] != undeliveredExpired || ev["interaction_id"] != task.ID {
		t.Fatalf("evidence %v", ev)
	}
	// The client's retry of the message is a new attempt.
	req.clock = nil
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = false })
	retry, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("hello", "", "m-x"), ReturnImmediately: true})
	if err != nil || retry.ID == task.ID {
		t.Fatalf("retry = %s (%v), want a new task", retry.ID, err)
	}
}

// 0017 Q5: an end request and a follow-up are recorded and queued while the
// relay is down, and delivered after; an undelivered cancel does not fail
// the task it canceled.
func TestRequesterMessagesGoThroughTheQueue(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
	if err := req.SendMessage(ctx, id, "more detail", nil); err != nil {
		t.Fatalf("follow-up with the relay down: %v", err)
	}
	if err := req.RequestEnd(ctx, id); err != nil {
		t.Fatalf("end request with the relay down: %v", err)
	}
	rows, _ := req.ix.Outbox(id)
	if len(rows) != 2 {
		t.Fatalf("outbox = %+v, want the follow-up and the end request", rows)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = false })
	for _, r := range rows {
		if err := req.deliverQueued(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, req, id); st != interactions.StateCompleted {
		t.Fatalf("requester state after the end request = %s, want completed", st)
	}

	// A cancel that cannot be delivered leaves the canceled task canceled.
	id2, err := req.Delegate(ctx, prov.AID(), "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := req.CancelTask(ctx, id2); err != nil {
		t.Fatal(err)
	}
	now := req.nowMS()
	req.clock = func() uint64 { return now + messageLifetimeMS + 60_000 }
	req.flushOutboxAt(ctx, int64(req.nowMS()))
	if st := stateOf(t, req, id2); st != interactions.StateCanceled {
		t.Fatalf("state after an undelivered cancel = %s", st)
	}
}

// 0017 Q9: both sides record a message under the envelope's message id in
// hex — the opening goal, a follow-up and a status — and the A2A views
// name it alike.
func TestBothSidesNameAMessageByItsEnvelopeID(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "the goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendStatus(ctx, id, interactions.StateWorking, "on it", nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := req.SendMessage(ctx, id, "and a detail", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ids := func(d *Daemon) map[string]string {
		t.Helper()
		msgs, err := d.ix.Messages(id)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, m := range msgs {
			key := m.Kind + ":" + m.Body
			if _, err := hex.DecodeString(m.MsgID); err != nil || len(m.MsgID) != 2*seal.MIDLen {
				t.Fatalf("%s message %q has msg_id %q, want the envelope id in hex", d.AID(), key, m.MsgID)
			}
			out[key] = m.MsgID
		}
		return out
	}
	mine, theirs := ids(req), ids(prov)
	for _, key := range []string{"text:the goal", "status:on it", "text:and a detail"} {
		if mine[key] == "" || mine[key] != theirs[key] {
			t.Fatalf("%s: requester %q, provider %q", key, mine[key], theirs[key])
		}
	}
	// The projections carry the same ids.
	reqView, err := req.taskView(mustIX(t, req, id), viewOpts{})
	if err != nil {
		t.Fatal(err)
	}
	provView, err := prov.taskView(mustIX(t, prov, id), viewOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reqView.History) == 0 || len(provView.History) == 0 || reqView.History[0].ID != provView.History[0].ID ||
		reqView.History[0].ID != mine["text:the goal"] {
		t.Fatalf("history ids: requester %+v provider %+v", reqView.History, provView.History)
	}
}

func mustIX(t *testing.T, d *Daemon, id string) *interactions.Interaction {
	t.Helper()
	ix, err := d.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// A retry is never scheduled past the row's deadline: the row is abandoned,
// and the task it leaves waiting failed, when the deadline passes, not up
// to a backoff or a Retry-After later (0017 Q5).
func TestARetryIsNotScheduledPastTheDeadline(t *testing.T) {
	_, req, _ := registeredPair(t)
	now := req.nowMS()
	far := int64(math.MaxInt64 / 2) // out of the background loop's way
	id, err := req.ix.EnqueueOutbox(interactions.OutboxItem{IX: "ix_dl", ToAID: "did:anet:x", Type: seal.TypeStatus,
		Envelope: []byte("env"), Exp: now + 10_000, NextAt: far})
	if err != nil {
		t.Fatal(err)
	}
	it, err := req.ix.GetOutbox(id)
	if err != nil {
		t.Fatal(err)
	}
	_ = req.rescheduleOutbox(it, &hubError{path: "/relay/send", code: http.StatusTooManyRequests, retryAfter: "3600"})
	if got, _ := req.ix.GetOutbox(id); got == nil || got.NextAt != int64(it.Exp)+1 {
		t.Fatalf("next attempt %+v, want right after the deadline %d", got, it.Exp)
	}
}

// A requester's follow-up the hub refuses for good on its first attempt is
// recorded, fails the task (anet.reason=undeliverable) and is an error for
// the caller, as a refused delegation is; over the task surface the failed
// task is the answer.
func TestARefusedFollowUpFailsTheTaskAndSaysSo(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.maxEnvelope = 1 })
	if err := req.SendMessage(ctx, id, "too big for this hub", nil); !errors.Is(err, errUndeliverable) {
		t.Fatalf("refused follow-up: %v, want errUndeliverable", err)
	}
	if st := stateOf(t, req, id); st != interactions.StateFailed {
		t.Fatalf("state after a refused follow-up = %s", st)
	}
	if m := resultMetaOf(t, req, id); m[a2ashape.KeyReason] != a2ashape.ReasonUndeliverable {
		t.Fatalf("result meta %v", m)
	}

	setFake(t, srv.URL, func(h *fakeHub) { h.maxEnvelope = 0 })
	seam := req.TaskSeam()
	task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("second", "", "m-a"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.maxEnvelope = 1 })
	got, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: followUp(task.ID, "too big", "m-b"), ReturnImmediately: true})
	if err != nil || got.Status.State != a2ashape.TaskStateFailed || got.Metadata[a2ashape.KeyReason] != a2ashape.ReasonUndeliverable {
		t.Fatalf("refused follow-up over the task surface = %s %v (%v)", got.Status.State, got.Metadata, err)
	}
}

// 0017 Q9 for a delegation held for approval: the provider records the goal
// and the held follow-ups, once approved, under the ids the requester
// recorded them under.
func TestAnApprovedDelegationKeepsTheRequestersMessageIDs(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	s := registered(t, srv.URL, "stranger")
	setPolicy(t, prov, PolicyApprove)
	id, err := s.Delegate(ctx, prov.AID(), "held goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SendMessage(ctx, id, "held detail", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestEnd(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ApprovePending(id); err != nil {
		t.Fatal(err)
	}
	ids := func(d *Daemon) map[string]string {
		t.Helper()
		msgs, err := d.ix.Messages(id)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, m := range msgs {
			out[m.Kind+":"+m.Body] = m.MsgID
		}
		return out
	}
	mine, theirs := ids(s), ids(prov)
	for _, key := range []string{"text:held goal", "text:held detail", "end_request:"} {
		if len(mine[key]) != 2*seal.MIDLen || mine[key] != theirs[key] {
			t.Fatalf("%s: requester %q, provider %q (requester %v, provider %v)", key, mine[key], theirs[key], mine, theirs)
		}
	}
}

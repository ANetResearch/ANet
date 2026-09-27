package daemon

// Tests of B3-02 (wire_edges.go): a stop does not answer the calls it cut
// off (SI-10); calls an earlier process left open are recovered at start
// and on redelivery (§3.6); a denied peer writing to an unknown task is
// answered like a stranger (X2); every auto-reply backend reads the deny
// list per turn (§5.1); follow-ups held for approval keep their kinds
// (§5.3).

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/provider"
)

// stopAwareCap is served by stopAware.
const stopAwareCap = "wait.stop"

// stopAware is a short capability that honors its context: with block set
// it waits until the context ends and returns its error.
type stopAware struct {
	block   bool
	started chan struct{}
	calls   atomic.Int32
}

func (p *stopAware) ID() string { return "stopaware" }
func (p *stopAware) Capabilities(context.Context) ([]string, error) {
	return []string{stopAwareCap}, nil
}
func (p *stopAware) Describe(context.Context) (string, error) { return "", nil }
func (p *stopAware) Health(context.Context) error             { return nil }
func (p *stopAware) Invoke(ctx context.Context, _ provider.Call) (effect.Effect, error) {
	p.calls.Add(1)
	if !p.block {
		return effect.Effect{Status: effect.OK, Record: &tsir.EffectRecord{}}, nil
	}
	p.started <- struct{}{}
	<-ctx.Done()
	return effect.Effect{}, ctx.Err()
}

// quietPair is registeredPair with both relay loops stopped, so the test
// alone decides what is received when.
func quietPair(t *testing.T) (*httptest.Server, *Daemon, *Daemon) {
	t.Helper()
	srv, req, prov := registeredPair(t)
	req.stopRelayLoop()
	prov.stopRelayLoop()
	return srv, req, prov
}

// restartDaemon closes d and starts a daemon on its data directory, with
// the relay loop stopped (reopen, without the payment tests' build tag).
func restartDaemon(t *testing.T, d *Daemon) *Daemon {
	t.Helper()
	layout := d.layout
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	n.stopRelayLoop()
	t.Cleanup(func() { n.Close() })
	return n
}

// storeAfterStop opens the interactions store a stopped daemon left, to
// read what it recorded.
func storeAfterStop(t *testing.T, l Layout) *interactions.Store {
	t.Helper()
	st, err := interactions.Open(l.InteractionsDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// capabilityDoc is a signed TaskDoc calling capID, as a requester sends it.
func capabilityDoc(t *testing.T, c *identity.Controller, capID string) []byte {
	t.Helper()
	goal := "invoke capability " + capID
	td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{{
		Intent:   tsir.Intent{Summary: goal, Body: goal},
		Requires: []tsir.Require{{ID: capID, Type: RequireTypeCapability, Necessity: "must"}},
		Contexts: []tsir.Context{{Key: "args", Value: "{}", Format: "json"}},
	}}}
	if err := td.Sign(c); err != nil {
		t.Fatal(err)
	}
	doc, err := coredet.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// leftOpen records an inbound capability call from req on prov, and the
// requester's side of it, as a process that stopped right after
// committing the delegation leaves them: submitted, nothing run.
func leftOpen(t *testing.T, req, prov *Daemon, id, capID string) {
	t.Helper()
	goal := "invoke capability " + capID
	if err := prov.ix.Create(interactions.New{ID: id, Role: interactions.RoleInbound, PeerAID: req.AID(),
		Goal: goal, RequestDoc: capabilityDoc(t, req.self, capID), IsCapability: true,
		Trust: interactions.TrustPeer, TaskNonce: "bm9uY2U"}); err != nil {
		t.Fatal(err)
	}
	if err := req.ix.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: prov.AID(),
		Goal: goal, IsCapability: true}); err != nil {
		t.Fatal(err)
	}
}

// wasInterrupted fails the test unless ix ended failed, effect UNVERIFIED,
// anet.reason=interrupted, with a receipt.
func wasInterrupted(t *testing.T, d *Daemon, id string) {
	t.Helper()
	pix, err := d.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	var res capabilityResult
	_ = json.Unmarshal(pix.Result, &res)
	meta := decodeMeta([]byte(pix.ResultMeta))
	if pix.State != interactions.StateFailed || res.Status != string(effect.Unverified) ||
		meta["anet.reason"] != "interrupted" || len(pix.Receipt) == 0 {
		t.Fatalf("%s: state %s, effect %q, meta %v, receipt %d bytes; want failed/UNVERIFIED/interrupted",
			id, pix.State, res.Status, meta, len(pix.Receipt))
	}
}

// SI-10: a short capability call the daemon's stop cuts off is a
// temporary failure. Nothing is answered — no result, no receipt, nothing
// queued — the delegation is not acknowledged, and the next process runs
// the call when the envelope comes again.
func TestAStopDoesNotAnswerAShortCallItCutOff(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	p := &stopAware{block: true, started: make(chan struct{}, 1)}
	if err := prov.Providers().Register(ctx, p); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), stopAwareCap, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	got := make(chan rxResult, 1)
	if !prov.goBackground(func() { got <- prov.receiveEnvelope(ctx, env) }) {
		t.Fatal("daemon already stopping")
	}
	<-p.started
	layout := prov.layout
	if err := prov.Close(); err != nil {
		t.Fatal(err)
	}
	if r := <-got; r.class != rxTransient || r.reason != transientStopping {
		t.Fatalf("the cut-off delegation: %+v, want not acknowledged (%s)", r, transientStopping)
	}
	st := storeAfterStop(t, layout)
	pix, err := st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := st.Outbox(id)
	st.Close()
	if pix.IsTerminal() || len(pix.Result) > 0 || len(pix.Receipt) > 0 || len(rows) != 0 {
		t.Fatalf("the stop answered the call: state %s, result %q, receipt %d bytes, %d queued",
			pix.State, pix.Result, len(pix.Receipt), len(rows))
	}
	if n := len(queuedFor(t, srv, req.AID())); n != 0 {
		t.Fatalf("%d messages sent to the requester for a call the stop cut off", n)
	}

	prov2 := restartDaemon(t, prov)
	again := &stopAware{}
	if err := prov2.Providers().Register(ctx, again); err != nil {
		t.Fatal(err)
	}
	if r := receive(t, prov2, env); r.reason != dropDuplicate {
		t.Fatalf("the redelivered delegation: %+v", r)
	}
	if pix, _ := prov2.ix.Get(id); pix.State != interactions.StateCompleted || len(pix.Receipt) == 0 || again.calls.Load() != 1 {
		t.Fatalf("after the redelivery: state %s, receipt %d bytes, ran %d times", pix.State, len(pix.Receipt), again.calls.Load())
	}
}

// A long call cut off by the stop records nothing either; the next start
// reports it interrupted (UNVERIFIED), not as the FAILED effect the
// provider returned when its context ended.
func TestAStopLeavesALongCallToStartupRecovery(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	gate := newGateProvider()
	if err := prov.Providers().Register(ctx, gate); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), gateCap, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	<-gate.started
	layout := prov.layout
	if err := prov.Close(); err != nil {
		t.Fatal(err)
	}
	if !gate.wasCanceled() {
		t.Fatal("the stop did not reach the running call")
	}
	st := storeAfterStop(t, layout)
	pix, _ := st.Get(id)
	rows, _ := st.Outbox(id)
	st.Close()
	if pix == nil || pix.State != interactions.StateWorking || len(pix.Receipt) > 0 || len(rows) != 0 {
		t.Fatalf("the stop answered the long call: %+v, %d queued", pix, len(rows))
	}
	prov2 := restartDaemon(t, prov)
	wasInterrupted(t, prov2, id)
	waitUntil(t, "the interrupted result to reach the hub", func() bool {
		return len(queuedFor(t, srv, req.AID())) == 1
	})
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, req, id); st != interactions.StateFailed {
		t.Fatalf("requester state %s", st)
	}
}

// §3.6 startup recovery: a long call recorded and never marked working —
// the process stopped between committing the delegation and starting it —
// is reported interrupted, like one that was working. A short call so left
// waits for its redelivery; a call waiting on a payment is not touched.
func TestALongCallRecordedButNeverStartedIsReportedAtStart(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	gate := newGateProvider()
	lamp := &lampProvider{}
	for _, p := range []provider.CapabilityProvider{gate, lamp} {
		if err := prov.Providers().Register(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	leftOpen(t, req, prov, "ix_long_left", gateCap)
	leftOpen(t, req, prov, "ix_short_left", lampCap)
	leftOpen(t, req, prov, "ix_long_quoted", gateCap)
	if _, err := prov.ix.SetPayment("ix_long_quoted", interactions.PayUpdate{
		State: interactions.PayState(interactions.PayRequired)}); err != nil {
		t.Fatal(err)
	}
	// A short call that was working: at-least-once, it runs again.
	leftOpen(t, req, prov, "ix_short_working", lampCap)
	if _, err := prov.ix.SetState("ix_short_working", interactions.StateWorking); err != nil {
		t.Fatal(err)
	}
	// What New does once the modules have registered their providers.
	prov.recoverInterrupted()

	wasInterrupted(t, prov, "ix_long_left")
	for _, id := range []string{"ix_short_left", "ix_long_quoted"} {
		if st := stateOf(t, prov, id); st != interactions.StateSubmitted {
			t.Errorf("%s: %s, want left submitted", id, st)
		}
	}
	waitUntil(t, "the short call to run again", func() bool {
		return stateOf(t, prov, "ix_short_working") == interactions.StateCompleted
	})
	gate.mu.Lock()
	calls := gate.calls
	gate.mu.Unlock()
	if calls != 0 || len(lamp.invoked) != 1 || lamp.invoked[0].CallID != "ix_short_working" {
		t.Fatalf("recovery ran: long %d, short %+v; want only the working short call", calls, lamp.invoked)
	}
	waitUntil(t, "both answers to reach the hub", func() bool {
		return len(queuedFor(t, srv, req.AID())) == 2
	})
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, req, "ix_long_left"); st != interactions.StateFailed {
		t.Fatalf("requester state %s", st)
	}
}

// The same window when startup recovery could not tell the call was long
// (its provider registered after the start): the redelivered delegation,
// which the stop left unacknowledged, reports it interrupted rather than
// leaving it submitted for good. It is not run.
func TestARedeliveryReportsALongCallAnEarlierProcessNeverStarted(t *testing.T) {
	_, req, prov := quietPair(t)
	const id = "ix_never_started"
	leftOpen(t, req, prov, id, gateCap)
	prov2 := restartDaemon(t, prov)
	if st := stateOf(t, prov2, id); st != interactions.StateSubmitted {
		t.Fatalf("with no provider registered, startup recovery decided %s", st)
	}
	gate := newGateProvider()
	if err := prov2.Providers().Register(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	env := sealFrom(t, req, prov2, seal.TypeDelegate, id, delegateBody(t, req.self, id, "", gateCap))
	if r := receive(t, prov2, env); r.class != rxAccepted {
		t.Fatalf("redelivery: %+v", r)
	}
	wasInterrupted(t, prov2, id)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.calls != 0 {
		t.Fatalf("the long call ran %d times on redelivery", gate.calls)
	}
}

// X2: under closed, a denied peer writing to a task this node does not hold
// gets what a stranger gets — held inside the wait window, then
// TaskNotFound from the same notice limiter — so it cannot tell it is
// denied. On a task it does hold, its messages are still dropped.
func TestADeniedPeerWritingToAnUnknownTaskIsAnsweredLikeAStranger(t *testing.T) {
	srv := newFakeHub(t)
	prov := registered(t, srv.URL, "prov")
	denied := registered(t, srv.URL, "denied")
	stranger := registered(t, srv.URL, "stranger")
	denyPeers(t, prov, denied.AID())
	const ix = "ix_unknown_to_provider"
	type outcome struct {
		early, late rxResult
		state       interactions.State
		meta        map[string]any
	}
	send := func(from *Daemon) outcome {
		t.Helper()
		if err := from.ix.Put(ix, interactions.RoleOutbound, prov.AID(), "never delivered", "", nil); err != nil {
			t.Fatal(err)
		}
		var o outcome
		o.early = receive(t, prov, craft(t, senderOf(from), prov, seal.TypeMessage, ix, chatBody(t, "early", ""), nil))
		o.late = receive(t, prov, craft(t, senderOf(from), prov, seal.TypeMessage, ix, chatBody(t, "late", ""),
			func(in *seal.SealedInner) { in.TS -= uint64((unknownIXWait + time.Minute).Milliseconds()) }))
		o.state, o.meta = lastStatusMeta(t, from, ix)
		return o
	}
	s, d := send(stranger), send(denied)
	if s.early.reason != transientUnknownIX || s.late.reason != dropUnknownIX || s.meta["anet.a2aError"] != "TaskNotFoundError" {
		t.Fatalf("stranger: early %+v, late %+v, answer %s %v", s.early, s.late, s.state, s.meta)
	}
	if d.early != s.early || d.late != s.late || d.state != s.state ||
		string(mustJSON(t, d.meta)) != string(mustJSON(t, s.meta)) {
		t.Fatalf("denied peer: early %+v, late %+v, answer %s %v; a stranger: %+v, %+v, %s %v",
			d.early, d.late, d.state, d.meta, s.early, s.late, s.state, s.meta)
	}
	if n := counter(prov, noticeTaskNotFound); n != 2 {
		t.Fatalf("%d TaskNotFound notices, want one each", n)
	}
}

// A2A-DESIGN §5.1: the deny list is read on every auto-reply turn whatever
// the backend. The OpenAI-compatible backend does not answer a peer denied
// since its message landed, before any revocation sweep.
func TestTheOpenAIBackendReadsTheDenyListEveryTurn(t *testing.T) {
	api := &fakeOpenAI{reply: "hello"}
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api)
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "first question", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if n := api.calls.Load(); n != 1 {
		t.Fatalf("backend called %d times for an allowed peer, want 1", n)
	}
	if err := f.req.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.req.SendMessage(f.ctx, id, "second question", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.prov.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	denyPeers(t, f.prov, f.req.AID()) // an edit outside the CLI; no sweep has run
	f.prov.autoReplyOnce(f.ctx, f.cfg, f.replier)
	if n := api.calls.Load(); n != 1 {
		t.Fatalf("backend called %d times after the deny, want still 1", n)
	}
}

// tapEvents subscribes to d's events for ix, whether or not ix exists yet.
func tapEvents(d *Daemon, ix string) <-chan Event {
	s := &subscriber{ch: make(chan Event, eventBufferSize)}
	d.bus.mu.Lock()
	defer d.bus.mu.Unlock()
	if d.bus.subs == nil {
		d.bus.subs = map[string]map[*subscriber]struct{}{}
	}
	if d.bus.subs[ix] == nil {
		d.bus.subs[ix] = map[*subscriber]struct{}{}
	}
	d.bus.subs[ix][s] = struct{}{}
	return s.ch
}

// messageEvents drains the message events buffered on ch.
func messageEvents(ch <-chan Event) []string {
	var kinds []string
	for {
		select {
		case e := <-ch:
			if e.Kind == EventMessage {
				kinds = append(kinds, e.MsgKind)
			}
		default:
			return kinds
		}
	}
}

// §5.3: follow-ups held with an approval-queue item are recorded, on
// approval, as the receive path records them on a live task: a message
// carrying x402.payment.status is a payment message (metadata only on a
// capability call), and each stored message is published.
func TestApprovedFollowUpsKeepTheirKinds(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	s := registered(t, srv.URL, "stranger")
	setPolicy(t, prov, PolicyApprove)
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	payment := func(id, body string) {
		t.Helper()
		meta, _ := json.Marshal(map[string]any{x402a2a.KeyStatus: x402a2a.StatusRejected})
		b, err := (&delegation.ChatMsg{Kind: delegation.ChatText, Body: body, MsgID: "msg_pay_" + id,
			Metadata: meta}).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if r := receive(t, prov, sealFrom(t, s, prov, seal.TypeMessage, id, b)); r.class != rxAccepted {
			t.Fatalf("held payment message: %+v", r)
		}
	}
	kinds := func(id string) (out []string, bodies []string) {
		msgs, err := prov.ix.Messages(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			out, bodies = append(out, m.Kind), append(bodies, m.Body)
		}
		return out, bodies
	}

	// A natural-language task.
	text, err := s.Delegate(ctx, prov.AID(), "held goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SendMessage(ctx, text, "held detail", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	payment(text, "about the price")
	events := tapEvents(prov, text)
	if _, err := prov.ApprovePending(text); err != nil {
		t.Fatal(err)
	}
	got, bodies := kinds(text)
	want := []string{interactions.MsgText, interactions.MsgText, interactions.MsgPayment}
	if string(mustJSON(t, got)) != string(mustJSON(t, want)) || bodies[2] != "about the price" {
		t.Fatalf("stored kinds %v bodies %q, want %v", got, bodies, want)
	}
	if ev := messageEvents(events); string(mustJSON(t, ev)) != string(mustJSON(t, want)) {
		t.Fatalf("published message events %v, want %v", ev, want)
	}

	// A capability call: the payment message keeps its metadata only.
	call, err := s.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	payment(call, "not stored on a capability call")
	if _, err := prov.ApprovePending(call); err != nil {
		t.Fatal(err)
	}
	msgs, _ := prov.ix.Messages(call)
	var pay *interactions.Message
	for i := range msgs {
		if msgs[i].Kind == interactions.MsgPayment {
			pay = &msgs[i]
		}
	}
	if pay == nil || pay.Body != "" || !hasPaymentStatus([]byte(pay.Metadata)) {
		t.Fatalf("capability call's held payment message: %+v", pay)
	}
}

// SI-10 on the "not served" answer: a delegation for a capability with no
// provider, reached after the stop began, is not answered and not
// acknowledged, rather than acknowledged with nothing recorded — which
// would leave it open with no redelivery to come.
func TestAStopBeforeAnUnservedCallIsAnsweredLeavesItUnacknowledged(t *testing.T) {
	_, req, prov := quietPair(t)
	const id = "ix_unserved_at_stop"
	leftOpen(t, req, prov, id, "no.such.cap")
	prov.cancel() // the stop has begun; Close runs at cleanup
	if prov.runCapabilityCall(id, "no.such.cap", map[string]any{}, nil, nil) {
		t.Fatal("a call the stop cut off was reported handled, so its delegation would be acknowledged")
	}
	pix, err := prov.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := prov.ix.Outbox(id)
	if pix.IsTerminal() || len(pix.Result) > 0 || len(rows) != 0 {
		t.Fatalf("the stop answered: state %s, result %q, %d queued", pix.State, pix.Result, len(rows))
	}
}

// §3.6 startup recovery beyond redelivery: an approved call has no
// redelivery to come (its delegation was acknowledged when it was held),
// so one left submitted runs at start; a quoted call is not reported
// interrupted however it came to be working (it never ran: priced work
// runs once paid); unpaid work of a peer denied since is left to the
// revocation sweep, not run.
func TestStartupRecoveryRunsApprovedCallsAndLeavesUnpaidOrDeniedOnes(t *testing.T) {
	_, req, prov := quietPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	create := func(id, peer, trust string, st interactions.State, pay string) {
		t.Helper()
		if err := prov.ix.Create(interactions.New{ID: id, Role: interactions.RoleInbound, PeerAID: peer,
			Goal: "invoke capability " + lampCap, RequestDoc: capabilityDoc(t, req.self, lampCap), IsCapability: true,
			Trust: trust, TaskNonce: "bm9uY2U"}); err != nil {
			t.Fatal(err)
		}
		if st != interactions.StateSubmitted {
			if _, err := prov.ix.SetState(id, st); err != nil {
				t.Fatal(err)
			}
		}
		if pay != "" {
			if _, err := prov.ix.SetPayment(id, interactions.PayUpdate{State: interactions.PayState(pay)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	const deniedAID = "did:anet:denied-since"
	create("ix_approved_left", req.AID(), interactions.TrustApproved, interactions.StateSubmitted, "")
	create("ix_quoted_working", req.AID(), interactions.TrustPeer, interactions.StateWorking, interactions.PayRequired)
	create("ix_failed_pay_working", req.AID(), interactions.TrustPeer, interactions.StateWorking, interactions.PayFailed)
	create("ix_denied_working", deniedAID, interactions.TrustPeer, interactions.StateWorking, "")
	if err := req.ix.Create(interactions.New{ID: "ix_approved_left", Role: interactions.RoleOutbound,
		PeerAID: prov.AID(), Goal: "invoke capability " + lampCap, IsCapability: true}); err != nil {
		t.Fatal(err)
	}
	denyPeers(t, prov, deniedAID)

	prov.recoverInterrupted()
	waitUntil(t, "the approved call to run", func() bool {
		return stateOf(t, prov, "ix_approved_left") == interactions.StateCompleted
	})
	for _, id := range []string{"ix_quoted_working", "ix_failed_pay_working", "ix_denied_working"} {
		if pix := getIXOf(t, prov, id); pix.State != interactions.StateWorking || len(pix.Receipt) > 0 {
			t.Errorf("%s: %s with %d receipt bytes; want left working, unanswered", id, pix.State, len(pix.Receipt))
		}
	}
	// Recovery decides before it returns; only the approved call was
	// started, and it has finished.
	if len(lamp.invoked) != 1 || lamp.invoked[0].CallID != "ix_approved_left" {
		t.Fatalf("recovery ran %+v; want only the approved call", lamp.invoked)
	}
}

// getIXOf reads an interaction (getIX lives with the payment tests).
func getIXOf(t *testing.T, d *Daemon, id string) *interactions.Interaction {
	t.Helper()
	ix, err := d.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// denyDuring is a replier through which the peer is denied while the
// backend works.
type denyDuring struct {
	inner autoReplier
	deny  func()
}

func (r denyDuring) Reply(ctx context.Context, rc replyContext, turns []chatTurn) (string, error) {
	r.deny()
	return r.inner.Reply(ctx, rc, turns)
}

// §5.1: a peer denied while the backend was producing its reply is not
// sent that reply.
func TestAReplyIsNotSentToAPeerDeniedWhileTheBackendWorked(t *testing.T) {
	api := &fakeOpenAI{reply: "hello"}
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api)
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "a question", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.prov.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	replier := denyDuring{inner: f.replier, deny: func() { denyPeers(t, f.prov, f.req.AID()) }}
	f.prov.autoReplyOnce(f.ctx, f.cfg, replier)
	if n := api.calls.Load(); n != 1 {
		t.Fatalf("backend called %d times, want 1", n)
	}
	msgs, err := f.prov.ix.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.SenderAID == f.prov.AID() {
			t.Fatalf("a reply went to the peer denied during the call: %+v", m)
		}
	}
}

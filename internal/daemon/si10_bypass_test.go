package daemon

// Regression tests for ways around the SI-10 delivery fixes (F12, F22-F25,
// F27, F28, F30, Q29) found in the adversarial review of those fixes. Each
// asserts that the bypass fails.

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// directTo is a direct transport to one daemon: Send hands the envelope to
// its Inbound, as module/p2p and tools/anetpeer do, and reports what the
// receiving daemon answered. While refuse is above zero it refuses that
// many sends before any daemon sees them, as a peer process does whose
// receiving side is over its rate limit (a nack, not_delivered).
type directTo struct {
	to     *Daemon
	refuse atomic.Int32
	sent   atomic.Int32
}

func (p *directTo) Name() string                           { return "p2p-direct" }
func (p *directTo) Reachable(context.Context, string) bool { return true }
func (p *directTo) Send(ctx context.Context, _ string, env []byte) ([]byte, bool, error) {
	if p.refuse.Add(-1) >= 0 {
		return nil, false, errors.New("p2p: receiving daemon refused the delivery for now")
	}
	p.refuse.Store(0)
	p.sent.Add(1)
	dack, err := p.to.Inbound().Receive(ctx, env)
	return dack, true, err
}

// [redteam:F25][redteam:F23] bypass: a follow-up from a sender the provider
// has no standing relationship with (a stranger's task under inbound policy
// open, a public capability call) overtakes its own delegation by taking
// another path. The requester's outbox keeps a task's messages in order,
// but order on one path is not order on arrival: the delegation was turned
// away by the direct path (the provider's peer process over its limit) and
// went into the provider's hub mailbox; the follow-up then went direct, and
// arrived first. Answered TaskNotFound there at once (the F25 rule for
// strangers), it was lost — the provider never saw it, the requester's task
// was failed by the notice — while the delegation behind it was taken and
// worked on. Over a direct path a message for a task the node does not
// know is refused for now instead, whoever sent it: it goes through the hub
// after the delegation already there, and arrives in order. And the
// requester sends no follow-up at all until its delegation is confirmed
// (engine ENG-O2): the follow-up cannot overtake it on any path.
func TestADirectFollowUpThatOvertakesItsDelegationArrivesAfterIt(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	setPolicy(t, prov, PolicyOpen)
	direct := &directTo{to: prov}
	direct.refuse.Store(1) // the delegation is turned away by the direct path
	stranger.RegisterTransport(direct)

	id, err := stranger.Delegate(ctx, prov.AID(), "a task for anyone", nil)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("setup: the hub holds %d envelopes for the provider, want the delegation", n)
	}
	if err := stranger.SendMessage(ctx, id, "and one more thing", nil); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	if direct.sent.Load() != 0 {
		t.Fatalf("the follow-up went before its delegation was confirmed")
	}
	if rows := outboxRows(t, stranger, id); len(rows) != 2 {
		t.Fatalf("the requester's queue: %+v, want the delegation and the follow-up waiting behind it", rows)
	}
	// The provider collects its mail (the delegation), the requester hears
	// it arrived, and the follow-up goes, here over the direct path, whose
	// ack carries its token (engine ENG-V2): confirmed there.
	settleDelivery(t, stranger, prov)
	if rows := outboxRows(t, stranger, id); len(rows) > 0 {
		if err := stranger.deliverQueued(ctx, rows[0].ID); err != nil {
			t.Fatalf("follow-up after the confirmation: %v", err)
		}
	}
	waitUntil(t, "the follow-up to go and be confirmed", func() bool { return len(outboxRows(t, stranger, id)) == 0 })
	if direct.sent.Load() != 1 {
		t.Fatalf("the follow-up went %d times over the direct path, want once", direct.sent.Load())
	}
	if n := counter(prov, dropUnknownIX); n != 0 {
		t.Fatalf("the direct follow-up was answered TaskNotFound and dropped (%v)", prov.ReceiveStats())
	}
	msgs, err := prov.ix.Messages(id)
	if err != nil {
		t.Fatalf("the provider does not have the task: %v", err)
	}
	var bodies []string
	for _, m := range msgs {
		if m.Kind == interactions.MsgText {
			bodies = append(bodies, m.Body)
		}
	}
	if len(bodies) != 2 || bodies[1] != "and one more thing" {
		t.Fatalf("the provider's conversation is %q, want the goal then the follow-up", bodies)
	}
	// Nothing told the requester its task does not exist.
	if err := stranger.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, _ := stranger.ix.Get(id); cur.IsTerminal() {
		t.Fatalf("the requester's task ended (%s): a TaskNotFound notice reached it", cur.State)
	}
	all, _ := stranger.ix.Messages(id)
	for _, m := range all {
		if m.Kind == interactions.MsgStatus && decodeMeta([]byte(m.Metadata))["anet.a2aError"] == "TaskNotFoundError" {
			t.Fatalf("the requester was told TaskNotFound: %+v", m)
		}
	}
}

// [redteam:F24] bypass: the cursor fix sends a round back to the head of the
// mailbox every relayHeadEvery rounds, and the rounds in between read on
// from where the last one stopped. A mailbox holding more than
// relayHeadEvery+1 pages of held envelopes was therefore never read past
// them: the forward read was reset to the head before it got there, and
// everything queued behind them — every new message for this node — waited
// until the held ones turned permanent, however often the node polled. Pages
// are short when envelopes are large (the hub's poll byte budget), so a few
// dozen held envelopes are enough — here signed by a sender whose witness
// the node does not know yet (step 6½, witness-unknown). Now the rounds that
// go back over held envelopes keep their own place, and the rounds between
// them read on from the forward cursor, which never goes back.
func TestHeldPagesDoNotStarveNewMail(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	peer, _ := unknownWitnessSender(t)
	const held = 2*relayHeadEvery + 4
	for i := 0; i < held; i++ {
		injectEnvelope(t, srv, prov.AID(), craft(t, peer, prov, seal.TypeMessage,
			"ix_early_"+time.Duration(i).String(), chatBody(t, "x", ""), nil))
	}
	// One envelope per page, as for large envelopes.
	setFake(t, srv.URL, func(h *fakeHub) { h.pollPage = 1 })
	id, err := req.Delegate(ctx, prov.AID(), "new work", nil)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	const rounds = 3 * held
	takenAt := -1
	for i := 0; i < rounds && takenAt < 0; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := prov.ix.Get(id); err == nil {
			takenAt = i
		}
	}
	if takenAt < 0 {
		t.Fatalf("the new delegation was not read in %d rounds behind %d held envelopes (%v)", rounds, held, prov.ReceiveStats())
	}
	// The held envelopes are still gone back to, page by page: each is
	// read again within two rounds per page. Each one, not reads adding up
	// to their number: rounds that go back only ever as far as the head
	// page read its one envelope again and again, as many times, and left
	// the rest where they were (mutation fx24-2, an internal note).
	heldIDs := mailboxIDsFor(srv.URL, prov.AID())
	before := counter(prov, seal.ReasonWitnessUnknown)
	servedBefore := len(relayServedFor(srv.URL, prov.AID()))
	for i := 0; i < 2*held+2; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := counter(prov, seal.ReasonWitnessUnknown) - before; n < held {
		t.Fatalf("%d held envelopes read %d times in %d rounds; not every one was gone back to", held, n, 2*held+2)
	}
	readAgain := map[int64]bool{}
	for _, id := range relayServedFor(srv.URL, prov.AID())[servedBefore:] {
		readAgain[id] = true
	}
	for _, id := range heldIDs {
		if !readAgain[id] {
			t.Fatalf("held envelope %d was not read again in %d rounds (%d of %d were)", id, 2*held+2, len(readAgain), len(heldIDs))
		}
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != held {
		t.Fatalf("%d envelopes queued, want the %d held", n, held)
	}
}

// deliverThenDie is a direct transport whose send delivers the envelope and
// then, before the sender hears the answer, the sending process dies: the
// attempt's outcome is never recorded. It stands the death in with a store
// that refuses every write to the retry queue from that moment (die), the
// state a process that stopped there leaves behind.
type deliverThenDie struct {
	to  *Daemon
	on  atomic.Bool
	die func()
}

func (p *deliverThenDie) Name() string { return "p2p-sender-dies" }
func (p *deliverThenDie) Reachable(context.Context, string) bool {
	return p.on.Load()
}
func (p *deliverThenDie) Send(ctx context.Context, _ string, env []byte) ([]byte, bool, error) {
	_ = p.to.receiveEnvelope(ctx, env)
	p.die()
	return nil, true, errors.New("p2p: no answer within the send timeout")
}

// [redteam:F23][redteam:F12] bypass: a delegation row is marked "may have
// been delivered" only after the attempt that may have delivered it
// returns. A process that stops during that attempt — a p2p send may wait
// 20 s for its answer — or a store error on that write leaves the row
// unmarked, as if no attempt had reached anybody. After the restart, while
// the hub is still unreachable, the requester cancels: the delegation is
// "withdrawn" (nothing sent, anet.delivery.expired reason=withdrawn), yet
// the provider has it and runs it, and no cancel ever reaches it; were the
// hub to refuse the row for good it would be abandoned as UNAVAILABLE ("not
// delivered, did not run"). Now an attempt is recorded as under way before
// it is made, so a row whose attempt's outcome was never recorded counts
// as possibly delivered.
func TestADelegationWhoseAttemptWasNeverRecordedIsNotWithdrawn(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
	var recovered func()
	direct := &deliverThenDie{to: prov}
	direct.die = func() {
		recovered = storeFault(t, req, "rt_outbox_dead",
			`CREATE TRIGGER rt_outbox_dead BEFORE UPDATE ON outbox BEGIN SELECT RAISE(ABORT, 'process gone (injected)'); END`)
	}
	direct.on.Store(true)
	req.RegisterTransport(direct)

	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("setup: the provider ran the call %d times, want once", n)
	}
	// The process comes back; the direct path is gone, the hub still down.
	recovered()
	direct.on.Store(false)
	rows := outboxRows(t, req, id)
	if len(rows) != 1 {
		t.Fatalf("setup: outbox %+v, want the delegation", rows)
	}
	if !rows[0].Attempting && rows[0].HandedAt == 0 {
		t.Fatalf("a delegation whose attempt's outcome was never recorded counts as never handed over")
	}

	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	rows = outboxRows(t, req, id)
	if len(rows) != 2 || !rows[0].Frozen {
		t.Fatalf("outbox after the cancel: %d rows, want the delegation and the cancel behind it (withdrawn?)", len(rows))
	}
	// The hub is back: the cancel reaches the provider, after the
	// delegation it already has.
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = false })
	req.flushOutboxAt(ctx, math.MaxInt64)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, err := prov.ix.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	if last := msgs[len(msgs)-1]; last.Kind != interactions.MsgCancel {
		t.Fatalf("the provider's last message is %s; the cancel never reached it", last.Kind)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("the call ran %d times, want once", n)
	}
}

// [redteam:F30] bypass: a direct delivery that arrives while the daemon is
// starting waits for start-up to finish — and it waited before the
// daemon-wide rate limit of §3.6 step 0 was applied, so during start-up
// that limit did not hold at all: every delivery the peer process handed
// over was kept, envelope bytes and goroutine, until start-up finished or
// its context ended. A start-up held up by a slow module is a window in
// which anyone who can reach the peer process piles up work and memory.
// The limit is applied first now: over it, a delivery is refused at once
// (the sender goes to the hub), starting or not.
//
// The limiter runs on the daemon's clock, held still here: the bucket holds
// p2pRxSharedBurst tokens and gets none back however long the goroutines take to
// reach it (on the wall clock a busy machine refilled it between them, and
// fewer than `over` were refused within the second the test allowed). No
// deadline decides the outcome: a delivery over the limit that waited for
// start-up instead returns only when ctx ends, with something other than
// errP2PRateLimited.
func TestTheDirectRateLimitHoldsDuringStartUp(t *testing.T) {
	_, req, prov := registeredPair(t)
	prov.ready = make(chan struct{}) // start-up not finished
	env := craft(t, senderOf(req), prov, seal.TypeMessage, "ix_during_start", chatBody(t, "x", ""), nil)
	now := prov.nowMS()
	prov.setClock(func() uint64 { return now })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A first contact (kind 0) is charged to the shared bucket.
	const over = 5
	errc := make(chan error, p2pRxSharedBurst+over)
	for i := 0; i < p2pRxSharedBurst+over; i++ {
		go func() { errc <- recvErr(prov.Inbound(), ctx, env) }()
	}
	for limited := 0; limited < over; limited++ {
		if err := <-errc; !errors.Is(err, errP2PRateLimited) {
			t.Fatalf("%d deliveries over the rate limit refused at once, then one returned %v before start-up finished; "+
				"want %d refused at once and the rest waiting for start-up", limited, err, over)
		}
	}
	// The ones within the limit still wait for start-up, then go through.
	select {
	case err := <-errc:
		t.Fatalf("a delivery within the limit returned before start-up finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(prov.ready)
	for i := 0; i < p2pRxSharedBurst; i++ {
		if err := <-errc; errors.Is(err, errNotReady) || errors.Is(err, errP2PRateLimited) {
			t.Fatalf("a delivery within the limit, after start-up: %v", err)
		}
	}
}

// [an internal note item] bypass: a direct delivery is acknowledged at its step-10
// commit, but its pipeline goes on and may end in a temporary result (the
// daemon stopping while the call runs). When the pipeline returned before
// Receive saw the commit, Receive chose between the two at random and could
// answer the committed envelope with a nack — "reached no daemon" to the
// sender, which may then withdraw on a cancel, or report as never run, a
// delegation this node holds and runs at its next start. Once committed,
// the answer is an ack whatever came after.
func TestACommittedDirectDeliveryIsNeverRefused(t *testing.T) {
	committed := make(chan []byte, 1)
	stopping := rxResult{class: rxTransient, reason: transientStopping}
	if _, err := directAnswer(committed, stopping); err == nil {
		t.Fatal("an uncommitted envelope refused for now was acknowledged")
	}
	tok1 := []byte("the commit's token")
	committed <- tok1
	if dack, err := directAnswer(committed, stopping); err != nil || string(dack) != string(tok1) {
		t.Fatalf("a committed envelope was refused or answered without its token: %q %v", dack, err)
	}
	committed <- tok1
	if dack, err := directAnswer(committed, rxResult{class: rxDropped, reason: dropDuplicate}); err != nil || string(dack) != string(tok1) {
		t.Fatalf("a committed duplicate was refused or answered without its token: %q %v", dack, err)
	}
}

// [redteam:F23] bypass (liveness): a provider's answers to one task go out
// in order, so its result waits behind a status queued before it. A
// redelivered delegation — the requester showing it is there and still
// waiting — made only the result row due at once; the status ahead of it
// stayed in its backoff (up to 24 h), and the result behind it with it,
// where before the order it went out on the next pass. Now every row
// queued for the task to the requester is made due.
func TestARedeliveredDelegationHurriesTheWholeAnswerQueue(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	clk := newSharedClock(uint64(time.Now().UnixMilli()))
	prov.setClock(clk.fn())
	id, err := req.Delegate(ctx, prov.AID(), "a text task", nil)
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// The hub stops carrying mail: a status, then the completion, queue up
	// in that order, the status backing off.
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
	if err := prov.SendStatus(ctx, id, interactions.StateWorking, "on it", nil); err != nil {
		t.Logf("status: %v", err)
	}
	if err := prov.CompleteTask(ctx, id); err != nil {
		t.Logf("complete: %v", err)
	}
	rows := outboxRows(t, prov, id)
	if len(rows) != 2 || rows[1].Type != seal.TypeResult {
		t.Fatalf("setup: provider outbox %+v, want a status and then the result", rows)
	}
	if uint64(rows[0].NextAt) <= clk.now() {
		t.Fatalf("setup: the status is due already")
	}
	// The hub is back, and the requester sends its delegation again.
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = false })
	injectEnvelope(t, srv, prov.AID(), env)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	prov.flushOutbox(ctx)
	if n := len(queuedFor(t, srv, req.AID())); n != 2 {
		rows := outboxRows(t, prov, id)
		t.Fatalf("after the redelivery %d answers reached the hub, want both (the first due at +%d ms)", n,
			rows[0].NextAt-int64(clk.now()))
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, _ := req.ix.Get(id); cur.State != interactions.StateCompleted {
		t.Fatalf("requester: %s, want completed", cur.State)
	}
}

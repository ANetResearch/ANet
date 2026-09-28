package daemon

// Regression tests for ways around the SI-10 delivery fixes (F12, F22-F25,
// F27, F28, F30, Q29) found in the adversarial review of those fixes. Each
// asserts that the bypass fails.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
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
func (p *directTo) Send(ctx context.Context, _ string, env []byte) error {
	if p.refuse.Add(-1) >= 0 {
		return fmt.Errorf("p2p: receiving daemon refused the delivery for now: %w", module.ErrNotDelivered)
	}
	p.refuse.Store(0)
	p.sent.Add(1)
	return p.to.Inbound().Receive(ctx, env)
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
// after the delegation already there, and arrives in order.
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
	if direct.sent.Load() != 1 {
		t.Fatalf("setup: the follow-up was not tried over the direct path")
	}
	if n := counter(prov, dropUnknownIX); n != 0 {
		t.Fatalf("the direct follow-up was answered TaskNotFound and dropped (%v)", prov.ReceiveStats())
	}
	if rows, _ := stranger.ix.Outbox(id); len(rows) != 0 {
		t.Fatalf("the follow-up is still queued: %+v", rows)
	}
	// The provider collects its mail: the delegation, then the follow-up.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
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
// dozen held envelopes are enough; a peer the node deals with may hold 32
// at a time, and can send them again when they expire. Now the rounds that
// go back over held envelopes keep their own place, and the rounds between
// them read on from the forward cursor, which never goes back.
func TestHeldPagesDoNotStarveNewMail(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	peer := newStranger(t)
	allowPeers(t, prov, peer.aid)
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
	// read again within two rounds per page.
	before := counter(prov, transientUnknownIX)
	for i := 0; i < 2*held+2; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := counter(prov, transientUnknownIX) - before; n < held {
		t.Fatalf("%d held envelopes read %d times in %d rounds; not every one was gone back to", held, n, 2*held+2)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != held {
		t.Fatalf("%d envelopes queued, want the %d held", n, held)
	}
}

package daemon

// A delegation for an interaction id this node already holds is a
// redelivery of the request that interaction was accepted for, and nothing
// else [redteam:F6][redteam:F7]. These began as the red-team PoCs
// TestRedteamSI4_*Swapped*/RemovedPeer*, TestRedteamSI5_*DelegateSwap and
// TestRedteamOpenRedeliverySwap*/ApprovedChatEscalates*: a second
// anet.delegate/1 on the same ix, under a new message id, with a TaskDoc
// naming a capability the node never admitted, ran that capability. Each
// now asserts the swap is refused as an ix collision and nothing runs.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// swapDelegate seals a second anet.delegate/1 for an interaction id the
// sender already holds on prov, with a new message id and a TaskDoc that
// calls capID.
func swapDelegate(t *testing.T, from, prov *Daemon, ix, capID string) rxResult {
	t.Helper()
	body := delegateBody(t, from.self, ix, "anything", capID)
	env := craft(t, senderOf(from), prov, seal.TypeDelegate, ix, body, nil)
	return receive(t, prov, env)
}

// assertSwapRefused checks that a swapped delegation was dropped as an ix
// collision in step 9 and that no capability ran.
func assertSwapRefused(t *testing.T, prov *Daemon, r rxResult, lamp *lampProvider, ix string) {
	t.Helper()
	if r.class != rxDropped || r.reason != dropIXCollision {
		t.Fatalf("swapped delegation: %+v, want dropped as %s", r, dropIXCollision)
	}
	waitUntil(t, "no call running", func() bool {
		_, running := prov.running.Load(ix)
		return !running
	})
	if n := len(lamp.invoked); n != 0 {
		t.Fatalf("the swapped-in capability ran %d times", n)
	}
}

// Policy open takes a stranger's natural-language task (trust=public) and
// refuses its capability calls (§5.2 row 5). A delegation on the task's id
// naming a non-public capability is refused; the same TaskDoc sealed again
// under a new message id is still a redelivery.
func TestARedeliveryUnderOpenCannotSwapInACapability(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, prov, PolicyOpen) // lampCap is not public
	s := newStranger(t)

	// Control: the direct capability call is refused under open.
	direct, _ := newInteractionID()
	if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, direct,
		delegateBody(t, s.ctrl, direct, "turn it on", lampCap), nil)); r.class != rxDropped ||
		r.reason != dropRefusedPrefix+reasonCapabilityNotPub {
		t.Fatalf("control: direct private call under open = %+v", r)
	}

	ix, _ := newInteractionID()
	chat := delegateBody(t, s.ctrl, ix, "hi, just chatting", "")
	if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix, chat, nil)); r.class != rxAccepted {
		t.Fatalf("chat task: %+v", r)
	}
	if pix := getIXRT(t, prov, ix); pix.Trust != interactions.TrustPublic || pix.IsCapability {
		t.Fatalf("chat task stored as trust %q cap %v", pix.Trust, pix.IsCapability)
	}
	r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix,
		delegateBody(t, s.ctrl, ix, "turn it on", lampCap), nil))
	assertSwapRefused(t, prov, r, lamp, ix)
	if pix := getIXRT(t, prov, ix); pix.IsCapability || pix.IsTerminal() {
		t.Fatalf("the chat task changed: cap %v state %s", pix.IsCapability, pix.State)
	}

	// The same request under a new message id is a redelivery, as before.
	if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix, chat, nil)); r.class != rxAccepted {
		t.Fatalf("redelivery of the same request: %+v", r)
	}
	if n := len(lamp.invoked); n != 0 {
		t.Fatalf("lamp ran %d times", n)
	}
}

// The same as a daemon peer sends it (TestRedteamSI4_OpenPolicy… and
// TestRedteamSI5_OpenPolicy…): through the real send path of a registered
// stranger.
func TestARegisteredStrangerCannotSwapACapabilityIntoItsOpenTask(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, prov, PolicyOpen)

	gid, err := stranger.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.Get(gid); !errors.Is(err, interactions.ErrNotFound) || len(lamp.invoked) != 0 {
		t.Fatalf("control: a non-public capability call under open was accepted")
	}
	if st, meta := lastStatusMeta(t, stranger, gid); st != interactions.StateRejected || meta["anet.reason"] != reasonCapabilityNotPub {
		t.Fatalf("control: %s %v", st, meta)
	}

	id, err := stranger.Delegate(ctx, prov.AID(), "please say hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if pix, err := prov.ix.Get(id); err != nil || pix.Trust != interactions.TrustPublic || pix.IsCapability {
		t.Fatalf("setup: %+v %v", pix, err)
	}
	assertSwapRefused(t, prov, swapDelegate(t, stranger, prov, id, lampCap), lamp, id)
	// sealFrom is the daemon's own sealWith: the same bytes a modified
	// client sends.
	r := receive(t, prov, sealFrom(t, stranger, prov, seal.TypeDelegate, id, delegateBody(t, stranger.self, id, "x", lampCap)))
	assertSwapRefused(t, prov, r, lamp, id)
}

// Approve: the operator approves a held natural-language task (PendingView
// shows no capability). A delegation on the approved id naming a
// capability is refused; nobody approved a capability call.
func TestAnApprovedChatCannotBeSwappedForACapability(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, prov, PolicyApprove)
	s := newStranger(t)

	// A bare stranger identity, held and approved.
	ix, _ := newInteractionID()
	if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix,
		delegateBody(t, s.ctrl, ix, "could you summarize a paper for me?", ""), nil)); r.class != rxAccepted {
		t.Fatalf("held: %+v", r)
	}
	// While held, another TaskDoc under its id is not the held request.
	if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix,
		delegateBody(t, s.ctrl, ix, "turn it on", lampCap), nil)); r.reason != dropIXCollision {
		t.Fatalf("swap while held: %+v", r)
	}
	views, err := prov.PendingList()
	if err != nil || len(views) != 1 || views[0].Capability != "" {
		t.Fatalf("pending view: %+v %v", views, err)
	}
	if _, err := prov.ApprovePending(ix); err != nil {
		t.Fatal(err)
	}
	r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix,
		delegateBody(t, s.ctrl, ix, "turn it on", lampCap), nil))
	assertSwapRefused(t, prov, r, lamp, ix)

	// A registered daemon, through its own send path.
	id, err := stranger.Delegate(ctx, prov.AID(), "summarise this paragraph", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ApprovePending(id); err != nil {
		t.Fatal(err)
	}
	r = receive(t, prov, sealFrom(t, stranger, prov, seal.TypeDelegate, id, delegateBody(t, stranger.self, id, "x", lampCap)))
	assertSwapRefused(t, prov, r, lamp, id)
}

// §5.1: revocation reaches existing interactions, and a trust=peer
// interaction re-checks allow. A peer taken off the allow list (not put on
// deny) can neither swap a capability into its old task nor redeliver the
// task itself.
func TestARevokedPeerCannotUseItsOldInteraction(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	op, err := seal.Open(onlyQueuedEnvelope(t, srv, prov.AID()), prov.AID(), prov.enc)
	if err != nil {
		t.Fatal(err)
	}
	accepted := op.Inner.Body
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if ix, err := prov.ix.Get(id); err != nil || ix.Trust != interactions.TrustPeer {
		t.Fatalf("setup: %+v %v", ix, err)
	}
	// Control: while allowed, the accepted request sealed again under a
	// new message id is a redelivery.
	if r := receive(t, prov, sealFrom(t, req, prov, seal.TypeDelegate, id, accepted)); r.class != rxAccepted {
		t.Fatalf("control: redelivery by an allowed peer: %+v", r)
	}
	if _, err := prov.RemovePeer(req.AID()); err != nil {
		t.Fatal(err)
	}
	// Control: its text is refused.
	if r := receive(t, prov, sealFrom(t, req, prov, seal.TypeMessage, id, chatBody(t, "more", "msg_more"))); r.reason != dropNotAllowed {
		t.Fatalf("control: %+v", r)
	}
	// A swapped TaskDoc is a collision whoever sends it.
	assertSwapRefused(t, prov, swapDelegate(t, req, prov, id, lampCap), lamp, id)
	r := receive(t, prov, sealFrom(t, req, prov, seal.TypeDelegate, id, delegateBody(t, req.self, id, "x", lampCap)))
	assertSwapRefused(t, prov, r, lamp, id)
	// The accepted request itself, sealed again under a new message id: the
	// allow list is checked as for a message.
	if r := receive(t, prov, sealFrom(t, req, prov, seal.TypeDelegate, id, accepted)); r.reason != dropNotAllowed {
		t.Fatalf("redelivery by a revoked peer: %+v, want %s", r, dropNotAllowed)
	}
}

// fillDelegate decodes m.body into the fields step 9 fills for a
// delegation, without step 9's checks.
func fillDelegate(t *testing.T, m *rxMsg) {
	t.Helper()
	dr, err := delegation.UnmarshalDelegateReq(m.body)
	if err != nil {
		t.Fatal(err)
	}
	td, err := decodeTaskDoc(dr.TaskDoc)
	if err != nil {
		t.Fatal(err)
	}
	m.dr, m.td, m.tdBytes = dr, td, dr.TaskDoc
}

// A node upgraded from wire 1 keeps an inbound row accepted under the old
// accept-anyone default, with no request CID. A delegation on its id is a
// collision: the stored row names no request it could be a redelivery of.
func TestARedeliveryOnAMigratedRowCannotRunACapability(t *testing.T) {
	srv := newFakeHub(t)
	stranger := registered(t, srv.URL, "stranger")
	prov, legacyIX, lamp := migratedV01Node(t, srv.URL, stranger)
	assertSwapRefused(t, prov, swapDelegate(t, stranger, prov, legacyIX, lampCap), lamp, legacyIX)
}

// redeliveredDelegate, reached without step 9's comparison (the replay
// table's branch, or a copy that raced the interaction's creation), runs
// the call the interaction was recorded with: a TaskDoc on the redelivered
// envelope does not make a chat task a capability call, nor change which
// capability a recorded call runs.
func TestARedeliveryRunsTheRecordedCallNotTheEnvelopes(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, prov, PolicyOpen)
	s := newStranger(t)
	ix, _ := newInteractionID()
	if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix,
		delegateBody(t, s.ctrl, ix, "light.onoff@sim/lamp-1", ""), nil)); r.class != rxAccepted {
		t.Fatalf("chat task: %+v", r)
	}
	// A text task whose goal reads like a capability id, and an envelope
	// whose TaskDoc names the capability.
	swap := &rxMsg{from: s.aid, typ: seal.TypeDelegate, ix: ix}
	env := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "x", lampCap), nil)
	op, err := seal.Open(env, prov.AID(), prov.enc)
	if err != nil {
		t.Fatal(err)
	}
	swap.body = op.Inner.Body
	fillDelegate(t, swap)
	if !prov.redeliveredDelegate(ctx, swap) {
		t.Fatal("redeliveredDelegate reported a stop")
	}
	if n := len(lamp.invoked); n != 0 {
		t.Fatalf("a chat task's redelivery ran %s %d times", lampCap, n)
	}
}

// An approval runs the request the operator was shown, and a requester on
// the deny list is not approved (deny first, §5.1).
func TestADeniedRequesterIsNotApproved(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, prov, PolicyApprove)
	s := newStranger(t)
	ix, _ := newInteractionID()
	if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix,
		delegateBody(t, s.ctrl, ix, "turn it on", lampCap), nil)); r.class != rxAccepted {
		t.Fatalf("held: %+v", r)
	}
	if _, err := prov.DenyPeer(ctx, s.aid); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ApprovePending(ix); err == nil || !strings.Contains(err.Error(), "deny") {
		t.Fatalf("approval of a denied requester: %v", err)
	}
	if _, err := prov.ix.Get(ix); !errors.Is(err, interactions.ErrNotFound) || len(lamp.invoked) != 0 {
		t.Fatalf("the held call ran for a denied requester")
	}
}

func getIXRT(t *testing.T, d *Daemon, id string) *interactions.Interaction {
	t.Helper()
	ix, err := d.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// The legitimate side of the binding: a call the operator approved and this
// node answered is, when its delegation comes again — the same envelope, or
// the same request sealed under a new message id — answered again and not
// run again. Neither copy is taken for a collision, and neither is judged
// by the policy a second time (the requester is not on the allow list).
func TestAnApprovedCallRedeliveredIsAnsweredAgainNotRun(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	req := registered(t, srv.URL, "req")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, prov, PolicyApprove)
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	clearMailbox(t, srv, prov.AID())
	if r := receive(t, prov, env); r.class != rxAccepted {
		t.Fatalf("held: %+v", r)
	}
	if _, err := prov.ApprovePending(id); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the approved call answered", func() bool {
		ix, err := prov.ix.Get(id)
		return err == nil && len(ix.Receipt) > 0
	})
	// Everything the approval sent has left the outbox (the answer queues
	// behind the status rows of its task, F23) before it is lost.
	waitUntil(t, "the approved call's answers sent", func() bool {
		rows, err := prov.ix.Outbox(id)
		return err == nil && len(rows) == 0
	})
	clearMailbox(t, srv, req.AID()) // the held notice and the answer are lost

	if r := receive(t, prov, env); !r.ack() || r.reason != dropDuplicate {
		t.Fatalf("the same envelope again: %+v", r)
	}
	if n := len(queuedFor(t, srv, req.AID())); n != 1 {
		t.Fatalf("%d answers re-sent for the redelivered envelope, want 1", n)
	}
	op, err := seal.Open(env, prov.AID(), prov.enc)
	if err != nil {
		t.Fatal(err)
	}
	resealed := craft(t, senderOf(req), prov, seal.TypeDelegate, id, op.Inner.Body, nil)
	if r := receive(t, prov, resealed); r.class != rxAccepted {
		t.Fatalf("the same request under a new message id: %+v", r)
	}
	if n := len(queuedFor(t, srv, req.AID())); n != 2 {
		t.Fatalf("%d answers queued after the re-sealed copy, want 2", n)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("the approved call ran %d times", n)
	}
	if n := counter(prov, dropIXCollision); n != 0 {
		t.Fatalf("a redelivery of the approved request was taken for a collision %d times", n)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, req, id); st != interactions.StateCompleted {
		t.Fatalf("requester: %s, want completed", st)
	}
}

package daemon

// One envelope is judged once [redteam:F26]: the (from, mid) lock and the
// replay table come before step 9, so a second copy of a delegation (the
// hub's, while the p2p copy is still committing) neither takes admission
// nor tells the requester `rejected` for the task the first copy runs.

import (
	"context"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// From TestRedteamSI10_ConcurrentCopiesOfAPublicCallGetBothExecutedAndRejected:
// a public capability with max_inflight 1; the p2p copy holds its step-10
// transaction open (a slow disk, a busy write lock) when the hub copy
// arrives. The hub copy waits for it and is a duplicate: no admission
// refusal, no rejected notice, the call runs once and the requester ends
// completed.
func TestConcurrentCopiesOfAPublicCallAreJudgedOnce(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	prov.mu.Lock()
	in := prov.cfg.inbound()
	in.PublicCapabilities = []PublicCapability{{ID: lampCap, MaxInflight: 1}}
	prov.cfg.Inbound = &in
	prov.mu.Unlock()

	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	clearMailbox(t, srv, prov.AID())

	entered, release := make(chan struct{}), make(chan struct{})
	var once bool
	prov.setRxFault(func(typ string) error {
		if typ == seal.TypeDelegate && !once {
			once = true
			close(entered)
			<-release
		}
		return nil
	})
	p2pDone := make(chan error, 1)
	go func() { p2pDone <- prov.Inbound().Receive(ctx, env) }()
	<-entered
	hubDone := make(chan rxResult, 1)
	go func() { hubDone <- prov.receiveEnvelope(ctx, env) }()
	select {
	case r := <-hubDone:
		t.Fatalf("the hub copy was decided while the p2p copy was still committing: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-p2pDone; err != nil {
		t.Fatalf("p2p copy: %v", err)
	}
	hubCopy := <-hubDone
	prov.setRxFault(nil)
	if hubCopy.class != rxDropped || hubCopy.reason != dropDuplicate {
		t.Fatalf("hub copy: %+v, want a duplicate", hubCopy)
	}
	if n := counter(prov, dropRefusedPrefix+admitInflightLimit); n != 0 {
		t.Fatalf("the second copy was refused by admission %d times", n)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("capability ran %d times", n)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	cur, err := req.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if cur.State != interactions.StateCompleted || len(cur.Receipt) == 0 {
		t.Fatalf("requester: %s receipt=%d, want completed with the receipt", cur.State, len(cur.Receipt))
	}
	msgs, err := req.ix.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Kind == interactions.MsgStatus && decodeMeta([]byte(m.Metadata))["anet.state"] == string(interactions.StateRejected) {
			t.Fatalf("the requester was told rejected: %+v", m)
		}
	}
}

// The same delegation sealed twice (two message ids) and delivered at once,
// with the public capability at its max_inflight edge: the second copy
// waits for the first to create the interaction and is a redelivery of it,
// not a second admission refused with `rejected`.
func TestADelegationSealedTwiceAtTheQuotaEdgeIsJudgedOnce(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	prov.mu.Lock()
	in := prov.cfg.inbound()
	in.PublicCapabilities = []PublicCapability{{ID: lampCap, MaxInflight: 1}}
	prov.cfg.Inbound = &in
	prov.mu.Unlock()

	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	first := onlyQueuedEnvelope(t, srv, prov.AID())
	clearMailbox(t, srv, prov.AID())
	op, err := seal.Open(first, prov.AID(), prov.enc)
	if err != nil {
		t.Fatal(err)
	}
	second := craft(t, senderOf(req), prov, seal.TypeDelegate, id, op.Inner.Body, nil)

	entered, release := make(chan struct{}), make(chan struct{})
	var once bool
	prov.setRxFault(func(typ string) error {
		if typ == seal.TypeDelegate && !once {
			once = true
			close(entered)
			<-release
		}
		return nil
	})
	firstDone := make(chan rxResult, 1)
	go func() { firstDone <- prov.receiveEnvelope(ctx, first) }()
	<-entered
	secondDone := make(chan rxResult, 1)
	go func() { secondDone <- prov.receiveEnvelope(ctx, second) }()
	select {
	case r := <-secondDone:
		t.Fatalf("the second copy was decided while the first was still committing: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if r := <-firstDone; r.class != rxAccepted {
		t.Fatalf("first copy: %+v", r)
	}
	r2 := <-secondDone
	prov.setRxFault(nil)
	if !r2.ack() || r2.reason == dropRefusedPrefix+admitInflightLimit {
		t.Fatalf("second copy: %+v", r2)
	}
	if n := counter(prov, dropRefusedPrefix+admitInflightLimit); n != 0 {
		t.Fatalf("the second copy was refused by admission %d times", n)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("capability ran %d times", n)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, err := req.ix.Get(id); err != nil || cur.State != interactions.StateCompleted {
		t.Fatalf("requester: %+v %v, want completed", cur, err)
	}
}

// A refused delegation's second copy, arriving while the first is being
// refused, is not refused (and answered) again: it finds the first's
// entry once the lock is free.
func TestConcurrentCopiesOfARefusedDelegationAreAnsweredOnce(t *testing.T) {
	srv := newFakeHub(t)
	prov := registered(t, srv.URL, "prov") // closed
	s := newStranger(t)
	ix, _ := newInteractionID()
	env := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "a task", ""), nil)
	// Both copies passed the in-memory check at step 5 before either was
	// refused: take the lock as the first copy would and let the second
	// queue behind it.
	unlock := prov.rxLocks.lock(replayKey(s.aid, mustMID(t, prov, env)))
	second := make(chan rxResult, 1)
	go func() { second <- prov.receiveEnvelope(context.Background(), env) }()
	time.Sleep(50 * time.Millisecond)
	prov.refused.add(replayKey(s.aid, mustMID(t, prov, env))) // what the first copy's refusal records
	unlock()
	if r := <-second; r.reason != dropRefusedReplay {
		t.Fatalf("second copy: %+v, want %s", r, dropRefusedReplay)
	}
	if n := counter(prov, noticeSent); n != 0 {
		t.Fatalf("%d notices sent for the second copy", n)
	}
}

// mustMID is the inner message id of env, sealed to d.
func mustMID(t *testing.T, d *Daemon, env []byte) []byte {
	t.Helper()
	op, err := seal.Open(env, d.AID(), d.enc)
	if err != nil {
		t.Fatal(err)
	}
	return op.Inner.MID
}

// From TestRedteamSI10_ReplayedDelegationMakesTheProviderResendWithoutLimit
// [redteam:F29]: a caller replays its answered delegation forty times. Each
// replay is acknowledged as a duplicate, the call ran once, and the answer
// is re-sent at most resendBurst times, not once per replay: the replays
// do not spend the provider's hub send budget.
func TestReplaysOfAnAnsweredDelegationAreNotEachAnswered(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setPublic(prov, lampCap)
	if _, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true}); err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	clearMailbox(t, srv, req.AID())
	sendsBefore := relayCountFor(srv.URL)
	const replays = 40
	for i := 0; i < replays; i++ {
		if r := receive(t, prov, env); r.reason != dropDuplicate {
			t.Fatalf("replay %d: %+v", i, r)
		}
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("ran %d times", n)
	}
	if got := relayCountFor(srv.URL) - sendsBefore; got != resendBurst {
		t.Fatalf("provider sent %d answers for %d replays, want %d", got, replays, resendBurst)
	}
	if n := len(queuedFor(t, srv, req.AID())); n != resendBurst {
		t.Fatalf("%d answers queued for the caller, want %d", n, resendBurst)
	}
	if n := counter(prov, resendSuppressed); n != replays-resendBurst {
		t.Fatalf("%d re-sends suppressed, want %d", n, replays-resendBurst)
	}
}

// The re-send limit is per (peer, interaction) and refills: another
// interaction, or the same one after the refill period, is answered again.
func TestTheResendLimitIsPerInteractionAndRefills(t *testing.T) {
	var l resendLimiter
	const t0 = 1_000_000
	for i := 0; i < resendBurst; i++ {
		if !l.allow("peer", "ix_a", t0) {
			t.Fatalf("re-send %d refused within the burst", i)
		}
	}
	if l.allow("peer", "ix_a", t0) {
		t.Fatal("a re-send past the burst was allowed")
	}
	if !l.allow("peer", "ix_b", t0) || !l.allow("other", "ix_a", t0) {
		t.Fatal("another interaction's re-send was refused")
	}
	if l.allow("peer", "ix_a", t0+resendEveryMS/2) {
		t.Fatal("refilled too early")
	}
	if !l.allow("peer", "ix_a", t0+resendEveryMS) {
		t.Fatal("not refilled after the period")
	}
}

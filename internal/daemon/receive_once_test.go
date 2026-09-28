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

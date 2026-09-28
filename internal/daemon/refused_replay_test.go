package daemon

// A delegation refused in step 9 stays refused [redteam:F5]: it is recorded
// in the refused table beside the replay table, so the same envelope handed
// in again after a restart, after the in-memory list forgot it, or after the
// operator changed the policy is dropped without being judged again and
// without a second reply. From TestRedteamSI4_RefusedDelegationReplayed…,
// TestRedteamSI3_RefusedEnvelopeAccepted… and
// TestRedteamSI10_ReplayOfARefusedDelegation….

import (
	"context"
	"errors"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// A stranger's capability call is refused under closed and acknowledged.
// The operator later allows the stranger and the daemon restarts; a hub
// that kept the envelope hands it in again. It is refused as a replay,
// nothing runs, and no second notice goes out.
func TestARefusedDelegationStaysRefusedAfterAllowAndRestart(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	ix := "ix_refused_replay_0001"
	env := sealFrom(t, stranger, prov, seal.TypeDelegate, ix, delegateBody(t, stranger.self, ix, "lamp on", lampCap))
	if r := receive(t, prov, env); r.class != rxDropped || r.reason != dropNotAccepting {
		t.Fatalf("first delivery: %+v", r)
	}
	if r := receive(t, prov, env); r.reason != dropRefusedReplay {
		t.Fatalf("immediate replay: %+v", r)
	}
	allowPeers(t, prov, stranger.AID())
	prov = restartDaemon(t, prov)
	lamp2 := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp2); err != nil {
		t.Fatal(err)
	}
	notices := counter(prov, noticeSent)
	if r := receive(t, prov, env); r.class != rxDropped || r.reason != dropRefusedReplay {
		t.Fatalf("replay after allow and restart: %+v, want %s", r, dropRefusedReplay)
	}
	if len(lamp2.invoked) != 0 {
		t.Fatalf("the refused delegation ran %d times", len(lamp2.invoked))
	}
	if _, err := prov.ix.Get(ix); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the refused delegation was recorded: %v", err)
	}
	if n := counter(prov, noticeSent); n != notices {
		t.Fatalf("the replay was answered again (%d notices)", n-notices)
	}
	// The stranger is allowed now: a new delegation of its own is taken.
	fresh := "ix_refused_replay_0002"
	if r := receive(t, prov, sealFrom(t, stranger, prov, seal.TypeDelegate, fresh,
		delegateBody(t, stranger.self, fresh, "lamp on", lampCap))); r.class != rxAccepted {
		t.Fatalf("a new delegation from the now allowed peer: %+v", r)
	}
}

// The in-memory list forgotten (4096 other refusals, or a restart) and the
// policy changed: the replay is still refused.
func TestARefusedDelegationStaysRefusedWhenTheListForgetsIt(t *testing.T) {
	prov := newTestDaemon(t, "", false) // default: closed
	s := newStranger(t)
	ix := "ix_refused_forgotten"
	env := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "a task", ""), nil)
	if r := receive(t, prov, env); r.class != rxDropped || r.reason != dropNotAccepting {
		t.Fatalf("first delivery: %+v, want refused as not-accepting", r)
	}
	prov.refused = boundedSet{}
	allowPeers(t, prov, s.aid)
	if r := receive(t, prov, env); r.class != rxDropped || r.reason != dropRefusedReplay {
		t.Fatalf("replay: %+v, want %s", r, dropRefusedReplay)
	}
	if _, err := prov.ix.Get(ix); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the refused delegation was recorded: %v", err)
	}
}

// Through the hub, as the red team ran it: the requester is told rejected;
// after allow and a restart the hub replays the envelope. It does not run,
// no answer reaches the requester, and its task stays rejected.
func TestAHubReplayOfARefusedDelegationDoesNotRun(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, false) // closed: refuses everyone
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.Providers().Register(ctx, &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID()) // what a malicious hub keeps
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if counter(prov, dropNotAccepting) != 1 || len(queuedFor(t, srv, prov.AID())) != 0 {
		t.Fatalf("setup: not refused and acked: %v", prov.ReceiveStats())
	}
	waitUntil(t, "the rejected notice", func() bool { return len(queuedFor(t, srv, req.AID())) == 1 })
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, _ := req.ix.Get(id); cur.State != interactions.StateRejected {
		t.Fatalf("requester state = %s, want rejected", cur.State)
	}

	allowPeers(t, prov, req.AID())
	prov2 := restartDaemon(t, prov)
	lamp := &lampProvider{}
	if err := prov2.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	injectEnvelope(t, srv, prov2.AID(), env)
	if err := prov2.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(lamp.invoked); n != 0 {
		t.Fatalf("the replayed refused delegation ran %d times", n)
	}
	if counter(prov2, dropRefusedReplay) != 1 || len(queuedFor(t, srv, prov2.AID())) != 0 {
		t.Fatalf("replay not refused and acked: %v", prov2.ReceiveStats())
	}
	if _, err := prov2.ix.Get(id); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the provider recorded the replayed delegation: %v", err)
	}
	if n := len(queuedFor(t, srv, req.AID())); n != 0 {
		t.Fatalf("%d envelopes sent to the requester for the replay", n)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, _ := req.ix.Get(id); cur.State != interactions.StateRejected || len(cur.Receipt) != 0 {
		t.Fatalf("requester state = %s, receipt %d bytes", cur.State, len(cur.Receipt))
	}
}

// The refused table is bounded per sender: a sender's oldest refusals go
// into its floor, and a replay of one of them is still refused (as older
// than the floor), without a reply. A newer delegation is judged as usual.
func TestAnEvictedRefusalIsCoveredByTheFloor(t *testing.T) {
	prov := newTestDaemon(t, "", false) // closed
	prov.ix.SetRefusedCaps(interactions.RefusedCaps{PerSender: 1})
	s := newStranger(t)
	first := "ix_refused_floor_1"
	env := craft(t, s, prov, seal.TypeDelegate, first, delegateBody(t, s.ctrl, first, "one", ""), func(in *seal.SealedInner) {
		in.TS -= 1000
	})
	if r := receive(t, prov, env); r.reason != dropNotAccepting {
		t.Fatalf("first: %+v", r)
	}
	second := "ix_refused_floor_2"
	if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, second, delegateBody(t, s.ctrl, second, "two", ""), nil)); r.reason != dropNotAccepting {
		t.Fatalf("second: %+v", r)
	}
	if rows, floors, err := prov.ix.RefusedCount(); err != nil || rows != 1 || floors != 1 {
		t.Fatalf("refused table: %d rows, %d floors, %v", rows, floors, err)
	}
	prov.refused = boundedSet{}
	allowPeers(t, prov, s.aid)
	notices := counter(prov, noticeSent)
	if r := receive(t, prov, env); r.class != rxDropped || r.reason != dropRefusedFloor {
		t.Fatalf("replay of the evicted refusal: %+v, want %s", r, dropRefusedFloor)
	}
	if _, err := prov.ix.Get(first); !errors.Is(err, interactions.ErrNotFound) || counter(prov, noticeSent) != notices {
		t.Fatalf("the evicted refusal was taken or answered: %v", err)
	}
	third := "ix_refused_floor_3"
	if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, third, delegateBody(t, s.ctrl, third, "three", ""), nil)); r.class != rxAccepted {
		t.Fatalf("a new delegation from the now allowed peer: %+v", r)
	}
}

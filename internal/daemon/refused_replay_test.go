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
	"database/sql"
	"errors"
	"path/filepath"
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

// The total bound is no lever on other senders [redteam:F5]. Strangers'
// refusals, dated as far ahead as the clock skew allows, push the refused
// table past its total twice over: the oldest refusal (the victim's) goes
// into the victim's own floor, and its replay is still refused without a
// reply; then some of the strangers' own go. No floor is raised for anyone
// but the senders whose rows went, so an allowed peer's fresh delegation —
// sent before those future-dated refusals' time — is taken, not dropped
// without a word as older than a floor shared by all.
func TestOtherSendersRefusalsDoNotDropAPeersDelegation(t *testing.T) {
	prov := newTestDaemon(t, "", false) // closed
	prov.ix.SetRefusedCaps(interactions.RefusedCaps{Total: 3})
	victim, friend := newStranger(t), newStranger(t)
	allowPeers(t, prov, friend.aid)
	first := "ix_refused_total_victim"
	env := craft(t, victim, prov, seal.TypeDelegate, first, delegateBody(t, victim.ctrl, first, "one", ""), func(in *seal.SealedInner) {
		in.TS -= 1000
	})
	if r := receive(t, prov, env); r.reason != dropNotAccepting {
		t.Fatalf("victim: %+v", r)
	}
	for i := 0; i < 5; i++ {
		s := newStranger(t)
		ix := "ix_refused_total_junk_" + string(rune('a'+i))
		junk := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "junk", ""), func(in *seal.SealedInner) {
			in.TS += seal.ClockSkewMS - 60_000
			in.Exp = in.TS + 3_600_000
		})
		if r := receive(t, prov, junk); r.reason != dropNotAccepting {
			t.Fatalf("junk %d: %+v", i, r)
		}
	}
	if exact, floor, err := prov.ix.Refused(victim.aid, []byte("none")); err != nil || exact || floor == 0 {
		t.Fatalf("the victim's refusal was not evicted into its floor: floor %d, %v", floor, err)
	}
	fresh := "ix_refused_total_friend"
	if r := receive(t, prov, craft(t, friend, prov, seal.TypeDelegate, fresh,
		delegateBody(t, friend.ctrl, fresh, "real work", ""), nil)); r.class != rxAccepted {
		t.Fatalf("an allowed peer's fresh delegation after strangers' refusals: %+v", r)
	}
	prov.refused = boundedSet{}
	allowPeers(t, prov, victim.aid)
	notices := counter(prov, noticeSent)
	if r := receive(t, prov, env); r.class != rxDropped || r.reason != dropRefusedFloor {
		t.Fatalf("replay of the victim's evicted refusal: %+v, want %s", r, dropRefusedFloor)
	}
	if _, err := prov.ix.Get(first); !errors.Is(err, interactions.ErrNotFound) || counter(prov, noticeSent) != notices {
		t.Fatalf("the evicted refusal was taken or answered: %v", err)
	}
}

// refusedTableFault makes every insert into d's refused table fail until
// the returned function is called (the store recovered).
func refusedTableFault(t *testing.T, d *Daemon) func() {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(d.layout.InteractionsDir(), "interactions.db")+"?_pragma=busy_timeout(15000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER refused_fault BEFORE INSERT ON refused
		BEGIN SELECT RAISE(ABORT, 'injected: refused table unwritable'); END`); err != nil {
		t.Fatalf("install fault: %v", err)
	}
	done := false
	recovered := func() {
		if done {
			return
		}
		done = true
		if _, err := db.Exec(`DROP TRIGGER IF EXISTS refused_fault`); err != nil {
			t.Fatalf("remove fault: %v", err)
		}
		db.Close()
	}
	t.Cleanup(recovered)
	return recovered
}

// A refusal the requester is told of is on disk first [redteam:F5]. While
// the refused table takes no rows, a delegation the policy refuses is
// neither answered nor acknowledged (T): the requester is never told
// `rejected` about an envelope that a later delivery — after a restart
// and a policy change — could still accept. Once the table takes rows
// again the envelope is judged as if for the first time. The same holds for
// the approval queue's pending_full refusal in step 10.
func TestARefusalIsStoredBeforeTheRequesterIsTold(t *testing.T) {
	prov := newTestDaemon(t, "", false) // closed
	s := newStranger(t)
	ix := "ix_refused_store_fails"
	env := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "a task", ""), nil)
	recovered := refusedTableFault(t, prov)
	notices := counter(prov, noticeSent)
	if r := receive(t, prov, env); r.class != rxTransient {
		t.Fatalf("refusal that could not be stored: %+v, want not acknowledged", r)
	}
	if n := counter(prov, noticeSent); n != notices {
		t.Fatalf("the requester was told rejected (%d notices) though the refusal was not stored", n-notices)
	}
	recovered()
	allowPeers(t, prov, s.aid)
	if r := receive(t, prov, env); r.class != rxAccepted {
		t.Fatalf("the redelivery once the store recovered: %+v, want judged again and taken", r)
	}

	// pending_full, refused in step 10.
	setPolicy(t, prov, PolicyApprove)
	prov.mu.Lock()
	prov.cfg.Inbound.Pending.MaxPerPeer = 1
	prov.mu.Unlock()
	q := newStranger(t)
	held := "ix_refused_store_held"
	if r := receive(t, prov, craft(t, q, prov, seal.TypeDelegate, held, delegateBody(t, q.ctrl, held, "one", ""), nil)); r.class != rxAccepted {
		t.Fatalf("first held: %+v", r)
	}
	over := "ix_refused_store_over"
	overEnv := craft(t, q, prov, seal.TypeDelegate, over, delegateBody(t, q.ctrl, over, "two", ""), nil)
	recovered = refusedTableFault(t, prov)
	notices = counter(prov, noticeSent)
	if r := receive(t, prov, overEnv); r.class != rxTransient {
		t.Fatalf("pending_full that could not be stored: %+v, want not acknowledged", r)
	}
	if n := counter(prov, noticeSent); n != notices {
		t.Fatalf("the requester was told pending_full (%d notices) though the refusal was not stored", n-notices)
	}
	recovered()
	if r := receive(t, prov, overEnv); r.class != rxDropped || r.reason != dropRefusedPrefix+reasonPendingFull {
		t.Fatalf("the redelivery once the store recovered: %+v", r)
	}
	prov.refused = boundedSet{}
	if r := receive(t, prov, overEnv); r.reason != dropRefusedReplay {
		t.Fatalf("the refusal told this time was not stored: %+v", r)
	}
}

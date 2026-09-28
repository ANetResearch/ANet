package daemon

// Tests added for mutations that survived the daemon's suite
// (docs/notes/0026). Each names the mutation it turns red.

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// §3.6 step 10: the replay row is claimed inside the business write's own
// transaction, and a claim that finds the row already there refuses the
// message before the write runs. process() also reads the replay table
// first, under the (from, mid) lock, so within one process this is the
// second line; commitRx's own contract is pinned here so that it does not
// quietly depend on its caller. Mutation si3-8 (the claim's result ignored)
// ran the write twice and returned accepted.
func TestCommitRxRunsTheBusinessWriteOncePerMessage(t *testing.T) {
	d := newTestDaemon(t, "", false)
	m := &rxMsg{from: "did:anet:commitrx-peer", mid: bytes.Repeat([]byte{7}, 16), exp: d.nowMS() + 60_000}
	runs := 0
	write := func(*interactions.Tx) error { runs++; return nil }
	if r := d.commitRx(m, write); r.class != rxAccepted {
		t.Fatalf("first commit = %+v, want accepted", r)
	}
	if r := d.commitRx(m, write); r.class != rxDropped || r.reason != dropDuplicate {
		t.Fatalf("second commit of the same (from, mid) = %+v, want dropped %s", r, dropDuplicate)
	}
	if runs != 1 {
		t.Fatalf("the business write ran %d times for one message", runs)
	}
}

// SI-5, fail closed: an inbound block that names no policy is closed. A
// hand-written block that sets only a list file (or a policy key lost in an
// edit) must not open the node. Mutation si5-2 (normalize filling "open")
// passed every fresh-install test, because those configs state the policy.
func TestAnInboundBlockWithoutAPolicyIsClosed(t *testing.T) {
	l := NewLayout(t.TempDir())
	raw := `{"control_addr":"127.0.0.1:0","inbound":{"allow_file":"peers.allow"}}`
	if err := os.WriteFile(l.ConfigPath(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := ReadPolicy(l)
	if err != nil {
		t.Fatal(err)
	}
	if st.Inbound.Policy != PolicyClosed {
		t.Fatalf("doctor reads policy %q from an inbound block without one, want %s", st.Inbound.Policy, PolicyClosed)
	}
	_, changed := st.SI5()
	for _, k := range changed {
		if k == "inbound.policy" {
			t.Fatalf("doctor reports inbound.policy as changed from the fresh-install value: %v", changed)
		}
	}
	d, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if p := d.config().inbound().Policy; p != PolicyClosed {
		t.Fatalf("the daemon runs an inbound block without a policy as %q, want %s", p, PolicyClosed)
	}
	if st := d.InboundStatus(); st.Policy != PolicyClosed {
		t.Fatalf("inbound status policy = %q, want %s", st.Policy, PolicyClosed)
	}
}

// §3.6 step 10 begins with an in-process lock on (from, mid): a second copy
// of an envelope (the hub's, while the p2p copy is being handled) waits for
// the first to finish. The replay row's transaction alone would keep a
// chat message from being stored twice, which is all the concurrency tests
// observe; what the lock adds is that nothing after the commit — a short
// capability call runs after its row is committed — is raced by the second
// copy. Mutation si10-4 (process without the lock) left every test green.
func TestASecondCopyWaitsForTheFirstUnderTheMessageLock(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	env := craft(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "one copy at a time", ""), nil)
	op, err := seal.Open(env, prov.AID(), prov.enc)
	if err != nil {
		t.Fatal(err)
	}
	unlock := prov.rxLocks.lock(replayKey(req.AID(), op.Inner.MID))
	done := make(chan rxResult, 1)
	go func() { done <- prov.receiveEnvelope(ctx, env) }()
	select {
	case r := <-done:
		unlock()
		t.Fatalf("the copy was handled (%+v) while another copy held its (from, mid) lock", r)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case r := <-done:
		if r.class != rxAccepted {
			t.Fatalf("after the lock was released: %+v, want accepted", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the copy did not proceed after the lock was released")
	}
}

// C1, at most once: a long call the previous process left working is
// reported interrupted at the next start even when its provider is
// registered by then, and it is not run again. The recovery tests either
// restart without the provider (recovery cannot tell the call is long) or
// leave the long call only submitted; mutation c1-1 (leftoverAction re-running
// every working call whose provider resolves) passed them all.
func TestALongCallLeftWorkingIsNotRunAgainWhenItsProviderIsBack(t *testing.T) {
	_, req, prov := quietPair(t)
	gate := newGateProvider()
	t.Cleanup(func() { close(gate.release) })
	if err := prov.Providers().Register(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	const id = "ix_long_working"
	leftOpen(t, req, prov, id, gateCap)
	if _, err := prov.ix.SetState(id, interactions.StateWorking); err != nil {
		t.Fatal(err)
	}
	// What New does once the modules have registered their providers.
	prov.recoverInterrupted()
	select {
	case <-gate.started:
		t.Fatal("startup recovery ran a long call that an earlier process had started")
	case <-time.After(300 * time.Millisecond):
	}
	wasInterrupted(t, prov, id)
}

// §3.6 step 9 [m]: a delegation naming an interaction this node started —
// its peer reusing our outbound ix — is refused as ix-collision, as a text
// task and as a capability call alike, and the interaction keeps its role,
// state and result. The refusal is a step-9 decision: nothing is written,
// and the same envelope again is dropped from the refused list without
// being decided twice. Step 10 repeats the check inside its transaction
// (against a delegation racing the first), which is why mutations m-ix-1
// and m-ix-2 (the step-9 check weakened or removed) left the step table
// green: the counter came out the same, one step later.
func TestACollidingDelegationIsRefusedBeforeAnyWrite(t *testing.T) {
	_, _, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	peer := newStranger(t)
	const ixo = "ix_ours_outbound"
	if err := prov.ix.Put(ixo, interactions.RoleOutbound, peer.aid, "ours", "", nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"text", delegateBody(t, peer.ctrl, ixo, "take this over", "")},
		{"capability", delegateBody(t, peer.ctrl, ixo, "", lampCap)},
	} {
		env := craft(t, peer, prov, seal.TypeDelegate, ixo, c.body, nil)
		if r := receive(t, prov, env); r.class != rxDropped || r.reason != dropIXCollision {
			t.Fatalf("%s: %+v, want dropped %s", c.name, r, dropIXCollision)
		}
		if r := receive(t, prov, env); r.class != rxDropped || r.reason != dropRefusedReplay {
			t.Fatalf("%s: the same envelope again = %+v, want %s (refused at step 9, before any write)",
				c.name, r, dropRefusedReplay)
		}
	}
	if len(lamp.invoked) != 0 {
		t.Fatalf("the colliding capability call ran: %+v", lamp.invoked)
	}
	ix, err := prov.ix.Get(ixo)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Role != interactions.RoleOutbound || ix.PeerAID != peer.aid || ix.State != interactions.StateSubmitted ||
		len(ix.Result) != 0 || len(ix.Receipt) != 0 {
		t.Fatalf("the outbound interaction changed: %+v", ix)
	}
}

// C19: a message for an interaction this node does not hold waits ten
// minutes for its delegation (§3.6 step 9), then is answered TaskNotFound.
// Ten minutes is the design's number: the other tests measure the window
// with unknownIXWait itself, so mutation c19-2 (10 → 100 minutes) moved
// them along and stayed green.
func TestTheUnknownTaskWindowIsTenMinutes(t *testing.T) {
	_, req, prov := registeredPair(t)
	const ix = "ix_not_here"
	aged := func(age time.Duration) []byte {
		return craft(t, senderOf(req), prov, seal.TypeMessage, ix, chatBody(t, "early or late", ""),
			func(in *seal.SealedInner) { in.TS -= uint64(age.Milliseconds()) })
	}
	if r := receive(t, prov, aged(9*time.Minute)); r.class != rxTransient || r.reason != transientUnknownIX {
		t.Fatalf("a message 9 minutes old = %+v, want held (%s)", r, transientUnknownIX)
	}
	if r := receive(t, prov, aged(11*time.Minute)); r.class != rxDropped || r.reason != dropUnknownIX {
		t.Fatalf("a message 11 minutes old = %+v, want %s", r, dropUnknownIX)
	}
}

// §3.5 step 1 [C2]: a stored key set is re-checked with the hub every ten
// minutes, and a hub that no longer answers for the recipient (404) does
// not take the stored set away; it stays in use until it holds no valid
// key. This is what delivers a reply past the cache time to a requester
// whose hub cannot be asked (scenario.sh 8.5/8.6). The re-check test's hub
// always answers, so mutation c2-1 (past the cache time only the hub's
// answer counts) left it green.
func TestAStoredKeySetOutlivesAHubThatNoLongerAnswersForIt(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := req.ix.PeerIdentity(prov.AID()); err != nil {
		t.Fatalf("no stored record for the provider after the delegation: %v", err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.agents[prov.AID()].keyset = nil })
	start := uint64(time.Now().UnixMilli())
	req.setClock(func() uint64 { return start + 11*60*1000 })
	before := len(queuedFor(t, srv, prov.AID()))
	if err := req.SendMessage(ctx, id, "past the cache time", nil); err != nil {
		t.Fatal(err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != before+1 {
		t.Fatalf("%d envelopes reached the hub for the provider, want %d: the stored key set was not used",
			n, before+1)
	}
}

// C2: a delegation held for approval keeps the requester's key set, and
// approving it records that set, so the answer reaches the requester even
// when the hub can no longer supply its keys (scenario.sh 8.5 approves past
// the key cache time for a pure requester on another hub). Mutation c2-2
// (approval records the KEL only) left the approval tests green: their hub
// always served the requester's keys.
func TestAnApprovedTaskIsAnsweredWithTheKeysItWasHeldWith(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	s := registered(t, srv.URL, "requester")
	setPolicy(t, prov, PolicyApprove)
	id, err := s.Delegate(ctx, prov.AID(), "held for the operator", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.agents[s.AID()].keyset = nil })
	if _, err := prov.ApprovePending(id); err != nil {
		t.Fatal(err)
	}
	before := len(queuedFor(t, srv, s.AID()))
	if err := prov.SendMessage(ctx, id, "approved; here is the answer", nil); err != nil {
		t.Fatal(err)
	}
	if n := len(queuedFor(t, srv, s.AID())); n != before+1 {
		t.Fatalf("%d envelopes reached the hub for the requester, want %d: the held key set was not kept",
			n, before+1)
	}
}

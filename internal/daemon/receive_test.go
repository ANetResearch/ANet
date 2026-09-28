package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The hub carries ciphertext: what it holds for a recipient contains
// neither the task nor the conversation, and still arrives intact at the
// other end (SI-1 at the unit level; the joint run checks the real hub).
func TestTheHubHoldsOnlyCiphertext(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	const goal = "canary-goal-4f1c"
	id, err := req.Delegate(ctx, prov.AID(), goal, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range queuedFor(t, srv, prov.AID()) {
		if bytes.Contains(env, []byte(goal)) || bytes.Contains(env, []byte(id)) || bytes.Contains(env, []byte(req.AID())) {
			t.Fatal("the hub holds the goal, the interaction id or the sender in the clear")
		}
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ix, err := prov.ix.Get(id)
	if err != nil || ix.Goal != goal || ix.PeerAID != req.AID() {
		t.Fatalf("provider did not receive the task intact: %+v %v", ix, err)
	}
	const reply = "canary-reply-9d2a"
	if err := prov.SendMessage(ctx, id, reply, nil); err != nil {
		t.Fatal(err)
	}
	for _, env := range queuedFor(t, srv, req.AID()) {
		if bytes.Contains(env, []byte(reply)) {
			t.Fatal("the hub holds the reply in the clear")
		}
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, _ := req.ix.Messages(id)
	if len(msgs) != 2 || msgs[1].Body != reply || msgs[1].SenderAID != prov.AID() {
		t.Fatalf("requester messages = %+v", msgs)
	}
	if n := len(queuedFor(t, srv, req.AID())) + len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("%d envelopes left unacked", n)
	}
}

// anet.status/1 end to end: a message for an interaction the provider does
// not hold is held back while the delegation may still be on its way, and
// answered with TaskNotFound after the window; the requester records it.
func TestAMessageForAnUnknownTaskGetsTaskNotFound(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	const ix = "ix_unknown_to_provider"
	if err := req.ix.Put(ix, interactions.RoleOutbound, prov.AID(), "never delivered", "", nil); err != nil {
		t.Fatal(err)
	}

	// Inside the window: not acknowledged, stays in the mailbox.
	early := craft(t, senderOf(req), prov, seal.TypeMessage, ix, chatBody(t, "early", ""), nil)
	injectEnvelope(t, srv, prov.AID(), early)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("a message inside the wait window was acknowledged (%d left)", n)
	}
	if counter(prov, transientUnknownIX) == 0 {
		t.Fatal("the early message was not counted as waiting")
	}
	clearMailbox(t, srv, prov.AID())

	// Past the window: dropped, and the sender told.
	late := craft(t, senderOf(req), prov, seal.TypeMessage, ix, chatBody(t, "late", ""), func(in *seal.SealedInner) {
		in.TS -= uint64((unknownIXWait + time.Minute).Milliseconds())
	})
	injectEnvelope(t, srv, prov.AID(), late)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("the late message was not acknowledged (%d left)", n)
	}
	waitUntil(t, "the TaskNotFound status to reach the hub", func() bool {
		return len(queuedFor(t, srv, req.AID())) == 1
	})
	// The refusal is remembered in memory: the same envelope again is
	// dropped before step 9 and does not produce a second notice.
	if r := receive(t, prov, late); r.class != rxDropped || r.reason != dropRefusedReplay {
		t.Fatalf("the refused envelope again: %+v, want %s", r, dropRefusedReplay)
	}
	if n := counter(prov, noticeTaskNotFound); n != 1 {
		t.Fatalf("%d TaskNotFound notices for one message", n)
	}
	// The sender was answered from the stranger cache; nothing about it was
	// written to peer_identity, which is only for peers this node accepted
	// something from (§3.8).
	if _, err := prov.ix.PeerIdentity(req.AID()); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("a sender refused at step 9 has a peer_identity row (%v)", err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := req.ix.Get(ix)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := req.ix.Messages(ix)
	if got.State != interactions.StateFailed || len(msgs) == 0 ||
		msgs[len(msgs)-1].Kind != interactions.MsgStatus || !strings.Contains(msgs[len(msgs)-1].Metadata, `"anet.a2aError":"TaskNotFoundError"`) {
		t.Fatalf("requester interaction = %s %+v, want failed with a TaskNotFound status", got.State, msgs)
	}
}

// Every step of §3.6 refuses what it must, with the class it must: P is
// acknowledged and dropped, T is left for redelivery. Nothing refused
// reaches the store.
func TestEachReceiveStepRefusesWithItsClass(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "a task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	mallory := newStranger(t)
	other := newTestDaemon(t, "", true)
	if err := prov.ix.Put("ix_prov_outbound", interactions.RoleOutbound, mallory.aid, "ours", "", nil); err != nil {
		t.Fatal(err)
	}

	// A peer whose stored KEL forks from the one it later presents.
	forkA, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	exported, err := forkA.Export()
	if err != nil {
		t.Fatal(err)
	}
	forkB, err := identity.Restore(exported)
	if err != nil {
		t.Fatal(err)
	}
	now := uint64(time.Now().UnixMilli())
	if err := forkA.Rotate(now - 1000); err != nil {
		t.Fatal(err)
	}
	if err := forkB.Rotate(now - 2000); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.pinPeerKEL(forkA.AID(), forkA.KEL(), ""); err != nil {
		t.Fatal(err)
	}
	forked := sender{aid: forkB.AID(), kel: forkB.KEL(), keys: signedKeysFor(t, forkB, 0), ksn: forkB.CurrentSeq(), sign: forkB.Sign}

	unknownKey, err := seal.GenerateKeyPair(seal.SuiteX25519, now, now+seal.KeyLifetimeMS)
	if err != nil {
		t.Fatal(err)
	}
	reqKELBytes, _ := identity.MarshalKEL(req.self.KEL())

	msg := func(text string) []byte { return chatBody(t, text, "") }
	cases := []struct {
		name   string
		env    func() []byte
		class  rxClass
		reason string
	}{
		{"not an envelope", func() []byte { return []byte("plaintext") }, rxDropped, seal.ReasonBadOuter},
		{"sealed to another node", func() []byte {
			return craft(t, senderOf(req), other, seal.TypeMessage, id, msg("x"), nil)
		}, rxDropped, seal.ReasonWrongRecipient},
		{"sealed to a key this node never held", func() []byte {
			kel, _ := identity.MarshalKEL(req.self.KEL())
			env, err := seal.Seal(&seal.SealedInner{From: req.AID(), KeyStateSeq: req.self.CurrentSeq(), To: prov.AID(),
				Type: seal.TypeMessage, IX: id, MID: seal.NewMID(), TS: now, Exp: now + messageLifetimeMS,
				Body: msg("x"), KEL: kel, Keys: req.enc.SignedSet()}, &unknownKey.Public, req.self.Sign)
			if err != nil {
				t.Fatal(err)
			}
			return env
		}, rxDropped, seal.ReasonUnknownKey},
		{"ciphertext altered", func() []byte {
			env := craft(t, senderOf(req), prov, seal.TypeMessage, id, msg("x"), nil)
			outer, err := seal.ParseOuter(env)
			if err != nil {
				t.Fatal(err)
			}
			outer.CT[len(outer.CT)-1] ^= 1
			b, err := outer.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			return b
		}, rxDropped, seal.ReasonDecrypt},
		{"expired", func() []byte {
			return craft(t, senderOf(req), prov, seal.TypeMessage, id, msg("x"), func(in *seal.SealedInner) {
				in.TS = now - 20*24*3600*1000
				in.Exp = in.TS + messageLifetimeMS
			})
		}, rxDropped, seal.ReasonExpired},
		{"from the future", func() []byte {
			return craft(t, senderOf(req), prov, seal.TypeMessage, id, msg("x"), func(in *seal.SealedInner) {
				in.TS = now + 10*60*1000
				in.Exp = in.TS + messageLifetimeMS
			})
		}, rxDropped, seal.ReasonFromFuture},
		{"KEL of someone else", func() []byte {
			return craft(t, mallory, prov, seal.TypeMessage, id, msg("x"), func(in *seal.SealedInner) { in.From = req.AID() })
		}, rxDropped, seal.ReasonFromMismatch},
		{"KEL forks from the stored one", func() []byte {
			return craft(t, forked, prov, seal.TypeMessage, id, msg("x"), nil)
		}, rxDropped, dropKELFork},
		{"signed by another key", func() []byte {
			return craft(t, mallory, prov, seal.TypeMessage, id, msg("x"), func(in *seal.SealedInner) {
				in.From, in.KEL, in.KeyStateSeq = req.AID(), reqKELBytes, 0
			})
		}, rxDropped, seal.ReasonBadSig},
		{"unknown type", func() []byte {
			return craft(t, senderOf(req), prov, "anet.bogus/1", id, msg("x"), nil)
		}, rxDropped, dropUnknownType},
		{"message from a party that is not the peer", func() []byte {
			return craft(t, mallory, prov, seal.TypeMessage, id, msg("x"), nil)
		}, rxDropped, dropNotPeer},
		{"result for an interaction this node did not start", func() []byte {
			return craft(t, senderOf(req), prov, seal.TypeResult, id, mustMarshal(t, &delegation.ResultResp{Status: delegation.StatusDone}), nil)
		}, rxDropped, dropWrongRole},
		{"result for an unknown interaction", func() []byte {
			return craft(t, senderOf(req), prov, seal.TypeResult, "ix_nope", mustMarshal(t, &delegation.ResultResp{Status: delegation.StatusDone}), nil)
		}, rxDropped, dropUnknownIX},
		{"delegation reusing this node's outbound interaction id", func() []byte {
			return craft(t, mallory, prov, seal.TypeDelegate, "ix_prov_outbound",
				delegateBody(t, mallory.ctrl, "ix_prov_outbound", "hijack", ""), nil)
		}, rxDropped, dropIXCollision},
		{"delegation reusing another peer's interaction id", func() []byte {
			return craft(t, mallory, prov, seal.TypeDelegate, id, delegateBody(t, mallory.ctrl, id, "hijack", ""), nil)
		}, rxDropped, dropIXCollision},
		{"delegation whose body names another interaction", func() []byte {
			return craft(t, mallory, prov, seal.TypeDelegate, "ix_a", delegateBody(t, mallory.ctrl, "ix_b", "x", ""), nil)
		}, rxDropped, dropIXMismatch},
		{"delegation whose TaskDoc another party signed", func() []byte {
			return craft(t, mallory, prov, seal.TypeDelegate, "ix_c", delegateBody(t, req.self, "ix_c", "x", ""), nil)
		}, rxDropped, dropBadTaskDoc},
		{"body that is not its type", func() []byte {
			return craft(t, senderOf(req), prov, seal.TypeMessage, id, []byte("not cbor"), nil)
		}, rxDropped, dropBadBody},
		{"message without an interaction id", func() []byte {
			return craft(t, senderOf(req), prov, seal.TypeMessage, "", msg("x"), nil)
		}, rxDropped, dropEmptyIX},
		// R02 D5-D7: a status or result is accepted only from the provider
		// of an interaction this node started. req is a peer of prov, but
		// not of ix_prov_outbound.
		{"status from a party that is not the provider", func() []byte {
			return craft(t, senderOf(req), prov, seal.TypeStatus, "ix_prov_outbound",
				mustMarshal(t, &delegation.StatusMsg{State: delegation.StateFailed, Text: "forged", At: now}), nil)
		}, rxDropped, dropNotPeer},
		{"result from a party that is not the provider", func() []byte {
			return craft(t, senderOf(req), prov, seal.TypeResult, "ix_prov_outbound",
				mustMarshal(t, &delegation.ResultResp{Status: delegation.StatusFailed, Deliverable: []byte("forged")}), nil)
		}, rxDropped, dropNotPeer},
		{"status with a state outside the six defined", func() []byte {
			return craft(t, mallory, prov, seal.TypeStatus, "ix_prov_outbound",
				mustMarshal(t, &delegation.StatusMsg{State: "completed-by-mallory", At: now}), nil)
		}, rxDropped, dropBadStatus},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := countMsgs(t, prov, id)
			r := receive(t, prov, tc.env())
			if r.class != tc.class || r.reason != tc.reason {
				t.Fatalf("outcome = %+v, want class %d reason %q", r, tc.class, tc.reason)
			}
			if got := countMsgs(t, prov, id); got != before {
				t.Fatalf("a refused envelope changed the conversation (%d → %d)", before, got)
			}
		})
	}
	// The collisions and the forged replies changed nothing about the
	// interactions they named.
	if ix, _ := prov.ix.Get("ix_prov_outbound"); ix.Role != interactions.RoleOutbound || ix.PeerAID != mallory.aid ||
		ix.State != interactions.StateSubmitted || len(ix.Result) != 0 {
		t.Fatalf("the outbound interaction was taken over or finished: %+v", ix)
	}
	if ix, _ := prov.ix.Get(id); ix.PeerAID != req.AID() {
		t.Fatalf("the inbound interaction was taken over: %+v", ix)
	}

	// Not accepting delegations (unchanged wire-1 semantics): dropped.
	closed := newTestDaemon(t, srv.URL, false)
	r := receive(t, closed, craft(t, mallory, closed, seal.TypeDelegate, "ix_d", delegateBody(t, mallory.ctrl, "ix_d", "x", ""), nil))
	if r.reason != dropNotAccepting {
		t.Fatalf("closed node outcome = %+v", r)
	}

	// A duplicate is acknowledged without being processed again.
	env := craft(t, senderOf(req), prov, seal.TypeMessage, id, msg("once"), nil)
	before := countMsgs(t, prov, id)
	if r := receive(t, prov, env); r.class != rxAccepted {
		t.Fatalf("first copy: %+v", r)
	}
	if r := receive(t, prov, env); r.class != rxDropped || r.reason != dropDuplicate {
		t.Fatalf("second copy: %+v", r)
	}
	if got := countMsgs(t, prov, id); got != before+1 {
		t.Fatalf("a duplicate was stored (%d → %d)", before, got)
	}

	// A store error is temporary: not acknowledged, nothing written.
	prov.setRxFault(func(string) error { return errors.New("injected store failure") })
	r = receive(t, prov, craft(t, senderOf(req), prov, seal.TypeMessage, id, msg("later"), nil))
	prov.setRxFault(nil)
	if r.class != rxTransient || r.reason != transientStore {
		t.Fatalf("store failure outcome = %+v", r)
	}
	if got := countMsgs(t, prov, id); got != before+1 {
		t.Fatal("a rolled-back message was stored")
	}
}

func mustMarshal(t testing.TB, v interface{ Marshal() ([]byte, error) }) []byte {
	t.Helper()
	b, err := v.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A hub that answers a key lookup with another AID's valid KEL and key set
// must not get the message: the sender checks the key set names the
// recipient it asked for (C0). Sealing to the substituted key would hand
// the plaintext to whoever holds it.
func TestAHubSubstitutingKeysForTheRecipientIsRefused(t *testing.T) {
	srv, req, prov := registeredPair(t)
	mallory := newStranger(t)
	kel, _ := identity.MarshalKEL(mallory.kel)
	fake := fakeHubAt(t, srv.URL)
	for name, sub := range map[string]hubapi.KeysResponse{
		"another AID's KEL and keys": {AID: prov.AID(), KeySet: base64.StdEncoding.EncodeToString(mallory.keys),
			KEL: base64.StdEncoding.EncodeToString(kel)},
		"the recipient's KEL with another AID's keys": {AID: prov.AID(), KeySet: base64.StdEncoding.EncodeToString(mallory.keys),
			KEL: base64.StdEncoding.EncodeToString(mustKEL(t, prov))},
	} {
		t.Run(name, func(t *testing.T) {
			fake.mu.Lock()
			fake.keysOverride[prov.AID()] = sub
			fake.mu.Unlock()
			if _, err := req.Delegate(context.Background(), prov.AID(), "secret task", nil); err == nil {
				t.Fatal("the delegation was sealed to keys the hub substituted")
			}
			if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
				t.Fatalf("%d envelopes reached the hub", n)
			}
			if _, err := req.ix.PeerIdentity(prov.AID()); err == nil {
				t.Fatal("the substituted material was recorded for the recipient")
			}
			// Refused by the key set check itself: the set does not name
			// the AID that was asked for.
			_, err := req.fetchRecipientKeys(context.Background(), srv.URL, prov.AID(), "")
			if seal.ReasonOf(err) != seal.ReasonAIDMismatch {
				t.Fatalf("refusal reason = %q (%v), want %s", seal.ReasonOf(err), err, seal.ReasonAIDMismatch)
			}
		})
	}
}

func mustKEL(t *testing.T, d *Daemon) []byte {
	t.Helper()
	b, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// C12: a peer's stored KEL is the rollback protection, and inbound traffic
// from new AIDs must not be able to remove it. After a peer rotated away
// from a key, a message signed by that retired key under the truncated KEL
// in which it was still current is refused — after 600 delegations from
// fresh AIDs (refused under the closed policy) and 600 calls from fresh AIDs
// to a public capability (accepted as public_cap), and again after a
// restart. Neither kind of stranger traffic writes peer_identity (§3.8).
func TestATruncatedKELIsRefusedAfterTableChurnAndRestart(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	prov.ix.SetPeerIdentityCap(100)
	const lampCap = "light.onoff@sim/lamp-1"
	if err := prov.Providers().Register(ctx, &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: lampCap, GlobalPerMin: 5000}}); err != nil {
		t.Fatal(err)
	}
	id, err := req.Delegate(ctx, prov.AID(), "long running work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// The requester rotates; its previous key is what an attacker holds.
	stale := sender{aid: req.AID(), kel: append([]identity.SignedEvent(nil), req.self.KEL()...),
		keys: req.enc.SignedSet(), ksn: req.self.CurrentSeq(), sign: retiredSigner(req.self)}
	rotatedAt := uint64(time.Now().Add(-2 * time.Hour).UnixMilli())
	if err := req.self.Rotate(rotatedAt); err != nil {
		t.Fatal(err)
	}
	// The hub authenticates the requester against its registered KEL, so
	// the rotation is registered before the requester signs with the new
	// key.
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil {
		t.Fatal(err)
	}
	// The provider learns the rotation from an ordinary message.
	if err := req.SendMessage(ctx, id, "rotated", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if kel, _ := prov.peerKEL(req.AID()); len(kel) != 2 {
		t.Fatalf("provider holds %d events of the requester's KEL, want 2", len(kel))
	}

	rows, err := prov.ix.PeerIdentityCount()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		s := newStranger(t)
		ix := "ix_churn_" + s.aid[len(s.aid)-12:]
		if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "hello", ""), nil)); r.reason != dropNotAccepting {
			t.Fatalf("fresh delegation %d: %+v, want refused %s", i, r, dropNotAccepting)
		}
	}
	for i := 0; i < 600; i++ {
		s := newStranger(t)
		ix := "ix_pubcap_" + s.aid[len(s.aid)-12:]
		if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "", lampCap), nil)); r.class != rxAccepted {
			t.Fatalf("fresh public capability call %d refused: %+v", i, r)
		}
	}
	if after, _ := prov.ix.PeerIdentityCount(); after != rows {
		t.Fatalf("stranger traffic changed peer_identity from %d to %d rows", rows, after)
	}

	attack := func(d *Daemon) rxResult {
		return receive(t, d, craft(t, stale, d, seal.TypeMessage, id, chatBody(t, "attacker", ""), func(in *seal.SealedInner) {
			in.TS = rotatedAt - 60_000
			in.Exp = in.TS + messageLifetimeMS
		}))
	}
	before := countMsgs(t, prov, id)
	if r := attack(prov); r.class != rxDropped || r.reason != seal.ReasonGraceExpired {
		t.Fatalf("truncated-KEL message outcome = %+v, want %s", r, seal.ReasonGraceExpired)
	}
	if countMsgs(t, prov, id) != before {
		t.Fatal("the attacker's message was stored")
	}

	// Restart: the record is persistent.
	layout := prov.layout
	if err := prov.Close(); err != nil {
		t.Fatal(err)
	}
	prov2, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	prov2.stopRelayLoop()
	t.Cleanup(func() { prov2.Close() })
	if r := attack(prov2); r.class != rxDropped || r.reason != seal.ReasonGraceExpired {
		t.Fatalf("after restart, truncated-KEL message outcome = %+v", r)
	}
}

// SI-10: a store failure is not a loss. The envelope is not acknowledged,
// nothing it would have written is kept, and the redelivery is processed —
// exactly once, with the capability executed once.
func TestAStoreFailureThenRedeliveryIsProcessedOnce(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1", map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	fail := true
	prov.setRxFault(func(string) error {
		if fail {
			return errors.New("injected store failure")
		}
		return nil
	})
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("the failed delivery was acknowledged (%d left)", n)
	}
	if _, err := prov.ix.Get(id); err == nil {
		t.Fatal("the rolled-back interaction was stored")
	}
	if len(lamp.invoked) != 0 {
		t.Fatal("the capability ran although the delivery failed")
	}
	fail = false
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("the capability ran %d times", n)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("%d envelopes left for the provider", n)
	}
	if got := countMsgs(t, prov, id); got != 2 { // the request line and the result line
		t.Fatalf("provider conversation has %d lines, want 2", got)
	}
}

// SI-10: the same envelope over p2p and the hub at the same moment is
// processed once. The message carries no sender message id (an older
// sender), so only the replay record can tell the two copies apart.
func TestTheSameEnvelopeOverP2PAndHubIsProcessedOnce(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	before := countMsgs(t, prov, id)
	env := craft(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "twice delivered", ""), nil)
	injectEnvelope(t, srv, prov.AID(), env)
	var wg sync.WaitGroup
	var p2pErr, pollErr error
	wg.Add(2)
	go func() { defer wg.Done(); p2pErr = prov.Inbound().Receive(ctx, env) }()
	go func() { defer wg.Done(); pollErr = prov.pollOnce(ctx) }()
	wg.Wait()
	if p2pErr != nil || pollErr != nil {
		t.Fatalf("p2p: %v, poll: %v", p2pErr, pollErr)
	}
	if got := countMsgs(t, prov, id); got != before+1 {
		t.Fatalf("the message was stored %d times", got-before)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("the hub copy was not acknowledged (%d left)", n)
	}
}

// SI-4: every message in an interaction is proven by signature to come from
// the interaction's peer. The hub does not check senders, so anything can
// be injected into a mailbox; both forgeries below are dropped.
func TestAForgedSenderInjectedAtTheHubIsDropped(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	mallory := newStranger(t)
	reqKEL := mustKEL(t, req)
	// Claims to be the requester, carries the requester's public KEL,
	// signed by mallory.
	injectEnvelope(t, srv, prov.AID(), craft(t, mallory, prov, seal.TypeMessage, id, chatBody(t, "forged 1", ""),
		func(in *seal.SealedInner) { in.From, in.KEL, in.KeyStateSeq = req.AID(), reqKEL, 0 }))
	// Honestly signed by mallory, into someone else's interaction.
	injectEnvelope(t, srv, prov.AID(), craft(t, mallory, prov, seal.TypeMessage, id, chatBody(t, "forged 2", ""), nil))
	before := countMsgs(t, prov, id)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countMsgs(t, prov, id); got != before {
		t.Fatalf("a forged message was stored (%d → %d)", before, got)
	}
	if counter(prov, seal.ReasonBadSig) != 1 || counter(prov, dropNotPeer) != 1 {
		t.Fatalf("counters: %v", prov.ReceiveStats())
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatal("forgeries are permanent refusals and must be acknowledged")
	}
}

// SI-3: an unsealed payload — what a wire-1 daemon put in a mailbox — is
// refused. There is no plaintext path.
func TestAPlaintextPayloadIsRefused(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	plain := delegateBody(t, req.self, "ix_plain", "do this in the clear", "")
	injectEnvelope(t, srv, prov.AID(), plain)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.Get("ix_plain"); err == nil {
		t.Fatal("a plaintext delegation was stored")
	}
	if counter(prov, seal.ReasonBadOuter) != 1 {
		t.Fatalf("counters: %v", prov.ReceiveStats())
	}
	// Over p2p an envelope this node cannot open is not acknowledged: it
	// may be another node's, and the sender is sent to the hub, which
	// refuses a plaintext payload itself ([redteam:F22]). Nothing is stored
	// either way.
	if r := prov.Inbound().Receive(ctx, plain); r == nil {
		t.Fatal("an unopenable envelope over p2p was acknowledged; its sender would not try the hub")
	}
	if _, err := prov.ix.Get("ix_plain"); err == nil {
		t.Fatal("a plaintext delegation was stored")
	}
}

// The key set a message carries is advisory (§3.6 step 8): an older one, a
// fork at the stored seq, or one that does not verify is counted and
// ignored, and the message itself is processed.
func TestAStaleOrBadKeysAttachmentDoesNotDropTheMessage(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := prov.ix.PeerIdentity(req.AID())
	if err != nil {
		t.Fatal(err)
	}
	seq0 := row.KeySetSeq
	newer := signedKeysFor(t, req.self, seq0+1000)
	fork := signedKeysFor(t, req.self, seq0+1000) // same seq, different key
	mallory := newStranger(t)

	send := func(keys []byte, text string) rxResult {
		return receive(t, prov, craft(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, text, ""),
			func(in *seal.SealedInner) { in.Keys = keys }))
	}
	if r := send(newer, "newer keys"); r.class != rxAccepted {
		t.Fatalf("newer keys: %+v", r)
	}
	if row, _ := prov.ix.PeerIdentity(req.AID()); row.KeySetSeq != seq0+1000 {
		t.Fatalf("stored seq = %d, want %d", row.KeySetSeq, seq0+1000)
	}
	for _, tc := range []struct {
		name, counter string
		keys          []byte
	}{
		{"older key set", keysRollback, req.enc.SignedSet()},
		{"same seq, different set", keysFork, fork},
		{"another identity's key set", keysInvalid, mallory.keys},
		{"same key set again", "", newer},
	} {
		c0 := counter(prov, tc.counter)
		before := countMsgs(t, prov, id)
		if r := send(tc.keys, tc.name); r.class != rxAccepted {
			t.Fatalf("%s: message refused: %+v", tc.name, r)
		}
		if countMsgs(t, prov, id) != before+1 {
			t.Fatalf("%s: message not stored", tc.name)
		}
		if tc.counter != "" && counter(prov, tc.counter) != c0+1 {
			t.Fatalf("%s: %s not counted: %v", tc.name, tc.counter, prov.ReceiveStats())
		}
		if row, _ := prov.ix.PeerIdentity(req.AID()); row.KeySetSeq != seq0+1000 || !bytes.Equal(row.KeySet, newer) {
			t.Fatalf("%s: stored key set changed", tc.name)
		}
	}
	_ = srv
}

// §3.6 step 7: after a peer's key rotation, a message signed by its
// previous key and dated before the rotation is accepted for rotation_grace
// after the rotation, and refused after it.
func TestRotationGrace(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	old := sender{aid: req.AID(), kel: append([]identity.SignedEvent(nil), req.self.KEL()...),
		keys: req.enc.SignedSet(), ksn: req.self.CurrentSeq(), sign: retiredSigner(req.self)}
	rotatedAt := uint64(time.Now().Add(-30 * time.Minute).UnixMilli())
	if err := req.self.Rotate(rotatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.pinPeerKEL(req.AID(), req.self.KEL(), ""); err != nil {
		t.Fatal(err)
	}
	inFlight := func() []byte {
		return craft(t, old, prov, seal.TypeMessage, id, chatBody(t, "sealed before the rotation", ""),
			func(in *seal.SealedInner) {
				in.TS = rotatedAt - 60_000
				in.Exp = in.TS + messageLifetimeMS
			})
	}
	if r := receive(t, prov, inFlight()); r.class != rxAccepted {
		t.Fatalf("inside the default one-hour grace: %+v", r)
	}
	prov.mu.Lock()
	prov.cfg.RotationGrace = "10m"
	prov.mu.Unlock()
	if r := receive(t, prov, inFlight()); r.class != rxDropped || r.reason != seal.ReasonGraceExpired {
		t.Fatalf("past a ten-minute grace: %+v", r)
	}
	// And a message dated after the rotation, signed by the retired key, is
	// refused whatever the grace.
	prov.mu.Lock()
	prov.cfg.RotationGrace = "3h"
	prov.mu.Unlock()
	late := craft(t, old, prov, seal.TypeMessage, id, chatBody(t, "after", ""), nil)
	if r := receive(t, prov, late); r.class != rxDropped || r.reason != seal.ReasonRevokedKey {
		t.Fatalf("retired key after the rotation: %+v", r)
	}
}

// Step 0: envelopes that arrive through a transport module are rate
// limited before any decryption, and the refusal is temporary, so the
// sender falls back to the hub.
func TestTheP2PRateLimitIsTemporary(t *testing.T) {
	_, req, prov := registeredPair(t)
	prov.p2pLimit.mu.Lock()
	prov.p2pLimit.b = newBucket(0.001, 1)
	prov.p2pLimit.mu.Unlock()
	env := sealFrom(t, req, prov, seal.TypeMessage, "ix_x", chatBody(t, "x", ""))
	if err := prov.Inbound().Receive(context.Background(), env); err != nil && errors.Is(err, errP2PRateLimited) {
		t.Fatal("the first envelope was rate limited")
	}
	err := prov.Inbound().Receive(context.Background(), env)
	if !errors.Is(err, errP2PRateLimited) {
		t.Fatalf("second envelope: %v, want the rate limit", err)
	}
	if counter(prov, transientP2PRate) != 1 {
		t.Fatalf("counters: %v", prov.ReceiveStats())
	}
}

// C16/C3: consumers use the three-branch high-water rule, so re-fetching an
// unchanged key set, and a peer's first three messages before any reply,
// all succeed.
func TestAnUnchangedKeySetIsAcceptedAgain(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := req.fetchRecipientKeys(ctx, srv.URL, prov.AID(), ""); err != nil {
			t.Fatalf("fetch %d of an unchanged key set: %v", i+1, err)
		}
	}
	id, err := req.Delegate(ctx, prov.AID(), "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"second", "third"} {
		if err := req.SendMessage(ctx, id, text, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countMsgs(t, prov, id); got != 3 {
		t.Fatalf("provider stored %d of the first three messages", got)
	}
	// The hub now serves a different set at the stored seq: a fork, refused.
	fake := fakeHubAt(t, srv.URL)
	row, _ := req.ix.PeerIdentity(prov.AID())
	kel := mustKEL(t, prov)
	fake.mu.Lock()
	fake.keysOverride[prov.AID()] = hubapi.KeysResponse{AID: prov.AID(),
		KeySet: base64.StdEncoding.EncodeToString(signedKeysFor(t, prov.self, row.KeySetSeq)),
		KEL:    base64.StdEncoding.EncodeToString(kel)}
	fake.mu.Unlock()
	if _, err := req.fetchRecipientKeys(ctx, srv.URL, prov.AID(), ""); err == nil {
		t.Fatal("a different key set at the stored seq was accepted")
	}
	if counter(req, keysFork) != 1 {
		t.Fatalf("fork not counted: %v", req.ReceiveStats())
	}
}

// A redelivered delegation that was already answered gets the answer
// again — the first may never have reached the requester — and is not
// executed again (§3.6 step 10, C33).
func TestARedeliveredDelegateResendsTheAnswer(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	if _, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1", map[string]any{"on": true}); err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	if r := receive(t, prov, env); r.class != rxAccepted {
		t.Fatalf("first delivery: %+v", r)
	}
	clearMailbox(t, srv, req.AID()) // the first answer is lost on the way
	if r := receive(t, prov, env); r.reason != dropDuplicate {
		t.Fatalf("redelivery: %+v", r)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("the capability ran %d times", n)
	}
	if n := len(queuedFor(t, srv, req.AID())); n != 1 {
		t.Fatalf("%d answers re-sent, want 1", n)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := req.Results(ctx)
	if err != nil || len(res) != 1 {
		t.Fatalf("the re-sent answer did not land: %v %v", res, err)
	}
	// The re-sent answer carries the metadata the first did, not only the
	// state: anet.effect_status (and anet.reason, anet.retry_after_ms when
	// there are any) exist nowhere else.
	got, err := req.ix.Get(res[0].InteractionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.ResultMeta, `"anet.effect_status"`) {
		t.Fatalf("re-sent answer metadata %q", got.ResultMeta)
	}
}

// signedKeysWith makes a one-key SignedEncKeySet for aid at seq, signed by
// sign: what a party holding one of aid's signing keys, current or
// retired, can produce.
func signedKeysWith(t *testing.T, aid string, sign seal.SignFunc, seq uint64) []byte {
	t.Helper()
	now := uint64(time.Now().UnixMilli())
	kp, err := seal.GenerateKeyPair(seal.SuiteX25519, now, now+seal.KeyLifetimeMS)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := seal.SignEncKeySet(&seal.EncKeySet{Type: seal.EncKeySetType, AID: aid, Seq: seq,
		Keys: []seal.EncKey{kp.Public}, IssuedAt: now}, sign)
	if err != nil {
		t.Fatal(err)
	}
	b, err := signed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// §3.5 step 1 and §3.8 on the sending side: once this node holds a
// recipient's KEL, a key set fetched from the hub is checked against that
// KEL, not against the one the hub serves with it. A hub (or anyone holding
// a signing key the recipient has since rotated away from) that serves the
// truncated KEL with a key set signed by the retired key, or a KEL that
// forks from the stored one with a key set signed under the fork, does not
// get this node to seal to its key.
func TestAHubCannotRollBackOrForkAKnownRecipientsKeys(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	if _, err := req.fetchRecipientKeys(ctx, srv.URL, prov.AID(), interactions.PinOutbound); err != nil {
		t.Fatal(err)
	}

	// A copy of the provider's identity before its rotation: whoever holds
	// it can sign under the retired key, and can fork the KEL from there.
	exported, err := prov.self.Export()
	if err != nil {
		t.Fatal(err)
	}
	fork, err := identity.Restore(exported)
	if err != nil {
		t.Fatal(err)
	}
	truncated := append([]identity.SignedEvent(nil), prov.self.KEL()...)
	retired := retiredSigner(prov.self)
	now := uint64(time.Now().UnixMilli())
	if err := prov.self.Rotate(now - 1000); err != nil {
		t.Fatal(err)
	}
	if err := fork.Rotate(now - 2000); err != nil {
		t.Fatal(err)
	}
	// The requester learns the rotation (an accepted message from the
	// provider would record it the same way).
	if _, err := req.pinPeerKEL(prov.AID(), prov.self.KEL(), ""); err != nil {
		t.Fatal(err)
	}
	stored, err := req.ix.PeerIdentity(prov.AID())
	if err != nil {
		t.Fatal(err)
	}

	fake := fakeHubAt(t, srv.URL)
	for _, tc := range []struct {
		name string
		kel  []identity.SignedEvent
		keys []byte
	}{
		{"truncated KEL, key set signed by the retired key", truncated,
			signedKeysWith(t, prov.AID(), retired, stored.KeySetSeq+1000)},
		{"forked KEL, key set signed under the fork", fork.KEL(),
			signedKeysWith(t, prov.AID(), fork.Sign, stored.KeySetSeq+1000)},
		{"current KEL, key set older than the stored one", prov.self.KEL(),
			signedKeysWith(t, prov.AID(), prov.self.Sign, stored.KeySetSeq-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kel, err := identity.MarshalKEL(tc.kel)
			if err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			fake.keysOverride[prov.AID()] = hubapi.KeysResponse{AID: prov.AID(),
				KeySet: base64.StdEncoding.EncodeToString(tc.keys), KEL: base64.StdEncoding.EncodeToString(kel)}
			fake.mu.Unlock()
			if _, err := req.fetchRecipientKeys(ctx, srv.URL, prov.AID(), ""); err == nil {
				t.Fatal("a key set the stored KEL does not support was accepted")
			}
			row, err := req.ix.PeerIdentity(prov.AID())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(row.KeySet, stored.KeySet) || !bytes.Equal(row.KEL, stored.KEL) {
				t.Fatal("the refused material changed the stored record")
			}
		})
	}
}

// §3.6 step 6: a failure to read the sender's peer_identity row is a store
// error, class T. The envelope is not acknowledged and is processed when it
// is delivered again, rather than being judged without the stored KEL.
func TestAPeerRecordReadErrorIsTemporary(t *testing.T) {
	_, req, prov := registeredPair(t)
	env := sealFrom(t, req, prov, seal.TypeMessage, "ix_any", chatBody(t, "x", ""))
	if err := prov.ix.Close(); err != nil {
		t.Fatal(err)
	}
	r := receive(t, prov, env)
	if r.class != rxTransient || r.reason != transientStore {
		t.Fatalf("outcome with the store unreadable = %+v, want class T %s", r, transientStore)
	}
	if err := prov.Inbound().Receive(context.Background(), env); err == nil {
		t.Fatal("a temporary refusal over p2p was acknowledged")
	}
}

// §3.6: a temporary failure becomes permanent once the message has
// expired. A store error that occurs after the message's exp would
// otherwise leave it in the mailbox until the hub's TTL, and every later
// delivery would fail step 5 anyway.
func TestATemporaryFailurePastExpiryIsPermanent(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	env := craft(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "near expiry", ""), func(in *seal.SealedInner) {
		in.TS = uint64(time.Now().UnixMilli()) - 1000
		in.Exp = in.TS + 60_000
	})
	var clock atomic.Uint64
	clock.Store(uint64(time.Now().UnixMilli()))
	prov.setClock(clock.Load)
	prov.setRxFault(func(string) error {
		clock.Add(120_000) // the store fails at a moment the message has expired
		return errors.New("injected store failure")
	})
	r := receive(t, prov, env)
	prov.setRxFault(nil)
	if r.class != rxDropped || r.reason != dropExpiredWait {
		t.Fatalf("store failure past exp: %+v, want class P %s", r, dropExpiredWait)
	}
}

// Step 10 with two copies of one delegation under different message ids
// (the requester sealed it twice) arriving together: step 9 reads the
// interaction for both before either commits. The interaction and its
// opening message are recorded once and the capability runs once.
func TestADelegationSealedTwiceArrivingTogetherIsRecordedOnce(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1", map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	first := onlyQueuedEnvelope(t, srv, prov.AID())
	op, err := seal.Open(first, prov.AID(), prov.enc)
	if err != nil {
		t.Fatal(err)
	}
	second := craft(t, senderOf(req), prov, seal.TypeDelegate, id, op.Inner.Body, nil)

	inTx := make(chan struct{})
	var once sync.Once
	prov.setRxFault(func(typ string) error {
		// The first copy holds its transaction open until the second has
		// had time to pass step 9.
		once.Do(func() { close(inTx); time.Sleep(300 * time.Millisecond) })
		return nil
	})
	var wg sync.WaitGroup
	var r1, r2 rxResult
	wg.Add(2)
	go func() { defer wg.Done(); r1 = receive(t, prov, first) }()
	go func() { defer wg.Done(); <-inTx; r2 = receive(t, prov, second) }()
	wg.Wait()
	prov.setRxFault(nil)
	if !r1.ack() || !r2.ack() {
		t.Fatalf("outcomes %+v %+v", r1, r2)
	}
	waitUntil(t, "the capability call to finish", func() bool {
		_, running := prov.running.Load(id)
		return !running
	})
	ix, err := prov.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := prov.ix.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	opening := 0
	for _, m := range msgs {
		if m.Body == ix.Goal {
			opening++
		}
	}
	if opening != 1 {
		t.Fatalf("the opening message is recorded %d times", opening)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("the capability ran %d times", n)
	}
}

// The requester's side of the same race: two copies of one result under
// different message ids arrive together. The result is stored once and
// "result accepted" is appended to the evidence chain once.
func TestAResultSealedTwiceArrivingTogetherIsRecordedOnce(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	if err := prov.Providers().Register(ctx, &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1", map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	first := onlyQueuedEnvelope(t, srv, req.AID())
	op, err := seal.Open(first, req.AID(), req.enc)
	if err != nil {
		t.Fatal(err)
	}
	second := craft(t, senderOf(prov), req, seal.TypeResult, id, op.Inner.Body, nil)

	before := chainLength(t, req)
	inTx := make(chan struct{})
	var once sync.Once
	req.setRxFault(func(string) error {
		once.Do(func() { close(inTx); time.Sleep(300 * time.Millisecond) })
		return nil
	})
	var wg sync.WaitGroup
	var r1, r2 rxResult
	wg.Add(2)
	go func() { defer wg.Done(); r1 = receive(t, req, first) }()
	go func() { defer wg.Done(); <-inTx; r2 = receive(t, req, second) }()
	wg.Wait()
	req.setRxFault(nil)
	if r1.class != rxAccepted || r2.class != rxAccepted {
		t.Fatalf("outcomes %+v %+v", r1, r2)
	}
	if got := chainLength(t, req) - before; got != 1 {
		t.Fatalf("two copies of one result added %d chain entries, want 1", got)
	}
	if ix, err := req.ix.Get(id); err != nil || ix.State != interactions.StateCompleted {
		t.Fatalf("interaction = %+v %v", ix, err)
	}
}

// §3.5 step 1: a stored recipient key set is used while it verifies, and
// re-checked against the hub in the background once it is older than ten
// minutes. A newer set the recipient published in the meantime replaces it.
func TestAStoredKeySetIsRecheckedAfterTenMinutes(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	row, err := req.ix.PeerIdentity(prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	seq0 := row.KeySetSeq
	// The provider rotates its encryption key and publishes the new set.
	start := uint64(time.Now().UnixMilli())
	prov.setClock(func() uint64 { return start + 7*dayMS + 1 })
	prov.maintainKeyRing()
	newSeq := prov.enc.Seq()
	if newSeq <= seq0 {
		t.Fatal("the provider did not rotate")
	}
	_ = srv

	// Five minutes later the stored set is used as it is.
	req.setClock(func() uint64 { return start + 5*60*1000 })
	if err := req.SendMessage(ctx, id, "at five minutes", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if row, _ := req.ix.PeerIdentity(prov.AID()); row.KeySetSeq != seq0 {
		t.Fatalf("the stored set was re-fetched before ten minutes (seq %d)", row.KeySetSeq)
	}
	// Eleven minutes later the send still uses it, and the check against
	// the hub picks up the newer set.
	req.setClock(func() uint64 { return start + 11*60*1000 })
	if err := req.SendMessage(ctx, id, "at eleven minutes", nil); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the re-check to store the provider's new key set", func() bool {
		row, err := req.ix.PeerIdentity(prov.AID())
		return err == nil && row.KeySetSeq == newSeq
	})
}

// A stored key set that no longer verifies is not sealed to. After the
// recipient rotated its identity key, the set signed under the retired key
// is no longer a current statement (§3.1 rule 2); the sender fetches the
// set the recipient signed again, without waiting for the ten-minute
// re-check.
func TestAStoredKeySetThatNoLongerVerifiesIsReplaced(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	row, err := req.ix.PeerIdentity(prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	seq0 := row.KeySetSeq
	if err := prov.self.Rotate(uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.enc.maintain(prov.self.Sign, prov.self.CurrentSeq()); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil, ""); err != nil {
		t.Fatal(err)
	}
	// The requester follows the rotation (an accepted message from the
	// provider records it the same way).
	if _, err := req.pinPeerKEL(prov.AID(), prov.self.KEL(), ""); err != nil {
		t.Fatal(err)
	}
	if err := req.SendMessage(ctx, id, "after the rotation", nil); err != nil {
		t.Fatal(err)
	}
	row, err = req.ix.PeerIdentity(prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	if row.KeySetSeq == seq0 || row.KeySetSeq != prov.enc.Seq() {
		t.Fatalf("stored seq %d after the send, want the re-signed set %d", row.KeySetSeq, prov.enc.Seq())
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countMsgs(t, prov, id); got != 2 {
		t.Fatalf("provider holds %d messages, want 2", got)
	}
}

// The execution slot re-reads the interaction: a capability call whose
// answer is already stored is not run again, whichever path asks.
func TestAnAnsweredCapabilityCallIsNotRunAgain(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1", map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(lamp.invoked) != 1 {
		t.Fatalf("first delivery ran the capability %d times", len(lamp.invoked))
	}
	prov.runCapabilityCall(id, "light.onoff@sim/lamp-1", map[string]any{"on": true}, nil, nil)
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("an answered call ran again (%d invocations)", n)
	}
}

// A failure status and a result for one interaction arriving together:
// the status passed step 9 before the result committed. The completion
// stands; the status does not turn it into a failure.
func TestAFailureStatusRacingAResultDoesNotUndoIt(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	if err := prov.Providers().Register(ctx, &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1", map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	result := onlyQueuedEnvelope(t, srv, req.AID())
	status := craft(t, senderOf(prov), req, seal.TypeStatus, id,
		mustMarshal(t, &delegation.StatusMsg{State: delegation.StateFailed, Text: "late", At: uint64(time.Now().UnixMilli())}), nil)

	inTx := make(chan struct{})
	var once sync.Once
	req.setRxFault(func(typ string) error {
		if typ == seal.TypeResult {
			once.Do(func() { close(inTx); time.Sleep(300 * time.Millisecond) })
		}
		return nil
	})
	var wg sync.WaitGroup
	var r1, r2 rxResult
	wg.Add(2)
	go func() { defer wg.Done(); r1 = receive(t, req, result) }()
	go func() { defer wg.Done(); <-inTx; r2 = receive(t, req, status) }()
	wg.Wait()
	req.setRxFault(nil)
	if r1.class != rxAccepted || !r2.ack() {
		t.Fatalf("outcomes %+v %+v", r1, r2)
	}
	ix, err := req.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if ix.State != interactions.StateCompleted || len(ix.Receipt) == 0 {
		t.Fatalf("interaction = %s with %d receipt bytes, want completed with the receipt", ix.State, len(ix.Receipt))
	}
}

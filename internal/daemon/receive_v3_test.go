package daemon

// The receive pipeline v3 (engine §4): its two entries, its steps' classes
// and tokens, and the cases engine §11.1 names for ENG-1c (P-ENG-7, E-T14,
// E-T16, E-T17, E-T22, E-T23, E-T29; DLV T8, T11, T13, T19, T27).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// ixLookupFunc is a seal.IXKeyLookup a test writes in place: the second
// layer's key store of step 3a (FS-7's, which a test stands in for).
type ixLookupFunc func(ikid []byte) (*seal.IXKey, error)

func (f ixLookupFunc) IXKey(ikid []byte) (*seal.IXKey, error) { return f(ikid) }

// holdingIXKey is a lookup that holds ks and nothing else.
func holdingIXKey(ks ...*seal.IXKey) seal.IXKeyLookup {
	return ixLookupFunc(func(ikid []byte) (*seal.IXKey, error) {
		for _, k := range ks {
			if string(ikid) == string(k.KID()) {
				return k, nil
			}
		}
		return nil, nil
	})
}

// newIXKey is an interaction key of this node for ix with peer.
func newIXKey(t testing.TB, ix, peer string) *seal.IXKey {
	t.Helper()
	k, err := seal.GenerateIXKey(seal.SuiteX25519, ix, peer)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// receiveDirect hands env to d the way a transport module does, and
// returns the delivery token its ack carries (nil for none) or the error a
// refusal for now is.
func receiveDirect(t testing.TB, d *Daemon, env []byte) ([]byte, error) {
	t.Helper()
	return d.Inbound().Receive(context.Background(), env)
}

// tokenOfDack reads a dack as its sender does, under ack.
func tokenOfDack(t testing.TB, dack []byte, ack seal.AckKey) (seal.Outcome, seal.Note, bool) {
	t.Helper()
	if dack == nil {
		return 0, seal.Note{}, false
	}
	_, o, n, noteOK, ok := seal.VerifyDAck([]seal.AckKey{ack}, dack)
	if !ok || !noteOK {
		t.Fatalf("the ack's token does not verify under the envelope's ack_key")
	}
	return o, n, true
}

// roomyRxBuckets makes d's step-0 buckets large enough for a test's direct
// deliveries.
func roomyRxBuckets(d *Daemon) {
	setRxBuckets(d, newBucket(1e6, 1e6), newBucket(1e6, 1e6))
}

// P-ENG-7 (engine §8.1, §4.8): each failure, injected once through the hub
// mailbox and once over a direct transport, gets the class and the token
// engine §4.2 gives it on each path: before the first layer opens the hub
// path drops it without a token and the direct path refuses it for now;
// from the first layer on both paths decide alike, and the two tokens'
// notes are the same plaintext (ENG-V3).
func TestEachFailureIsAnsweredAlikeOnBothPaths(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	roomyRxBuckets(prov)
	id, err := req.Delegate(ctx, prov.AID(), "a task", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	mallory := newStranger(t)
	other := newTestDaemon(t, "", true)
	unknown, _ := unknownWitnessSender(t)
	foreign := newIXKey(t, "ix_another", req.AID())
	prov.setIXLookup(holdingIXKey(foreign))
	now := uint64(time.Now().UnixMilli())
	msg := func() []byte { return chatBody(t, "x", "") }
	n := 0
	fresh := func() string { n++; return fmt.Sprintf("ix_fresh_%d", n) }

	cases := []struct {
		name    string
		env     func() ([]byte, seal.AckKey)
		layer1  bool // the first layer opens
		class   rxClass
		outcome seal.Outcome // 0: no token
		reason  string
	}{
		{"not an envelope", func() ([]byte, seal.AckKey) { return []byte("plaintext"), seal.AckKey{} },
			false, rxDropped, 0, ""},
		{"sealed to another node", func() ([]byte, seal.AckKey) {
			return craftAck(t, senderOf(req), other, seal.TypeMessage, id, msg(), nil)
		}, false, rxDropped, 0, ""},
		{"first layer altered", func() ([]byte, seal.AckKey) {
			env, ack := craftAck(t, senderOf(req), prov, seal.TypeMessage, id, msg(), nil)
			outer, err := seal.ParseOuter(env)
			if err != nil {
				t.Fatal(err)
			}
			outer.CT[len(outer.CT)-1] ^= 1
			b, err := outer.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			return b, ack
		}, false, rxDropped, 0, ""},
		{"second layer of another interaction", func() ([]byte, seal.AckKey) {
			return craftWith(t, senderOf(req), prov, seal.TypeMessage, id, msg(), craftOpt{peerRK: foreign.RK()})
		}, true, rxDropped, seal.OutcomeRefused, seal.NoteInvalid},
		{"no key set", func() ([]byte, seal.AckKey) {
			return craftAck(t, senderOf(req), prov, seal.TypeMessage, id, msg(), func(in *seal.SealedInner) { in.Keys = nil })
		}, true, rxDropped, seal.OutcomeRefused, seal.NoteInvalid},
		{"expired", func() ([]byte, seal.AckKey) {
			return craftAck(t, senderOf(req), prov, seal.TypeMessage, id, msg(), func(in *seal.SealedInner) {
				in.TS = now - 20*24*3600*1000
				in.Exp = in.TS + messageLifetimeMS
			})
		}, true, rxDropped, seal.OutcomeRefused, seal.NoteInvalid},
		{"a notice that is no notice", func() ([]byte, seal.AckKey) {
			return craftAck(t, mallory, prov, seal.TypeMessage, id, msg(), func(in *seal.SealedInner) {
				in.Notices = [][]byte{{1, 2, 3}}
			})
		}, true, rxDropped, seal.OutcomeRefused, seal.NoteKeyState},
		{"a witness not known yet", func() ([]byte, seal.AckKey) {
			return craftAck(t, unknown, prov, seal.TypeMessage, id, msg(), nil)
		}, true, rxTransient, 0, ""},
		{"signed by another key", func() ([]byte, seal.AckKey) {
			return craftAck(t, impersonating(mallory, req), prov, seal.TypeMessage, id, msg(), nil)
		}, true, rxDropped, seal.OutcomeRefused, seal.NoteInvalid},
		{"an unknown task", func() ([]byte, seal.AckKey) {
			return craftAck(t, mallory, prov, seal.TypeMessage, fresh(), msg(), nil)
		}, true, rxDropped, seal.OutcomeRefused, seal.NoteTaskNotFound},
		{"a stranger's delegation to a closed node", func() ([]byte, seal.AckKey) {
			ix := fresh()
			return craftAck(t, mallory, prov, seal.TypeDelegate, ix, delegateBody(t, mallory.ctrl, ix, "x", ""), nil)
		}, true, rxDropped, seal.OutcomeRefused, seal.NoteNotAccepting},
		{"the requester's message", func() ([]byte, seal.AckKey) {
			return craftAck(t, senderOf(req), prov, seal.TypeMessage, id, msg(), nil)
		}, true, rxAccepted, seal.OutcomeProcessed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hubEnv, hubAck := tc.env()
			r := receive(t, prov, hubEnv)
			if r.class != tc.class {
				t.Fatalf("hub path: %+v, want class %d", r, tc.class)
			}
			ho, hn, hok := tokenOf(t, r, hubAck)
			dirEnv, dirAck := tc.env()
			dack, err := receiveDirect(t, prov, dirEnv)
			switch {
			case !tc.layer1 || tc.class == rxTransient:
				if err == nil {
					t.Fatalf("direct path: acknowledged (%x), want refused for now", dack)
				}
			case err != nil:
				t.Fatalf("direct path: refused for now (%v), want acknowledged", err)
			}
			do, dn, dok := tokenOfDack(t, dack, dirAck)
			if hok != (tc.outcome != 0) || dok != hok {
				t.Fatalf("tokens: hub %v, direct %v; want %v", hok, dok, tc.outcome != 0)
			}
			if ho != tc.outcome || do != tc.outcome || hn.Reason != tc.reason || hn != dn {
				t.Fatalf("hub path %d %+v, direct path %d %+v; want %d %q", ho, hn, do, dn, tc.outcome, tc.reason)
			}
		})
	}
}

// Step 4a (core CORE-K3, K5, G7, G11): a message type must carry its KEL,
// notice and key set and must not carry outer key 8 unless it is one of the
// three one-shot types, which have rules of their own. What breaks them is
// refused bad-kind-keys — tok(2) invalid, unless the envelope is one-shot,
// which no token answers (ENG-V6).
func TestStepFourAChecksTheKeysOfEachType(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "a task", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	const cardIX = "card-0123456789abcdef0123456789abcdef"
	rk, cardRK := newIXKey(t, id, req.AID()), newIXKey(t, cardIX, req.AID())
	prov.setIXLookup(holdingIXKey(rk, cardRK))
	preview, err := (&delegation.ChatMsg{Kind: delegation.ChatStreamPreview, Body: "so far"}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	cardGet, _ := delegation.CardGet{}.Marshal()
	noKEL := func(in *seal.SealedInner) { in.KEL, in.Notices, in.Keys = nil, nil, nil }
	s := senderOf(req)
	cases := []struct {
		name    string
		typ     string
		body    []byte
		o       craftOpt
		refused bool // tok(2) invalid; otherwise no token at all
	}{
		{"a delegation without its key set", seal.TypeDelegate, delegateBody(t, req.self, "ix_k1", "x", ""),
			craftOpt{edit: func(in *seal.SealedInner) { in.Keys = nil }}, true},
		{"a message with outer key 8", seal.TypeMessage, chatBody(t, "x", ""), craftOpt{oneShot: true}, false},
		{"a preview frame without outer key 8", seal.TypeMessage, preview, craftOpt{edit: noKEL, peerRK: rk.RK()}, true},
		{"a preview frame with a KEL", seal.TypeMessage, preview, craftOpt{oneShot: true, peerRK: rk.RK()}, false},
		{"a preview frame of one layer", seal.TypeMessage, preview, craftOpt{oneShot: true, edit: noKEL}, false},
		{"a card request of two layers", delegation.TypeCardGet, cardGet, craftOpt{oneShot: true, peerRK: cardRK.RK()}, false},
		{"a card request without a KEL", delegation.TypeCardGet, cardGet, craftOpt{oneShot: true, edit: noKEL}, false},
		{"a card request without outer key 8", delegation.TypeCardGet, cardGet, craftOpt{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ix := id
			if tc.typ == delegation.TypeCardGet {
				ix = cardIX
			} else if tc.typ == seal.TypeDelegate {
				ix = "ix_k1"
			}
			env, ack := craftWith(t, s, prov, tc.typ, ix, tc.body, tc.o)
			r := receive(t, prov, env)
			if r.class != rxDropped || r.reason != dropBadKindKeys {
				t.Fatalf("%+v, want %s", r, dropBadKindKeys)
			}
			if tc.refused {
				wantToken(t, r, ack, seal.OutcomeRefused, seal.NoteInvalid)
			} else {
				wantToken(t, r, ack, 0, "")
			}
		})
	}
}

// E-T14 (ENG-V13, V18; mutation ENG-M17): a delegation the provider took,
// whose token was lost, sent again after the provider took the requester
// off its allow list, is answered tok(1) — the requester does not read a
// refusal of what was accepted — and nothing is done for it: no re-run, no
// answer sent again.
func TestET14_ADelegationTakenThenUnlistedIsStillTaken(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.Get(id); err != nil {
		t.Fatalf("setup: the provider did not take the delegation: %v", err)
	}
	// The token is lost: the hub keeps none.
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDacks = map[string]string{} })
	if _, err := prov.RemovePeer(req.AID()); err != nil {
		t.Fatal(err)
	}
	sent := relayCountFor(srv.URL)
	injectEnvelope(t, srv, prov.AID(), env) // the requester sends it again
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := counter(prov, redeliveryNotAuthorized); n != 1 {
		t.Fatalf("redelivery-not-authorized %d, want 1", n)
	}
	if n := relayCountFor(srv.URL) - sent; n != 0 {
		t.Fatalf("the provider sent %d envelopes for the redelivery", n)
	}
	askHubNow(t, req)
	if rows := outboxRows(t, req, id); len(rows) != 0 {
		t.Fatalf("the delegation is not confirmed: %+v", rows)
	}
	if cur, _ := req.ix.Get(id); cur.State == interactions.StateRejected {
		t.Fatalf("the requester reads the accepted delegation as refused: %s %s", cur.State, cur.ResultMeta)
	}
}

// E-T16 (ENG-V18; mutation ENG-M21): an answered delegation redelivered
// five times hurries its queued answer, rate limited, and never seals a
// second: the provider keeps one result row, and the requester gets one
// result. Once the requester confirmed it, a redelivery sends nothing.
func TestET16_AnAnsweredDelegationRedeliveredIsAnsweredOnce(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the answer to be queued", func() bool { return len(resultRows(t, prov, id)) == 1 })
	for i := 0; i < 5; i++ {
		injectEnvelope(t, srv, prov.AID(), env)
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(resultRows(t, prov, id)); n != 1 {
		t.Fatalf("%d result rows after five redeliveries, want the one", n)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("the call ran %d times", n)
	}
	waitUntil(t, "the requester to have its result", func() bool {
		_ = req.pollOnce(ctx)
		cur, err := req.ix.Get(id)
		return err == nil && len(cur.Receipt) > 0
	})
	askHubNow(t, prov)
	waitUntil(t, "the answer to be confirmed", func() bool {
		askHubNow(t, prov)
		return len(resultRows(t, prov, id)) == 0
	})
	sent := relayCountFor(srv.URL)
	injectEnvelope(t, srv, prov.AID(), env)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(outboxRows(t, prov, id)); n != 0 {
		t.Fatalf("a redelivery after the answer was confirmed queued %d rows", n)
	}
	if n := relayCountFor(srv.URL) - sent; n != 0 {
		t.Fatalf("a redelivery after the answer was confirmed sent %d envelopes", n)
	}
}

// resultRows are d's queued result rows of task ix.
func resultRows(t testing.TB, d *Daemon, ix string) []interactions.OutboxRow {
	t.Helper()
	var out []interactions.OutboxRow
	for _, r := range outboxRows(t, d, ix) {
		if r.Type == seal.TypeResult {
			out = append(out, r)
		}
	}
	return out
}

// E-T17 (ENG-C2; mutation ENG-M22): the provider's status confirms the
// requester's delegation in the transaction that takes it, and only the
// delegation: the cancel handed over behind it still waits for its own
// token.
func TestET17_AStatusConfirmsOnlyTheDelegation(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the cancel to be handed over", func() bool {
		rows := outboxRows(t, req, id)
		return len(rows) == 2 && rows[1].HandedAt != 0
	})
	_ = srv
	body, err := (&delegation.StatusMsg{State: delegation.StateWorking, At: uint64(time.Now().UnixMilli())}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if r := receive(t, req, craft(t, senderOf(prov), req, seal.TypeStatus, id, body, nil)); r.class != rxAccepted {
		t.Fatalf("the provider's status: %+v", r)
	}
	rows := outboxRows(t, req, id)
	if len(rows) != 1 || rows[0].Kind != rowKindCancel {
		t.Fatalf("after the status: %+v, want the cancel alone", rows)
	}
}

// E-T22 (ENG-V8; mutation ENG-M33): when the interaction key store cannot
// be read, a two-layer envelope is temporary — not acknowledged, no token —
// and not taken as sealed to a closed interaction; once the store reads
// again it is processed.
func TestET22_AStoreErrorAtTheSecondLayerIsTemporary(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	k := newIXKey(t, id, req.AID())
	env, ack := craftWith(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "layered", ""), craftOpt{peerRK: k.RK()})
	prov.setIXLookup(ixLookupFunc(func([]byte) (*seal.IXKey, error) { return nil, errors.New("disk I/O error") }))
	before := countMsgs(t, prov, id)
	r := receive(t, prov, env)
	if r.class != rxTransient || r.reason != transientStore {
		t.Fatalf("with the store failing: %+v, want %s", r, transientStore)
	}
	wantToken(t, r, ack, 0, "")
	injectEnvelope(t, srv, prov.AID(), env)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("the envelope was acknowledged while the store failed (%d left)", n)
	}
	prov.setIXLookup(holdingIXKey(k))
	r = receive(t, prov, env)
	if r.class != rxAccepted {
		t.Fatalf("with the store back: %+v", r)
	}
	wantToken(t, r, ack, seal.OutcomeProcessed, "")
	if countMsgs(t, prov, id) != before+1 {
		t.Fatal("the message was not stored")
	}
}

// issueCapTo has d issue a peer capability of class to grantee, as its
// grant would (caps_issued under d's mailbox key), and returns it with the
// holder key the grantee proves possession with.
func issueCapTo(t testing.TB, d *Daemon, grantee string, class uint64) (*seal.Cap, interactions.IssuedCap, ed25519.PrivateKey) {
	t.Helper()
	mbox, ok := d.mbox.Current()
	if !ok {
		t.Fatal("no mailbox key")
	}
	pub, holder, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	capID := make([]byte, seal.CapIDLen)
	_, _ = rand.Read(capID)
	now := d.nowMS()
	exp := now + 7*24*3600*1000
	c, err := d.mbox.SignCap(&seal.CapBody{Owner: d.AID(), Epoch: mbox.Epoch, CapID: capID, Class: class, CapExp: exp, Holder: pub})
	if err != nil {
		t.Fatal(err)
	}
	row := interactions.IssuedCap{CapID: capID, Grantee: grantee, Class: class, Scope: interactions.CapScopePeer,
		HolderSeed: holder.Seed(), HolderPub: pub, Epoch: mbox.Epoch, CapExp: exp, IssuedAt: now}
	if err := d.ix.Update(func(tx *interactions.Tx) error { return tx.PutIssuedCap(row) }); err != nil {
		t.Fatal(err)
	}
	return c, row, holder
}

// underCap puts a capability's credential in env's outer key 7: the
// holder's proof over env's digest, or, forged, over another digest.
func underCap(t testing.TB, env []byte, c *seal.Cap, row interactions.IssuedCap, holder ed25519.PrivateKey, forged bool) []byte {
	t.Helper()
	digest, err := seal.EnvDigest(env)
	if err != nil {
		t.Fatal(err)
	}
	if forged {
		digest[0] ^= 1
	}
	out, err := seal.SetAccess(env, &seal.Access{Kind: seal.AccessKindCap, Cap: c, Proof: seal.SignAccess(holder, row.CapID, digest)})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// revokedAt is when d revoked the capability capID, 0 while it has not.
func revokedAt(t testing.TB, d *Daemon, capID []byte) uint64 {
	t.Helper()
	c, err := d.ix.IssuedCap(capID)
	if err != nil {
		t.Fatal(err)
	}
	return c.RevokedAt
}

// E-T23 (step 1a, ENG-V7; mutation ENG-M34): a kind-1 credential on a
// mailbox envelope that does not check here is no capability — counted
// access-invalid-at-rx — and the message goes on as one under none; the
// capability is not revoked, nor is it for any failure that envelope then
// meets. Under a credential that checks, a failure its holder cannot meet
// by accident revokes it, and a benign one does not.
func TestET23_ACredentialThatFailsAtTheRecipientIsNoCapability(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	c, row, holder := issueCapTo(t, prov, req.AID(), seal.CapClassContact)
	before := countMsgs(t, prov, id)

	env := craft(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "under a forged proof", ""), nil)
	if r := receive(t, prov, underCap(t, env, c, row, holder, true)); r.class != rxAccepted {
		t.Fatalf("a message under a credential that does not check: %+v, want taken as one under none", r)
	}
	if counter(prov, accessInvalidAtRx) != 1 || countMsgs(t, prov, id) != before+1 {
		t.Fatalf("counters %v", prov.ReceiveStats())
	}
	bad := craft(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "x", ""), func(in *seal.SealedInner) {
		in.Notices = [][]byte{{1, 2, 3}}
	})
	if r := receive(t, prov, underCap(t, bad, c, row, holder, true)); r.reason != seal.ReasonWitnessInvalid {
		t.Fatalf("a bad notice under a forged proof: %+v", r)
	}
	if revokedAt(t, prov, row.CapID) != 0 {
		t.Fatal("a capability was revoked for an envelope whose credential did not check")
	}
	// A credential that checks: an expired message is benign, a bad
	// notice is misuse (SS §4.3.8 item 2).
	expired := craft(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "x", ""), func(in *seal.SealedInner) {
		in.TS = uint64(time.Now().Add(-20 * 24 * time.Hour).UnixMilli())
		in.Exp = in.TS + messageLifetimeMS
	})
	if r := receive(t, prov, underCap(t, expired, c, row, holder, false)); r.reason != seal.ReasonExpired {
		t.Fatalf("an expired message: %+v", r)
	}
	if revokedAt(t, prov, row.CapID) != 0 {
		t.Fatal("a benign failure revoked the capability")
	}
	bad = craft(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "x", ""), func(in *seal.SealedInner) {
		in.Notices = [][]byte{{1, 2, 3}}
	})
	if r := receive(t, prov, underCap(t, bad, c, row, holder, false)); r.reason != seal.ReasonWitnessInvalid {
		t.Fatalf("a bad notice: %+v", r)
	}
	if revokedAt(t, prov, row.CapID) == 0 || counter(prov, cntCapMisuse) != 1 {
		t.Fatal("misuse under a credential that checks did not revoke it")
	}
}

// Step 7½ (SS §4.3.10): a capability used by someone other than its
// grantee is revoked, and the authenticated message goes on as one under no
// capability.
func TestStep7HalfRevokesACapabilityPassedOn(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	stranger := newStranger(t)
	c, row, holder := issueCapTo(t, prov, stranger.aid, seal.CapClassStranger)
	env := craft(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "with someone else's capability", ""), nil)
	if r := receive(t, prov, underCap(t, env, c, row, holder, false)); r.class != rxAccepted {
		t.Fatalf("the message: %+v, want taken as one under no capability", r)
	}
	if revokedAt(t, prov, row.CapID) == 0 || counter(prov, cntCapMisuse) != 1 {
		t.Fatal("a capability used by another sender was not revoked")
	}
}

// E-T29 (ENG-K4; mutation ENG-M41): merging a peer's emergency rotation in
// step 10 voids what it gave this node and revokes what this node gave it,
// in the transaction that commits the message: rolled back with it, and
// done with it. A credential the peer's old data directory holds then
// counts for nothing: a direct delivery under it is charged to the shared
// bucket.
func TestET29_APeersEmergencyRotationGoesWithTheMessage(t *testing.T) {
	keepRevokeLoopOut(t)
	recv := newTestDaemon(t, "", false)
	w := witnessedIdentity(t)
	allowPeers(t, recv, w.AID())
	ws := sender{aid: w.AID(), kel: w.KEL(), keys: signedKeysFor(t, w, 0), ksn: w.CurrentSeq(), sign: w.Sign, ctrl: w}
	if r := receive(t, recv, craft(t, ws, recv, seal.TypeDelegate, "ix-w", delegateBody(t, w, "ix-w", "hi", ""), nil)); r.class != rxAccepted {
		t.Fatalf("the peer's delegation: %+v", r)
	}
	c, row, holder := issueCapTo(t, recv, w.AID(), seal.CapClassContact)
	if err := identitytest.RotateEmergency(w, uint64(time.Now().Add(-time.Minute).UnixMilli())); err != nil {
		t.Fatal(err)
	}
	ws = sender{aid: w.AID(), kel: w.KEL(), keys: signedKeysFor(t, w, 0), ksn: w.CurrentSeq(), sign: w.Sign, ctrl: w}
	rotated := craft(t, ws, recv, seal.TypeMessage, "ix-w", chatBody(t, "rotated", ""), nil)
	recv.setRxFault(func(string) error { return errors.New("store full") })
	if r := receive(t, recv, rotated); r.class != rxTransient {
		t.Fatalf("with step 10 failing: %+v", r)
	}
	recv.setRxFault(nil)
	if revokedAt(t, recv, row.CapID) != 0 {
		t.Fatal("the revocation outlived the transaction that rolled back")
	}
	if r := receive(t, recv, rotated); r.class != rxAccepted {
		t.Fatalf("the rotated peer's message: %+v", r)
	}
	if got, err := recv.ix.IssuedCap(row.CapID); err != nil || got.RevokedAt == 0 || got.RevokePushed {
		t.Fatalf("what this node gave the rotated peer: %+v %v; want revoked, to be pushed", got, err)
	}
	old := craft(t, ws, recv, seal.TypeMessage, "ix-w", chatBody(t, "with the old credential", ""), nil)
	path := rxPath{direct: true}
	recv.rxStep0(underCap(t, old, c, row, holder, false), &path)
	if path.issued != nil {
		t.Fatal("step 0 still takes the revoked credential for a capability")
	}
}

// DLV T8 (ENG-V15): the recipient sends nothing for an immediate refusal.
// A thousand delegations a closed node refuses are acknowledged with their
// tokens, and the node makes no /relay/send.
func TestDLVT8_AFloodOfRefusalsSendsNothing(t *testing.T) {
	srv, _, prov := registeredPair(t)
	ctx := context.Background()
	quietLog(t)
	const flood = 1000
	s := newStranger(t)
	for i := 0; i < flood; i++ {
		ix := fmt.Sprintf("ix_flood_%04d", i)
		injectEnvelope(t, srv, prov.AID(), craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "x", ""), nil))
	}
	sent := relayCountFor(srv.URL)
	for round := 0; round < 2*flood/relayPollLimit && len(queuedFor(t, srv, prov.AID())) > 0; round++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("%d left in the mailbox", n)
	}
	if n := counter(prov, dropNotAccepting); n != flood {
		t.Fatalf("%d refused, want %d", n, flood)
	}
	if n := relayCountFor(srv.URL) - sent; n != 0 {
		t.Fatalf("the refusing node made %d sends", n)
	}
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	tokens := len(h.relayDacks)
	h.mu.Unlock()
	if tokens != flood {
		t.Fatalf("the hub keeps %d tokens, want one per refusal", tokens)
	}
}

// DLV T11 (engine §4.5; mutation DLV M11): the hub holds a delegation back
// and lets its cancel through first. The provider keeps a tombstone for the
// cancel, refuses the delegation when it comes (tok(2) canceled) and runs
// nothing; the requester's task stays canceled.
func TestDLVT11_ACancelAheadOfItsDelegationStopsIt(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "both at the hub", func() bool { return len(queuedFor(t, srv, prov.AID())) == 2 })
	envs := queuedFor(t, srv, prov.AID())
	clearMailbox(t, srv, prov.AID())
	injectEnvelope(t, srv, prov.AID(), envs[1]) // the cancel first
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if counter(prov, cancelTombstoned) != 1 {
		t.Fatalf("no tombstone: %v", prov.ReceiveStats())
	}
	injectEnvelope(t, srv, prov.AID(), envs[0]) // then the delegation
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if counter(prov, dropCanceled) != 1 || len(lamp.invoked) != 0 {
		t.Fatalf("the delegation behind its cancel: %v, %d runs", prov.ReceiveStats(), len(lamp.invoked))
	}
	if _, err := prov.ix.Get(id); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the provider holds the canceled task: %v", err)
	}
	askHubNow(t, req)
	if rows := outboxRows(t, req, id); len(rows) != 0 {
		t.Fatalf("the requester's rows are not confirmed: %+v", rows)
	}
	if cur, _ := req.ix.Get(id); cur.State != interactions.StateCanceled {
		t.Fatalf("the requester's task: %s", cur.State)
	}
}

// DLV T13 (FS-R2; mutation DLV M13): a result sealed to an interaction key
// the requester has deleted — the task ended here — is taken (tok(1)): the
// provider's row is confirmed and not sent again until its deadline.
func TestDLVT13_AResultToAClosedInteractionIsTaken(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	gone := newIXKey(t, id, prov.AID())
	req.setIXLookup(ixLookupFunc(func([]byte) (*seal.IXKey, error) { return nil, nil }))
	body := mustMarshal(t, &delegation.ResultResp{Status: delegation.StatusDone, Deliverable: []byte("late")})
	env, ack := craftWith(t, senderOf(prov), req, seal.TypeResult, id, body, craftOpt{peerRK: gone.RK()})
	r := receive(t, req, env)
	if r.class != rxAccepted || r.reason != seal.ReasonSealedToClosedInteraction {
		t.Fatalf("%+v, want taken as %s", r, seal.ReasonSealedToClosedInteraction)
	}
	wantToken(t, r, ack, seal.OutcomeProcessed, "")
}

// DLV T19 (ENG-V4; mutation DLV M4): the hub puts a copy with one bit of its
// ciphertext flipped into the provider's mailbox, then the original. The
// copy is dropped without a token — nothing can make one before the first
// layer opens — the original is taken, and the requester reads it as
// processed, never as refused.
func TestDLVT19_AnAlteredCopyIsNotAnsweredRefused(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	clearMailbox(t, srv, prov.AID())
	outer, err := seal.ParseOuter(env)
	if err != nil {
		t.Fatal(err)
	}
	outer.CT[len(outer.CT)/2] ^= 0x10
	altered, err := outer.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	injectEnvelope(t, srv, prov.AID(), altered)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	h := fakeHubAt(t, srv.URL)
	items := ackRequestsOf(h)
	if len(items) != 1 || len(items[0]) != 1 || items[0][0].Handle != "" || items[0][0].DAck != "" {
		t.Fatalf("the altered copy's ack: %+v, want its id alone", items)
	}
	injectEnvelope(t, srv, prov.AID(), env)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	askHubNow(t, req)
	if rows := outboxRows(t, req, id); len(rows) != 0 {
		t.Fatalf("not confirmed: %+v", rows)
	}
	if cur, _ := req.ix.Get(id); cur.State == interactions.StateRejected {
		t.Fatal("the requester reads its delegation as refused")
	}
}

// DLV T27 (ENG-V13; DLV-ST-8): a refused delegation sent again after the
// provider restarted is answered with the reason it was first refused with,
// from the refused table.
func TestDLVT27_ARefusalKeepsItsReasonAcrossARestart(t *testing.T) {
	_, _, prov := registeredPair(t)
	s := newStranger(t)
	const ix = "ix_refused_once"
	env, ack := craftAck(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "x", ""), nil)
	r := receive(t, prov, env)
	if r.reason != dropNotAccepting {
		t.Fatalf("first: %+v", r)
	}
	wantToken(t, r, ack, seal.OutcomeRefused, seal.NoteNotAccepting)
	prov2 := restartDaemon(t, prov)
	allowPeers(t, prov2, s.aid) // the policy changed since; the answer does not
	r = receive(t, prov2, env)
	if r.reason != dropRefusedReplay {
		t.Fatalf("after the restart: %+v, want %s", r, dropRefusedReplay)
	}
	wantToken(t, r, ack, seal.OutcomeRefused, seal.NoteNotAccepting)
}

// Step 6 (KEL R-9, D-23): a KEL that brings an event dated more than five
// minutes after this node's clock is refused kel-from-future, answered
// key_state; the events already held are not judged again.
func TestStep6RefusesAKeyEventFromTheFuture(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	if err := identitytest.Rotate(req.self, uint64(time.Now().Add(time.Hour).UnixMilli())); err != nil {
		t.Fatal(err)
	}
	s := senderOf(req)
	s.notice = []byte{0xa0} // step 6 refuses before step 6½ reads it
	env, ack := craftAck(t, s, prov, seal.TypeMessage, id, chatBody(t, "from the future", ""), nil)
	r := receive(t, prov, env)
	if r.reason != seal.ReasonKELFromFuture {
		t.Fatalf("%+v, want %s", r, seal.ReasonKELFromFuture)
	}
	wantToken(t, r, ack, seal.OutcomeRefused, seal.NoteKeyState)
}

// Step 6½ (KEL D-21; ENG-V7): every envelope with a KEL has its notice
// checked, whether or not this node holds a record of the sender. Each
// refusal is answered key_state; a witness this node does not know is
// temporary, and is known once its KEL is.
func TestStep6HalfChecksTheNoticeOfEveryEnvelope(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	s := senderOf(req)
	stale := suiteNotice(t, newStranger(t).kel, uint64(time.Now().UnixMilli()))
	cases := []struct {
		name, reason string
		notice       []byte
		at           uint64
	}{
		{"a notice of another identity", seal.ReasonWitnessMismatch, stale, 0},
		{"no notice", seal.ReasonWitnessInvalid, []byte{}, 0},
		{"a notice older than six hours", seal.ReasonWitnessStale, nil, uint64(time.Now().UnixMilli()) + identity.NoticeMaxAge + 3_600_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			edit := func(in *seal.SealedInner) {
				if tc.notice != nil {
					in.Notices = [][]byte{tc.notice}
				}
			}
			env, ack := craftAck(t, s, prov, seal.TypeMessage, id, chatBody(t, "x", ""), edit)
			if tc.at != 0 {
				prov.setClock(func() uint64 { return tc.at })
				defer prov.setClock(nil)
				env, ack = craftAck(t, s, prov, seal.TypeMessage, id, chatBody(t, "x", ""), func(in *seal.SealedInner) {
					in.TS, in.Exp = tc.at, tc.at+messageLifetimeMS
				})
			}
			r := receive(t, prov, env)
			if r.reason != tc.reason {
				t.Fatalf("%+v, want %s", r, tc.reason)
			}
			wantToken(t, r, ack, seal.OutcomeRefused, seal.NoteKeyState)
		})
	}
	unknown, w := unknownWitnessSender(t)
	env := craft(t, unknown, prov, seal.TypeMessage, "ix_any", chatBody(t, "x", ""), nil)
	if r := receive(t, prov, env); r.class != rxTransient || r.reason != seal.ReasonWitnessUnknown {
		t.Fatalf("a witness not known: %+v", r)
	}
	trustWitness(t, prov, w)
	if r := receive(t, prov, env); r.reason != dropUnknownIX {
		t.Fatalf("once the witness is known: %+v, want judged (%s)", r, dropUnknownIX)
	}
}

// KEL D-20, D-25 (engine §4.7 item 8): a delegation held for approval keeps
// its arrival — from the hub mailbox, the hub's accepted_at, never before
// the sender's time less the clock skew, never after now.
func TestAHeldDelegationKeepsItsArrival(t *testing.T) {
	_, _, prov := registeredPair(t)
	setPolicy(t, prov, PolicyApprove)
	s := newStranger(t)
	const ix = "ix_held_arrival"
	env := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "please", ""), nil)
	now := prov.nowMS()
	accepted := now - 60_000
	if r := prov.receiveEnvelopeVia(context.Background(), env, rxPath{acceptedAt: accepted}); r.class != rxAccepted {
		t.Fatalf("the held delegation: %+v", r)
	}
	item, err := prov.ix.GetPending(ix)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(item.ArrivedAt) != accepted {
		t.Fatalf("held with arrival %d, want the hub's accepted_at %d", item.ArrivedAt, accepted)
	}
}

// Engine §4.5 (HPP PAY-C8, C9; ENG-K2): a card request is answered under the
// one-shot capability it brought, and under no other: without one it is not
// answered. A stranger and a denied peer get the same answer. Nothing is
// written for it: no interaction, no approval, no replay row, no token.
func TestACardRequestIsAnsweredUnderItsOwnGrant(t *testing.T) {
	_, req, prov := registeredPair(t)
	var answers []string
	prov.setCardAnswerHook(func(to, ix string, body []byte) { answers = append(answers, to+" "+ix+" "+string(body)) })
	setPolicy(t, prov, PolicyApprove)
	cardGet, _ := delegation.CardGet{}.Marshal()
	ask := func(from *Daemon, ix string, withGrant bool) rxResult {
		t.Helper()
		var grant []byte
		if withGrant {
			now := from.nowMS()
			grant, _ = issueNow(t, from, prov.AID(), grantFor(capMsg{Kind: capMsgCardGet, IX: ix}, capRel{}, now), now)
		}
		env, ack := craftWith(t, senderOf(from), prov, delegation.TypeCardGet, ix, cardGet,
			craftOpt{oneShot: true, edit: func(in *seal.SealedInner) { in.Grant = grant }})
		r := receive(t, prov, env)
		wantToken(t, r, ack, 0, "")
		return r
	}
	const ix1, ix2 = "card-00000000000000000000000000000001", "card-00000000000000000000000000000002"
	if r := ask(req, ix1, false); r.reason != cardGetNoGrant {
		t.Fatalf("without a grant: %+v", r)
	}
	if len(answers) != 0 {
		t.Fatalf("answered without a grant: %q", answers)
	}
	if r := ask(req, ix2, true); r.class != rxAccepted {
		t.Fatalf("with its grant: %+v", r)
	}
	if len(answers) != 1 {
		t.Fatalf("answers %q, want one", answers)
	}
	// The answer goes under the grant its request brought, kept in memory
	// and not in caps_held (SSR-W8), and only once; the request without a
	// grant has none to go under (ENG-K2).
	cardChoice := func(ix string) AccessChoice {
		t.Helper()
		c, err := prov.accessChoiceFor(req.AID(), ix, false, accessCard, prov.nowMS())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := cardChoice(ix1); c.Send {
		t.Fatalf("the answer to a request without a grant goes under %+v", c)
	}
	held, err := prov.ix.HeldCaps(req.AID())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range held {
		if h.IX == ix2 {
			t.Fatalf("a card request's grant in caps_held: %+v", h)
		}
	}
	if c := cardChoice(ix2); !c.Send || c.Kind != seal.AccessKindCap || c.Cap == nil || c.Cap.IX != ix2 ||
		c.Cap.Issuer != req.AID() {
		t.Fatalf("the answer's credential: %+v, want the request's one-shot grant", c)
	}
	if c := cardChoice(ix2); c.Send {
		t.Fatalf("the reply grant was kept for a second answer: %+v", c)
	}
	for _, ix := range []string{ix1, ix2} {
		if _, err := prov.ix.Get(ix); !errors.Is(err, interactions.ErrNotFound) {
			t.Fatalf("an interaction for a card request: %v", err)
		}
		if _, err := prov.ix.GetPending(ix); !errors.Is(err, interactions.ErrNotFound) {
			t.Fatalf("a card request held for approval: %v", err)
		}
	}
	stranger := registered(t, prov.config().HubURL, "stranger")
	denied := registered(t, prov.config().HubURL, "denied")
	denyPeers(t, prov, denied.AID())
	answers = nil
	ask(stranger, "card-00000000000000000000000000000003", true)
	ask(denied, "card-00000000000000000000000000000004", true)
	if len(answers) != 2 {
		t.Fatalf("answers %q", answers)
	}
	if cardBody(answers[0]) == "" || cardBody(answers[0]) != cardBody(answers[1]) {
		t.Fatalf("a stranger and a denied peer got different answers: %q", answers)
	}
}

// cardBody is the body part of an answer the card hook recorded.
func cardBody(a string) string {
	for i, n := 0, 0; i < len(a); i++ {
		if a[i] == ' ' {
			n++
			if n == 2 {
				return a[i+1:]
			}
		}
	}
	return ""
}

// Engine §4.5, CUX §4.1: a delegation whose TaskDoc does not decode, or
// holds no task, is bad-body (delegation.ErrBody), answered tok(2) invalid
// like any body that is not its type; bad-taskdoc is a TaskDoc whose
// signature does not verify.
func TestATaskDocThatDoesNotDecodeIsBadBody(t *testing.T) {
	_, _, prov := registeredPair(t)
	s := newStranger(t)
	const ix = "ix_bad_taskdoc"
	dr, err := delegation.UnmarshalDelegateReq(delegateBody(t, s.ctrl, ix, "x", ""))
	if err != nil {
		t.Fatal(err)
	}
	dr.TaskDoc = []byte{0xa0} // a TaskDoc of no task
	body, err := dr.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	env, ack := craftAck(t, s, prov, seal.TypeDelegate, ix, body, nil)
	r := receive(t, prov, env)
	if r.reason != dropBadBody {
		t.Fatalf("%+v, want %s", r, dropBadBody)
	}
	wantToken(t, r, ack, seal.OutcomeRefused, seal.NoteInvalid)
	// Its refusal is recorded like every refused delegation's (ENG-V12).
	if exact, reason, _, err := prov.ix.Refused(s.aid, mustMID(t, prov, env)); err != nil || !exact || reason != seal.NoteInvalid {
		t.Fatalf("refused table: %v %q %v", exact, reason, err)
	}
}

// Engine §4.2 step 2, FS §2.10: an envelope from the hub mailbox sealed to an
// epoch key this node does not hold is counted for unknown_key_drops_24h; one
// that came directly is temporary and is not; the count covers 24 hours.
func TestUnknownKeyDropsAreCountedForADay(t *testing.T) {
	_, req, prov := registeredPair(t)
	roomyRxBuckets(prov)
	now := uint64(time.Now().UnixMilli())
	clock := now
	prov.setClock(func() uint64 { return clock })
	stray, err := seal.GenerateKeyPair(seal.SuiteX25519, now, now+seal.MaxKeyValidityMS)
	if err != nil {
		t.Fatal(err)
	}
	sealed := func() []byte {
		kel, _ := identity.MarshalKEL(req.self.KEL())
		env, _, err := seal.Seal(&seal.SealedInner{From: req.AID(), KeyStateSeq: req.self.CurrentSeq(), To: prov.AID(),
			Type: seal.TypeMessage, IX: "ix_x", MID: seal.NewMID(), TS: now, Exp: now + messageLifetimeMS,
			Body: chatBody(t, "x", ""), KEL: kel, Notices: senderOf(req).notices(t), Keys: req.enc.SignedSet()},
			&stray.Public, req.self.Sign, seal.SealOpts{})
		if err != nil {
			t.Fatal(err)
		}
		return withSessionAccess(t, env)
	}
	if r := receive(t, prov, sealed()); r.reason != seal.ReasonUnknownKey {
		t.Fatalf("%+v", r)
	}
	if _, err := receiveDirect(t, prov, sealed()); err == nil {
		t.Fatal("a direct delivery to an unknown key was acknowledged")
	}
	if n := prov.unknownKeyDrops24h(); n != 1 {
		t.Fatalf("%d counted, want the mailbox's one", n)
	}
	clock = now + 23*3600*1000
	if n := prov.unknownKeyDrops24h(); n != 1 {
		t.Fatalf("23 hours on: %d", n)
	}
	clock = now + 25*3600*1000
	if n := prov.unknownKeyDrops24h(); n != 0 {
		t.Fatalf("25 hours on: %d", n)
	}
}

// Engine ENG-V12, [redteam:F5][redteam:F26]: a refusal sticks before the
// envelope's locks are released. A copy of a refused delegation that takes
// the locks the moment the first copy lets them go — after the node began
// to accept its sender — finds the refusal and is not judged again, and
// nothing runs (mutation eng1c-stick-under-lock; the racing version is
// TestSpotCopiesRacingAnAllowAreDecidedOnce).
func TestARefusalSticksBeforeItsLocksAreReleased(t *testing.T) {
	prov := newTestDaemon(t, "", false) // closed
	ctx := context.Background()
	lamp := &countingLamp{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	s := newStranger(t)
	const ix = "ix_stick_under_lock"
	env := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "on", lampCap), nil)
	var second rxResult
	fired := false
	prov.setRxUnlockHook(func() {
		if fired {
			return
		}
		fired = true
		if _, err := prov.AllowPeer(ctx, s.aid, ListAllow); err != nil {
			t.Error(err)
		}
		second = prov.receiveEnvelope(ctx, env)
	})
	first := prov.receiveEnvelope(ctx, env)
	prov.setRxUnlockHook(nil)
	if first.reason != dropNotAccepting {
		t.Fatalf("the first copy: %+v, want %s", first, dropNotAccepting)
	}
	if !fired {
		t.Fatal("the unlock hook did not run")
	}
	if second.class != rxDropped || second.reason != dropRefusedReplay {
		t.Fatalf("the copy behind the locks: %+v, want %s (judged once)", second, dropRefusedReplay)
	}
	waitUntil(t, "no call running", func() bool {
		_, running := prov.running.Load(ix)
		return !running
	})
	if n := lamp.by(s.aid); n != 0 {
		t.Fatalf("the refused delegation ran %d times", n)
	}
}

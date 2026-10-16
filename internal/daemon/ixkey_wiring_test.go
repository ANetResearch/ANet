package daemon

// FS-7: the interaction keys wired into the daemon (forward-secrecy §4.5,
// FS-R1–R5, FS-X1–X7; engine §2.2, §3.2, §4.2). Each side makes its key with
// the task, announces it as rk while it holds it, records the peer's, seals
// to it once it has it, opens with its own, and settles it at the end of
// the task by the table of FS §4.5.4.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// ixKeyRows is how many interaction keys d's store holds, read from the
// database itself.
func ixKeyRows(t testing.TB, d *Daemon) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(d.layout.InteractionsDir(), "interactions.db")+"?_pragma=busy_timeout(15000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ix_key`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ownKID is the kid of d's own key for task ix; it fails the test when d
// holds none.
func ownKID(t testing.TB, d *Daemon, ix string) []byte {
	t.Helper()
	rk, err := d.ix.OwnRK(ix)
	if err != nil || rk == nil {
		t.Fatalf("%s holds no interaction key for %s (%v)", d.AID(), ix, err)
	}
	return seal.KID(rk.Suite, rk.Pub)
}

// keyFate is what became of the key whose kid is kid in d's store:
// "deleted", "close_after" (kept until its close_after) or "kept".
func keyFate(t testing.TB, d *Daemon, kid []byte) string {
	t.Helper()
	k, err := d.ix.IXKeyByKID(kid)
	switch {
	case err != nil:
		t.Fatal(err)
	case k == nil:
		return "deleted"
	case k.CloseAfter != 0:
		return "close_after"
	}
	return "kept"
}

// rkOf is the rk d announces on task ix, as the store records a peer's
// (coredet(RK)).
func rkOf(t testing.TB, d *Daemon, ix string) []byte {
	t.Helper()
	rk, err := d.ix.OwnRK(ix)
	if err != nil || rk == nil {
		t.Fatalf("%s holds no interaction key for %s (%v)", d.AID(), ix, err)
	}
	b, err := rkBytes(&seal.RK{Suite: rk.Suite, Pub: rk.Pub})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// heldPair is a provider under the approve policy and a stranger to it,
// both registered with one fake hub and accepting nobody: what the stranger
// delegates is held for approval.
func heldPair(t *testing.T) (*httptest.Server, *Daemon, *Daemon) {
	t.Helper()
	srv := newFakeHub(t)
	prov := registered(t, srv.URL, "prov")
	s := registered(t, srv.URL, "stranger")
	setPolicy(t, prov, PolicyApprove)
	return srv, s, prov
}

// otherRK is an rk nobody holds a task under.
func otherRK(t testing.TB, ix, peer string) *seal.RK {
	t.Helper()
	k, err := seal.GenerateIXKey(seal.SuiteX25519, ix, peer)
	if err != nil {
		t.Fatal(err)
	}
	return k.RK()
}

// FS-X1 (ENG-Q6): the requester's key is made before the transaction that
// records the task and stored in it, with the task, its first message and
// its outbox row. A transaction that fails leaves no key, no task and no
// envelope; one that commits leaves the key, and the delegation carries its
// rk — one layer, the provider having announced none yet (FS-X5).
func TestDelegateWritesERInSameTx(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	remove := storeFault(t, req, "fs7_outbox_fault",
		`CREATE TRIGGER fs7_outbox_fault BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT, 'disk I/O error (injected)'); END`)
	if _, err := req.Delegate(ctx, prov.AID(), "never recorded", nil); err == nil {
		t.Fatal("a delegation whose transaction failed was recorded")
	}
	if _, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true}); err == nil {
		t.Fatal("a capability call whose transaction failed was recorded")
	}
	if n := ixKeyRows(t, req); n != 0 {
		t.Fatalf("%d interaction keys left by transactions that failed", n)
	}
	if list, err := req.ix.List(interactions.RoleOutbound, "", 0, 100); err != nil || len(list) != 0 {
		t.Fatalf("tasks left by transactions that failed: %d (%v)", len(list), err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("%d envelopes sent for transactions that failed", n)
	}
	remove()

	id, err := req.Delegate(ctx, prov.AID(), "recorded with its key", nil)
	if err != nil {
		t.Fatal(err)
	}
	kid := ownKID(t, req, id)
	op := openAt(t, prov, onlyQueuedEnvelope(t, srv, prov.AID()))
	if op.Layers != 1 || op.RK == nil || !bytes.Equal(op.RK.KID(), kid) {
		t.Fatalf("the delegation: %d layers, rk %v; want one layer carrying the requester's rk", op.Layers, op.RK)
	}
	call, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	ownKID(t, req, call)
	if n := ixKeyRows(t, req); n != 2 {
		t.Fatalf("%d interaction keys for two tasks", n)
	}
}

// FS-X2, FS-X3, FS-R4: the provider makes its key when it takes a
// delegation, in the transaction that records the task, and records the
// requester's rk on it; a held delegation keeps the rk it carried and gets
// a key only when it is approved, in the approval's transaction, with that
// rk as the task's.
func TestProviderKeyOnAcceptAndApprove(t *testing.T) {
	_, req, prov := quietPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "taken", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ownKID(t, prov, id)
	if ix, err := prov.ix.Get(id); err != nil || !bytes.Equal(ix.PeerRK, rkOf(t, req, id)) {
		t.Fatalf("the provider's task records rk %x (%v), want the requester's", ix.PeerRK, err)
	}

	// A delegation held for approval.
	_, s, prov := heldPair(t)
	held, err := s.Delegate(ctx, prov.AID(), "held, then approved", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	item, err := prov.ix.GetPending(held)
	if err != nil {
		t.Fatalf("not held: %v", err)
	}
	if !bytes.Equal(item.PeerRK, rkOf(t, s, held)) {
		t.Fatalf("the held delegation keeps rk %x, want the requester's", item.PeerRK)
	}
	if rk, err := prov.ix.OwnRK(held); err != nil || rk != nil {
		t.Fatalf("a held delegation has a key of the provider's (%v, %v)", rk, err)
	}
	if _, err := prov.ApprovePending(held); err != nil {
		t.Fatal(err)
	}
	ownKID(t, prov, held)
	if ix, err := prov.ix.Get(held); err != nil || !bytes.Equal(ix.PeerRK, item.PeerRK) {
		t.Fatalf("the approved task records rk %x (%v), want the held delegation's", ix.PeerRK, err)
	}
}

// FS-X2: no key is made for a delegation that is refused, held, or held and
// then rejected.
func TestNoKeyOnRefuseOrPending(t *testing.T) {
	_, req, prov := quietPair(t)
	ctx := context.Background()
	// req accepts nobody: prov's delegation to it is refused.
	refused, err := prov.Delegate(ctx, req.AID(), "refused", nil)
	if err != nil {
		t.Fatal(err)
	}
	before := ixKeyRows(t, req)
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := req.ix.Get(refused); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the refused delegation made a task (%v)", err)
	}
	if n := ixKeyRows(t, req); n != before {
		t.Fatalf("a refused delegation left %d keys", n-before)
	}

	_, s, prov := heldPair(t)
	held, err := s.Delegate(ctx, prov.AID(), "held, then rejected", nil)
	if err != nil {
		t.Fatal(err)
	}
	before = ixKeyRows(t, prov)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.GetPending(held); err != nil {
		t.Fatalf("not held: %v", err)
	}
	if n := ixKeyRows(t, prov); n != before {
		t.Fatalf("a held delegation made %d keys", n-before)
	}
	if err := prov.RejectPending(held); err != nil {
		t.Fatal(err)
	}
	if n := ixKeyRows(t, prov); n != before {
		t.Fatalf("a rejected delegation made %d keys", n-before)
	}
}

// FS-R4 with the approval: what the held delegation carried is the task's
// rk once approved, and the provider's answers are sealed to it (two layers).
func TestPendingRKCarriedOnApprove(t *testing.T) {
	srv, s, prov := heldPair(t)
	ctx := context.Background()
	held, err := s.Delegate(ctx, prov.AID(), "held with its rk", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ApprovePending(held); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendStatus(ctx, held, interactions.StateWorking, "on it", nil); err != nil {
		t.Fatal(err)
	}
	// The working status among what the requester's mailbox holds (the
	// pending_approval notice went before the approval).
	waitUntil(t, "the working status at the hub", func() bool {
		for _, env := range queuedFor(t, srv, s.AID()) {
			op := openAt(t, s, env)
			if op.Inner.Type != seal.TypeStatus {
				continue
			}
			if sm, err := delegation.UnmarshalStatusMsg(op.Inner.Body); err != nil || sm.State != delegation.StateWorking {
				continue
			}
			if op.Layers != 2 || !bytes.Equal(op.IKID, ownKID(t, s, held)) || op.RK == nil ||
				!bytes.Equal(op.RK.KID(), ownKID(t, prov, held)) {
				t.Fatalf("the approved task's status: %d layers, ikid %x, rk %v", op.Layers, op.IKID, op.RK)
			}
			return true
		}
		return false
	})
}

// FS-X4, FS-X5 (ENG-K10, K11): one layer until the peer's rk is known, two
// after; every version carries the sealer's rk while it holds its key, and
// none after it is deleted. The provider's result goes after its own key
// went with the task's end, so it carries no rk; the requester's key goes
// when the result is stored.
func TestLayerFollowsPeerRK(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "layers", nil)
	if err != nil {
		t.Fatal(err)
	}
	reqKID := ownKID(t, req, id)
	confirmDelegation(t, req, prov)
	provKID := ownKID(t, prov, id)

	if err := prov.SendStatus(ctx, id, interactions.StateWorking, "on it", nil); err != nil {
		t.Fatal(err)
	}
	op := openAt(t, req, onlyQueuedEnvelope(t, srv, req.AID()))
	if op.Layers != 2 || !bytes.Equal(op.IKID, reqKID) || op.RK == nil || !bytes.Equal(op.RK.KID(), provKID) {
		t.Fatalf("the provider's status: %d layers, ikid %x, rk %v", op.Layers, op.IKID, op.RK)
	}
	settleDelivery(t, prov, req)
	if ix, _ := req.ix.Get(id); !bytes.Equal(ix.PeerRK, rkOf(t, prov, id)) {
		t.Fatalf("the requester recorded rk %x, want the provider's", ix.PeerRK)
	}

	if err := req.SendMessage(ctx, id, "and one more thing", nil); err != nil {
		t.Fatal(err)
	}
	op = openAt(t, prov, onlyQueuedEnvelope(t, srv, prov.AID()))
	if op.Layers != 2 || !bytes.Equal(op.IKID, provKID) || op.RK == nil || !bytes.Equal(op.RK.KID(), reqKID) {
		t.Fatalf("the requester's follow-up: %d layers, ikid %x, rk %v", op.Layers, op.IKID, op.RK)
	}
	settleDelivery(t, req, prov)

	if err := prov.CompleteTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if f := keyFate(t, prov, provKID); f != "deleted" {
		t.Fatalf("the provider's key after it completed the task: %s", f)
	}
	op = openAt(t, req, onlyQueuedEnvelope(t, srv, req.AID()))
	if op.Layers != 2 || !bytes.Equal(op.IKID, reqKID) || op.RK != nil {
		t.Fatalf("the result: %d layers, ikid %x, rk %v; want two layers and no rk", op.Layers, op.IKID, op.RK)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if ix, _ := req.ix.Get(id); len(ix.Receipt) == 0 {
		t.Fatal("the result was not stored")
	}
	if f := keyFate(t, req, reqKID); f != "deleted" {
		t.Fatalf("the requester's key after the result: %s", f)
	}
}

// FS-X4 for every version: a new version sealed while this node holds its
// key carries its rk; one sealed after the key is deleted carries none,
// and still goes in two layers to the peer's rk (ENG-K11, ENG-E2).
func TestEveryVersionCarriesRKWhileHeld(t *testing.T) {
	_, req, prov := quietPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "versions", nil)
	if err != nil {
		t.Fatal(err)
	}
	reqKID := ownKID(t, req, id)
	confirmDelegation(t, req, prov)
	if err := prov.SendStatus(ctx, id, interactions.StateWorking, "on it", nil); err != nil {
		t.Fatal(err)
	}
	settleDelivery(t, prov, req)
	now := req.nowMS()
	row := &interactions.OutboxRow{IX: id, ToAID: prov.AID(), Type: seal.TypeMessage, MID: seal.NewMID(),
		TS: now, Exp: now + messageLifetimeMS}
	body := chatBody(t, "sealed again", "")
	for i := 0; i < 2; i++ {
		sv, err := req.sealVersion(ctx, row, body, i == 1)
		if err != nil {
			t.Fatal(err)
		}
		op := openAt(t, prov, withSessionAccess(t, sv.env))
		if op.Layers != 2 || op.RK == nil || !bytes.Equal(op.RK.KID(), reqKID) {
			t.Fatalf("version %d: %d layers, rk %v", i+1, op.Layers, op.RK)
		}
	}
	// The key goes (the provider's terminal status); a version sealed after
	// carries no rk.
	env, _ := craftWith(t, senderOf(prov), req, seal.TypeStatus, id,
		mustMarshal(t, &delegation.StatusMsg{State: delegation.StateFailed, Text: "gave up", At: now}), craftOpt{})
	if r := receive(t, req, env); r.class != rxAccepted {
		t.Fatalf("the provider's failed status: %+v", r)
	}
	if f := keyFate(t, req, reqKID); f != "deleted" {
		t.Fatalf("the requester's key after the provider failed the task: %s", f)
	}
	sv, err := req.sealVersion(ctx, row, body, false)
	if err != nil {
		t.Fatal(err)
	}
	if op := openAt(t, prov, withSessionAccess(t, sv.env)); op.Layers != 2 || op.RK != nil {
		t.Fatalf("after the key went: %d layers, rk %v; want two layers and no rk", op.Layers, op.RK)
	}
}

// FS-X5, core G11: the card messages belong to no task: one layer and no
// rk, whatever keys the two hold for their tasks; an rk one carries is not
// recorded anywhere.
func TestCardsAreSingleLayerWithoutRK(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	_, _ = previewTask(t, req, prov) // both hold keys for a task of theirs
	log := &sentLog{}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	cix := cardIX(t)
	body, _ := delegation.CardGet{}.Marshal()
	if res := req.oneShotSend(ctx, oneShotCardGet, prov.AID(), cix, body); !res.Handed {
		t.Fatalf("the card request: %+v", res)
	}
	if op := openAt(t, prov, log.last(t)); op.Layers != 1 || op.RK != nil {
		t.Fatalf("the card request: %d layers, rk %v", op.Layers, op.RK)
	}
	// A card request that carries an rk anyway: nothing records it.
	env, _ := craftWith(t, senderOf(req), prov, delegation.TypeCardGet, cix, body,
		craftOpt{oneShot: true, edit: func(in *seal.SealedInner) { in.RK = otherRK(t, cix, prov.AID()) }})
	before := ixKeyRows(t, prov)
	_ = receive(t, prov, env)
	if _, err := prov.ix.Get(cix); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("a card request made a task (%v)", err)
	}
	if n := ixKeyRows(t, prov); n != before {
		t.Fatal("a card request changed the interaction keys")
	}
}

// FS-X6, ENG-K19 (mutation 24): a preview frame goes in two layers, to the
// requester's rk, carrying the provider's; on a task without the peer's rk
// none goes (preview-no-rk).
func TestPreviewIsDoubleLayered(t *testing.T) {
	srv, req, prov := quietPair(t)
	id, reqKey := previewTask(t, req, prov)
	co := &testCoalescer{d: prov}
	prov.setPreviewer(co)
	log := &sentLog{}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	if res := co.frame(t, req.AID(), id, "the first words"); !res.Handed {
		t.Fatalf("the preview frame: %+v", res)
	}
	op, err := seal.Open(log.last(t), req.AID(), req.enc, oneIXKey{reqKey})
	if err != nil {
		t.Fatalf("the requester cannot open the frame with its own key: %v", err)
	}
	if op.Layers != 2 || !bytes.Equal(op.IKID, reqKey.KID()) || op.RK == nil || !bytes.Equal(op.RK.KID(), ownKID(t, prov, id)) {
		t.Fatalf("the preview frame: %d layers, ikid %x, rk %v", op.Layers, op.IKID, op.RK)
	}
	// The same frame through the requester's own receive path: its store
	// opens the second layer.
	if _, err := seal.Open(log.last(t), req.AID(), req.enc, req.ixKeys()); err != nil {
		t.Fatalf("the requester's own lookup does not open the frame: %v", err)
	}

	// A task whose requester announced no rk.
	if err := prov.ix.Update(func(tx *interactions.Tx) error {
		return tx.Create(interactions.New{ID: "ix_no_rk", Role: interactions.RoleInbound, PeerAID: req.AID(), Goal: "g"})
	}); err != nil {
		t.Fatal(err)
	}
	k, err := newOwnIXKey("ix_no_rk", req.AID())
	if err != nil {
		t.Fatal(err)
	}
	putIXKey(t, prov, k)
	sent := log.count()
	before := counter(prov, cntPreviewNoRK)
	if res := co.frame(t, req.AID(), "ix_no_rk", "no rk"); res.Handed {
		t.Fatal("a frame went on a task without the peer's rk")
	}
	if log.count() != sent || counter(prov, cntPreviewNoRK) != before+1 {
		t.Fatalf("sent %d, preview-no-rk %d → %d", log.count()-sent, before, counter(prov, cntPreviewNoRK))
	}
}

// FS-R4 (mutation 11): a message under another rk than the peer announced
// on the task is refused at step 9, rk-mismatch, tok(2) invalid, before
// anything is written — on either side, and for a redelivered delegation
// under a new message id (FS-R5) as for any message. The same rk again is
// taken.
func TestRKMismatchRefusedBeforeWrites(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "one rk per task", nil)
	if err != nil {
		t.Fatal(err)
	}
	delegEnv := onlyQueuedEnvelope(t, srv, prov.AID())
	confirmDelegation(t, req, prov)
	recorded := rkOf(t, req, id)
	reqRK := func(in *seal.SealedInner) { in.RK, _ = storedRK(recorded) }
	stranger := otherRK(t, id, prov.AID())

	// The provider's side: a follow-up under another rk.
	msgs := countMsgs(t, prov, id)
	env, ack := craftWith(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "under another rk", ""),
		craftOpt{edit: func(in *seal.SealedInner) { in.RK = stranger }})
	r := receive(t, prov, env)
	if r.class != rxDropped || r.reason != dropRKMismatch || r.step != rxStep9 {
		t.Fatalf("another rk: %+v, want rk-mismatch at step 9", r)
	}
	wantToken(t, r, ack, seal.OutcomeRefused, seal.NoteInvalid)
	if countMsgs(t, prov, id) != msgs {
		t.Fatal("a message under another rk was stored")
	}
	if ix, _ := prov.ix.Get(id); !bytes.Equal(ix.PeerRK, recorded) {
		t.Fatalf("the task's rk changed to %x", ix.PeerRK)
	}
	// The same rk is taken.
	env, ack = craftWith(t, senderOf(req), prov, seal.TypeMessage, id, chatBody(t, "under its rk", ""), craftOpt{edit: reqRK})
	if r := receive(t, prov, env); r.class != rxAccepted {
		t.Fatalf("the announced rk: %+v", r)
	}
	// A copy of the delegation under a new message id and another rk.
	op := openAt(t, prov, delegEnv)
	env, ack = craftWith(t, senderOf(req), prov, seal.TypeDelegate, id, op.Inner.Body,
		craftOpt{edit: func(in *seal.SealedInner) { in.RK = stranger }})
	r = receive(t, prov, env)
	if r.class != rxDropped || r.reason != dropRKMismatch {
		t.Fatalf("a redelivered delegation under another rk: %+v", r)
	}
	wantToken(t, r, ack, seal.OutcomeRefused, seal.NoteInvalid)

	// The requester's side: the provider announced its rk with a status;
	// a result under another one does not end the task.
	if err := prov.SendStatus(ctx, id, interactions.StateWorking, "on it", nil); err != nil {
		t.Fatal(err)
	}
	settleDelivery(t, prov, req)
	provRK := rkOf(t, prov, id)
	if ix, _ := req.ix.Get(id); !bytes.Equal(ix.PeerRK, provRK) {
		t.Fatalf("the requester recorded %x", ix.PeerRK)
	}
	res := mustMarshal(t, &delegation.ResultResp{Status: delegation.StatusFailed, Deliverable: []byte("forged end")})
	env, ack = craftWith(t, senderOf(prov), req, seal.TypeResult, id, res,
		craftOpt{edit: func(in *seal.SealedInner) { in.RK = otherRK(t, id, req.AID()) }})
	r = receive(t, req, env)
	if r.class != rxDropped || r.reason != dropRKMismatch {
		t.Fatalf("a result under another rk: %+v", r)
	}
	wantToken(t, r, ack, seal.OutcomeRefused, seal.NoteInvalid)
	if ix, _ := req.ix.Get(id); ix.IsTerminal() || !bytes.Equal(ix.PeerRK, provRK) {
		t.Fatalf("after a result under another rk: %s, rk %x", ix.State, ix.PeerRK)
	}
	ownKID(t, req, id)
}

// FS-R4, engine §4.7 item 3: the step-10 transaction checks the rk again —
// a copy that raced this one may have recorded another since step 9 read
// the task — and refuses the message, rolling back what it held.
func TestRKRecheckedInTheReceiveTransaction(t *testing.T) {
	_, req, prov := quietPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "raced", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	other, _ := rkBytes(otherRK(t, id, prov.AID()))
	msgs := countMsgs(t, prov, id)
	err = prov.ix.Update(func(tx *interactions.Tx) error {
		if _, _, err := tx.AddMessageRecord(interactions.MessageRecord{InteractionID: id, SenderAID: req.AID(),
			Kind: interactions.MsgText, Body: "written before the check"}); err != nil {
			return err
		}
		return recordPeerRKTx(tx, &rxMsg{ix: id, rk: other})
	})
	var perm *rxPermanentErr
	if !errors.As(err, &perm) || perm.reason != dropRKMismatch {
		t.Fatalf("the step-10 check: %v, want a permanent rk-mismatch", err)
	}
	if countMsgs(t, prov, id) != msgs {
		t.Fatal("the transaction refused for its rk kept a write")
	}
	// No rk, a held delegation's message, a tombstone or a task gone:
	// nothing to record.
	for _, m := range []*rxMsg{{ix: id}, {ix: id, rk: other, pendingRoute: true}, {ix: id, rk: other, tombstone: true},
		{ix: "ix_gone", rk: other}} {
		if err := prov.ix.Update(func(tx *interactions.Tx) error { return recordPeerRKTx(tx, m) }); err != nil {
			t.Fatalf("%+v: %v", m, err)
		}
	}
}

// FS-R5 (ENG-V13, V18): a copy of a delegation the replay table holds is
// answered tok(1) whatever rk it carries; under another rk than the task's
// it is counted rk-mismatch and nothing follows it — the queued answer is
// not hurried.
func TestDuplicateWithOtherRKGetsTok1NoSideEffect(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "answered once", nil)
	if err != nil {
		t.Fatal(err)
	}
	op := openAt(t, prov, onlyQueuedEnvelope(t, srv, prov.AID()))
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	prov.setOutboxHold(true)
	setHubURL(prov, deadHubURL) // the answer stays queued, in backoff
	if err := prov.CompleteTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	rows := resultRows(t, prov, id)
	if len(rows) != 1 {
		t.Fatalf("%d result rows", len(rows))
	}
	before := rows[0]
	mismatches := counter(prov, dropRKMismatch)
	env, ack := craftWith(t, senderOf(req), prov, seal.TypeDelegate, id, op.Inner.Body,
		craftOpt{edit: func(in *seal.SealedInner) { in.MID = op.Inner.MID; in.RK = otherRK(t, id, prov.AID()) }})
	r := receive(t, prov, env)
	if r.class != rxDropped || r.reason != dropDuplicate {
		t.Fatalf("the copy: %+v, want a duplicate", r)
	}
	wantToken(t, r, ack, seal.OutcomeProcessed, "")
	if got := counter(prov, dropRKMismatch); got != mismatches+1 {
		t.Fatalf("rk-mismatch %d → %d", mismatches, got)
	}
	time.Sleep(50 * time.Millisecond) // a hurried answer would be attempted in the background
	after := resultRows(t, prov, id)
	if len(after) != 1 || after[0].NextAt != before.NextAt || after[0].Attempts != before.Attempts {
		t.Fatalf("the answer was hurried: next_at %d → %d, attempts %d → %d", before.NextAt, after[0].NextAt,
			before.Attempts, after[0].Attempts)
	}
}

// FS-X7 X7b, X7c (mutation 15; J8): a requester's local cancel keeps its
// key for the answer, so the provider's result that comes after is opened
// and stored as a late result, and the key goes with it; the same result
// again is sealed to a closed interaction: taken, tok(1), nothing stored.
func TestLateResultAfterCancelIsStored(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "canceled, then answered", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	kid := ownKID(t, req, id)
	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if f := keyFate(t, req, kid); f != "close_after" {
		t.Fatalf("the key after a local cancel: %s, want kept until close_after", f)
	}
	// The provider has not read the cancel, and answers.
	if err := prov.CompleteTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	result := onlyQueuedEnvelope(t, srv, req.AID())
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ix, err := req.ix.Get(id)
	if err != nil || ix.State != interactions.StateCanceled || len(ix.Receipt) == 0 {
		t.Fatalf("the late result: %+v %v; want it stored on the canceled task", ix, err)
	}
	if f := keyFate(t, req, kid); f != "deleted" {
		t.Fatalf("the key after the late result: %s", f)
	}
	r := receive(t, req, result)
	if r.class != rxAccepted || r.reason != seal.ReasonSealedToClosedInteraction {
		t.Fatalf("the result again: %+v, want sealed-to-closed-interaction", r)
	}
}

// terminalWrites records the Cause of every terminal write per task
// (interactions.SetTerminalWriteHook).
type terminalWrites struct {
	mu sync.Mutex
	m  map[string][]interactions.Cause
}

func watchTerminalWrites(t *testing.T) *terminalWrites {
	w := &terminalWrites{m: map[string][]interactions.Cause{}}
	restore := interactions.SetTerminalWriteHook(func(ix string, c interactions.Cause) {
		w.mu.Lock()
		w.m[ix] = append(w.m[ix], c)
		w.mu.Unlock()
	})
	t.Cleanup(restore)
	return w
}

func (w *terminalWrites) of(ix string) []interactions.Cause {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]interactions.Cause(nil), w.m[ix]...)
}

// FS-X7 (mutation 14; P-5 at the daemon): every path that ends a task
// settles this node's key for it in its own terminal write, with the Cause
// of FS §4.5.4's table — the provider's end deletes E_p; the peer's end
// deletes E_r; a local end while the peer may still answer keeps E_r until
// close_after; a delegation that never left deletes it; a close deletes
// every key. One that passes CauseNotTerminal is refused by the store, and
// its path fails here.
func TestEveryTerminalPathSettlesIXKey(t *testing.T) {
	type side uint8
	const (
		onReq side = iota
		onProv
	)
	type pathCase struct {
		name string
		side side
		// run ends task id (made by req, taken by prov unless neverSent)
		// the way the path does.
		run       func(t *testing.T, hub string, req, prov *Daemon, id string)
		neverSent bool
		cause     interactions.Cause
		fate      string
	}
	ctx := context.Background()
	failedStatus := func(t *testing.T, prov *Daemon, req *Daemon, id string) []byte {
		env, _ := craftWith(t, senderOf(prov), req, seal.TypeStatus, id,
			mustMarshal(t, &delegation.StatusMsg{State: delegation.StateFailed, Text: "gave up", At: prov.nowMS()}), craftOpt{})
		return env
	}
	failedResult := func(t *testing.T, prov *Daemon, req *Daemon, id string) []byte {
		env, _ := craftWith(t, senderOf(prov), req, seal.TypeResult, id,
			mustMarshal(t, &delegation.ResultResp{Status: delegation.StatusFailed, Deliverable: []byte("no")}), craftOpt{})
		return env
	}
	take := func(t *testing.T, d *Daemon, env []byte) {
		t.Helper()
		if r := receive(t, d, env); r.class != rxAccepted {
			t.Fatalf("not taken: %+v", r)
		}
	}
	cancel := func(t *testing.T, d *Daemon, id string) {
		t.Helper()
		if _, err := d.CancelTask(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	cases := []pathCase{
		{name: "provider completes", side: onProv, cause: interactions.CauseProviderEnd, fate: "deleted",
			run: func(t *testing.T, _ string, _, prov *Daemon, id string) {
				if err := prov.CompleteTask(ctx, id); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "provider fails it with a status", side: onProv, cause: interactions.CauseProviderEnd, fate: "deleted",
			run: func(t *testing.T, _ string, _, prov *Daemon, id string) {
				if err := prov.SendStatus(ctx, id, interactions.StateFailed, "cannot", nil); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "provider cancels", side: onProv, cause: interactions.CauseProviderEnd, fate: "deleted",
			run: func(t *testing.T, _ string, _, prov *Daemon, id string) { cancel(t, prov, id) }},
		{name: "provider is told the requester canceled", side: onProv, cause: interactions.CauseProviderEnd, fate: "deleted",
			run: func(t *testing.T, _ string, req, prov *Daemon, id string) {
				cancel(t, req, id)
				if err := prov.pollOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "requester stores the result", side: onReq, cause: interactions.CausePeerEnded, fate: "deleted",
			run: func(t *testing.T, _ string, req, prov *Daemon, id string) {
				if err := prov.CompleteTask(ctx, id); err != nil {
					t.Fatal(err)
				}
				if err := req.pollOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "requester stores a failed result", side: onReq, cause: interactions.CausePeerEnded, fate: "deleted",
			run: func(t *testing.T, _ string, req, prov *Daemon, id string) {
				take(t, req, failedResult(t, prov, req, id))
			}},
		{name: "requester stores a terminal status", side: onReq, cause: interactions.CausePeerEnded, fate: "deleted",
			run: func(t *testing.T, _ string, req, prov *Daemon, id string) {
				take(t, req, failedStatus(t, prov, req, id))
			}},
		{name: "requester cancels after the delegation left", side: onReq, cause: interactions.CauseLocalEnded, fate: "close_after",
			run: func(t *testing.T, _ string, req, _ *Daemon, id string) { cancel(t, req, id) }},
		{name: "requester canceled, then the provider's failed status", side: onReq, cause: interactions.CausePeerEnded, fate: "deleted",
			run: func(t *testing.T, _ string, req, prov *Daemon, id string) {
				cancel(t, req, id)
				take(t, req, failedStatus(t, prov, req, id))
			}},
		{name: "requester canceled, then the provider's failed result", side: onReq, cause: interactions.CausePeerEnded, fate: "deleted",
			run: func(t *testing.T, _ string, req, prov *Daemon, id string) {
				cancel(t, req, id)
				take(t, req, failedResult(t, prov, req, id))
			}},
		{name: "requester canceled, then the provider's late result", side: onReq, cause: interactions.CausePeerEnded, fate: "deleted",
			run: func(t *testing.T, _ string, req, prov *Daemon, id string) {
				cancel(t, req, id)
				if err := prov.CompleteTask(ctx, id); err != nil {
					t.Fatal(err)
				}
				if err := req.pollOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "requester withdraws a delegation that never left", side: onReq, neverSent: true,
			cause: interactions.CauseNeverSent, fate: "deleted",
			run: func(t *testing.T, _ string, req, _ *Daemon, id string) { cancel(t, req, id) }},
		{name: "requester gives up a delegation that left (deadline)", side: onReq, neverSent: true,
			cause: interactions.CauseLocalEnded, fate: "close_after",
			run: func(t *testing.T, hub string, req, _ *Daemon, id string) {
				// neverSent here keeps the delegation queued; it is handed to
				// the hub now, and its deadline passes unconfirmed.
				setHubURL(req, hub)
				_ = retryNow(t, req, outboxRows(t, req, id)[0].ID)
				holdGateOpen(req)
				now := req.nowMS()
				req.setClock(func() uint64 { return now + messageLifetimeMS + 60_000 })
				defer req.setClock(nil)
				_ = retryNow(t, req, outboxRows(t, req, id)[0].ID)
			}},
		{name: "requester refused (outcome 2)", side: onReq, neverSent: true, cause: interactions.CausePeerEnded, fate: "deleted",
			run: func(t *testing.T, _ string, req, _ *Daemon, id string) {
				row := outboxRows(t, req, id)[0]
				if err := req.confirmOutbox(row.ID, seal.OutcomeRefused, seal.Note{Reason: "provider_policy"}, cntDAckConfirmedHub); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "requester: no response", side: onReq, cause: interactions.CauseLocalEnded, fate: "close_after",
			run: func(t *testing.T, _ string, req, _ *Daemon, id string) {
				failed := req.failUnanswered(time.Now().Add(DefaultNoResponseAfter + time.Hour).UnixMilli())
				if len(failed) != 1 || failed[0] != id {
					t.Fatalf("no_response failed %v", failed)
				}
			}},
		{name: "a close (emergency rotation), requester", side: onReq, cause: interactions.CauseClosed, fate: "deleted",
			run: func(t *testing.T, _ string, req, _ *Daemon, id string) { closeAll(t, req) }},
		{name: "a close (emergency rotation), provider", side: onProv, cause: interactions.CauseClosed, fate: "deleted",
			run: func(t *testing.T, _ string, _, prov *Daemon, id string) { closeAll(t, prov) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, req, prov := quietPair(t)
			w := watchTerminalWrites(t)
			hub := srv.URL
			if c.neverSent {
				setHubURL(req, deadHubURL)
			}
			id, err := req.Delegate(ctx, prov.AID(), c.name, nil)
			if err != nil {
				t.Fatal(err)
			}
			d := req
			if !c.neverSent {
				confirmDelegation(t, req, prov)
			}
			if c.side == onProv {
				d = prov
			}
			kid := ownKID(t, d, id)
			c.run(t, hub, req, prov, id)
			if ix, err := d.ix.Get(id); err != nil || !ix.IsTerminal() {
				t.Fatalf("the task did not end: %+v %v", ix, err)
			}
			got := w.of(id)
			found := false
			for _, cause := range got {
				if cause == c.cause {
					found = true
				}
			}
			if !found {
				t.Fatalf("terminal writes on %s: %v; want one with %s", id, got, c.cause)
			}
			if f := keyFate(t, d, kid); f != c.fate {
				t.Fatalf("the key: %s, want %s", f, c.fate)
			}
		})
	}
}

// closeAll is an emergency rotation's close of every task on d (FS-X10).
func closeAll(t *testing.T, d *Daemon) {
	t.Helper()
	var c *closedSet
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		c, err = d.closeInteractionsTx(tx, closeKeyCompromise)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	d.afterClose(c)
	if n := ixKeyRows(t, d); n != 0 {
		t.Fatalf("%d interaction keys after a close", n)
	}
}

// FS-R1, FS-R2 (ENG-V8; with ENG-M33's E-T22 at the pipeline): the lookup
// receive step 3a opens second layers with is the store's IXKeyByKID — a
// key this node holds comes back as seal.IXKey field for field, one it
// does not hold is (nil, nil) (sealed-to-closed-interaction), and a store
// that cannot be read is an error, which Open passes on without a seal
// reason, so the envelope is temporary and not taken as closed.
func TestIXLookupErrorIsTransient(t *testing.T) {
	s, err := interactions.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k, err := newOwnIXKey("ix_1", "aid_peer")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		if err := tx.Create(interactions.New{ID: "ix_1", Role: interactions.RoleOutbound, PeerAID: "aid_peer", Goal: "g"}); err != nil {
			return err
		}
		return putOwnIXKeyTx(tx, k)
	}); err != nil {
		t.Fatal(err)
	}
	l := &storeIXKeys{s: s}
	got, err := l.IXKey(k.KID())
	if err != nil || got == nil || got.IX != k.IX || got.PeerAID != k.PeerAID || got.Suite != k.Suite ||
		!bytes.Equal(got.Pub, k.Pub) || !bytes.Equal(got.Private, k.Private) {
		t.Fatalf("the key it holds: %v %v", got, err)
	}
	if got, err := l.IXKey(bytes.Repeat([]byte{7}, seal.KIDLen)); got != nil || err != nil {
		t.Fatalf("a key it does not hold: %v %v, want (nil, nil)", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = l.IXKey(k.KID())
	if err == nil || got != nil {
		t.Fatalf("a store that cannot be read: %v %v, want an error", got, err)
	}
	if seal.ReasonOf(err) != "" {
		t.Fatalf("a store error carries seal reason %q", seal.ReasonOf(err))
	}
}

// FS-X8: the private half of a key the store's lookup handed to Open is
// zeroed once the receive path is done with it; a test's own lookup keeps
// its keys.
func TestOpenedKeyIsWiped(t *testing.T) {
	_, req, prov := quietPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "wiped after use", nil)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := req.ixKeys().(*storeIXKeys)
	if !ok {
		t.Fatalf("the daemon's lookup is %T, want the store's", req.ixKeys())
	}
	k, err := l.IXKey(ownKID(t, req, id))
	if err != nil || k == nil {
		t.Fatalf("lookup: %v %v", k, err)
	}
	priv := k.Private
	if bytes.Equal(priv, make([]byte, len(priv))) {
		t.Fatal("the key read back is zero already")
	}
	wipeLookup(l)
	if !bytes.Equal(priv, make([]byte, len(priv))) {
		t.Fatal("the private half handed to Open was not wiped")
	}
	if again, err := l.IXKey(ownKID(t, req, id)); err != nil || again == nil || bytes.Equal(again.Private, priv) {
		t.Fatalf("the stored key went with the wipe: %v %v", again, err)
	}
	fixed := holdingIXKey(k)
	wipeLookup(fixed) // a test's lookup is not the store's: nothing to wipe
}

// FS-X8 on the receive path: the key a second layer was opened with is
// wiped before the message is handled any further.
func TestReceiveWipesTheKeyItOpenedWith(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "opened, then wiped", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	if err := prov.SendStatus(ctx, id, interactions.StateWorking, "on it", nil); err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, req.AID())
	l := &storeIXKeys{s: req.ix}
	req.setIXLookup(l)
	if r := receive(t, req, env); r.class != rxAccepted {
		t.Fatalf("the status: %+v", r)
	}
	if len(l.gave) != 1 {
		t.Fatalf("the lookup handed out %d keys, want the one", len(l.gave))
	}
	if p := l.gave[0].Private; len(p) == 0 || !bytes.Equal(p, make([]byte, len(p))) {
		t.Fatal("the key the second layer was opened with was not wiped")
	}
	ownKID(t, req, id) // the stored key stays
}

// FS-R4, step 9 for the two routes the plain message test does not reach: a
// message for a delegation held for approval is compared with the rk the
// held delegation carried, and a preview frame with the rk of its task
// (never recorded); another rk is refused rk-mismatch before anything is
// written.
func TestRKComparedOnHeldAndPreviewRoutes(t *testing.T) {
	ctx := context.Background()

	// A held delegation.
	_, s, hp := heldPair(t)
	held, err := s.Delegate(ctx, hp.AID(), "held with its rk", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := hp.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	item, err := hp.ix.GetPending(held)
	if err != nil {
		t.Fatalf("not held: %v", err)
	}
	other, err := rkBytes(otherRK(t, held, s.AID()))
	if err != nil {
		t.Fatal(err)
	}
	if r := hp.authorizeRK(&rxMsg{ix: held, rk: other, pendingRoute: true}); r == nil || r.reason != dropRKMismatch {
		t.Fatalf("a message for a held delegation under another rk: %+v", r)
	}
	if r := hp.authorizeRK(&rxMsg{ix: held, rk: item.PeerRK, pendingRoute: true}); r != nil {
		t.Fatalf("a message for a held delegation under its rk: %+v", r)
	}

	// A preview frame.
	_, req, prov := quietPair(t)
	id, reqKey := previewTask(t, req, prov)
	if err := prov.SendStatus(ctx, id, interactions.StateWorking, "on it", nil); err != nil {
		t.Fatal(err)
	}
	settleDelivery(t, prov, req)
	provRK := rkOf(t, prov, id)
	body, err := (&delegation.ChatMsg{Kind: delegation.ChatStreamPreview, Body: "so far"}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	frame := func(rk *seal.RK) rxResult {
		env, _ := craftWith(t, senderOf(prov), req, seal.TypeMessage, id, body, craftOpt{oneShot: true, peerRK: reqKey.RK(),
			edit: func(in *seal.SealedInner) { in.KEL, in.Notices, in.Keys, in.RK = nil, nil, nil, rk }})
		return receive(t, req, env)
	}
	before := counter(req, dropRKMismatch)
	if r := frame(otherRK(t, id, req.AID())); r.class != rxDropped || r.reason != dropRKMismatch || r.step != rxStep9 {
		t.Fatalf("a preview frame under another rk: %+v", r)
	}
	if counter(req, dropRKMismatch) != before+1 {
		t.Fatal("the mismatch was not counted")
	}
	if r := frame(nil); r.reason == dropRKMismatch {
		t.Fatalf("a preview frame without an rk: %+v", r)
	}
	if ix, _ := req.ix.Get(id); !bytes.Equal(ix.PeerRK, provRK) {
		t.Fatalf("the task's rk changed to %x", ix.PeerRK)
	}
}

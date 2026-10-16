package daemon

// ENG-1d: the crash and restart cases of the outbound engine (engine §6.2,
// E-T1 to E-T4, E-T7) and the delivery cases of p2p-receipts §7 that need a
// sender and a recipient together (DLV T5, T6, T7, T20, T31, T35).
//
// A crash is made the way it happens: the data directory is copied while a
// request of the daemon is in flight (the hub has read the bytes and not
// answered yet), the daemon is closed, and a new daemon opens the copy. What
// the daemon held in memory is gone; only what was written stays.

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// crashCopy is d's data directory as it stands now, copied to a new
// directory: the state a crash at this moment leaves. d must have nothing
// else writing (a quietPair node with its loops idle).
func crashCopy(t testing.TB, d *Daemon) Layout {
	t.Helper()
	src, dst := d.layout.Root, t.TempDir()
	err := filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		switch {
		case e.IsDir():
			return os.MkdirAll(to, info.Mode().Perm())
		case info.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(to, b, info.Mode().Perm())
		}
		return nil // sockets and the like are not state
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewLayout(dst)
}

// startFrom closes old (it is the process that died) and starts a daemon on
// the crashed directory.
func startFrom(t testing.TB, old *Daemon, l Layout) *Daemon {
	t.Helper()
	_ = old.Close()
	n, err := New(l)
	if err != nil {
		t.Fatalf("the start after the crash: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	return n
}

// hubRowRID is the rid of the one row the fake hub holds for aid.
func hubRowRID(t testing.TB, url, aid string) []byte {
	t.Helper()
	h := fakeHubAt(t, url)
	h.mu.Lock()
	defer h.mu.Unlock()
	var rid []byte
	n := 0
	for _, m := range h.mailbox {
		if m.toAID == aid {
			rid = bytes.Clone(m.rid)
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the hub holds %d rows for the recipient, want 1", n)
	}
	return rid
}

// crashedAtSend is a requester that was killed while the hub was reading its
// delegation: the hub took it (the mailbox row exists), the requester never
// heard. It returns the hub, the started-again requester, the provider, the
// task and the version the first send carried.
func crashedAtSend(t *testing.T) (srv string, req, prov *Daemon, id string, sent [32]byte, row interactions.OutboxRow) {
	t.Helper()
	s, req0, prov := quietPair(t)
	var snap Layout
	var once atomic.Bool
	var seen [32]byte
	log := &sentLog{next: func(_ int, env []byte) fakeSendAnswer {
		if once.CompareAndSwap(false, true) {
			// The first byte is at the hub: the row says attempting, the
			// version and its ack_key are written, no rid yet.
			rows := allOutboxRows(t, req0)
			if len(rows) != 1 || !rows[0].Attempting || rows[0].CurVer == 0 {
				t.Errorf("at the first byte: %+v, want one row attempting under a written version", rows)
			}
			seen, _ = seal.EnvDigest(env)
			snap = crashCopy(t, req0)
		}
		return fakeSendAnswer{}
	}}
	setFake(t, s.URL, func(h *fakeHub) { h.sendHook = log.hook })
	id, err := req0.Delegate(context.Background(), prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !once.Load() {
		t.Fatal("the hub was never reached")
	}
	req = startFrom(t, req0, snap)
	req.stopRelayLoop()
	return s.URL, req, prov, id, seen, outboxRows(t, req, id)[0]
}

// E-T1 (CR1): a delegation queued and the process killed before any attempt
// went through: the first round after the start sends it at once, whatever
// backoff the row carries, and the message identity is the one queued.
func TestET1_AQueuedRowGoesInTheFirstRoundAfterAKill(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	knowPeer(t, req, prov.AID())
	setHubURL(req, deadHubURL)
	id, err := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, req, id)
	if len(rows) != 1 || rows[0].HandedAt != 0 || rows[0].NextAt <= int64(req.nowMS()) {
		t.Fatalf("setup: %+v, want a row never handed over and waiting out a backoff", rows)
	}
	mid := bytes.Clone(rows[0].MID)
	req = restartDaemon(t, req)
	waitUntil(t, "the first round to send it", func() bool {
		rs := outboxRows(t, req, id)
		return len(rs) == 1 && rs[0].HandedAt != 0 && !rs[0].Attempting
	})
	if rs := outboxRows(t, req, id); !bytes.Equal(rs[0].MID, mid) {
		t.Fatalf("the message id changed across the kill: %x → %x", mid, rs[0].MID)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("the hub holds %d envelopes, want 1", n)
	}
}

// E-T2 (CR2, CR3; fx12-2): killed inside an attempt, after the version and
// the mark were written and before the hub answered. After the start the row
// is handed over: a cancel does not withdraw the delegation, and the
// recipient's token for it can still be verified.
func TestET2_AKillInsideAnAttemptLeavesTheRowHandedOver(t *testing.T) {
	srv, req, prov, id, _, _ := crashedAtSend(t)
	// The start counts the row handed over on its own schedule; looking at
	// the row the instant the daemon is up raced it under load (T-1).
	waitUntil(t, "the start to count the row handed over", func() bool {
		r := outboxRows(t, req, id)
		return len(r) == 1 && r[0].HandedAt != 0 && !r[0].Attempting
	})
	if _, err := req.CancelTask(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, req, id)
	if len(rows) != 2 || rows[0].Type != seal.TypeDelegate || !rows[0].Frozen || rows[1].Kind != rowKindCancel {
		t.Fatalf("after the cancel: %+v, want the delegation frozen and the cancel behind it", rows)
	}
	// The provider took the delegation from the hub; its token is checked
	// against the ack_key the crashed process had written.
	if err := prov.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.Get(id); err != nil {
		t.Fatalf("the provider did not take the delegation: %v", err)
	}
	r := outboxRows(t, req, id)[0]
	k := ackKeysOf(rowVersions(t, req, r.ID))
	h := k[0].Handle()
	fh := fakeHubAt(t, srv)
	fh.mu.Lock()
	raw := fh.relayDacks[hubapi.B64(h[:])]
	fh.mu.Unlock()
	dack, _ := hubapi.DecodeB64(raw, seal.DAckLen)
	if _, o, _, _, ok := seal.VerifyDAck(k, dack); !ok || o != seal.OutcomeProcessed {
		t.Fatalf("the provider's token does not verify under the ack_key the crashed process wrote (ok %v, outcome %d)", ok, o)
	}
}

// E-T3 (CR4): the hub answered 200 and the requester died before it wrote
// the rid. After the start the same version is sent again, the hub knows it
// and answers under the same rid, and the delegation is confirmed.
func TestET3_AKillAfterTheHubsAnswerSendsTheSameVersionAndGetsTheSameRID(t *testing.T) {
	srv, req, prov, id, sent, _ := crashedAtSend(t)
	rid := hubRowRID(t, srv, prov.AID())
	waitUntil(t, "the version to be sent again and its rid written", func() bool {
		rs := outboxRows(t, req, id)
		if len(rs) != 1 || rs[0].Attempting {
			return false
		}
		vs := rowVersions(t, req, rs[0].ID)
		return len(vs) == 1 && bytes.Equal(vs[0].RID, rid)
	})
	if d := currentDigest(t, req, outboxRows(t, req, id)[0].ID); d != sent {
		t.Fatalf("the version sent again is another one: %x, was %x", d[:4], sent[:4])
	}
	hubRowRID(t, srv, prov.AID()) // still one row: the hub did not take a second
	settleDelivery(t, req, prov)
	if rows := outboxRows(t, req, id); len(rows) != 0 {
		t.Fatalf("not confirmed: %+v", rows)
	}
	if n := len(allInbound(t, prov)); n != 1 {
		t.Fatalf("the provider holds %d tasks, want 1", n)
	}
}

// allInbound are d's inbound tasks.
func allInbound(t testing.TB, d *Daemon) []*interactions.Interaction {
	t.Helper()
	list, err := d.ix.List(interactions.RoleInbound, "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// E-T4 (CR6): killed after the status query found the message done and
// before the confirmation. After the start the row is confirmed once, and
// the task changes state at most once.
func TestET4_AKillAfterDoneConfirmsOnceAfterTheStart(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var snap Layout
	var once atomic.Bool
	setFake(t, srv.URL, func(h *fakeHub) {
		h.statusHook = func(hubapi.RelayStatusItem) (hubapi.RelayStatusResult, bool) {
			if once.CompareAndSwap(false, true) {
				snap = crashCopy(t, req)
			}
			return hubapi.RelayStatusResult{}, false
		}
	})
	askHubNow(t, req)
	if !once.Load() {
		t.Fatal("the hub was never asked")
	}
	req = startFrom(t, req, snap)
	req.stopRelayLoop()
	before := ixOf(t, req, id).StateSeq
	if rows := outboxRows(t, req, id); len(rows) != 1 {
		t.Fatalf("the crashed state has %d rows, want the unconfirmed one", len(rows))
	}
	askHubNow(t, req)
	waitUntil(t, "the confirmation", func() bool { return len(outboxRows(t, req, id)) == 0 })
	if n := counter(req, cntDAckConfirmedHub) + counter(req, cntDAckConfirmedP2P); n != 1 {
		t.Fatalf("confirmed %d times after the start, want once", n)
	}
	if after := ixOf(t, req, id).StateSeq; after > before+1 {
		t.Fatalf("the task changed state %d times, want at most once", after-before)
	}
}

// E-T7 (CR13): killed while the recipient's keys were being fetched again
// after a kid_stale. After the start the trigger is judged again, the
// delegation is sealed to the new keys and delivered, and its message id is
// the one the crashed process had written.
func TestET7_AKillDuringTheDirectoryRequestKeepsTheMessageIdentity(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	knowPeer(t, req, prov.AID())
	old := keySetOf(t, prov)
	if _, err := prov.regenerateKeys(prov.self.CurrentSeq() + 100); err != nil {
		t.Fatal(err)
	}
	if err := prov.publishKeys(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	isOld := func(kid []byte) bool {
		for _, k := range old.Keys {
			if bytes.Equal(k.KID, kid) {
				return true
			}
		}
		return false
	}
	var snap Layout
	var once atomic.Bool
	log := &sentLog{next: func(_ int, env []byte) fakeSendAnswer {
		if o, err := seal.ParseOuter(env); err == nil && isOld(o.KID) {
			if once.CompareAndSwap(false, true) {
				snap = crashCopy(t, req) // the 409 is out; the directory request is next
			}
			return fakeSendAnswer{kind: fakeSendRefuse, status: 409, code: codeKidStale}
		}
		return fakeSendAnswer{}
	}}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !once.Load() {
		t.Fatal("no send under the old keys")
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = nil })
	crashed := outboxRows(t, req, id)[0]
	req = startFrom(t, req, snap)
	req.stopRelayLoop()
	waitUntil(t, "the delegation to go again", func() bool {
		rs := outboxRows(t, req, id)
		return len(rs) == 1 && rs[0].HandedAt != 0 && !rs[0].Attempting
	})
	rows := outboxRows(t, req, id)
	if !bytes.Equal(rows[0].MID, crashed.MID) || rows[0].TS != crashed.TS || rows[0].Exp != crashed.Exp {
		t.Fatalf("the message identity changed across the crash: %x/%d/%d → %x/%d/%d",
			crashed.MID, crashed.TS, crashed.Exp, rows[0].MID, rows[0].TS, rows[0].Exp)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(allInbound(t, prov)); n != 1 {
		t.Fatalf("the provider holds %d tasks, want 1", n)
	}
}

// DLV T5 (DLV-C-4; ENG-M18): the hub loses the token (its staging is gone).
// The sender is told unknown, sends the same message again, the recipient
// gives the duplicate tok(1) without doing anything twice, and the row is
// confirmed.
func TestDLVT5_ALostTokenIsGivenAgainForTheDuplicate(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDacks = map[string]string{} })
	askHubNow(t, req)
	if counter(req, cntDAckStatusUnknown) == 0 {
		t.Fatal("the lost token was not found unknown")
	}
	row := outboxRows(t, req, id)
	if len(row) != 1 {
		t.Fatalf("the row went without a token: %+v", row)
	}
	if err := retryNow(t, req, row[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	askHubNow(t, req)
	if rows := outboxRows(t, req, id); len(rows) != 0 {
		t.Fatalf("the duplicate's token did not confirm: %+v", rows)
	}
	if n := len(allInbound(t, prov)); n != 1 {
		t.Fatalf("the provider holds %d tasks, want 1", n)
	}
}

// DLV T6 (a): the recipient committed a direct delivery and the answer never
// came back, so there is no token on that path. The sender goes to the hub,
// the recipient gives the duplicate tok(1), and the row is confirmed. (The
// sender killed inside an attempt is E-T2.)
func TestDLVT6_ACommitWithoutAnAnswerIsConfirmedByTheDuplicate(t *testing.T) {
	_, req, prov := quietPair(t)
	ctx := context.Background()
	req.RegisterTransport(deliverThenErrorP2P{to: prov})
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(allInbound(t, prov)); n != 1 {
		t.Fatalf("the direct delivery was not committed: %d tasks", n)
	}
	if len(outboxRows(t, req, id)) != 1 {
		t.Fatal("the row is gone without a token")
	}
	settleDelivery(t, req, prov)
	if rows := outboxRows(t, req, id); len(rows) != 0 {
		t.Fatalf("not confirmed by the duplicate: %+v", rows)
	}
	if n := len(allInbound(t, prov)); n != 1 {
		t.Fatalf("the duplicate ran again: %d tasks", n)
	}
}

// DLV T7 (ENG-C1): a refused delegation is answered tok(2) and the task is
// rejected. A note moved from another message's token leaves the outcome
// (refused) and is itself void: counted, and no reason shown.
func TestDLVT7_TheOutcomeIsBoundAndANoteFromAnotherMessageIsVoid(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	denyPeers(t, prov, req.AID())
	a, err := req.Delegate(ctx, prov.AID(), "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := req.Delegate(ctx, prov.AID(), "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	handle := func(id string) string {
		r := outboxRows(t, req, id)[0]
		k := ackKeysOf(rowVersions(t, req, r.ID))[0]
		h := k.Handle()
		return hubapi.B64(h[:])
	}
	ha, hb := handle(a), handle(b)
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	da, db := h.relayDacks[ha], h.relayDacks[hb]
	h.mu.Unlock()
	rawA, _ := hubapi.DecodeB64(da, seal.DAckLen)
	rawB, _ := hubapi.DecodeB64(db, seal.DAckLen)
	if rawA == nil || rawB == nil {
		t.Fatalf("the hub holds no token for a refusal (%q, %q)", ha, hb)
	}
	swapped := append(bytes.Clone(rawB[:seal.TokenLen]), rawA[seal.TokenLen:]...)
	setFake(t, srv.URL, func(h *fakeHub) {
		h.statusHook = func(it hubapi.RelayStatusItem) (hubapi.RelayStatusResult, bool) {
			if it.Handle == hb {
				return hubapi.RelayStatusResult{State: hubapi.RelayStatusDone, DAck: hubapi.B64(swapped)}, true
			}
			return hubapi.RelayStatusResult{}, false
		}
	})
	askHubNow(t, req)
	for _, id := range []string{a, b} {
		if st := ixOf(t, req, id).State; st != interactions.StateRejected {
			t.Fatalf("task %s (a=%v) is %s, want rejected; rows %+v; stats %v", id, id == a, st, outboxRows(t, req, id), req.ReceiveStats())
		}
		if rows := outboxRows(t, req, id); len(rows) != 0 {
			t.Fatalf("task %s: the refusal did not confirm: %+v", id, rows)
		}
	}
	if n := counter(req, cntDAckNoteInvalid); n != 1 {
		t.Fatalf("dack-note-invalid %d, want 1 (the moved note)", n)
	}
	if n := counter(req, cntDAckInvalid); n != 0 {
		t.Fatalf("dack-invalid %d: the token itself is valid", n)
	}
}

// DLV T20 (ENG-C4): the provider's first status message arrives before any
// status query; the delegation row is confirmed by it and the hub is not
// asked.
func TestDLVT20_AnImplicitConfirmationNeedsNoStatusQuery(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	asked := statusCalls(srv.URL)
	body, err := (&delegation.StatusMsg{State: delegation.StateWorking, At: uint64(time.Now().UnixMilli())}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if r := receive(t, req, craft(t, senderOf(prov), req, seal.TypeStatus, id, body, nil)); r.class != rxAccepted {
		t.Fatalf("the provider's status: %+v", r)
	}
	if rows := outboxRows(t, req, id); len(rows) != 0 {
		t.Fatalf("the delegation is not confirmed by the status: %+v", rows)
	}
	if counter(req, cntDAckConfirmedImplicit) != 1 {
		t.Fatalf("implicit confirmations %d, want 1", counter(req, cntDAckConfirmedImplicit))
	}
	if n := statusCalls(srv.URL); n != asked {
		t.Fatalf("%d status queries went out, want none", n-asked)
	}
}

// DLV T31 (ENG-K13 corollary): with the hub path blocked, two nodes reach
// each other over the direct path and the direct token confirms; with no
// path at all the message is given up at its deadline.
func TestDLVT31_OnlyTheDirectPathConfirmsAndNoPathGivesUp(t *testing.T) {
	t.Run("direct only", func(t *testing.T) {
		srv, req, prov := quietPair(t)
		direct := &switchP2P{to: prov}
		direct.on.Store(true)
		req.RegisterTransport(direct)
		setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
		sends := relayCountFor(srv.URL)
		id, err := req.Delegate(context.Background(), prov.AID(), "work", nil)
		if err != nil {
			t.Fatal(err)
		}
		if rows := outboxRows(t, req, id); len(rows) != 0 {
			t.Fatalf("not confirmed by the direct token: %+v", rows)
		}
		if relayCountFor(srv.URL) != sends || direct.sends.Load() != 1 {
			t.Fatalf("hub sends %d, direct sends %d", relayCountFor(srv.URL)-sends, direct.sends.Load())
		}
	})
	t.Run("no path", func(t *testing.T) {
		_, req, prov := quietPair(t)
		knowPeer(t, req, prov.AID())
		setHubURL(req, deadHubURL)
		holdGateOpen(req)
		id, err := req.DelegateCapability(context.Background(), prov.AID(), lampCap, map[string]any{"on": true})
		if err != nil {
			t.Fatal(err)
		}
		now := req.nowMS()
		req.setClock(func() uint64 { return now + messageLifetimeMS + 60_000 })
		_ = req.deliverQueued(context.Background(), outboxRows(t, req, id)[0].ID)
		st, _, reason := taskViewMap(t, req, id)
		if st != string(a2ashape.TaskStateFailed) || (reason != reasonUndeliverable && reason != reasonDeliveryUnconfirmed) {
			t.Fatalf("at the deadline: %s %s, want failed and given up", st, reason)
		}
	})
}

// mangleP2P hands the envelope to the recipient and passes its token back
// with one byte changed.
type mangleP2P struct {
	to *Daemon
	at int
}

func (p *mangleP2P) Name() string                           { return "p2p-mangle" }
func (p *mangleP2P) Reachable(context.Context, string) bool { return true }
func (p *mangleP2P) Send(ctx context.Context, _ string, env []byte) ([]byte, bool, error) {
	dack, err := p.to.Inbound().Receive(ctx, env)
	if len(dack) > p.at {
		dack = bytes.Clone(dack)
		dack[p.at] ^= 0x01
	}
	return dack, true, err
}

// DLV T35: a direct frame whose note was altered still confirms (the token
// is good), with an empty reason and dack-note-invalid counted; one whose
// token was altered is invalid and the message goes to the hub.
func TestDLVT35_AnAlteredNoteOrTokenOnTheDirectPath(t *testing.T) {
	t.Run("note", func(t *testing.T) {
		srv, req, prov := quietPair(t)
		req.RegisterTransport(&mangleP2P{to: prov, at: seal.TokenLen + 5})
		sends := relayCountFor(srv.URL)
		id, err := req.Delegate(context.Background(), prov.AID(), "work", nil)
		if err != nil {
			t.Fatal(err)
		}
		if rows := outboxRows(t, req, id); len(rows) != 0 {
			t.Fatalf("the token did not confirm: %+v", rows)
		}
		if counter(req, cntDAckNoteInvalid) != 1 || relayCountFor(srv.URL) != sends {
			t.Fatalf("note-invalid %d, hub sends %d; want 1 and 0", counter(req, cntDAckNoteInvalid), relayCountFor(srv.URL)-sends)
		}
	})
	t.Run("token", func(t *testing.T) {
		srv, req, prov := quietPair(t)
		req.RegisterTransport(&mangleP2P{to: prov, at: 0})
		id, err := req.Delegate(context.Background(), prov.AID(), "work", nil)
		if err != nil {
			t.Fatal(err)
		}
		if rows := outboxRows(t, req, id); len(rows) != 1 {
			t.Fatalf("a bad token confirmed the row: %+v", rows)
		}
		if counter(req, cntDAckInvalid) == 0 {
			t.Fatal("the bad token was not counted")
		}
		if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
			t.Fatalf("the hub holds %d envelopes, want the message sent on there", n)
		}
	})
}

// E-T28 (ENG-U1, ENG-U3, SI-79): every code the engine gives up a message
// with is a code of the reason table, the task's reason is that code, and
// the status text is the table's sentence for it: the projection every door
// returns (control plane, MCP, local A2A) is one function of the task.
func TestET28_EveryEngineCodeIsTheTablesSentence(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		code, reason string
	}{
		{"too large", 413, "too_large", "too_large"},
		{"bad envelope", 400, "bad_envelope", "bad_envelope"},
		{"recipient unknown", 404, "recipient_unknown", reasonRecipientUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, req, prov := registeredPair(t)
			setFake(t, srv.URL, func(h *fakeHub) {
				h.sendHook = func([]byte) fakeSendAnswer {
					return fakeSendAnswer{kind: fakeSendRefuse, status: c.status, code: c.code}
				}
			})
			_, err := req.DelegateCapability(context.Background(), prov.AID(), lampCap, map[string]any{"on": true})
			if err == nil {
				t.Fatal("the delegation was not given up")
			}
			list, _ := req.ix.List(interactions.RoleOutbound, "", 0, 10)
			if len(list) != 1 {
				t.Fatalf("%d tasks", len(list))
			}
			_, _, reason := taskViewMap(t, req, list[0].ID)
			if reason != c.reason {
				t.Fatalf("reason %q, want %q", reason, c.reason)
			}
			if _, ok := a2ashape.Lookup(reason); !ok {
				t.Fatalf("%q is not a code of the reason table", reason)
			}
			if want := a2ashape.Sentence(reason, a2ashape.English, nil); string(ixOf(t, req, list[0].ID).Result) != want {
				t.Fatalf("status text %q, want the table's %q", ixOf(t, req, list[0].ID).Result, want)
			}
		})
	}
	// The codes the engine writes beyond those: each has a row, with its
	// variants where the table has them.
	for _, key := range []string{reasonUndeliverable, reasonDeliveryUnconfirmed + ".sent", reasonDeliveryUnconfirmed + ".expired", "strangers_busy",
		"notice_stale", "rotation_pending", "credential_refused", "session_unavailable"} {
		if _, ok := a2ashape.Lookup(key); !ok {
			t.Errorf("the engine writes %q and the reason table has no row for it", key)
		}
	}
}

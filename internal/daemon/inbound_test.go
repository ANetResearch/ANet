package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

const lampCap = "light.onoff@sim/lamp-1"

// registered builds a daemon on srv that accepts no one and registers it.
func registered(t *testing.T, srvURL, name string) *Daemon {
	t.Helper()
	d := newTestDaemon(t, srvURL, false)
	if err := d.RegisterWithHub(context.Background(), srvURL, name, nil, ""); err != nil {
		t.Fatal(err)
	}
	d.stopRelayLoop()
	return d
}

// lastStatusMeta polls d until ix has a status message (notices are sent in
// the background) or two seconds pass, and returns the state of ix and the
// metadata of its last status message.
func lastStatusMeta(t *testing.T, d *Daemon, ix string) (interactions.State, map[string]any) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := d.pollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		got, err := d.ix.Get(ix)
		if err != nil {
			t.Fatal(err)
		}
		msgs, _ := d.ix.Messages(ix)
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Kind == interactions.MsgStatus {
				return got.State, decodeMeta([]byte(msgs[i].Metadata))
			}
		}
		if time.Now().After(deadline) {
			return got.State, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// awaitState polls d until ix is in state want, then returns the metadata
// of its last status message.
func awaitState(t *testing.T, d *Daemon, ix string, want interactions.State) map[string]any {
	t.Helper()
	waitUntil(t, "state "+string(want)+" on "+ix, func() bool {
		_ = d.pollOnce(context.Background())
		got, err := d.ix.Get(ix)
		return err == nil && got.State == want
	})
	_, meta := lastStatusMeta(t, d, ix)
	return meta
}

// setPolicy sets the inbound policy or fails the test.
func setPolicy(t *testing.T, d *Daemon, p string) {
	t.Helper()
	if err := d.SetInboundPolicy(p); err != nil {
		t.Fatal(err)
	}
}

// The decision order of A2A-DESIGN §5.2, row by row, as the requester sees
// it: a denied peer under closed gets exactly what a stranger gets (row 1 =
// row 6); a public capability is served to anyone (row 2); an allowed peer
// is accepted (row 3); approve holds (row 4); open takes natural-language
// tasks and refuses capability calls (row 5); a denied peer under open is
// told it is denied (row 1).
func TestTheInboundDecisionOrder(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	allowed := registered(t, srv.URL, "allowed")
	denied := registered(t, srv.URL, "denied")
	stranger := registered(t, srv.URL, "stranger")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: lampCap}}); err != nil {
		t.Fatal(err)
	}
	allowPeers(t, prov, allowed.AID(), denied.AID())
	denyPeers(t, prov, denied.AID())

	delegate := func(from *Daemon, goal string) string {
		t.Helper()
		id, err := from.Delegate(ctx, prov.AID(), goal, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		return id
	}

	// Row 6 and row 1 under closed: the same state and the same metadata.
	sid := delegate(stranger, "hello from a stranger")
	did := delegate(denied, "hello from a denied peer")
	sState, sMeta := lastStatusMeta(t, stranger, sid)
	dState, dMeta := lastStatusMeta(t, denied, did)
	if sState != interactions.StateRejected || sMeta["anet.reason"] != reasonNotAccepting {
		t.Fatalf("row 6: stranger sees %s %v", sState, sMeta)
	}
	if dState != sState || string(mustJSON(t, dMeta)) != string(mustJSON(t, sMeta)) {
		t.Fatalf("row 1 under closed: denied peer sees %s %v, a stranger %s %v", dState, dMeta, sState, sMeta)
	}
	for _, id := range []string{sid, did} {
		if _, err := prov.ix.Get(id); !errors.Is(err, interactions.ErrNotFound) {
			t.Fatalf("a refused delegation was stored (%s)", id)
		}
	}

	// Row 2: a stranger's call to a public capability is served; its intent
	// is not stored as conversation, the goal is the capability id.
	cid, err := stranger.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	pix, err := prov.ix.Get(cid)
	if err != nil {
		t.Fatalf("row 2: public capability call not stored: %v", err)
	}
	if pix.Trust != interactions.TrustPublicCap || pix.Goal != lampCap || len(lamp.invoked) != 1 {
		t.Fatalf("row 2: trust %q goal %q, invoked %d", pix.Trust, pix.Goal, len(lamp.invoked))
	}
	msgs, _ := prov.ix.Messages(cid)
	for _, m := range msgs {
		if m.SenderAID == stranger.AID() {
			t.Fatalf("row 2: the stranger's text was stored: %+v", m)
		}
	}
	if _, err := prov.ix.PeerIdentity(stranger.AID()); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("row 2: a public_cap requester was recorded in peer_identity")
	}
	if err := stranger.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, stranger, cid); st != interactions.StateCompleted {
		t.Fatalf("row 2: caller sees %s", st)
	}

	// Row 3.
	aid := delegate(allowed, "hello from an allowed peer")
	if pix, err := prov.ix.Get(aid); err != nil || pix.Trust != interactions.TrustPeer {
		t.Fatalf("row 3: %+v %v", pix, err)
	}

	// Row 4.
	setPolicy(t, prov, PolicyApprove)
	hid := delegate(stranger, "please approve me")
	if _, err := prov.ix.Get(hid); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("row 4: a held delegation is in the interaction table")
	}
	if held, err := prov.ix.GetPending(hid); err != nil || held.FromAID != stranger.AID() {
		t.Fatalf("row 4: not held: %v", err)
	}
	hState, hMeta := lastStatusMeta(t, stranger, hid)
	if hState != interactions.StateSubmitted || hMeta["anet.inbound"] != "pending_approval" {
		t.Fatalf("row 4: requester sees %s %v", hState, hMeta)
	}
	d4 := delegate(denied, "again")
	if st, meta := lastStatusMeta(t, denied, d4); st != interactions.StateRejected || meta["anet.reason"] != reasonDenied {
		t.Fatalf("row 1 under approve: %s %v", st, meta)
	}

	// Row 5.
	setPolicy(t, prov, PolicyOpen)
	oid := delegate(stranger, "open to all")
	if pix, err := prov.ix.Get(oid); err != nil || pix.Trust != interactions.TrustPublic {
		t.Fatalf("row 5: natural-language task under open: %+v %v", pix, err)
	}
	gid, err := stranger.DelegateCapability(ctx, prov.AID(), "ghost.capability", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.Get(gid); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("row 5: a non-public capability call under open was stored")
	}
	if st, meta := lastStatusMeta(t, stranger, gid); st != interactions.StateRejected || meta["anet.reason"] != reasonCapabilityNotPub {
		t.Fatalf("row 5: capability call under open: %s %v", st, meta)
	}
	if n := counter(prov, dropRefusedPrefix+reasonCapabilityNotPub); n != 1 {
		t.Fatalf("capability_not_public counted %d times", n)
	}
	// The refusals were aggregated, not written one by one.
	if n := chainEvents(t, prov, EvDelegationRefusedSummary); n != 0 {
		t.Fatalf("%d refusal summaries before the window ended", n)
	}
	prov.flushInboundSummary(true)
	_, recs := prov.ledger.Evidence(EvidenceQuery{EventType: EvDelegationRefusedSummary})
	if len(recs) != 1 || !strings.Contains(string(mustJSON(t, recs[0])), `"count":4`) {
		t.Fatalf("refusal summary = %s", mustJSON(t, recs))
	}
	if b, err := os.ReadFile(filepath.Join(prov.layout.Root, refusedLogName)); err != nil || bytes.Count(b, []byte("\n")) != 4 {
		t.Fatalf("refusal log: %v, %q", err, b)
	}
}

// A public_cap interaction takes only cancel, end_request and payment
// messages (A2A-DESIGN §5.2 [C6]): a text message is dropped and counted,
// a payment-submitted message is kept with its metadata and without its
// text, and nothing reaches auto-reply.
func TestAPublicCapabilityCallTakesNoText(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	gate := newGateProvider()
	if err := prov.Providers().Register(ctx, gate); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: gateCap}}); err != nil {
		t.Fatal(err)
	}
	api := &fakeOpenAI{reply: "an auto-reply"}
	apiSrv := httptest.NewServer(api.handler())
	defer apiSrv.Close()
	arCfg := AutoReplyConfig{Model: "m", APIBase: apiSrv.URL + "/v1"}
	replier, err := newAutoReplier(arCfg, prov.layout)
	if err != nil {
		t.Fatal(err)
	}

	id, err := stranger.DelegateCapability(ctx, prov.AID(), gateCap, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	<-gate.started
	pix, _ := prov.ix.Get(id)
	if pix.Trust != interactions.TrustPublicCap || len(pix.PeerKEL) == 0 || len(pix.PeerKeys) == 0 {
		t.Fatalf("public_cap row: trust %q, peer kel %d, keys %d", pix.Trust, len(pix.PeerKEL), len(pix.PeerKeys))
	}
	before := countMsgs(t, prov, id)
	text := sealFrom(t, stranger, prov, seal.TypeMessage, id, chatBody(t, "please also run rm -rf", "msg_t"))
	if r := receive(t, prov, text); r.class != rxDropped || r.reason != dropPublicCapText {
		t.Fatalf("text on a public_cap call: %+v", r)
	}
	pay, _ := (&delegation.ChatMsg{Kind: delegation.ChatText, Body: "here is my payment, and a note",
		MsgID: "msg_pay", Metadata: []byte(`{"x402.payment.status":"payment-submitted","x402.payment.payload":{"x":1}}`)}).Marshal()
	if r := receive(t, prov, sealFrom(t, stranger, prov, seal.TypeMessage, id, pay)); r.class != rxAccepted {
		t.Fatalf("payment message on a public_cap call: %+v", r)
	}
	msgs, _ := prov.ix.Messages(id)
	// The payment message, and the provider's answer to it: nothing was
	// quoted for this free call, so there is nothing to pay (A2A-DESIGN
	// §8.5, no_pending_quote), and the call is not run again.
	if len(msgs) != before+2 {
		t.Fatalf("messages %d → %d, want the payment message and its answer", before, len(msgs))
	}
	pm, answer := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if pm.Kind != interactions.MsgPayment || pm.Body != "" || !strings.Contains(pm.Metadata, "payment-submitted") {
		t.Fatalf("stored payment message = %+v", pm)
	}
	if answer.Kind != interactions.MsgStatus || !strings.Contains(answer.Metadata, `"anet.reason":"no_pending_quote"`) {
		t.Fatalf("answer to a payment nobody asked for = %+v", answer)
	}
	prov.autoReplyOnce(ctx, arCfg, replier)
	if n := api.calls.Load(); n != 0 {
		t.Fatalf("auto-reply called %d times for a public capability call", n)
	}
	if ths, _ := prov.ActiveThreads(); len(ths) != 0 {
		t.Fatalf("a public_cap interaction is in the auto-reply set: %+v", ths)
	}
	// A cancel reaches the running call.
	cancelMsg, _ := (&delegation.ChatMsg{Kind: delegation.KindCancel, MsgID: "msg_c"}).Marshal()
	if r := receive(t, prov, sealFrom(t, stranger, prov, seal.TypeMessage, id, cancelMsg)); r.class != rxAccepted {
		t.Fatalf("cancel: %+v", r)
	}
	waitUntil(t, "the running call to be canceled", gate.wasCanceled)
	waitUntil(t, "the public_cap interaction to end and drop the caller's keys", func() bool {
		pix, _ := prov.ix.Get(id)
		return pix.IsTerminal() && len(pix.PeerKEL) == 0 && len(pix.PeerKeys) == 0
	})
	if _, err := prov.ix.PeerIdentity(stranger.AID()); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("a public_cap caller was recorded in peer_identity")
	}
}

// The kernel's admission check (A2A-DESIGN §5.4): deny list, membership of
// public_capabilities, argument size, per-caller per-minute and per-day
// quotas, the global per-minute quota and the in-flight bound. A refused
// call consumes no quota; a released call frees its slot.
func TestAdmissionLimits(t *testing.T) {
	d := newTestDaemon(t, "", false)
	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC).UnixMilli()
	if err := d.SetPublicCapabilities([]PublicCapability{{ID: "c", PerCallerPerMin: 2, PerCallerPerDay: 3,
		GlobalPerMin: 4, MaxInflight: 1, MaxArgsBytes: 10}}); err != nil {
		t.Fatal(err)
	}
	must := func(caller string) func() {
		t.Helper()
		rel, refusal, _ := d.admitAt(now, caller, "c", 5)
		if refusal != "" {
			t.Fatalf("%s refused: %s", caller, refusal)
		}
		return rel
	}
	refused := func(caller, capID string, argsLen int, want string) {
		t.Helper()
		if _, refusal, _ := d.admitAt(now, caller, capID, argsLen); refusal != want {
			t.Fatalf("%s %s %d: refusal %q, want %q", caller, capID, argsLen, refusal, want)
		}
	}
	rel := must("did:anet:a")
	refused("did:anet:b", "c", 5, admitInflightLimit)
	rel()
	rel() // idempotent
	must("did:anet:b")()
	must("did:anet:a")()
	if _, refusal, retry := d.admitAt(now, "did:anet:a", "c", 5); refusal != admitCallerMinute || retry <= 0 {
		t.Fatalf("third call in a minute: %q retry %d", refusal, retry)
	}
	must("did:anet:c")()
	refused("did:anet:d", "c", 5, admitGlobalMinute) // four admitted in this minute
	now += 61_000
	must("did:anet:a")() // third of the day
	now += 61_000
	refused("did:anet:a", "c", 5, admitCallerDay)
	refused("did:anet:e", "c", 11, admitArgsTooLarge)
	refused("did:anet:e", "not.public", 1, admitNotPublic)
	denyPeers(t, d, "did:anet:e")
	refused("did:anet:e", "c", 1, admitDenied)
	// moduleHost.Admit is the same check.
	if _, refusal := (moduleHost{d}).Admit("did:anet:e", "c", 1); refusal != admitDenied {
		t.Fatalf("module seam refusal %q", refusal)
	}
}

// A caller over its quota for a public capability is refused with the
// reason and a retry hint, and the capability is not invoked.
func TestAPublicCapabilityQuotaIsToldToTheCaller(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	caller := registered(t, srv.URL, "caller")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: lampCap, PerCallerPerMin: 1}}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 2; i++ {
		id, err := caller.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(lamp.invoked) != 1 {
		t.Fatalf("invoked %d times, want 1", len(lamp.invoked))
	}
	st, meta := lastStatusMeta(t, caller, ids[1])
	if st != interactions.StateRejected || meta["anet.reason"] != admitCallerMinute || meta["anet.retry_after_ms"] == nil {
		t.Fatalf("over-quota caller sees %s %v", st, meta)
	}
}

// Putting a peer on the deny list cancels its active interactions in both
// roles and tells it; its later messages are dropped (A2A-DESIGN §5.1). A
// deny list edited without the CLI takes effect through the periodic
// sweep.
func TestDenyingAPeerCancelsItsInteractions(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := prov.DenyPeer(ctx, req.AID())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Canceled) != 1 || res.Canceled[0] != id || stateOf(t, prov, id) != interactions.StateCanceled {
		t.Fatalf("deny canceled %v; provider state %s", res.Canceled, stateOf(t, prov, id))
	}
	_, recs := prov.ledger.Evidence(EvidenceQuery{EventType: EvPolicyChanged})
	if len(recs) != 1 || !strings.Contains(string(mustJSON(t, recs[0])), id) {
		t.Fatalf("policy change evidence = %s", mustJSON(t, recs))
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, req, id); st != interactions.StateCanceled {
		t.Fatalf("requester state %s", st)
	}
	later := sealFrom(t, req, prov, seal.TypeMessage, id, chatBody(t, "still there?", "msg_after_deny"))
	if r := receive(t, prov, later); r.class != rxDropped || r.reason != dropDenied {
		t.Fatalf("message from a denied peer: %+v", r)
	}

	// Out-of-band edit, then the sweep.
	_, req2, prov2 := registeredPair(t)
	id2, err := req2.Delegate(ctx, prov2.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov2.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	denyPeers(t, prov2, req2.AID())
	prov2.revocationSweep()
	if st := stateOf(t, prov2, id2); st != interactions.StateCanceled {
		t.Fatalf("after the sweep: %s", st)
	}
}

// Removal from the allow list stops a trust=peer interaction taking new
// messages (A2A-DESIGN §5.1: revocation applies to existing interactions).
func TestRemovingAnAllowedPeerStopsItsMessages(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.RemovePeer(req.AID()); err != nil {
		t.Fatal(err)
	}
	msg := sealFrom(t, req, prov, seal.TypeMessage, id, chatBody(t, "more", "msg_more"))
	if r := receive(t, prov, msg); r.class != rxDropped || r.reason != dropNotAllowed {
		t.Fatalf("message after removal: %+v", r)
	}
}

// The approval queue (A2A-DESIGN §5.3): a held delegation is listed with
// metadata only, keeps at most five follow-ups, is removed by the
// requester's cancel, respects the per-peer limit, becomes an approved
// interaction on approval, is refused on rejection and expires after the
// TTL — the requester is told in each case.
func TestTheApprovalQueue(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	s := registered(t, srv.URL, "stranger")
	setPolicy(t, prov, PolicyApprove)
	// This test sends one requester more notices than the default per-peer
	// rate (6 an hour) allows.
	prov.notices.configure(100, 100)
	hold := func(goal string) string {
		t.Helper()
		id, err := s.Delegate(ctx, prov.AID(), goal, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := hold("a secret goal the operator must approve")
	list, err := prov.PendingList()
	if err != nil || len(list) != 1 || list[0].InteractionID != id || list[0].Requester != s.AID() || list[0].RequestCID == "" {
		t.Fatalf("pending list = %+v %v", list, err)
	}
	if strings.Contains(string(mustJSON(t, list)), "secret goal") {
		t.Fatal("the pending list shows the task's goal")
	}
	for i := 0; i < 7; i++ {
		if err := s.SendMessage(ctx, id, "follow-up", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	held, _ := prov.ix.GetPending(id)
	if len(held.Followups) != pendingFollowupsMax || counter(prov, "pending-followups-full") != 2 {
		t.Fatalf("held follow-ups %d, over the limit counted %d", len(held.Followups), counter(prov, "pending-followups-full"))
	}
	ix, err := prov.ApprovePending(id)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Trust != interactions.TrustApproved || ix.State != interactions.StateWorking || countMsgs(t, prov, id) != 1+pendingFollowupsMax {
		t.Fatalf("approved: trust %q state %s messages %d", ix.Trust, ix.State, countMsgs(t, prov, id))
	}
	if _, err := prov.ix.GetPending(id); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("approved item still held")
	}
	if _, err := prov.ix.PeerIdentity(s.AID()); err != nil {
		t.Fatalf("approval did not record the requester's identity: %v", err)
	}
	if _, err := prov.ApprovePending(id); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second approval: %v", err)
	}

	// The per-peer limit.
	prov.mu.Lock()
	prov.cfg.Inbound.Pending.MaxPerPeer = 1
	prov.mu.Unlock()
	r1 := hold("one")
	r2 := hold("two")
	if st, meta := lastStatusMeta(t, s, r2); st != interactions.StateRejected || meta["anet.reason"] != reasonPendingFull {
		t.Fatalf("over the per-peer limit: %s %v", st, meta)
	}
	// Rejection.
	if err := prov.RejectPending(r1); err != nil {
		t.Fatal(err)
	}
	if meta := awaitState(t, s, r1, interactions.StateRejected); meta["anet.reason"] != reasonOperatorRejected {
		t.Fatalf("rejected: %v", meta)
	}
	// The requester's cancel removes a held item.
	r3 := hold("three")
	if _, err := s.CancelTask(ctx, r3); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.GetPending(r3); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("a canceled held item is still held")
	}
	// Expiry.
	r4 := hold("four")
	prov.expirePendingAt(int64(prov.nowMS()) + 73*3600*1000)
	if _, err := prov.ix.GetPending(r4); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("an expired item is still held")
	}
	if meta := awaitState(t, s, r4, interactions.StateRejected); meta["anet.reason"] != reasonPendingExpired {
		t.Fatalf("expired: %v", meta)
	}
}

// Approval verifies the held delegation again (A2A-DESIGN §5.3): one whose
// TaskDoc is not signed by the requester it names is not approved and stays
// held.
func TestApprovalVerifiesTheDelegationAgain(t *testing.T) {
	d := newTestDaemon(t, "", false)
	claimed, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{{Intent: tsir.Intent{Body: "x"}}}}
	if err := td.Sign(signer); err != nil {
		t.Fatal(err)
	}
	doc, _ := coredet.Marshal(td)
	dr, _ := (&delegation.DelegateReq{TaskDoc: doc, Envelope: td.Envelope, InteractionID: "ix_forged"}).Marshal()
	kel, _ := identity.MarshalKEL(claimed.KEL())
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		return tx.PutPending(interactions.PendingItem{IX: "ix_forged", FromAID: claimed.AID(), ArrivedAt: 1,
			MsgTS: uint64(time.Now().UnixMilli()), Delegate: dr, KEL: kel}, 0, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApprovePending("ix_forged"); err == nil {
		t.Fatal("a delegation signed by someone else than its requester was approved")
	}
	if _, err := d.ix.GetPending("ix_forged"); err != nil {
		t.Fatal("a failed approval removed the held item")
	}
	if _, err := d.ix.Get("ix_forged"); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("a failed approval created an interaction")
	}
}

// The key-state rule applied at approval time: a retired key state is
// usable only within the rotation grace after its retirement.
func TestApprovalAppliesTheRotationGraceNow(t *testing.T) {
	d := newTestDaemon(t, "", false)
	c, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	retiredAt := uint64(time.Now().Add(-2 * time.Hour).UnixMilli())
	if err := c.Rotate(retiredAt); err != nil {
		t.Fatal(err)
	}
	if err := d.keyStateUsable(c.KEL(), c.CurrentSeq()); err != nil {
		t.Fatalf("the current key state was refused: %v", err)
	}
	if err := d.keyStateUsable(c.KEL(), 0); err == nil {
		t.Fatal("a key state retired two hours ago was accepted with the default one-hour grace")
	}
	d.mu.Lock()
	d.cfg.RotationGrace = "3h"
	d.mu.Unlock()
	if err := d.keyStateUsable(c.KEL(), 0); err != nil {
		t.Fatalf("within a three-hour grace: %v", err)
	}
}

// The configuration check of A2A-DESIGN §5.1: policy open together with exec
// for untrusted peers (or an untrusted backend) is refused whichever is
// written first, over the control plane with 409, and at start.
func TestOpenAndExecForUntrustedPeersConflict(t *testing.T) {
	sandboxExec := &AutoReplyConfig{Backend: "exec", Agent: "cursor", Untrusted: UntrustedSandbox}

	a := newTestDaemon(t, "", false)
	if err := a.SetAutoReply(sandboxExec); err != nil {
		t.Fatal(err)
	}
	if err := a.SetInboundPolicy(PolicyOpen); !errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("open after exec-for-untrusted: %v", err)
	}
	if a.config().inbound().Policy != PolicyClosed {
		t.Fatal("the refused policy was applied")
	}
	rec := httptest.NewRecorder()
	a.hInboundPolicy(rec, httptest.NewRequest(http.MethodPost, "/inbound/policy", strings.NewReader(`{"policy":"open"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("control plane: %d %s", rec.Code, rec.Body)
	}

	b := newTestDaemon(t, "", false)
	setPolicy(t, b, PolicyOpen)
	if err := b.SetAutoReply(sandboxExec); !errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("exec-for-untrusted after open: %v", err)
	}
	rec = httptest.NewRecorder()
	b.hAutoReply(rec, httptest.NewRequest(http.MethodPost, "/autoreply",
		strings.NewReader(`{"backend":"exec","agent":"cursor","untrusted":"sandbox"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("control plane: %d %s", rec.Code, rec.Body)
	}
	// Exec for trusted peers only is compatible with open.
	if err := b.SetAutoReply(&AutoReplyConfig{Backend: "exec", Agent: "cursor"}); err != nil {
		t.Fatalf("exec for trusted peers under open: %v", err)
	}

	c := newTestDaemon(t, "", false)
	(moduleHost{c}).DeclareUntrustedBackend()
	if err := c.SetInboundPolicy(PolicyOpen); !errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("open with an untrusted backend declared: %v", err)
	}

	// At start.
	root := t.TempDir()
	cfg := map[string]any{"control_addr": "127.0.0.1:0", "inbound": map[string]any{"policy": "open"},
		"auto_reply": map[string]any{"backend": "exec", "agent": "cursor", "untrusted": "sandbox"}}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(root, "config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := New(NewLayout(root)); !errors.Is(err, ErrPolicyConflict) {
		if d != nil {
			d.Close()
		}
		t.Fatalf("start with a conflicting config: %v", err)
	}
}

// SI-5: a fresh install is closed, with no allowed, trusted or public
// anything, and the exec backend runs nothing for untrusted peers.
func TestAFreshInstallIsClosed(t *testing.T) {
	l := NewLayout(t.TempDir())
	cfg, err := LoadConfig(l)
	if err != nil {
		t.Fatal(err)
	}
	in := cfg.inbound()
	if in.Policy != PolicyClosed || in.AllowFile != "peers.allow" || in.TrustFile != "peers.trust" ||
		in.DenyFile != "peers.deny" || in.PublicCapabilities == nil || len(in.PublicCapabilities) != 0 {
		t.Fatalf("fresh inbound block = %+v", in)
	}
	var disk map[string]any
	b, _ := os.ReadFile(l.ConfigPath())
	if err := json.Unmarshal(b, &disk); err != nil {
		t.Fatal(err)
	}
	inbound, _ := disk["inbound"].(map[string]any)
	if inbound["policy"] != "closed" || inbound["public_capabilities"] == nil {
		t.Fatalf("config.json does not state the closed default explicitly: %s", b)
	}
	if _, ok := disk["accept_delegations"]; ok {
		t.Fatal("config.json still carries accept_delegations")
	}
	d, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st := d.InboundStatus()
	if len(st.Allow) != 0 || len(st.Trust) != 0 || len(st.Deny) != 0 || st.UntrustedAutoReply != UntrustedOff || st.Policy != PolicyClosed {
		t.Fatalf("fresh inbound status = %+v", st)
	}
	if (AutoReplyConfig{Backend: "exec"}).UntrustedMode() != UntrustedOff {
		t.Fatal("auto_reply.untrusted does not default to off")
	}
}

// The wire-1 accept_delegations key maps to closed in every case and is not
// written back (A2A-DESIGN §5.1).
func TestAcceptDelegationsMigratesToClosed(t *testing.T) {
	for _, legacy := range []string{`true`, `false`, ``} {
		root := t.TempDir()
		raw := `{"control_addr":"127.0.0.1:0"}`
		if legacy != "" {
			raw = `{"control_addr":"127.0.0.1:0","accept_delegations":` + legacy + `}`
		}
		if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		d, err := New(NewLayout(root))
		if err != nil {
			t.Fatal(err)
		}
		if p := d.config().inbound().Policy; p != PolicyClosed {
			t.Errorf("accept_delegations=%q → policy %s", legacy, p)
		}
		d.Close()
		b, _ := os.ReadFile(filepath.Join(root, "config.json"))
		if strings.Contains(string(b), "accept_delegations") || !strings.Contains(string(b), `"policy": "closed"`) {
			t.Errorf("accept_delegations=%q: saved config %s", legacy, b)
		}
	}
}

// `accept on` has no safe equivalent and is refused with the three policies
// and the allow-list command; `accept off` sets closed; hub-register with
// accept_delegations=true is refused the same way (A2A-DESIGN §5.1 [C8]).
func TestAcceptOnIsRefused(t *testing.T) {
	d := newTestDaemon(t, "", false)
	setPolicy(t, d, PolicyApprove)
	rec := httptest.NewRecorder()
	d.hAccept(rec, httptest.NewRequest(http.MethodPost, "/accept", strings.NewReader(`{"enabled":true}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("accept on: %d", rec.Code)
	}
	for _, want := range []string{"closed", "approve", "open", "anet peers allow"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the refusal does not mention %q: %s", want, rec.Body)
		}
	}
	if d.config().inbound().Policy != PolicyApprove {
		t.Fatal("accept on changed the policy")
	}
	rec = httptest.NewRecorder()
	d.hAccept(rec, httptest.NewRequest(http.MethodPost, "/accept", strings.NewReader(`{"enabled":false}`)))
	if rec.Code != http.StatusOK || d.config().inbound().Policy != PolicyClosed {
		t.Fatalf("accept off: %d, policy %s", rec.Code, d.config().inbound().Policy)
	}
	rec = httptest.NewRecorder()
	d.hHubRegister(rec, httptest.NewRequest(http.MethodPost, "/hub-register",
		strings.NewReader(`{"hub":"http://hub.invalid","accept_delegations":true}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "anet peers allow") {
		t.Fatalf("hub-register --accept-delegations true: %d %s", rec.Code, rec.Body)
	}
}

// The peer list routes: allow, trust, deny and remove edit the files the
// decisions read, keep what else the file holds, and refuse a value that is
// not an AID.
func TestPeerListEdits(t *testing.T) {
	d := newTestDaemon(t, "", false)
	path := d.peerFile(d.config().inbound().AllowFile)
	if err := os.WriteFile(path, []byte("# my peers\ndid:anet:bkeep # a friend\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := d.AllowPeer(ctx, "did:anet:bnew", ListAllow); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AllowPeer(ctx, "not an aid", ListAllow); err == nil {
		t.Fatal("a non-AID was written to the allow list")
	}
	if _, err := d.AllowPeer(ctx, "did:anet:btrusted", ListTrust); err != nil {
		t.Fatal(err)
	}
	st := d.InboundStatus()
	if strings.Join(st.Allow, ",") != "did:anet:bkeep,did:anet:bnew" || strings.Join(st.Trust, ",") != "did:anet:btrusted" {
		t.Fatalf("lists = %+v", st)
	}
	ps := d.readPeers()
	if !ps.allowed("did:anet:btrusted") || ps.trusted("did:anet:bnew") {
		t.Fatal("trust must include allow, and allow must not include trust")
	}
	if _, err := d.RemovePeer("did:anet:bnew"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "# my peers\ndid:anet:bkeep # a friend\n" {
		t.Fatalf("allow file after remove = %q", b)
	}
	if _, err := d.DenyPeer(ctx, "did:anet:bkeep"); err != nil {
		t.Fatal(err)
	}
	if d.readPeers().allowed("did:anet:bkeep") {
		t.Fatal("deny does not win over allow")
	}
}

package daemon

// Red-team PoCs for SI-6 ("不知道" 与 "知道没问题" 不合并), lens si6.
//
// Each test passes when the defect is present: it asserts that the attack
// succeeded. A fix makes these tests fail; they are then flipped or removed.
//
// All attacks here come from the provider of an outbound task (a peer this
// node chose to call): every forged object is sealed and signed by the real
// provider, so steps 1-9 of the receive pipeline accept it. What is tested is
// whether what the peer controls can stand in for what this node knows, in
// the A2A projection that the control plane, MCP and module/a2a all return.
// (F1, which runs the real service module on the provider, is fixed; its
// regression test is service_outcome_test.go, behind !no_service.)

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/transcript"
)

// rtPair is a requester and a provider registered with one fake hub, and a
// free capability call from the first to the second, not yet answered.
func rtPair(t *testing.T) (req, prov *Daemon, id string) {
	t.Helper()
	srv := newFakeHub(t)
	ctx := context.Background()
	req = newTestDaemon(t, srv.URL, false)
	prov = newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil, ""); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "text.free", map[string]any{"q": 1})
	if err != nil {
		t.Fatal(err)
	}
	return req, prov, id
}

// rtResult is an anet.result/1 the provider signs and seals: a receipt that
// honestly covers deliverable, and meta as the result metadata.
func rtResult(t *testing.T, req, prov *Daemon, id string, deliverable []byte, meta map[string]any, requestCID string) []byte {
	t.Helper()
	cid, err := anetcid.Sum(deliverable)
	if err != nil {
		t.Fatal(err)
	}
	if requestCID == "" {
		requestCID = mustIX(t, req, id).RequestCID
	}
	rc := &evidence.Receipt{InteractionID: id, RequesterAID: req.AID(), ProviderAID: prov.AID(),
		RequestCID: requestCID, ResultCID: cid, CompletedAt: uint64(nowMillis())}
	if err := rc.Sign(prov.self); err != nil {
		t.Fatal(err)
	}
	receipt, err := rc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	kel, err := identity.MarshalKEL(prov.self.KEL())
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := json.Marshal(meta)
	payload := mustMarshal(t, &delegation.ResultResp{Status: delegation.StatusDone, Deliverable: deliverable,
		Receipt: receipt, KEL: kel, Metadata: mb})
	return sealFrom(t, prov, req, seal.TypeResult, id, payload)
}

// rtView is the projection every door returns (control plane /tasks/get,
// MCP get_task, module/a2a GetTask), as JSON-decoded maps.
func rtView(t *testing.T, d *Daemon, id string) map[string]any {
	t.Helper()
	task, err := d.taskView(mustIX(t, d, id), viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func rtPath(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[k]
	}
	return v
}

// noAuthorizationSigned reports that this node never signed a payment for
// the task: no anet.payment.authorized on its chain, pay_state empty.
func noAuthorizationSigned(t *testing.T, d *Daemon, id string) bool {
	t.Helper()
	signed := false
	d.ledger.scan(EvPaymentAuthorized, 0, func(_ int64, p map[string]any) {
		if p["interaction_id"] == id {
			signed = true
		}
	})
	return !signed && mustIX(t, d, id).PayState == interactions.PayNone
}

// F2a/F2b (red team F11, forged receipts and "paid" projected as
// payment-completed) are fixed; their PoCs are regression tests in
// payverdict_test.go.

// ---------------------------------------------------------------------------
// F3: a deliverable without a status and result metadata
// anet.effect_status=OK. The receipt verifies (it covers the deliverable),
// the deliverable says nothing about the effect, and the task's
// anet.effect_status is OK — taken from metadata no receipt covers, in place
// of the UNVERIFIED this node would derive for a terminal task whose result
// says nothing (a2ashape project.go effectStatus).
func TestRedteamSI6_UnreceiptedMetadataSuppliesEffectOK(t *testing.T) {
	req, prov, id := rtPair(t)
	deliverable := []byte(`{"capability":"text.free","message":"done"}`) // no status: the receipt covers no claim
	env := rtResult(t, req, prov, id, deliverable, map[string]any{"anet.effect_status": "OK"}, "")
	if r := receive(t, req, env); r.class != rxAccepted {
		t.Fatalf("result not accepted: %+v", r)
	}
	v := rtView(t, req, id)
	es := rtPath(v, "metadata", a2ashape.KeyEffectStatus)
	rv := rtPath(v, "metadata", a2ashape.KeyReceiptVerified)
	if rtPath(v, "status", "state") != string(a2ashape.TaskStateCompleted) || es != "OK" || rv != a2ashape.ReceiptVerified {
		t.Fatalf("defect not reproduced: state=%v effect_status=%v receipt_verified=%v", rtPath(v, "status", "state"), es, rv)
	}
	if strings.Contains(string(mustIX(t, req, id).Result), "OK") {
		t.Fatal("precondition: the receipted deliverable must not state OK")
	}
	t.Logf("ATTACK OK: COMPLETED + effect_status=%v + receipt_verified=%v, and the verified receipt covers no OK claim", es, rv)
}

// ---------------------------------------------------------------------------
// F4: a status message whose metadata carries the SI-6 keys. The task's
// metadata says what this node knows (effect UNVERIFIED, no receipt); the
// status.message a client reads with it — and every TaskStatusUpdateEvent of
// a stream — says effect OK and receipt verified, verbatim from the peer.
func TestRedteamSI6_PeerStatusMetadataContradictsTaskMetadata(t *testing.T) {
	req, prov, id := rtPair(t)
	mb, _ := json.Marshal(map[string]any{
		"anet.effect_status": "OK", "anet.receipt_verified": "verified",
		"anet.receipt": map[string]any{"verified": "verified", "receipt": "Zm9yZ2Vk"},
	})
	body := mustMarshal(t, &delegation.StatusMsg{State: delegation.StateFailed, Text: "all done, effect confirmed",
		Metadata: mb, At: uint64(time.Now().UnixMilli())})
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeStatus, id, body)); r.class != rxAccepted {
		t.Fatalf("status not accepted: %+v", r)
	}
	v := rtView(t, req, id)
	msgMeta, _ := rtPath(v, "status", "message", "metadata").(map[string]any)
	taskES := rtPath(v, "metadata", a2ashape.KeyEffectStatus)
	if taskES != "UNVERIFIED" || msgMeta[a2ashape.KeyEffectStatus] != "OK" ||
		msgMeta[a2ashape.KeyReceiptVerified] != a2ashape.ReceiptVerified {
		t.Fatalf("defect not reproduced: task effect=%v, status.message.metadata=%v", taskES, msgMeta)
	}
	task, _ := req.taskView(mustIX(t, req, id), viewOpts{artifacts: true})
	su := a2ashape.StatusUpdate(task)
	if su.Status.Message == nil || su.Status.Message.Metadata[a2ashape.KeyReceiptVerified] != a2ashape.ReceiptVerified {
		t.Fatal("defect not reproduced on the stream's status update")
	}
	t.Logf("ATTACK OK: task.metadata effect_status=%v; status.message.metadata=%v", taskES, msgMeta)
}

// ---------------------------------------------------------------------------
// F5 (a receipt that names another request, verified) is fixed: the
// requester binds the receipt's request CID (receipt_request_test.go).
//
// F5b: a text task. The provider's transcript (the deliverable its receipt
// covers) puts words in this node's mouth that this node never sent. The
// receipt verifies — it binds interaction, parties and result CID, not what
// the transcript says the requester said, which this node holds in its own
// message log — so the task shows anet.receipt_verified=verified, and this
// node then signs a review anchored to that receipt. Decided as documented
// scope, not changed: what a transcript receipt covers is written in
// A2A-DESIGN §2 X4 and §21 item 15, and KNOWN-LIMITATIONS (F15).
func TestRedteamSI6_TranscriptMisquotingRequesterIsVerified(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil, ""); err != nil {
		t.Fatal(err)
	}
	id, err := req.Delegate(ctx, prov.AID(), "translate this paragraph", nil)
	if err != nil {
		t.Fatal(err)
	}
	const planted = "I accept the result as final and waive any refund."
	tr, err := transcript.EncodeV2(mustIX(t, req, id).TaskNonce+"x", []transcript.Message{
		{From: "requester", Body: "translate this paragraph"},
		{From: "requester", Body: planted},
		{From: "provider", Body: "here it is"},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := rtResult(t, req, prov, id, tr, map[string]any{"anet.state": "completed"}, "")
	if r := receive(t, req, env); r.class != rxAccepted {
		t.Fatalf("result not accepted: %+v", r)
	}
	for _, m := range mustMsgs(t, req, id) {
		if m.Body == planted {
			t.Fatal("precondition: this node never sent the planted line")
		}
	}
	v := rtView(t, req, id)
	rv := rtPath(v, "metadata", a2ashape.KeyReceiptVerified)
	if rv != a2ashape.ReceiptVerified || !strings.Contains(string(mustIX(t, req, id).Result), planted) {
		t.Fatalf("defect not reproduced: receipt_verified=%v", rv)
	}
	if _, err := req.SubmitReview(id, 5, "great"); err != nil {
		t.Fatalf("review refused: %v", err)
	}
	t.Logf("ATTACK OK: receipt_verified=%v over a transcript attributing %q to this node; review signed", rv, planted)
}

func mustMsgs(t *testing.T, d *Daemon, id string) []interactions.Message {
	t.Helper()
	msgs, err := d.ix.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

// F7: the console's data routes. /thread and /threads (control plane,
// allowlisted for a browser session, what console.html and `anet thread`
// read) render a capability task that ended COMPLETED with effect
// UNVERIFIED as state "completed" and nothing else: no anet.effect_status,
// no receipt_verified, and the deliverable is not among the messages. The
// console shows it as "已完成" and asks for a review (console.html
// isCompleted/statusText).
func TestRedteamSI6_ThreadViewDropsEffectStatus(t *testing.T) {
	req, prov, id := rtPair(t)
	deliverable := []byte(`{"capability":"text.free","status":"UNVERIFIED","verifiable":false,"message":"sent; no readback"}`)
	env := rtResult(t, req, prov, id, deliverable, map[string]any{"anet.state": "completed", "anet.effect_status": "UNVERIFIED"}, "")
	if r := receive(t, req, env); r.class != rxAccepted {
		t.Fatalf("result not accepted: %+v", r)
	}
	if es := rtPath(rtView(t, req, id), "metadata", a2ashape.KeyEffectStatus); es != "UNVERIFIED" {
		t.Fatalf("precondition: the /tasks projection says %v", es)
	}
	th, err := req.Thread(id)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(th)
	if th.State != "completed" || strings.Contains(string(b), "UNVERIFIED") || strings.Contains(string(b), "effect") ||
		strings.Contains(string(b), "receipt_verified") {
		t.Fatalf("defect not reproduced: %s", b)
	}
	t.Logf("ATTACK OK: /thread for an UNVERIFIED effect: %s", b)
}

// ackLostP2P delivers every envelope to the provider and then reports
// failure, as module.Transport requires of "a Send that partially
// succeeded" (module/transport.go) — e.g. p2p's round trip timing out
// while the receiver was still running a short call before its ack.
type ackLostP2P struct {
	to    *Daemon
	calls atomic.Int32
}

func (p *ackLostP2P) Name() string                           { return "p2p-ack-lost" }
func (p *ackLostP2P) Reachable(context.Context, string) bool { return true }
func (p *ackLostP2P) Send(ctx context.Context, _ string, env []byte) error {
	p.calls.Add(1)
	_ = p.to.receiveEnvelope(ctx, env)
	return errors.New("p2p: no ack within the dial timeout")
}

// F8: the requester's retry queue abandons a delegation on a permanent hub
// refusal (here 413, an envelope over the hub's cap, which only p2p could
// carry) and fails the task with effect UNAVAILABLE — "never delivered, did
// not run" (undelivered.go) — although the attempt that just failed on the
// p2p path had delivered it and the provider ran the capability.
func TestRedteamSI6_AbandonedButDeliveredDelegationIsNotUnavailable(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil, ""); err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.maxEnvelope = 1 }) // the hub refuses the envelope for good (413)
	p2p := &ackLostP2P{to: prov}
	req.RegisterTransport(p2p)

	_, derr := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1", map[string]any{"on": true})
	list, err := req.ix.List(interactions.RoleOutbound, "", 0, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("outbound tasks: %v %d", err, len(list))
	}
	id := list[0].ID
	if len(lamp.invoked) != 1 || p2p.calls.Load() != 1 {
		t.Fatalf("precondition: the provider ran the call %d times (p2p sends %d)", len(lamp.invoked), p2p.calls.Load())
	}
	v := rtView(t, req, id)
	state, es := rtPath(v, "status", "state"), rtPath(v, "metadata", a2ashape.KeyEffectStatus)
	// Fixed by wp/fx-b [redteam:F12]: the p2p attempt may have delivered it,
	// so abandoning the row does not say "never delivered": the effect is
	// unknown (UNVERIFIED), not UNAVAILABLE.
	if state != string(a2ashape.TaskStateFailed) || es != "UNVERIFIED" {
		t.Fatalf("abandoned after a p2p attempt that may have delivered it: state=%v effect_status=%v, want failed/UNVERIFIED (delegate err %v)",
			state, es, derr)
	}
}

package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/ANetResearch/ANetCore/payment"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/provider"
)

// lampProvider is a deterministic C1 provider for tests.
type lampProvider struct{ invoked []provider.Call }

func (l *lampProvider) ID() string { return "lamp" }
func (l *lampProvider) Capabilities(context.Context) ([]string, error) {
	return []string{"light.onoff@sim/lamp-1"}, nil
}
func (l *lampProvider) Describe(context.Context) (string, error) { return "", nil }
func (l *lampProvider) Health(context.Context) error             { return nil }
func (l *lampProvider) Invoke(_ context.Context, c provider.Call) (effect.Effect, error) {
	l.invoked = append(l.invoked, c)
	v := 0.0
	if on, _ := c.Args["on"].(bool); on {
		v = 1.0
	}
	return effect.Effect{Status: effect.OK, Record: &tsir.EffectRecord{Metrics: map[string]float64{"power_state": v}}}, nil
}

func TestCapabilityCallDetection(t *testing.T) {
	td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{{
		Intent:   tsir.Intent{Body: "invoke capability light.onoff@sim/lamp-1"},
		Requires: []tsir.Require{{ID: "light.onoff@sim/lamp-1", Type: RequireTypeCapability, Necessity: "must"}},
		Contexts: []tsir.Context{{Key: "args", Value: `{"on":true}`, Format: "json"}},
	}}}
	capID, args, ok := capabilityCall(td)
	if !ok || capID != "light.onoff@sim/lamp-1" || args["on"] != true {
		t.Fatalf("detection failed: %v %v %v", capID, args, ok)
	}

	plain := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{{Intent: tsir.Intent{Body: "write a haiku"}}}}
	if _, _, ok := capabilityCall(plain); ok {
		t.Fatal("plain goal must not be a capability call")
	}

	badArgs := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{{
		Requires: []tsir.Require{{ID: "x", Type: RequireTypeCapability}},
		Contexts: []tsir.Context{{Key: "args", Value: "{not json", Format: "json"}},
	}}}
	if _, _, ok := capabilityCall(badArgs); ok {
		t.Fatal("malformed args must fall through, not fail the task")
	}
}

// TestCapabilityDelegationRoundTrip: requester delegates a capability call;
// the provider daemon executes it via its registry (no LLM), answers with
// the effect as deliverable + signed receipt; requester sees a done result.
func TestCapabilityDelegationRoundTrip(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}

	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "LinkBox", []string{"devices"}, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}

	id, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1", map[string]any{"on": true})
	if err != nil {
		t.Fatalf("delegate capability: %v", err)
	}

	// provider pulls its mailbox — execution happens inside the ingest.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatalf("provider poll: %v", err)
	}
	if len(lamp.invoked) != 1 || lamp.invoked[0].Capability != "light.onoff@sim/lamp-1" {
		t.Fatalf("provider not invoked exactly once: %+v", lamp.invoked)
	}
	if lamp.invoked[0].CallerAID != req.AID() {
		t.Fatalf("caller AID must be the requester, got %q", lamp.invoked[0].CallerAID)
	}

	// requester pulls the result.
	results, err := req.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) != 1 || results[0].InteractionID != id {
		t.Fatalf("results = %+v, want done capability call %s", results, id)
	}
	r := results[0].Result
	if !strings.Contains(r, `"status":"OK"`) || !strings.Contains(r, `"power_state":1`) || !strings.Contains(r, `"verifiable":true`) {
		t.Fatalf("deliverable must carry the honest effect, got %q", r)
	}

	// C5: the effect must be on the provider daemon's evidence chain.
	prov.ledger.mu.Lock()
	seq := prov.ledger.nextSeq
	prov.ledger.mu.Unlock()
	if seq == 0 {
		t.Fatal("capability effect never reached the evidence ledger")
	}
}

// TestCapabilityUnresolvableFallsThrough: nobody provides the capability →
// the delegation stays a pending inbound task for auto-reply/human handling.
func TestCapabilityUnresolvableFallsThrough(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Plain Bot", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}

	id, err := req.DelegateCapability(ctx, prov.AID(), "ghost.capability", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	inbox, err := prov.Inbox(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].InteractionID != id {
		t.Fatalf("unresolvable capability must stay pending inbound: %+v", inbox)
	}
}

// TestBothSidesRecordEvidence pins the C5 property that real deployment
// exposed: after a completed interaction, the REQUESTER's chain must carry
// what it delegated and what it accepted — a provider's ledger alone cannot
// prove what the other side acknowledged.
func TestBothSidesRecordEvidence(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Requester", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Provider", []string{"haiku"}, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}

	id, err := req.Delegate(ctx, prov.AID(), "write a haiku", nil)
	if err != nil {
		t.Fatal(err)
	}
	// delegating must already leave a trace on the requester's chain
	req.ledger.mu.Lock()
	afterDelegate := req.ledger.nextSeq
	req.ledger.mu.Unlock()
	if afterDelegate == 0 {
		t.Fatal("delegating must be recorded on the requester's evidence chain")
	}

	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, id, "signed bytes drift", nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := req.RequestEnd(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.AcceptEnd(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := req.Results(ctx); err != nil {
		t.Fatal(err)
	}

	req.ledger.mu.Lock()
	reqSeq := req.ledger.nextSeq
	req.ledger.mu.Unlock()
	prov.ledger.mu.Lock()
	provSeq := prov.ledger.nextSeq
	prov.ledger.mu.Unlock()

	if provSeq == 0 {
		t.Fatal("the provider must record the receipt it issued")
	}
	if reqSeq <= afterDelegate {
		t.Fatalf("the requester must record the accepted result too (seq %d → %d)", afterDelegate, reqSeq)
	}
}

// quirkyProvider reports a reading it had to correct, the way a real
// vendor adapter does: the device put 2212.93 on the wire and the
// correction turned it into 22.13 °C.
type quirkyProvider struct{}

func (quirkyProvider) ID() string { return "quirky" }
func (quirkyProvider) Capabilities(context.Context) ([]string, error) {
	return []string{"sensor.temperature@aqara/th-1"}, nil
}
func (quirkyProvider) Describe(context.Context) (string, error) { return "", nil }
func (quirkyProvider) Health(context.Context) error             { return nil }
func (quirkyProvider) Invoke(context.Context, provider.Call) (effect.Effect, error) {
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"temperature": 22.13}},
		Evidence: &effect.Evidence{
			Protocol: "zigbee", ObservedState: "2212.93", Quirk: "aqara.temp.scale100",
			VerifyTrust: 2, AuthTrust: 1, NativeAck: true,
		},
	}, nil
}

// A correction reaches every surface or none — including the chain.
//
// The ledger used to record status, verifiability, metrics and the result
// CID: everything about what happened and nothing about how we know it. A
// corrected reading is not what the device put on the wire, and a chain
// that cannot tell the two apart cannot audit the correction, which is the
// one thing evidence exists for. This is the process boundary where that
// was quietly being lost.
func TestEvidenceProvenanceReachesTheChain(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := prov.Providers().Register(ctx, quirkyProvider{}); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "LinkBox", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if _, err := req.DelegateCapability(ctx, prov.AID(),
		"sensor.temperature@aqara/th-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	rec := lastLedgerPayload(t, prov, EvCapabilityEffect)
	ev, ok := rec["evidence"].(map[string]any)
	if !ok {
		t.Fatalf("the effect's provenance is missing from the chain: %+v", rec)
	}
	if ev["quirk"] != "aqara.temp.scale100" {
		t.Errorf("the correction must be named on the chain, got %v", ev["quirk"])
	}
	if ev["observed_state"] != "2212.93" {
		t.Errorf("what the device actually reported must survive, got %v", ev["observed_state"])
	}
	if ev["protocol"] != "zigbee" {
		t.Errorf("protocol = %v, want zigbee", ev["protocol"])
	}
	// Trust is evidence too: a readback (V2) and an independent confirmation
	// (V3) are not interchangeable, and the chain has to keep them apart.
	if fmt.Sprint(ev["verify_trust"]) != "2" || fmt.Sprint(ev["auth_trust"]) != "1" {
		t.Errorf("trust levels lost: verify=%v auth=%v", ev["verify_trust"], ev["auth_trust"])
	}
}

// A read has to be able to return what it read.
//
// It could not. A capability reports its reading in Evidence.ObservedState
// — the CID a store wrote, the units on a board, the org a node serves —
// and the deliverable carried status, verifiability and metrics only.
// Metrics are map[string]float64, so there was no channel for a CID, a
// blob, or a list: every read in the system was write-only across the
// wire, and cas.get could not be called at all because cas.put had no way
// to tell the caller the CID it wrote.
//
// Both suites passed throughout. The requester test asserted on a lamp,
// whose answer fits in a metric; the module tests call Invoke in-process,
// where the Effect is right there. It took two daemons and a real store.
func TestAReadReturnsWhatItRead(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := prov.Providers().Register(ctx, quirkyProvider{}); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "LinkBox", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if _, err := req.DelegateCapability(ctx, prov.AID(),
		"sensor.temperature@aqara/th-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	results, err := req.Results(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want one", results)
	}
	var got capabilityResult
	if err := json.Unmarshal([]byte(results[0].Result), &got); err != nil {
		t.Fatalf("deliverable is not a capability result: %v", err)
	}
	if got.Evidence == nil {
		t.Fatal("the requester received no provenance at all")
	}
	if got.Evidence["observed_state"] != "2212.93" {
		t.Errorf("the reading did not reach the requester: %v", got.Evidence["observed_state"])
	}
	// The correction has to reach the peer above all: it is the one party
	// that cannot check the device for itself.
	if got.Evidence["quirk"] != "aqara.temp.scale100" {
		t.Errorf("the correction was not disclosed to the requester: %v", got.Evidence["quirk"])
	}
	if fmt.Sprint(got.Evidence["verify_trust"]) != "2" || fmt.Sprint(got.Evidence["auth_trust"]) != "1" {
		t.Errorf("trust levels lost on the wire: %+v", got.Evidence)
	}

	// And the two accounts of one effect must agree. Evidence that differs
	// depending on who reads it is worse than none: it lets a provider tell
	// its own chain one thing and its peer another, which is precisely what
	// a signed receipt over the deliverable is supposed to prevent.
	chain := lastLedgerPayload(t, prov, EvCapabilityEffect)
	onChain, _ := chain["evidence"].(map[string]any)
	if onChain == nil {
		t.Fatal("no provenance on the provider chain")
	}
	for k, v := range got.Evidence {
		if fmt.Sprint(onChain[k]) != fmt.Sprint(v) {
			t.Errorf("evidence disagrees on %q: chain=%v requester=%v", k, onChain[k], v)
		}
	}
	if len(onChain) != len(got.Evidence) {
		t.Errorf("chain evidence has %d fields, the requester got %d", len(onChain), len(got.Evidence))
	}
}

// lastLedgerPayload returns the payload of the most recent event of a kind.
//
// It reads through the ledger's own decoder rather than parsing the file:
// a test that re-implements the storage format is a test that keeps
// passing after the format changes underneath it, which is how the chain
// came to be stored in an encoding that could not represent it.
func lastLedgerPayload(t *testing.T, d *Daemon, kind string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(d.layout.EvidenceLedgerPath())
	if err != nil {
		t.Fatalf("read evidence chain: %v", err)
	}
	var found map[string]any
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rec, err := decodeRecord(line)
		if err != nil {
			t.Fatalf("decode evidence line: %v", err)
		}
		if rec.EventType == kind {
			found = plainMap(rec.Payload)
		}
	}
	if found == nil {
		t.Fatalf("no %s event on the chain", kind)
	}
	return found
}

// A capability call must be reachable from outside the daemon.
//
// DelegateCapability existed and worked, and nothing in the product could
// call it: the control API accepted only provider+goal, so `anet delegate
// --capability …` put the capability id into the goal text, where no
// resolver looks. Both repositories' suites passed throughout — each fakes
// the other, and the capability round-trip test calls the method
// in-process. What surfaced it was a joint run against a real hub, a real
// ANetLink and real mock hardware: the delegation arrived, the provider
// could have served it, and the camera did not move.
func TestCapabilityCallIsReachableThroughTheControlAPI(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "LinkBox", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}

	// Exactly what the CLI posts for `delegate <aid> --capability <id> --args …`.
	body, _ := json.Marshal(map[string]any{
		"provider":   prov.AID(),
		"capability": "light.onoff@sim/lamp-1",
		"args":       map[string]any{"on": true},
	})
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest("POST", "/delegate", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	req.hDelegate(rec, httpReq)

	if rec.Code != 200 {
		t.Fatalf("delegate with a capability = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		InteractionID string `json:"interaction_id"`
		Capability    string `json:"capability"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Capability != "light.onoff@sim/lamp-1" {
		t.Fatalf("the response must confirm it was a capability call: %s", rec.Body.String())
	}

	// The provider must execute it rather than queue it for an agent.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(lamp.invoked) != 1 {
		t.Fatalf("the provider should have invoked the capability, got %d calls", len(lamp.invoked))
	}
	if on, _ := lamp.invoked[0].Args["on"].(bool); !on {
		t.Fatalf("the args must survive the round trip: %+v", lamp.invoked[0].Args)
	}
}

// A delegation with neither goal nor capability is refused, and one with a
// goal alone still works — the capability path is an addition, not a
// replacement.
func TestDelegateStillRequiresGoalOrCapability(t *testing.T) {
	srv := newFakeHub(t)
	d := newTestDaemon(t, srv.URL, false)

	body, _ := json.Marshal(map[string]any{"provider": "some-aid"})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/delegate", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	d.hDelegate(rec, r)

	if rec.Code != 400 {
		t.Fatalf("neither goal nor capability must be refused, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "capability") {
		t.Errorf("the error should mention both ways to ask: %s", rec.Body.String())
	}
}

// The requester must actually check the receipt it accepts.
//
// It did not, and could not: Receipt.Verify had zero call sites in this
// repository, and a completion carried no provider KEL, so there was no
// key to check against. The requester stored whatever bytes arrived and
// wrote "result accepted" on its own evidence chain.
//
// This asserts the check ran and passed, from the chain — not from the
// absence of an error, which is what the old path also produced.
func TestAnAcceptedResultWasActuallyVerified(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := prov.Providers().Register(ctx, &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "LinkBox", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if _, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1",
		map[string]any{"on": true}); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := req.Results(ctx); err != nil {
		t.Fatal(err)
	}

	rec := lastLedgerPayload(t, req, EvResultAccepted)
	if rec["receipt_verified"] != true {
		t.Fatalf("the chain must record that the receipt was checked, got %v\n"+
			"A chain saying only \"accepted\" cannot tell an audited completion "+
			"from one nobody could audit.", rec["receipt_verified"])
	}

	// And the provider's key is now known, which is what makes it possible.
	if _, ok := req.peers.resolve(prov.AID()); !ok {
		t.Error("verifying a completion must leave the provider's KEL known")
	}
}

// A completion that does not answer the request is refused, not stored.
func TestAResultForDifferentContentIsRefused(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "LinkBox", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1", nil)
	if err != nil {
		t.Fatal(err)
	}

	// A provider signing a real receipt, then delivering other bytes.
	rc := &evidence.Receipt{
		InteractionID: id, RequesterAID: req.AID(), ProviderAID: prov.AID(),
		RequestCID: "bafyrequest", ResultCID: "bafynotwhatwesend", CompletedAt: uint64(nowMillis()),
	}
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
	payload, err := (&delegation.ResultResp{
		Status: delegation.StatusDone, Deliverable: []byte(`{"status":"OK"}`),
		Receipt: receipt, KEL: kel,
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}

	req.ingestResult(id, payload)

	results, err := req.Results(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.InteractionID == id {
			t.Fatal("a result whose receipt covers different content must not be stored as done")
		}
	}
}

// A delegation delivered twice must be executed once.
//
// The relay acks a message only after it has been handled, which is the
// right order — acking first would lose work on a crash. The cost is that
// a crash between doing the work and acking leaves the message in the
// mailbox, and the next poll delivers it again. That window is not small:
// it spans signing a receipt, writing the evidence chain and relaying the
// result.
//
// Re-executing is not a wasted cycle. It is a second physical effect for
// a capability that has one, a second signed receipt for work that
// happened once, a second entry on the evidence chain, and a second
// result for the requester to reconcile — from a node whose whole claim
// is that its chain says what happened.
func TestARedeliveredDelegationIsNotExecutedTwice(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "LinkBox", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1",
		map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}

	// The delegation payload, exactly as the hub holds it.
	payload := onlyRelayPayload(t, srv, prov.AID(), "delegate")

	// Delivered, handled, and then — because the ack never landed —
	// delivered again.
	if !prov.ingestDelegate(payload) {
		t.Fatal("first delivery was refused")
	}
	if len(lamp.invoked) != 1 {
		t.Fatalf("first delivery invoked the capability %d times", len(lamp.invoked))
	}
	if !prov.ingestDelegate(payload) {
		t.Fatal("a redelivery must be acked, not retried forever")
	}

	if n := len(lamp.invoked); n != 1 {
		t.Errorf("the lamp was switched %d times for one delegation", n)
	}
	// One receipt, not two. A second would be a signed claim about work
	// that happened once.
	seq := chainLength(t, prov)
	if seq != 1 {
		t.Errorf("the provider chain has %d entries for one delegation", seq)
	}
	// And the transcript must not gain a duplicate of the opening message.
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
		t.Errorf("the opening message appears %d times", opening)
	}
}

// chainLength reports how many events this node has recorded.
func chainLength(t *testing.T, d *Daemon) uint64 {
	t.Helper()
	d.ledger.mu.Lock()
	defer d.ledger.mu.Unlock()
	return d.ledger.nextSeq
}

// The same window exists on the requester's side, and it lands on the
// evidence chain.
//
// A result is acked after it is stored, so a crash in between means the
// next poll delivers it again. Storing it twice is harmless — the content
// is identical — but appending "result accepted" twice is not: the chain
// would say a node accepted two results for one interaction, and a chain
// that miscounts what happened is worse than no chain, because it is
// believed.
func TestARedeliveredResultIsRecordedOnce(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := prov.Providers().Register(ctx, &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "LinkBox", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1",
		map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	payload := onlyRelayPayload(t, srv, req.AID(), "result")

	before := chainLength(t, req)
	if !req.ingestResult(id, payload) {
		t.Fatal("first delivery refused")
	}
	afterFirst := chainLength(t, req)
	if afterFirst != before+1 {
		t.Fatalf("one result should add one chain entry, got %d", afterFirst-before)
	}
	if !req.ingestResult(id, payload) {
		t.Fatal("a redelivery must be acked, not retried forever")
	}
	if got := chainLength(t, req); got != afterFirst {
		t.Errorf("a redelivered result added %d more chain entries", got-afterFirst)
	}
}

// What a node advertises and what it actually serves must be the same list.
//
// They were two lists kept in step by hand, and nothing checked. A node
// could register "digest" while its provider answered "text.digest" —
// harmless while discovery searched prose, because "digest" is a substring
// of the goal text and the caller found it anyway. The moment discovery
// became exact, that node stopped being findable for the thing it does,
// and the directory was confidently wrong rather than merely vague.
func TestRegistrationAdvertisesWhatIsActuallyServed(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	d := newTestDaemon(t, srv.URL, true)
	if err := d.Providers().Register(ctx, &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	// The operator writes a human label; the daemon knows the ids.
	if err := d.HubRegister(ctx, srv.URL, "LinkBox", []string{"devices"}, nil, nil); err != nil {
		t.Fatal(err)
	}

	agents, err := d.Find(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var caps []string
	for _, a := range agents {
		if a.AID == d.AID() {
			caps = a.Caps
		}
	}
	has := func(want string) bool {
		for _, c := range caps {
			if c == want {
				return true
			}
		}
		return false
	}
	if !has("light.onoff@sim/lamp-1") {
		t.Errorf("the id this node will actually answer is not advertised: %v", caps)
	}
	// The operator's own word is kept, not replaced: "devices" is not a
	// capability id and is still how a person says what they offer.
	if !has("devices") {
		t.Errorf("the operator's label was dropped: %v", caps)
	}
}

// A redelivered chat message must not become a second line.
//
// The transcript is what the receipt is taken over, so a duplicate is not
// cosmetic. Deduping by body would be worse than not deduping: people
// repeat themselves, and an agent that says "ok" twice means it twice.
// Only an id minted by the sender — the one party present on every
// delivery path — can distinguish the two.
func TestARedeliveredChatMessageIsStoredOnce(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "A", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "B", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	id, err := req.Delegate(ctx, prov.AID(), "do a thing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	base := countMessages(t, prov, id)

	payload, err := (&delegation.ChatMsg{
		Kind: delegation.ChatText, Body: "on my way", MsgID: "msg_deadbeef",
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	prov.ingestMessage(id, req.AID(), payload)
	if got := countMessages(t, prov, id); got != base+1 {
		t.Fatalf("first delivery stored %d messages", got-base)
	}
	prov.ingestMessage(id, req.AID(), payload)
	if got := countMessages(t, prov, id); got != base+1 {
		t.Errorf("a redelivery added a second line to the transcript (now %d)", got-base)
	}

	// Saying the same thing again is a different event, and must land.
	again, err := (&delegation.ChatMsg{
		Kind: delegation.ChatText, Body: "on my way", MsgID: "msg_cafebabe",
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	prov.ingestMessage(id, req.AID(), again)
	if got := countMessages(t, prov, id); got != base+2 {
		t.Errorf("repeating yourself must be recorded, got %d", got-base)
	}

	// An older sender mints no id. Its messages must still land, every
	// one of them — an empty id is not an identity they all share.
	for i := 0; i < 2; i++ {
		old, err := (&delegation.ChatMsg{Kind: delegation.ChatText, Body: "no id here"}).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		prov.ingestMessage(id, req.AID(), old)
	}
	if got := countMessages(t, prov, id); got != base+4 {
		t.Errorf("messages without ids must not dedupe against each other, got %d", got-base)
	}
}

func countMessages(t *testing.T, d *Daemon, ixID string) int {
	t.Helper()
	msgs, err := d.ix.Messages(ixID)
	if err != nil {
		t.Fatal(err)
	}
	return len(msgs)
}

// pricedProvider costs money and says so.
type pricedProvider struct {
	price   uint64
	invoked int
}

func (p *pricedProvider) ID() string { return "priced" }
func (p *pricedProvider) Capabilities(context.Context) ([]string, error) {
	return []string{"work.do"}, nil
}
func (p *pricedProvider) Describe(context.Context) (string, error) { return "", nil }
func (p *pricedProvider) Health(context.Context) error             { return nil }
func (p *pricedProvider) Price(cap string) (uint64, bool) {
	if cap == "work.do" {
		return p.price, true
	}
	return 0, false
}
func (p *pricedProvider) Invoke(context.Context, provider.Call) (effect.Effect, error) {
	p.invoked++
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"done": 1}},
		Evidence: &effect.Evidence{Protocol: "test", Requested: "work.do",
			NativeAck: true, VerifyTrust: 1},
	}, nil
}

// Priced work is quoted, not refused — and the quote is a real answer.
//
// A provider that wants paying has not failed, so PAYMENT_REQUIRED is its
// own status; and the quote is signed, receipted and recorded like any
// other result, because a price nobody can point back at is not a price.
func TestPricedWorkIsQuotedNotRefused(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	work := &pricedProvider{price: 120}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Payer", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Worker", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}

	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if work.invoked != 0 {
		t.Fatalf("unpaid work was done %d times", work.invoked)
	}
	if _, err := req.Results(ctx); err != nil {
		t.Fatal(err)
	}
	quote := lastResultFor(t, req, id)
	if quote.Status != string(effect.PaymentRequired) {
		t.Fatalf("status = %s, want PAYMENT_REQUIRED", quote.Status)
	}
	if quote.Payment == nil || len(quote.Payment.Accepts) == 0 {
		t.Fatal("the quote carried no price")
	}
	opt := quote.Payment.Accepts[0]
	if opt.Scheme != payment.SchemeCredit || opt.Amount != "120" || opt.PayTo != prov.AID() {
		t.Errorf("quote = %+v", opt)
	}
	// The quote is on the provider's own chain, like any answer.
	if got := lastLedgerPayload(t, prov, EvCapabilityEffect)["status"]; got != string(effect.PaymentRequired) {
		t.Errorf("the quote is not on the chain as such: %v", got)
	}
}

// An authorization names the work it pays for, so it cannot be spent on
// anything else the provider is owed for.
func TestAnAuthorizationIsBoundToItsInteraction(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	d := newTestDaemon(t, srv.URL, false)
	if err := d.RegisterWithHub(ctx, srv.URL, "Payer", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	opt := payment.PaymentOption{
		Scheme: payment.SchemeCredit, Network: payment.CreditNetwork("did:anet:hub"),
		Amount: "50", Asset: payment.AssetCredit, PayTo: "did:anet:provider",
	}
	pp, err := d.Authorize(opt, "ix-42")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(pp.Payload["authorization"].(string))
	auth, err := payment.UnmarshalAuthorization(raw)
	if err != nil {
		t.Fatal(err)
	}
	if auth.InteractionID != "ix-42" {
		t.Errorf("authorization is not bound to the work: %q", auth.InteractionID)
	}
	if auth.Payer != d.AID() {
		t.Errorf("payer = %s, want this node", auth.Payer)
	}
	if err := auth.Verify(d.self.KEL(), time.Now().UnixMilli()); err != nil {
		t.Errorf("our own authorization does not verify: %v", err)
	}
	// Signing it put it on our chain: an authorization we cannot show we
	// signed is one we cannot dispute later.
	if got := lastLedgerPayload(t, d, EvPaymentAuthorized)["interaction_id"]; got != "ix-42" {
		t.Errorf("the authorization is not on our chain: %v", got)
	}
}

func lastResultFor(t *testing.T, d *Daemon, ixID string) capabilityResult {
	t.Helper()
	results, err := d.Results(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.InteractionID == ixID {
			var out capabilityResult
			if err := json.Unmarshal([]byte(r.Result), &out); err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	t.Fatalf("no result for %s", ixID)
	return capabilityResult{}
}

// The loop that had never run: quote → authorize → settle → work.
//
// Every piece of this existed and compiled for a month, and no test and
// no caller ever put them in a line. That is the same mistake as the
// module seam nothing imported, and it is why this test asserts the whole
// arc rather than each step: a step that passes in isolation is exactly
// what was already true.
func TestThePaidLoopClosesEndToEnd(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	work := &pricedProvider{price: 120}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Payer", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Worker", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	grantOn(srv.URL, req.AID(), 500)

	// 1. Ask, and be quoted.
	quoteID, err := req.DelegateCapability(ctx, prov.AID(), "work.do", map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := req.Results(ctx); err != nil {
		t.Fatal(err)
	}
	quote := lastResultFor(t, req, quoteID)
	if quote.Status != string(effect.PaymentRequired) {
		t.Fatalf("step 1: status = %s, want PAYMENT_REQUIRED", quote.Status)
	}
	if work.invoked != 0 {
		t.Fatalf("step 1: unpaid work was done %d times", work.invoked)
	}

	// 2. Pay it, and ask again. A second interaction, because the first
	// ended with a real answer that stays on both chains.
	paidID, err := req.PayAndRetry(ctx, prov.AID(), "work.do", map[string]any{"n": 1}, quote.Payment)
	if err != nil {
		t.Fatalf("step 2: %v", err)
	}
	if paidID == quoteID {
		t.Error("step 2: paying must not overwrite the interaction the quote lives in")
	}

	// 3. The provider settles before working.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if work.invoked != 1 {
		t.Fatalf("step 3: paid work ran %d times, want 1", work.invoked)
	}
	if _, err := req.Results(ctx); err != nil {
		t.Fatal(err)
	}
	done := lastResultFor(t, req, paidID)
	if done.Status != string(effect.OK) {
		t.Fatalf("step 3: status = %s (%s), want OK", done.Status, done.Message)
	}
	if done.Paid == nil || done.Paid.Transaction == "" {
		t.Fatal("step 3: the result does not say it was paid for")
	}

	// 4. The credit actually moved. A loop that reports success without
	// this is the failure mode worth testing for.
	if got := balanceOf(srv.URL, req.AID()); got != 380 {
		t.Errorf("step 4: payer balance = %d, want 380", got)
	}
	if got := balanceOf(srv.URL, prov.AID()); got != 120 {
		t.Errorf("step 4: provider balance = %d, want 120", got)
	}

	// 5. The hub's signed statement reached the payer. Without it the
	// payer holds a transaction string it cannot check, which is the
	// provider's word for the provider having been paid.
	if done.Paid.Receipt == "" {
		t.Fatal("step 5: the hub's settlement receipt did not travel")
	}
	raw, err := base64.StdEncoding.DecodeString(done.Paid.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := payment.UnmarshalReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Verify(hubKELOf(t, srv.URL), hubAIDOf(srv.URL), time.Now().UnixMilli()); err != nil {
		t.Errorf("step 5: the settlement receipt does not verify: %v", err)
	}
	if rec.Payer != req.AID() || rec.PayTo != prov.AID() || rec.Amount != 120 {
		t.Errorf("step 5: receipt = %+v", rec)
	}

	// 6. Both chains carry it, which is the whole custody bargain: the
	// balance is the hub's, the record is the parties'.
	if got := lastLedgerPayload(t, req, EvPaymentAuthorized)["interaction_id"]; got != paidID {
		t.Errorf("step 6: payer's authorization is not on its chain: %v", got)
	}
	payerSettled := lastLedgerPayload(t, req, EvPaymentSettled)
	if payerSettled["interaction_id"] != paidID {
		t.Errorf("step 6: payer's settlement is not on its chain: %v", payerSettled)
	}
	if payerSettled["verified"] != true {
		t.Errorf("step 6: the payer recorded a settlement it could not verify: %v", payerSettled)
	}
	if got := lastLedgerPayload(t, prov, EvPaymentSettled)["interaction_id"]; got != paidID {
		t.Errorf("step 6: payee's settlement is not on its chain: %v", got)
	}

	// 7. And the payer can read its own standing off the custodian.
	bal, err := req.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bal["balance"] != int64(380) {
		t.Errorf("step 7: reported balance = %v, want 380", bal["balance"])
	}
}

// Paying more than you have must fail as a payment, not as the work.
//
// The distinction is the point: a caller told FAILED would look for a bug
// in the capability. PAYMENT_REQUIRED with a reason tells it the truth,
// which is that the work is fine and the money is not.
func TestPayingWithoutCreditIsRefusedAsAPayment(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	work := &pricedProvider{price: 120}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Payer", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Worker", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	grantOn(srv.URL, req.AID(), 10) // not enough

	quoted := &payment.PaymentRequired{
		X402Version: payment.Version,
		Accepts: []payment.PaymentOption{{
			Scheme: payment.SchemeCredit, Network: payment.CreditNetwork(hubAIDOf(srv.URL)),
			Amount: "120", Asset: payment.AssetCredit, PayTo: prov.AID(),
		}},
	}
	id, err := req.PayAndRetry(ctx, prov.AID(), "work.do", nil, quoted)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if work.invoked != 0 {
		t.Fatalf("work ran %d times for a payment that did not settle", work.invoked)
	}
	if _, err := req.Results(ctx); err != nil {
		t.Fatal(err)
	}
	res := lastResultFor(t, req, id)
	if res.Status != string(effect.PaymentRequired) {
		t.Fatalf("status = %s, want PAYMENT_REQUIRED", res.Status)
	}
	if !strings.Contains(res.Message, payment.ReasonInsufficientFunds) {
		t.Errorf("the payer is not told why: %q", res.Message)
	}
	if got := balanceOf(srv.URL, req.AID()); got != 10 {
		t.Errorf("a refused payment moved credit: balance = %d", got)
	}
}

// A settlement presented twice must move the credit once.
//
// Redelivery is normal here — the relay acks after processing, so a crash
// in between replays the delegation — and a facilitator that charged per
// delivery would bill for the network's retries.
func TestSettlingTheSameAuthorizationTwiceChargesOnce(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	d := newTestDaemon(t, srv.URL, false)
	if err := d.RegisterWithHub(ctx, srv.URL, "Payer", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	grantOn(srv.URL, d.AID(), 500)
	opt := payment.PaymentOption{
		Scheme: payment.SchemeCredit, Network: payment.CreditNetwork(hubAIDOf(srv.URL)),
		Amount: "120", Asset: payment.AssetCredit, PayTo: "did:anet:provider",
	}
	pp, err := d.Authorize(opt, "ix-replay")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(pp)
	if err != nil {
		t.Fatal(err)
	}
	first, err := d.settle(ctx, raw)
	if err != nil || !first.Success {
		t.Fatalf("first settlement: %v %+v", err, first)
	}
	second, err := d.settle(ctx, raw)
	if err != nil || !second.Success {
		t.Fatalf("a replay must be idempotent, not an error: %v %+v", err, second)
	}
	if first.Transaction != second.Transaction {
		t.Errorf("the replay settled separately: %s vs %s", first.Transaction, second.Transaction)
	}
	if got := balanceOf(srv.URL, d.AID()); got != 380 {
		t.Errorf("charged twice: balance = %d, want 380", got)
	}
}

// The voucher path: paid at the hub, worked here, and the hub saw neither
// the request nor the result.
func TestAVoucherBuysWorkTheHubNeverSees(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := newTestDaemon(t, srv.URL, true)
	work := &pricedProvider{price: 120}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Worker", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}

	v := signVoucher(t, srv.URL, prov.AID(), "work.do", "buyer-1", "nonce-1")
	res, code := prov.RedeemVoucher(ctx, redeemRequest{
		Voucher: v, Capability: "work.do", Args: map[string]any{"n": 1}})
	if code != 200 {
		t.Fatalf("redeem = %d %v", code, res)
	}
	if res["status"] != string(effect.OK) {
		t.Fatalf("status = %v", res["status"])
	}
	if work.invoked != 1 {
		t.Fatalf("work ran %d times, want 1", work.invoked)
	}
	// The effect is on the chain like any other. A second door to the same
	// work must not be a door around the evidence.
	eff := lastLedgerPayload(t, prov, EvCapabilityEffect)
	if eff["via"] != "voucher" {
		t.Errorf("the chain does not record how this was paid for: %v", eff)
	}
	if got := lastLedgerPayload(t, prov, EvVoucherRedeemed)["payer"]; got != "buyer-1" {
		t.Errorf("the redemption is not on the chain: %v", got)
	}
}

// Every way of getting free work out of the voucher door.
func TestTheVoucherDoorRefusesWhatItShould(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := newTestDaemon(t, srv.URL, true)
	work := &pricedProvider{price: 120}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Worker", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	other := newTestDaemon(t, srv.URL, true)
	if err := other.RegisterWithHub(ctx, srv.URL, "Other", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		req  redeemRequest
		code int
	}{
		{"no voucher at all", redeemRequest{Capability: "work.do"}, http.StatusPaymentRequired},
		{"not base64", redeemRequest{Voucher: "!!!", Capability: "work.do"}, http.StatusPaymentRequired},
		{"random bytes", redeemRequest{
			Voucher:    base64.StdEncoding.EncodeToString([]byte("nonsense")),
			Capability: "work.do"}, http.StatusPaymentRequired},
		{"bought from somebody else, presented here", redeemRequest{
			Voucher:    signVoucher(t, srv.URL, other.AID(), "work.do", "buyer-1", "n-a"),
			Capability: "work.do"}, http.StatusPaymentRequired},
		{"bought a different capability", redeemRequest{
			Voucher:    signVoucher(t, srv.URL, prov.AID(), "work.cheap", "buyer-1", "n-b"),
			Capability: "work.do"}, http.StatusPaymentRequired},
		{"signed by somebody who is not our hub", redeemRequest{
			Voucher:    forgeVoucher(t, prov.AID(), "work.do", "buyer-1", "n-c"),
			Capability: "work.do"}, http.StatusPaymentRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := work.invoked
			res, code := prov.RedeemVoucher(ctx, tc.req)
			if code != tc.code {
				t.Errorf("code = %d, want %d (%v)", code, tc.code, res)
			}
			if work.invoked != before {
				t.Error("work was done for a voucher that should have been refused")
			}
		})
	}
	// And the refusals are on the chain, so an operator can find out its
	// node is turning away every buyer.
	if got := lastLedgerPayload(t, prov, EvVoucherRefused)["reason"]; got == nil || got == "" {
		t.Error("refusals are not recorded")
	}
}

// A voucher is a bearer object. Presenting it twice must buy one job.
func TestAVoucherIsSpentOnce(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := newTestDaemon(t, srv.URL, true)
	work := &pricedProvider{price: 120}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Worker", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	v := signVoucher(t, srv.URL, prov.AID(), "work.do", "buyer-1", "nonce-once")
	req := redeemRequest{Voucher: v, Capability: "work.do"}
	if _, code := prov.RedeemVoucher(ctx, req); code != 200 {
		t.Fatalf("first redemption failed: %d", code)
	}
	if _, code := prov.RedeemVoucher(ctx, req); code != http.StatusConflict {
		t.Errorf("second redemption = %d, want 409", code)
	}
	if work.invoked != 1 {
		t.Errorf("work ran %d times for one voucher", work.invoked)
	}

	// And the guard survives a restart, because it is read back off the
	// chain rather than held only in memory. A buyer who kept a voucher
	// over a crash must not get a second job out of it.
	prov.spent = newSpentVouchers()
	if _, code := prov.RedeemVoucher(ctx, req); code != http.StatusConflict {
		t.Errorf("after a restart the voucher was spendable again: %d", code)
	}
	if work.invoked != 1 {
		t.Errorf("work ran %d times across the restart", work.invoked)
	}
}

// signVoucher mints one signed by the fake hub behind url.
func signVoucher(t *testing.T, url, payTo, capID, payer, nonce string) string {
	t.Helper()
	v := &payment.Voucher{
		AuthID: "auth-" + nonce, Payer: payer, PayTo: payTo, Capability: capID,
		Amount: 120, Network: payment.CreditNetwork(hubAIDOf(url)),
		NotAfter: time.Now().Add(time.Minute).UnixMilli(), Nonce: nonce,
	}
	vv, ok := hubsByURL.Load(url)
	if !ok {
		t.Fatal("no fake hub")
	}
	if err := v.Sign(vv.(*fakeHub).self); err != nil {
		t.Fatal(err)
	}
	raw, err := v.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// forgeVoucher mints a perfectly valid voucher signed by a stranger.
func forgeVoucher(t *testing.T, payTo, capID, payer, nonce string) string {
	t.Helper()
	impostor, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	v := &payment.Voucher{
		AuthID: "auth-" + nonce, Payer: payer, PayTo: payTo, Capability: capID,
		Amount: 120, Network: payment.CreditNetwork(impostor.AID()),
		NotAfter: time.Now().Add(time.Minute).UnixMilli(), Nonce: nonce,
	}
	if err := v.Sign(impostor); err != nil {
		t.Fatal(err)
	}
	raw, err := v.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

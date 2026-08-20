package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
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

// plainMap re-keys a CBOR-decoded payload for assertions. CBOR decodes a
// map into map[any]any because its keys need not be strings; ours always
// are.
func plainMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[k] = plainValue(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[fmt.Sprint(k)] = plainValue(val)
		}
		return out
	}
	return nil
}

func plainValue(v any) any {
	switch t := v.(type) {
	case map[any]any, map[string]any:
		return plainMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = plainValue(e)
		}
		return out
	}
	return v
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

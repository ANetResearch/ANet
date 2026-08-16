package daemon

import (
	"context"
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

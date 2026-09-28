//go:build !no_x402

package daemon

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// RT-IN-1c (closed, default policy, one priced public capability — the shape
// of an official public agent). A stranger calls the public priced
// capability; the node quotes and the task waits in input-required with no
// receipt. The stranger then re-sends a delegation for the same ix under a
// new message id naming a PRIVATE capability: step 9 skips the policy for a
// known ix and redeliveredDelegate executes the new TaskDoc's capability,
// without admission, without payment, without being public.
func TestRedteamPublicQuoteSwappedForPrivateCapability(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	work := &meteredWork{price: 20}
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	// Policy stays closed; only work.do is public.
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: "work.do"}}); err != nil {
		t.Fatal(err)
	}
	if pol := prov.config().inbound().Policy; pol != PolicyClosed {
		t.Fatalf("policy %s", pol)
	}
	id, err := stranger.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	pix := getIX(t, prov, id)
	if pix.Trust != interactions.TrustPublicCap || pix.PayState != interactions.PayRequired || len(pix.Receipt) != 0 {
		t.Fatalf("setup: trust %q pay %q receipt %d", pix.Trust, pix.PayState, len(pix.Receipt))
	}
	// Same ix, new message id, TaskDoc names the private lamp capability.
	task := tsir.Task{Intent: tsir.Intent{Summary: "x", Body: "x"},
		Requires: []tsir.Require{{ID: lampCap, Type: RequireTypeCapability, Necessity: "must"}},
		Contexts: []tsir.Context{{Key: "args", Value: `{"on":true}`, Format: "json"}}}
	td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{task}}
	if err := td.Sign(stranger.self); err != nil {
		t.Fatal(err)
	}
	doc, _ := coredet.Marshal(td)
	body, _ := (&delegation.DelegateReq{TaskDoc: doc, Envelope: td.Envelope, InteractionID: id}).Marshal()
	r := receive(t, prov, sealFrom(t, stranger, prov, seal.TypeDelegate, id, body))
	t.Logf("swapped delegation: %+v", r)
	if len(lamp.invoked) != 1 {
		t.Fatalf("attack failed: private capability invoked %d times", len(lamp.invoked))
	}
	if work.invoked.Load() != 0 {
		t.Fatalf("the priced work ran")
	}
	t.Logf("DEFECT: under policy closed a stranger ran private capability %s unpaid via a public quote's ix", lampCap)
}

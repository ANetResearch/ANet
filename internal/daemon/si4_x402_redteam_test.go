//go:build !no_x402

package daemon

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// SI4-RT-1d. The default policy (closed) and an official-agent style node
// that sells one priced public capability. A stranger calls the public
// capability; the node quotes (input-required, no receipt: not terminal).
// The stranger then sends a second anet.delegate/1 on the same ix whose
// TaskDoc names a NON-public, unpriced capability. authorizeDelegate takes
// the existing-ix branch (same peer, inbound) and skips the inbound policy
// and admission; ingestDelegate treats it as a redelivery, and
// redeliveredDelegate runs capabilityCall(m.td) — the swapped capability —
// for the stranger. Unauthorized execution on a fresh-install policy.
func TestRedteamSI4_ClosedPolicyPublicCapQuoteSwappedForPrivateCapability(t *testing.T) {
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
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: "work.do"}}); err != nil {
		t.Fatal(err)
	}
	if p := prov.config().inbound().Policy; p != PolicyClosed {
		t.Fatalf("setup: policy %s", p)
	}

	// Control: calling the private capability directly is refused.
	cid, err := stranger.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	if _, err := prov.ix.Get(cid); err == nil || len(lamp.invoked) != 0 {
		t.Fatal("control: private capability call accepted directly")
	}

	// Step 1: the public priced capability; the node quotes.
	id, err := stranger.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	pix := getIX(t, prov, id)
	if pix.Trust != interactions.TrustPublicCap || pix.PayState != interactions.PayRequired || pix.IsTerminal() {
		t.Fatalf("setup: trust %q pay %q state %s", pix.Trust, pix.PayState, pix.State)
	}

	// Step 2: same ix, TaskDoc swapped for the private capability.
	env := sealFrom(t, stranger, prov, seal.TypeDelegate, id, delegateBody(t, stranger.self, id, "x", lampCap))
	r := receive(t, prov, env)
	if len(lamp.invoked) != 1 {
		t.Fatalf("attack failed: rx=%+v invoked=%d", r, len(lamp.invoked))
	}
	if work.invoked.Load() != 0 {
		t.Fatalf("the paid work ran")
	}
	after := getIX(t, prov, id)
	t.Logf("DEFECT: under policy closed a stranger executed non-public %s on public_cap ix %s (state now %s, goal %q, rx=%+v)",
		lampCap, id, after.State, after.Goal, r)
}

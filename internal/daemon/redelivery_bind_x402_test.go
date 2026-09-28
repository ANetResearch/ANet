//go:build !no_x402

package daemon

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The default policy (closed) on a node that sells one priced public
// capability, the shape of an official public agent [redteam:F6][redteam:F7]
// (from TestRedteamSI4_ClosedPolicyPublicCapQuoteSwapped…,
// TestRedteamPublicQuoteSwapped… and TestRedteamSI5_ClosedPriced…). A
// stranger calls the public capability and does not pay: the node quotes and
// the task waits in input-required. A delegation on that id naming a
// private, unpriced capability is refused as a collision; neither runs, and
// the quote stands.
func TestAQuotedPublicCallCannotBeSwappedForAPrivateCapability(t *testing.T) {
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
	if st, meta := lastStatusMeta(t, stranger, cid); st != interactions.StateRejected || meta["anet.reason"] != reasonNotAccepting {
		t.Fatalf("control: %s %v", st, meta)
	}

	id, err := stranger.DelegateCapability(ctx, prov.AID(), "work.do", map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	pix := getIX(t, prov, id)
	if pix.Trust != interactions.TrustPublicCap || pix.PayState != interactions.PayRequired || pix.IsTerminal() {
		t.Fatalf("setup: trust %q pay %q state %s", pix.Trust, pix.PayState, pix.State)
	}

	assertSwapRefused(t, prov, swapDelegate(t, stranger, prov, id, lampCap), lamp, id)
	if work.invoked.Load() != 0 {
		t.Fatalf("the paid work ran unpaid")
	}
	after := getIX(t, prov, id)
	if after.State != pix.State || after.PayState != interactions.PayRequired || len(after.Receipt) != 0 || after.Goal != "work.do" {
		t.Fatalf("the quoted task changed: state %s pay %s receipt %d goal %q", after.State, after.PayState, len(after.Receipt), after.Goal)
	}
}

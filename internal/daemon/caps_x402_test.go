//go:build !no_x402

package daemon

// caps_x402_test.go: the parts of the credential policy that need the
// payment module — the grant's pay flag and the rotation after a real paid
// call (sealed-sender SSR-D8, §4.3.2, §4.3.8 item 3).

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// SSR-D8, the pay flag: a node that prices one capability it serves says
// so in its peer capabilities, and a call to a priced capability is a
// possibly paid interaction (§4.3.2).
func TestPricedCapabilitiesSetPay(t *testing.T) {
	_, _, prov := registeredPair(t)
	now := prov.nowMS()
	if prov.pricesSomething() {
		t.Fatal("a node with no priced capability says it prices")
	}
	if err := prov.Providers().Register(context.Background(), &meteredWork{price: 5}); err != nil {
		t.Fatal(err)
	}
	if !prov.pricesSomething() || !prov.capabilityPriced("work.do") || prov.capabilityPriced("other.do") {
		t.Fatal("a priced capability is not seen")
	}
	const caller = "did:anet:priced-peer-0001"
	if err := prov.ix.Create(interactions.New{ID: "ix-cap", Role: interactions.RoleInbound, PeerAID: caller,
		Goal: "work.do", Trust: interactions.TrustPublicCap, IsCapability: true}); err != nil {
		t.Fatal(err)
	}
	r, err := prov.grantRel(caller, capMsg{Kind: capMsgOther, IX: "ix-cap"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !r.MaybePaid || !r.Prices {
		t.Fatalf("a priced call: %+v", r)
	}
	if d := grantFor(capMsg{Kind: capMsgOther, IX: "ix-cap"}, r, now); !d.Issue || d.Scope != interactions.CapScopeIX || d.Pay {
		t.Fatalf("the grant of a priced call: %+v", d)
	}
	g, _ := issueNow(t, prov, caller, grantFor(capMsg{IX: "ix-1"}, capRel{Listed: true, Prices: r.Prices}, now), now)
	gr, _ := receivedGrant(t, prov, g, "", now)
	if gr.Pay == nil || !*gr.Pay {
		t.Fatal("the peer capability of a node that prices does not say pay")
	}
}

// SS §4.3.8 item 3 (M27) on a real a2a-x402 call: once the paid task has
// ended, both sides have given up the peer capabilities between them, the
// one-shot ones stay, and a capability given afterwards survives later
// writes to the ended task.
func TestAPaidCallRotatesPeerCaps(t *testing.T) {
	keepRevokeLoopOut(t)
	work := &meteredWork{price: 120}
	_, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 200, AgentDailyMax: 500, DailyMax: u64(1000)}, prov.AID())
	ctx := context.Background()
	pPeer, pOne, _, _ := capsBetween(t, prov, req.AID(), 10)
	rPeer, rOne, _, _ := capsBetween(t, req, prov.AID(), 20)

	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	poll(t, req)
	poll(t, prov)
	poll(t, req, req)
	if rix := getIX(t, req, id); rix.State != interactions.StateCompleted || rix.PayState != interactions.PayCompleted {
		t.Fatalf("requester at the end: %s / %q", rix.State, rix.PayState)
	}
	if pix := getIX(t, prov, id); !pix.IsTerminal() || pix.PayState != interactions.PayCompleted {
		t.Fatalf("provider at the end: %s / %q", pix.State, pix.PayState)
	}
	for _, c := range []struct {
		name    string
		d       *Daemon
		peer    string
		peerCap interactions.IssuedCap
		oneShot interactions.IssuedCap
		held    string
	}{
		// Each side also holds the one-shot capability the other's messages
		// granted it for the task (engine ENG-K1, SS grantFor): one-shot
		// ones stay.
		{"provider", prov, req.AID(), pPeer, pOne, "peer:given-up+p2p,ix:valid,ix:valid,"},
		{"requester", req, prov.AID(), rPeer, rOne, "peer:given-up+p2p,ix:valid,ix:valid,"},
	} {
		if got := issuedState(t, c.d, c.peerCap, c.oneShot); got != "withdrawn,given" {
			t.Fatalf("%s: issued %s", c.name, got)
		}
		if got := heldState(t, c.d, c.peer); got != c.held {
			t.Fatalf("%s: held %s", c.name, got)
		}
	}
	// A capability given after the settlement stays through later writes
	// to the ended task.
	fresh := issueTo(t, prov, req.AID(), "peer", "", seal.CapClassContact, 90)
	if _, err := prov.ix.SetPayment(id, interactions.PayUpdate{State: interactions.PayState(interactions.PayCompleted)}); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.SetState(id, interactions.StateCompleted, interactions.CauseProviderEnd); err != nil {
		t.Fatal(err)
	}
	if got := issuedState(t, prov, fresh); got != "given" {
		t.Fatalf("a later write to the settled task withdrew a new capability: %s", got)
	}
}

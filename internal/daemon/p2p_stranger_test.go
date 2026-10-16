package daemon

import (
	"testing"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// RT-SSRC-1: a stranger (not on any list of the provider) sends a delegation
// that carries a contact-class peer grant with a p2p address. The provider
// keeps it (step 10) and its p2p transport then reports the stranger as
// reachable at that address.
func TestP2PAddrRefusesStrangerGrant(t *testing.T) {
	_, sender, recv := registeredPair(t)
	removeAllowed(t, recv, sender.AID())
	setPolicy(t, recv, PolicyOpen)
	now := sender.nowMS()
	const evil = "198.51.100.77:4001"
	dec := GrantDecision{Rule: 5, Issue: true, Scope: interactions.CapScopePeer, Class: seal.CapClassContact,
		CapExp: now + capLifeContactMS, P2P: evil}
	g, _ := issueNow(t, sender, recv.AID(), dec, now)
	s := senderOf(sender)
	res := receive(t, recv, craftGrant(t, s, recv, seal.TypeDelegate, "ix-rt1", delegateBody(t, sender.self, "ix-rt1", "hi", ""), g, false))
	t.Logf("receive: class=%v reason=%q", res.class, res.reason)
	h := heldFrom(t, recv, sender.AID(), "peer", "")
	if h == nil {
		t.Fatalf("grant not kept (accepted=%v)", res.class == rxAccepted)
	}
	got, ok := p2pHost(recv).P2PAddr(sender.AID())
	t.Logf("P2PAddr for an unlisted stranger: %q %v (peer listed: %v)", got, ok, recv.readPeers().allowed(sender.AID()))
	if ok && got == evil {
		t.Errorf("stranger address returned: provider would dial the stranger-supplied address %s", got)
	}
}

// A peer on the allow list that sent the same grant is dialled at its address.
func TestP2PAddrAllowedPeerKept(t *testing.T) {
	_, sender, recv := registeredPair(t)
	setPolicy(t, recv, PolicyOpen)
	now := sender.nowMS()
	const addr = "198.51.100.77:4001"
	dec := GrantDecision{Rule: 5, Issue: true, Scope: interactions.CapScopePeer, Class: seal.CapClassContact,
		CapExp: now + capLifeContactMS, P2P: addr}
	g, _ := issueNow(t, sender, recv.AID(), dec, now)
	s := senderOf(sender)
	receive(t, recv, craftGrant(t, s, recv, seal.TypeDelegate, "ix-p2", delegateBody(t, sender.self, "ix-p2", "hi", ""), g, false))
	if !recv.readPeers().allowed(sender.AID()) {
		t.Skip("pair is not allow-listed in this fixture")
	}
	if got, ok := p2pHost(recv).P2PAddr(sender.AID()); !ok || got != addr {
		t.Fatalf("allowed peer: %q %v", got, ok)
	}
}

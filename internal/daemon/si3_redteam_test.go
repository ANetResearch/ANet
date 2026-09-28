package daemon

import (
	"testing"

	"github.com/ANetResearch/ANetCore/seal"
)

// TestRedteamSI3_RefusedEnvelopeAcceptedOnRedeliveryAfterPolicyChange
// records a gap between SI-3 ("replays are refused") and §3.6 [C15c]: an
// envelope refused in step 9 is remembered only in the in-memory LRU, so
// the same bytes presented again after a restart (or LRU eviction) and a
// later policy change are processed as a new delegation. The requester was
// already told "rejected".
//
// The test PASSES when the gap exists (the second delivery creates the
// interaction).
func TestRedteamSI3_RefusedEnvelopeAcceptedOnRedeliveryAfterPolicyChange(t *testing.T) {
	prov := newTestDaemon(t, "", false) // default: closed
	s := newStranger(t)
	ix := "ix_redteam_si3"
	env := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "a task", ""), nil)

	if r := receive(t, prov, env); r.class != rxDropped || r.reason != dropNotAccepting {
		t.Fatalf("first delivery: %+v, want refused as not-accepting", r)
	}
	if _, err := prov.ix.Get(ix); err == nil {
		t.Fatal("refused delegation was stored")
	}

	// A restart clears the in-memory refused list; nothing persistent
	// remembers the envelope.
	prov.refused = boundedSet{}
	// Later, the operator allows the peer.
	allowPeers(t, prov, s.aid)

	r := receive(t, prov, env)
	got, err := prov.ix.Get(ix)
	if r.class != rxAccepted || err != nil || got.PeerAID != s.aid {
		t.Fatalf("gap not reproduced: outcome %+v, interaction err %v", r, err)
	}
	t.Logf("the same envelope, refused earlier, was accepted: interaction %s state %s", got.ID, got.State)
}

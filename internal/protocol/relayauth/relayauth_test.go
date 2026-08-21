package relayauth

import (
	"strings"
	"testing"
)

// The preimage is a shared constant between two repos.
//
// The daemon builds these bytes and signs them; the hub builds them again
// and verifies. They are not exchanged, only recomputed — so a change on
// either side that looks harmless (a separator, a field order, an action
// name) locks every node out of its own mailbox, and the symptom at the
// hub is an invalid signature from a key that is perfectly valid.
//
// Pinning the exact bytes is the point. A test that only compared
// Preimage to itself would pass through any such change.
func TestPreimageBytesArePinned(t *testing.T) {
	const aid = "bafyreihnnooeomsi5widaw5oc2xiisivmuipzalyxxb7mybqwu4uj7ipay"
	got := string(Preimage(ActionPoll, aid, 1767225600000))
	want := "anet-relay/poll/" + aid + "/1767225600000"
	if got != want {
		t.Fatalf("the signed bytes moved:\n got %s\nwant %s\n"+
			"Every node signing the old form is now locked out of its mailbox.", got, want)
	}
}

// A signature authorises one action, for one AID, at one time. Anything
// less and a captured signature becomes a capability someone else holds.
func TestASignatureAuthorisesOnlyWhatItSays(t *testing.T) {
	const alice = "did:anet:alice"
	const bob = "did:anet:bob"
	base := string(Preimage(ActionPoll, alice, 1000))

	// Reading someone's mailbox and rewriting their public profile are not
	// the same permission. If the action were outside the preimage, a
	// captured poll signature would be a profile-overwrite signature.
	for _, action := range []string{ActionAck, ActionRegister, ActionProfile} {
		if other := string(Preimage(action, alice, 1000)); other == base {
			t.Errorf("%q and %q produce the same challenge — one signature would authorise both",
				ActionPoll, action)
		}
	}
	if other := string(Preimage(ActionPoll, bob, 1000)); other == base {
		t.Error("two AIDs share a challenge — Alice's signature would open Bob's mailbox")
	}
	if other := string(Preimage(ActionPoll, alice, 1001)); other == base {
		t.Error("the timestamp is not in the challenge — a captured signature would never expire")
	}
}

// Every action constant must be distinct, and distinct after being placed
// in the preimage. Adding a new action is the moment this can break, and
// adding one is a two-line change nobody reviews closely.
func TestEveryActionIsDistinct(t *testing.T) {
	actions := []string{ActionPoll, ActionAck, ActionRegister, ActionProfile}
	seen := map[string]string{}
	for _, a := range actions {
		p := string(Preimage(a, "did:anet:x", 1))
		if prev, dup := seen[p]; dup {
			t.Errorf("actions %q and %q produce the same challenge", prev, a)
		}
		seen[p] = a
	}
	if len(seen) != len(actions) {
		t.Errorf("%d actions collapsed to %d challenges", len(actions), len(seen))
	}
}

// The replay window has to be short enough to matter and long enough to
// survive ordinary clock drift. Five minutes is the number both repos
// enforce; pinning it here means changing one side alone fails a test
// rather than silently widening the window on the other.
func TestReplayWindowIsFiveMinutes(t *testing.T) {
	if MaxSkewMillis != 5*60*1000 {
		t.Fatalf("replay window = %dms, want 300000ms — the hub enforces this number too",
			MaxSkewMillis)
	}
}

// The separator only separates because no field contains it.
//
// "anet-relay/" + action + "/" + aid + "/" + ts is unambiguous exactly
// while action and aid are slash-free: Preimage("poll", "a/1", 2) and
// Preimage("poll/a", "1", 2) are the same bytes. Actions are the four
// constants above and AIDs are CIDs, so neither can carry a slash today —
// the ambiguity is a property of the inputs, not of the format.
//
// Written down because that is the kind of precondition a later change
// walks into. The day this function is asked to sign for a human-chosen
// name instead of a CID, this test is the one that has to be read.
func TestTheFormatIsUnambiguousOnlyForSlashFreeFields(t *testing.T) {
	for _, a := range []string{ActionPoll, ActionAck, ActionRegister, ActionProfile} {
		if strings.Contains(a, "/") {
			t.Errorf("action %q contains the separator: it can pose as a different action+aid pair", a)
		}
	}
	// The collision the constraint rules out, stated so it is visible.
	if string(Preimage("poll", "a/1", 2)) != string(Preimage("poll/a", "1", 2)) {
		t.Fatal("expected the documented ambiguity; the format changed, so re-derive the precondition")
	}
}

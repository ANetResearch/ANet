// Package relayauth defines the canonical challenge a client signs to authenticate a Hub relay mailbox
// operation (poll/ack). The client signs Preimage(action, aid, ts) with its KEL current key; the Hub
// rebuilds the identical bytes and verifies them against the registered KEL (identity.VerifyObject),
// bounded by MaxSkewMillis so a captured signature cannot be replayed indefinitely. Shared here so the
// signer (daemon) and verifier (hub) can never disagree on the preimage.
package relayauth

import "strconv"

// Auth actions (part of the signed preimage so a signature for one action cannot be reused for another).
const (
	ActionPoll = "poll"
	ActionAck  = "ack"
	// ActionRegister authenticates a registry publish (proves the registrant holds the AID's key, so a
	// public KEL cannot be replayed by a stranger to overwrite someone's registration).
	ActionRegister = "register"
	// ActionProfile authenticates a self-description update (summary/readme/pricing) for an AID.
	ActionProfile = "profile"
)

// MaxSkewMillis bounds the accepted clock skew / replay window for a signed relay auth challenge.
const MaxSkewMillis = 5 * 60 * 1000

// Preimage returns the exact bytes a caller signs to authenticate a relay mailbox operation for aid at
// ts (unix millis).
func Preimage(action, aid string, ts uint64) []byte {
	return []byte("anet-relay/" + action + "/" + aid + "/" + strconv.FormatUint(ts, 10))
}

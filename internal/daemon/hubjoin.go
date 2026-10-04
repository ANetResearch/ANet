package daemon

// hubjoin.go is the node side of joining a hub (v0.3.0 LEC §4.3.10, AD-1 to
// AD-9): one join loop per node that asks the hub whether it would take this
// AID (check_only), lets the identity area designate the hub as the
// identity's witness (the rot hook), registers, and keeps the registration
// current afterwards. The join state it keeps is what `anet status`, MCP
// node_status and the send gate read (AD-2, AD-6).
//
// There is no invitation and no secret anywhere in it: a hub that admits by
// AID answers admission_required naming the AID, and the operator runs
// `anet-hub admit <AID>`.

import (
	"context"
	"encoding/json"
)

// The join states: hub.state in /status and /hub-register (LEC AD-2). The
// value depends only on what the hub answered; whether the identity has a
// recovery passphrase is not a join state.
const (
	// JoinNone: no hub is configured.
	JoinNone = "none"
	// JoinPendingAdmission: the hub answered admission_required; the loop
	// retries in the background and finishes on its own once the operator
	// admits the AID.
	JoinPendingAdmission = "pending_admission"
	// JoinRetrying: a temporary failure before the first registration
	// finished; the loop retries in the background.
	JoinRetrying = "retrying"
	// JoinRefused: an answer that will not change by retrying; the loop
	// stops until `anet hub-register` runs again or the daemon restarts.
	JoinRefused = "refused"
	// JoinRegistered: the hub accepted the registration.
	JoinRegistered = "registered"
)

// JoinHooks are where the identity area hooks into a first join (LEC AD-4;
// KEL D-12, D-13). The join loop calls Prepare only after the hub confirmed
// it would take this AID (check_only answered admitted or open), and Adopt
// once after the hub accepted the registration Prepare's KEL was sent in.
// An identity that already has a witness registers without them.
//
// KEL-9 implements them (identity_ops.go); fakeJoinHooks stands in for them
// in tests.
type JoinHooks interface {
	// Prepare builds the rot that designates hubAID as the identity's
	// witness, saves it before it returns (reusing one already saved, so
	// a retry never builds a second rot for the same hub and key event),
	// and returns the KEL to register with (identity.MarshalKEL bytes).
	Prepare(ctx context.Context, hubAID string) (kel []byte, err error)
	// Adopt takes the registration the hub accepted: the rot becomes the
	// identity's accepted tip. It is idempotent: the loop may call it
	// again for the same answer after a crash.
	Adopt(ctx context.Context, resp RegisterResult) error
}

// RegisterResult is a /register answer the hub accepted (200, core
// §2.12.2), with what was sent to get it: what Adopt is given.
type RegisterResult struct {
	// Hub is the hub's URL and HubAID its AID.
	Hub    string
	HubAID string
	// KEL is the key history that was registered (identity.MarshalKEL
	// bytes): Prepare's, or the node's own when its identity already had
	// this hub as its witness.
	KEL []byte
	// The answer's fields (core §2.12.2).
	AID        string
	Status     string
	KeysStatus string
	KeysError  string
	CardStatus string
	CardError  string
	// ProbationUntil (unix ms) and Trial are there only for an account
	// that came in through open registration or a sign-up window.
	ProbationUntil *uint64
	Trial          string
	// KeyStateNotice is key_state_notice as the hub sent it (base64url of
	// the notice's bytes) and KeyStateNoticeError key_state_notice_error;
	// the identity area reads them (KEL D-50).
	KeyStateNotice      string
	KeyStateNoticeError string
	// Raw is the whole answer body, for the fields this struct does not
	// name.
	Raw json.RawMessage
}

// WitnessRot is hub_join.json's witness_rot (KEL §2.9): the commitment the
// witness designation rot will carry and the hub it designates. Public
// material only: next_digest commits to a key nobody can derive from what
// the data directory holds once the recovery passphrase is set (KEL D-10).
type WitnessRot struct {
	// NextDigest is the 32-byte next_digest (standard base64 in the file).
	NextDigest []byte `json:"next_digest"`
	// HubAID is the AID of the hub the rot designates.
	HubAID string `json:"hub_aid"`
}

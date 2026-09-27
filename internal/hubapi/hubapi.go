// Package hubapi holds the wire-format types and constants shared with the Hub service — the
// centralized registry + relay + review service every v0.1 agent connects to. The Hub itself is a
// separate closed-source service (the official deployment lives at https://hub.agentnetwork.org.cn);
// this package deliberately contains NO server logic, only the JSON shapes the daemon's HTTP client
// exchanges with it.
package hubapi

import (
	"encoding/json"

	"github.com/ANetResearch/ANetCore/relayauth"
)

// AgentView is an agent's public registry entry plus its aggregate rating. Agents are addressed purely
// by AID (v0.1 has no P2P endpoint) — all traffic flows through the Hub relay. The profile fields
// (summary/readme/pricing) are AGENT-authored self-description (set via `anet profile set`); pricing is
// display-only text in v0.1 (no settlement).
type AgentView struct {
	AID          string   `json:"aid"`
	Name         string   `json:"name"`
	Caps         []string `json:"caps"`
	Summary      string   `json:"summary,omitempty"` // one-line self-description
	Readme       string   `json:"readme,omitempty"`  // longer markdown self-description
	Pricing      string   `json:"pricing,omitempty"` // free-form pricing text (display-only in v0.1)
	Listed       bool     `json:"listed"`            // true if it advertises a service (caps or profile) — only listed agents appear in the starfield/find
	AvgRating    float64  `json:"avg_rating"`
	ReviewCount  int      `json:"review_count"`
	RegisteredAt string   `json:"registered_at"`
	// HomeHub is set only on an agent learned from a peer hub: which hub
	// an agent lives on decides where work for it is delivered.
	//
	// It was missing here while the hub had been sending it, so every
	// federated agent this daemon found arrived with the one fact that
	// says how to reach it silently dropped. Nothing failed — the field
	// simply was not in the struct, so encoding/json discarded it and
	// `anet find --cap` listed agents on other hubs as if they were
	// local. Found by pinning the wire, which is the only way this class
	// of drift ever shows up.
	HomeHub string `json:"home_hub,omitempty"`
}

// ReviewView is one stored, verified review. The hub verified the receipt's and the review's
// signatures and that the review is anchored to the receipt. It holds no task content (A2A-DESIGN
// §9): RequestCID and ResultCID are the receipt's commitments, and ContentBinding is always
// "UNVERIFIED" — whether they match any particular bytes was not checked by the hub.
type ReviewView struct {
	InteractionID  string `json:"interaction_id"`
	SubjectAID     string `json:"subject_aid"`
	ReviewerAID    string `json:"reviewer_aid"`
	Rating         int    `json:"rating"`
	Comment        string `json:"comment,omitempty"`
	ReceiptCID     string `json:"receipt_cid"`
	RequestCID     string `json:"request_cid"`     // the receipt's commitment to the request
	ResultCID      string `json:"result_cid"`      // the receipt's commitment to the result
	ContentBinding string `json:"content_binding"` // ContentBindingUnverified
	CompletedAt    uint64 `json:"completed_at"`    // provider's receipt time (unix millis)
	CreatedAt      uint64 `json:"created_at"`      // review time (unix millis)
}

// ContentBindingUnverified is ReviewView.ContentBinding on every review: the hub holds no content.
const ContentBindingUnverified = "UNVERIFIED"

// UploadReviewRequest is the body of POST /reviews: the provider-signed receipt and the
// requester-signed review, both base64 CoreDet-CBOR, and nothing else. Earlier versions also sent the
// request TaskDoc and the deliverable; the hub no longer receives task content and answers 400 to a
// body that carries either.
type UploadReviewRequest struct {
	Receipt string `json:"receipt"` // base64(evidence.Receipt.Marshal)
	Review  string `json:"review"`  // base64(evidence.Review.Marshal)
}

// C2 — the Hub wire contract's own version.
//
// The daemon, the hub and ANetLink each depend only on ANetCore, which is
// what keeps them from knowing about each other. That discipline is worth
// nothing if they silently disagree about which contract they are speaking:
// the three repos had drifted onto three different kernel versions at once,
// and nothing on the wire could have told anybody. So each side states the
// contract version it speaks, on every request and every response.
//
// The version belongs to the wire, not to a shared Go symbol — the hub has
// its own declaration of the same number, which is the point: two programs
// that never import each other still have to agree, and a header is how
// they say so.
//
// Wire 2 (A2A-DESIGN §3.7) is a clean break: every daemon-to-daemon
// message is a sealed envelope the hub cannot read, and every signed call
// carries relayauth v2 headers. The two sides refuse each other across the
// break rather than degrade: a wire-2 hub answers 426 to a daemon that sends
// no version or a lower one on /relay/*, and a wire-2 daemon refuses to
// operate against a hub that states a version below 2 (hub_client.go). There
// is no plaintext fallback in either direction.
const (
	WireVersion       = 2
	WireVersionHeader = "X-ANet-Wire"
)

// Relay v2 authentication headers (A2A-DESIGN §3.7). The values are the
// relayauth constants; they are restated here so that the whole hub wire
// contract of this daemon can be read, and pinned, in one package.
const (
	HeaderAID = relayauth.HeaderAID // signer AID
	HeaderTS  = relayauth.HeaderTS  // signing time, unix ms, decimal
	HeaderSeq = relayauth.HeaderSeq // signer key_state_seq, decimal
	HeaderSig = relayauth.HeaderSig // relayauth.EncodeSig over relayauth.PreimageV2
)

// RelaySendRequest is the body of POST /relay/send. The sender is the
// authenticated X-ANet-AID and is not part of the body; the hub uses it for
// rate limiting and quota and does not store it (A2A-DESIGN §2 X1).
type RelaySendRequest struct {
	ToAID string `json:"to_aid"`
	// Envelope is a seal.SealedEnvelope encoding, standard base64. The hub
	// checks only the outer structure (seal.ParseOuter) and that its to
	// equals ToAID.
	Envelope string `json:"envelope"`
}

// RelaySendResponse is the 200 answer to POST /relay/send. ID is the
// mailbox row when the hub queued the envelope itself; a hub that forwarded
// it to the recipient's home hub answers with Status "forwarded" and ViaHub
// instead.
type RelaySendResponse struct {
	ID     int64  `json:"id,omitempty"`
	Status string `json:"status"`
	ViaHub string `json:"via_hub,omitempty"`
	// RecipientQuiet and Warning are the hub's statement that the recipient
	// has not collected its mail for a long time. Optional: a hub that
	// does not track it omits both, and the daemon then reports nothing.
	RecipientQuiet bool   `json:"recipient_quiet,omitempty"`
	Warning        string `json:"warning,omitempty"`
}

// RelayPollRequest is the body of POST /relay/poll. The mailbox polled is
// the authenticated X-ANet-AID.
type RelayPollRequest struct {
	Limit int `json:"limit"`
}

// RelayPollResponse is the answer to POST /relay/poll.
type RelayPollResponse struct {
	Messages []RelayMessage `json:"messages"`
}

// RelayMessage is one undelivered envelope. The hub stores nothing else
// about it that it returns: no sender, no kind, no interaction id (SI-2).
type RelayMessage struct {
	ID       int64  `json:"id"`
	Envelope string `json:"envelope"` // standard base64
}

// RelayAckRequest is the body of POST /relay/ack. Acked rows are deleted.
type RelayAckRequest struct {
	IDs []int64 `json:"ids"`
}

// KeysResponse is the answer to GET /agents/{aid}/keys: the AID's signed
// encryption key set and the KEL it verifies against, both standard base64.
// A hub may answer for an AID registered elsewhere by asking a peer hub
// (/fed/v2/keys/{aid}); the daemon verifies the answer itself with
// expectAID = the AID it asked about, so the hub is not trusted with it.
type KeysResponse struct {
	AID    string `json:"aid"`
	KeySet string `json:"keyset"` // seal.SignedEncKeySet encoding
	KEL    string `json:"kel"`    // identity.MarshalKEL
}

// KeysPublishRequest is the body of POST /agents/{aid}/keys, signed with
// relayauth action "keys" by the AID itself. The hub accepts a strictly
// higher EncKeySet.seq, answers 200 without change to an identical set at
// the same seq, 409 to a lower seq or a different set at the same seq, and
// 400 to a set that does not verify.
type KeysPublishRequest struct {
	KeySet string `json:"keyset"` // seal.SignedEncKeySet encoding, standard base64
}

// KeysPublishResponse is the 200 answer to POST /agents/{aid}/keys.
type KeysPublishResponse struct {
	AID        string `json:"aid"`
	KeysStatus string `json:"keys_status"` // KeysStatusOK or KeysStatusUnchanged
}

// Values of keys_status, in the /register answer and the keys publish
// answer.
const (
	KeysStatusOK        = "ok"        // stored: the first set, or a higher seq
	KeysStatusUnchanged = "unchanged" // same seq and same set bytes as stored
	KeysStatusAbsent    = "absent"    // the registration carried no key set
	KeysStatusInvalid   = "invalid"   // did not decode or verify; not stored
	KeysStatusConflict  = "conflict"  // lower seq, or same seq with different bytes; not stored
)

// RegisterRequest is the body of POST /register. It is signed with
// relayauth v2 action "register" in the headers; the hub verifies the
// signature against KEL (the registrant's own KEL, standard base64), which
// must extend any KEL the hub already holds for the AID (A2A-DESIGN §3.8).
//
// Wire 2 carries no guest quota: guest mode is removed from the hub
// (A2A-DESIGN §9).
type RegisterRequest struct {
	AID    string   `json:"aid"`
	Name   string   `json:"name"`
	Caps   []string `json:"caps"`
	KEL    string   `json:"kel"`
	Invite string   `json:"invite,omitempty"`
	// Card is the signed ADP AgentCard (JSON), when the daemon could sign one.
	Card json.RawMessage `json:"card,omitempty"`
	// EncKeys is the AID's current seal.SignedEncKeySet encoding, standard
	// base64, published with every registration so a re-registration
	// after a restart also restates it.
	EncKeys string `json:"enc_keys,omitempty"`
	// A2ACard is the node's signed A2A network card (A2A-DESIGN §10.1),
	// the exact bytes a2acard.Sign returned. Absent from a node with no
	// public skill, which publishes no card.
	A2ACard json.RawMessage `json:"a2a_card,omitempty"`
}

// RegisterResponse is the answer to POST /register. KeysStatus and
// CardStatus report the enc_keys and a2a_card fields separately: a key set
// the hub refuses does not fail the registration (A2A-DESIGN §3.1).
type RegisterResponse struct {
	AID        string `json:"aid"`
	Status     string `json:"status"`
	KeysStatus string `json:"keys_status"`
	KeysError  string `json:"keys_error,omitempty"`
	CardStatus string `json:"card_status"`
	CardError  string `json:"card_error,omitempty"`
}

// Values of card_status in the /register answer: what the hub did with
// the a2a_card field (A2A-DESIGN §3.7, §10.3). Same names and meaning as
// keys_status.
const (
	CardStatusOK        = "ok"        // verified and stored: the first card, or a higher seq
	CardStatusUnchanged = "unchanged" // same seq and same signed content as stored
	CardStatusAbsent    = "absent"    // the registration carried no card
	CardStatusInvalid   = "invalid"   // did not verify; not stored
	CardStatusConflict  = "conflict"  // lower seq, or same seq with other content; not stored
	// CardStatusUnverified is a hub that stores the card before its
	// admission step exists: kept, nothing checked.
	CardStatusUnverified = "unverified"
)

// HubIdentity is the answer to GET /hub/identity: the hub's AID and KEL
// (standard base64). The daemon needs the AID before its first signed call,
// because relayauth v2 binds every signature to the hub it is addressed to.
type HubIdentity struct {
	AID string `json:"aid"`
	KEL string `json:"kel"`
}

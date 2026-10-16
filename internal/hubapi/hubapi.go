// Package hubapi holds the wire-format types and constants shared with the hub — the registry, relay,
// review, x402 facilitator and federation service a daemon registers with. The hub is its own
// repository (ANetHub, built as anet-hub; the official deployment lives at
// https://hub.agentnetwork.org.cn), and its wire contract tests pin the same shapes from the other
// side. This package deliberately contains NO server logic, only the JSON shapes the daemon's HTTP
// client exchanges with it.
package hubapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"
)

// AgentView is an agent's public registry entry plus its aggregate rating. Agents are addressed purely
// by AID; delivery goes through the hub relay, or directly when both nodes run the p2p module. The
// profile fields (summary/readme) are AGENT-authored self-description (set via `anet profile set`). No
// entry carries a price: what a skill costs is given in the quote, inside the task.
type AgentView struct {
	AID          string   `json:"aid"`
	Name         string   `json:"name"`
	Caps         []string `json:"caps"`
	Summary      string   `json:"summary,omitempty"` // one-line self-description
	Readme       string   `json:"readme,omitempty"`  // longer markdown self-description
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

// ReviewView is one stored, verified review as the hub lists it (HPP §2.5, PAY-V5). The hub holds
// no task content and no reviewer: a review names the reviewed agent, the receipt it rates and
// two UTC day numbers. The reviewer's AID and any millisecond time are not part of it (SI-48).
type ReviewView struct {
	SubjectAID   string `json:"subject_aid"`
	Rating       int    `json:"rating"`
	Comment      string `json:"comment,omitempty"`
	ReceiptCID   string `json:"receipt_cid"`
	CompletedDay uint64 `json:"completed_day"` // UTC day number of the receipt
	CreatedDay   uint64 `json:"created_day"`   // UTC day number of the review
}

// UploadReviewRequest is the body of POST /reviews (HPP §2.5): the provider-signed receipt, the
// requester-signed review (both CoreDet-CBOR) and the salt of requester_commit, all base64url
// without padding, and nothing else. The hub answers 400 to any other member.
type UploadReviewRequest struct {
	Receipt string `json:"receipt"` // B64(evidence.ReceiptWire.Marshal)
	Review  string `json:"review"`  // B64(evidence.ReviewWire.Marshal)
	RSalt   string `json:"rsalt"`   // B64 of the 32-byte salt of requester_commit
}

// UploadReviewResponse is the hub's 200 to POST /reviews: the hub's attestation (B64 of
// evidence.ReviewAttWire.Marshal), which says it stored this review on that day.
type UploadReviewResponse struct {
	Status      string `json:"status"` // "stored"
	Attestation string `json:"attestation"`
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
// Wire 3 (core CORE-V1) is a clean break, like wire 2 before it: a send
// names no sender (a first contact goes under a session MAC, every later
// message under a credential its recipient issued), the binary values of
// the JSON bodies are base64url without padding (CORE-E5), and a refusal
// is {reason, error} (core G1). Neither side degrades: a wire-3 hub answers
// 426 wire_too_old to a daemon that states no version or a lower one on any
// write and on /relay/* and /fed/*, and a wire-3 daemon stops talking to a
// hub that answers 426 or states another version (hub_client.go).
const (
	WireVersion       = 3
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

// RelaySendResponse is the 200 answer to POST /relay/send (core §2.12.2,
// DLV-W-12): Status is RelayQueued or RelayDuplicate; RID names the queued
// envelope for a later /relay/status, and RIDBytes decodes it; ViaHub is
// the AID of the hub it was queued at when that is not the one asked
// (DLV-W-12a). RecipientQuiet is the hub's statement that the recipient has
// not collected its mail for a long time; it says nothing more, and the
// daemon words it itself (engine ENG-H2). The request is the envelope's
// own bytes (EnvelopeContentType): there is no body of JSON to describe.
type RelaySendResponse struct {
	Status         string `json:"status"`
	RID            string `json:"rid"`
	ViaHub         string `json:"via_hub,omitempty"`
	RecipientQuiet bool   `json:"recipient_quiet"`
}

// The statuses of a RelaySendResponse (core G4: there is no "forwarded").
const (
	RelayQueued    = "queued"
	RelayDuplicate = "duplicate"
)

// RelayPollRequest is the body of POST /relay/poll. The mailbox polled is
// the authenticated X-ANet-AID.
//
// AfterID is the cursor (decision Q1): when set, only envelopes with an id
// above it are returned, oldest first, under the same limit and byte
// budget. Zero is the whole mailbox and is left out of the body, so a
// request without a cursor is the same bytes as before the field existed.
type RelayPollRequest struct {
	Limit   int   `json:"limit"`
	AfterID int64 `json:"after_id,omitempty"`
}

// RelayPollResponse is the answer to POST /relay/poll.
type RelayPollResponse struct {
	Messages []RelayMessage `json:"messages"`
	// More says the mailbox holds more after the last row of this page, in
	// the snapshot the hub read it from (forward-secrecy FS-H5): the page
	// was cut by its count or byte budget. A page that ends the mailbox has
	// more false; the key ring's drain mark rests on it (FS-K9).
	More bool `json:"more"`
	// KeyStateNotice is the witness notice the hub signed for the caller's
	// KEL tip (base64url of the NoticeWire bytes), or KeyStateNoticeError
	// why it gave none: quarantine, not_admitted or not_witness (KEL §2.7,
	// H-13).
	KeyStateNotice      string `json:"key_state_notice,omitempty"`
	KeyStateNoticeError string `json:"key_state_notice_error,omitempty"`
}

// RelayMessage is one undelivered envelope. The hub stores nothing else
// about it that it returns: no sender, no kind, no interaction id (SI-2).
// Envelope is base64url without padding (core G2), decoded with DecodeB64.
type RelayMessage struct {
	ID       int64  `json:"id"`
	Envelope string `json:"envelope"`
	// AcceptedAt is when the hub accepted the envelope, by the hub's
	// clock (unix ms; relay_message.created_at, core §2.16).
	AcceptedAt uint64 `json:"accepted_at"`
}

// RelayAckRequest is the body of POST /relay/ack (relayauth action ack;
// core §2.12.2, DLV §2.4.2): the mailbox rows this daemon has decided for
// good, at most RelayAckMaxItems. The hub deletes each row that is the
// caller's, and keeps the delivery token an item carries for the sender to
// fetch (DLV-H-3).
type RelayAckRequest struct {
	Items []RelayAckItem `json:"items"`
}

// RelayAckItem acknowledges one mailbox row. Handle and DAck go together:
// the row's delivery token, B64 of HandleLen and DAckLen bytes, when the
// receive path made one; neither when it made none (DLV-R-6a).
type RelayAckItem struct {
	ID     int64  `json:"id"`
	Handle string `json:"handle,omitempty"`
	DAck   string `json:"dack,omitempty"`
}

// RelayAckResponse is the answer to POST /relay/ack: Acked mailbox rows
// were deleted by this request, and Stored delivery tokens written or
// overwritten (DLV-W-15).
type RelayAckResponse struct {
	Acked  int `json:"acked"`
	Stored int `json:"stored"`
}

// RelayAckMaxItems is the most items one /relay/ack request may carry;
// the hub refuses a request with more, whole (DLV-W-14).
const RelayAckMaxItems = 1000

// RelayStatusRequest is the body of POST /relay/status, the anonymous
// question "did my envelope arrive?" (core §2.12.2, DLV §2.4.3): 1 to
// RelayStatusMaxItems items, all for one hub. It carries no identity
// header and goes on a connection of its own (DLV-W-17).
type RelayStatusRequest struct {
	Items []RelayStatusItem `json:"items"`
}

// RelayStatusItem asks about one queued envelope: the RID the send was
// answered with and the handle of its ack_key, both B64. ViaHub is the
// send answer's via_hub; every item of a request has the same one.
type RelayStatusItem struct {
	RID    string `json:"rid"`
	Handle string `json:"handle"`
	ViaHub string `json:"via_hub,omitempty"`
}

// RelayStatusResponse answers a RelayStatusRequest, one result per item, in
// the request's order.
type RelayStatusResponse struct {
	Items []RelayStatusResult `json:"items"`
}

// RelayStatusResult is the state of one envelope: RelayStatusQueued,
// RelayStatusDone with the recipient's DAck, or RelayStatusUnknown.
// DAckBytes decodes DAck.
type RelayStatusResult struct {
	State string `json:"state"`
	DAck  string `json:"dack,omitempty"`
}

// The states of a RelayStatusResult (DLV §2.4.3).
const (
	// RelayStatusQueued: the envelope is still in the recipient's mailbox.
	RelayStatusQueued = "queued"
	// RelayStatusDone: the recipient took it and left this delivery token.
	RelayStatusDone = "done"
	// RelayStatusUnknown: neither; the hub knows nothing of it.
	RelayStatusUnknown = "unknown"
)

// The relay routes of the mailbox and the status query (core §2.12.2):
// the signed poll and ack of a recipient, the anonymous status query of a
// sender (DLV §2.4.3).
const (
	RelayPollPath   = "/relay/poll"
	RelayAckPath    = "/relay/ack"
	RelayStatusPath = "/relay/status"
)

// RelayStatusMaxItems is the most items one /relay/status request may
// carry (DLV-W-16).
const RelayStatusMaxItems = 64

// Sizes of the binary values of the delivery-token endpoints (core §2.7,
// §2.16): HandleLen and DAckLen are the seal sizes the hub checks.
const (
	RIDLen    = 16
	HandleLen = seal.HandleLen
	DAckLen   = seal.DAckLen
)

// B64 is a binary value as hub wire 3 carries it: base64url without
// padding (core CORE-E5).
func B64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// ErrBadB64 is a binary value that is not strict base64url, or not of the
// length its member has.
var ErrBadB64 = errors.New("hubapi: not a strict base64url value of the expected length")

// DecodeB64 decodes a hub wire 3 binary value strictly (core CORE-E5):
// only the base64url alphabet, no padding, no white space or line breaks
// (which Go's decoder would skip), no stray bits in the last character —
// so a value has one spelling — and exactly n bytes; n < 0 takes any
// length.
func DecodeB64(s string, n int) ([]byte, error) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return nil, fmt.Errorf("%w: byte %d is %q", ErrBadB64, i, c)
		}
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadB64, err)
	}
	if n >= 0 && len(b) != n {
		return nil, fmt.Errorf("%w: %d bytes, want %d", ErrBadB64, len(b), n)
	}
	return b, nil
}

// RIDBytes decodes the send answer's rid. An answer whose rid is missing,
// not strict base64url or not RIDLen bytes is an error: the daemon takes
// the envelope as handed over without a rid (DLV-W-12b).
func (r RelaySendResponse) RIDBytes() ([RIDLen]byte, error) {
	var rid [RIDLen]byte
	b, err := DecodeB64(r.RID, RIDLen)
	if err != nil {
		return rid, fmt.Errorf("rid: %w", err)
	}
	copy(rid[:], b)
	return rid, nil
}

// DAckBytes decodes a done result's delivery token. A dack missing, not
// strict base64url or not DAckLen bytes is an error, which the daemon
// takes as a token that does not verify (DLV-W-18a, engine ENG-D4).
func (r RelayStatusResult) DAckBytes() ([DAckLen]byte, error) {
	var d [DAckLen]byte
	b, err := DecodeB64(r.DAck, DAckLen)
	if err != nil {
		return d, fmt.Errorf("dack: %w", err)
	}
	copy(d[:], b)
	return d, nil
}

// NewRelayAckItem is the /relay/ack item for mailbox row id, carrying the
// delivery token (handle, dack) the receive path made for it.
func NewRelayAckItem(id int64, handle [HandleLen]byte, dack [DAckLen]byte) RelayAckItem {
	return RelayAckItem{ID: id, Handle: B64(handle[:]), DAck: B64(dack[:])}
}

// KeysResponse is the answer to GET /agents/{aid}/keys, which this node
// asks of its own AID only (on the signed side, SSR-W16): its signed
// encryption key set and the KEL it verifies against. Another agent's key
// set comes from a directory bucket (dirquery.go), never by its AID.
type KeysResponse struct {
	AID    string `json:"aid"`
	KeySet string `json:"keys"` // seal.SignedEncKeySet encoding, base64url (B64)
	KEL    string `json:"kel"`  // identity.MarshalKEL, base64url
}

// KeysPublishRequest is the body of POST /agents/{aid}/keys (core
// §2.12.2), signed with relayauth action "keys" by the AID itself. Keys is
// the seal.SignedEncKeySet encoding in base64url (B64, CORE-E5); PrevSeq the
// seq of the last set this node signed before it (forward-secrecy FS-K5),
// 0 for none. The hub stores a set that follows the one it holds and
// answers 200 with keys_status ok, or unchanged for the set it holds; 409
// keys_rollback with stored_seq when it holds a newer set than PrevSeq, or
// the set's seq is not above the one it holds (FS-H4); 400 bad_request,
// saying why, to a set that does not verify or breaks the mailbox key rule
// (sealed-sender SSR-W9).
type KeysPublishRequest struct {
	Keys    string `json:"keys"`
	PrevSeq uint64 `json:"prev_seq"`
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
)

// KeysRollbackReason is the reason of the 409 a hub answers a key set
// publication (POST /agents/{aid}/keys, or /register with enc_keys) that
// does not follow the set it holds; the body names that set's seq,
// stored_seq (forward-secrecy FS-H4).
const KeysRollbackReason = "keys_rollback"

// RegisterRequest is the body of POST /register (core §2.12.2). It is
// signed with relayauth v2 action "register" in the headers; the hub
// verifies the signature against KEL (the registrant's own KEL, base64url
// without padding, core G2), which must extend any KEL the hub already holds
// for the AID (A2A-DESIGN §3.8).
//
// There is no invite member: a hub admits by AID, and answers a request
// that carries one with 400 invite_removed (LEC HN-5).
type RegisterRequest struct {
	AID  string   `json:"aid"`
	Name string   `json:"name,omitempty"`
	Caps []string `json:"caps,omitempty"`
	KEL  string   `json:"kel"`
	// EncKeys is the AID's current seal.SignedEncKeySet encoding, base64url
	// (B64, CORE-E5), published with every registration so a
	// re-registration after a restart also restates it. PrevSeq goes with
	// it: as in KeysPublishRequest, the seq of the last set signed before
	// it (forward-secrecy FS-K5).
	EncKeys string  `json:"enc_keys,omitempty"`
	PrevSeq *uint64 `json:"prev_seq,omitempty"`
	// A2ACard is the node's signed A2A network card (A2A-DESIGN §10.1),
	// the exact bytes a2acard.Sign returned. Absent from a node with no
	// public skill, which publishes no card; JSON null (WithdrawCard)
	// asks the hub to withdraw the card it holds (an internal note item).
	A2ACard json.RawMessage `json:"a2a_card,omitempty"`
	// PoW answers a pow_required challenge (LEC HN-3).
	PoW *RegisterPoW `json:"pow,omitempty"`
	// CheckOnly asks only whether the hub would take the AID: 200
	// {status: admitted|open} or 403 admission_required; nothing is
	// written (LEC HN-2).
	CheckOnly bool `json:"check_only,omitempty"`
}

// RegisterPoW is a proof of work: the challenge's seed and the nonce found
// for it, both base64url without padding (hub-payment-privacy §2.2).
type RegisterPoW struct {
	Seed  string `json:"seed"`
	Nonce string `json:"nonce"`
}

// RegisterResponse is the answer to POST /register. KeysStatus and
// CardStatus report the enc_keys and a2a_card fields separately: a key set
// the hub refuses does not fail the registration (A2A-DESIGN §3.1).
// ProbationUntil (unix ms) and Trial are there only for an account that came
// in through open registration or a sign-up window.
type RegisterResponse struct {
	AID            string  `json:"aid"`
	Status         string  `json:"status"`
	KeysStatus     string  `json:"keys_status"`
	KeysError      string  `json:"keys_error,omitempty"`
	CardStatus     string  `json:"card_status"`
	CardError      string  `json:"card_error,omitempty"`
	ProbationUntil *uint64 `json:"probation_until,omitempty"`
	Trial          string  `json:"trial,omitempty"`
	// Revocations is how many unexpired revocations of this agent's
	// delivery credentials the hub holds, the total of POST /mailbox/revoke
	// (core §2.12.2). A daemon that pushed more than that pushes them again
	// (sealed-sender SSR-D21): the hub was restored from a backup or lost
	// them. Nil from a hub that does not say.
	Revocations *uint64 `json:"revocations,omitempty"`
	// KeyStateNotice is the witness notice the hub signed for the caller's
	// KEL tip (base64url of the NoticeWire bytes), or KeyStateNoticeError
	// why it signed none: quarantine, not_admitted, not_witness (KEL H-13).
	// Every registration answer carries one of them, the card's and the
	// profile's second registrations included (KEL D-50).
	KeyStateNotice      string `json:"key_state_notice,omitempty"`
	KeyStateNoticeError string `json:"key_state_notice_error,omitempty"`
}

// MailboxRevokePath is POST /mailbox/revoke (core §2.12.2), signed with
// relayauth action revoke: the owner withdraws delivery credentials it
// issued (sealed-sender SSR-D20).
const MailboxRevokePath = "/mailbox/revoke"

// MailboxRevokeMaxIDs is the most capability ids one POST /mailbox/revoke
// carries; a daemon with more sends them in batches (SS §2.12).
const MailboxRevokeMaxIDs = 1000

// MailboxRevokeRequest is the body of POST /mailbox/revoke: 1 to
// MailboxRevokeMaxIDs capability ids (CapBody key 3), each B64 of 16
// bytes. An id given again is idempotent.
type MailboxRevokeRequest struct {
	CapIDs []string `json:"cap_ids"`
}

// MailboxRevokeResponse is its answer: Revoked is how many of the ids the
// hub had not held before, Total how many unexpired revocations of the
// owner it holds now. A hub that holds as many as it keeps answers 429
// revocation_list_full instead, and keeps none of the batch.
type MailboxRevokeResponse struct {
	Revoked uint64 `json:"revoked"`
	Total   uint64 `json:"total"`
}

// CheckOnlyResponse is the /register answer to check_only: admitted (the AID
// is registered or on the admission list) or open (it may register after a
// proof of work).
type CheckOnlyResponse struct {
	Status string `json:"status"`
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
	// CardStatusWithdrawn answers a2a_card: null — the hub holds no card
	// for the agent now (an internal note item).
	CardStatusWithdrawn = "withdrawn"
	// CardStatusUnverified is a hub that stores the card before its
	// admission step exists: kept, nothing checked.
	CardStatusUnverified = "unverified"
)

// WithdrawCard is the a2a_card value that withdraws the card a hub holds
// (an internal note item): JSON null, which omitempty keeps (it is not empty). A hub
// without the withdrawal answers card_status "invalid".
const WithdrawCard = "null"

// HubIdentity is the answer to GET /hub/identity: the hub's AID and KEL
// (base64url, CORE-E5), the wire it speaks and the widest directory query
// it answers (core §2.12.2). The daemon needs the AID before its first
// signed call, because relayauth v2 binds every signature to the hub it is
// addressed to. Wire is a pointer so that an answer without it (not a
// wire-3 hub) reads as absent, not as 0.
type HubIdentity struct {
	AID        string `json:"aid"`
	KEL        string `json:"kel"`
	Wire       *int   `json:"wire,omitempty"`
	DirMaxBits *int   `json:"dir_max_bits,omitempty"`
}

// KELPushPath is POST /agents/kel:push (KEL §2.7, core §2.12.2): a key
// history pushed to a hub that holds this identity's (a former witness, or
// the current one after it lost events), without authentication.
const KELPushPath = "/agents/kel:push"

// KELPushRequest is the POST /agents/kel:push body: the AID and its whole
// key history (identity.MarshalKEL bytes, base64url).
type KELPushRequest struct {
	AID string `json:"aid"`
	KEL string `json:"kel"`
}

// KELPushResponse is the answer to an accepted push: the tip the hub holds
// now, and whether the push added nothing.
type KELPushResponse struct {
	AID       string `json:"aid"`
	Seq       uint64 `json:"seq"`
	Head      string `json:"head"`
	Unchanged bool   `json:"unchanged"`
}

// AgentKELResponse is GET /agents/{aid}/kel: the key history the hub holds
// for aid (base64url), and whether aid is registered there; false means the
// history is the tombstone it keeps for an agent that is gone (KEL D-27 row
// d).
type AgentKELResponse struct {
	AID        string `json:"aid"`
	KEL        string `json:"kel"`
	Registered bool   `json:"registered"`
}

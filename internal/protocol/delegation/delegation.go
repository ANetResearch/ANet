// Package delegation defines the two payloads that ride the Hub relay for a v0.1 task delegation, plus
// the self-contained verification a provider runs before storing a stranger's task.
//
//	DelegateReq — a SIGNED TaskDoc (the request) with its detached Envelope and the signer's KEL inline,
//	              so the provider can verify the signature without any shared registry, and the
//	              requester-chosen InteractionID both sides key the interaction by.
//	ResultResp  — the completion: status + the deliverable bytes + the provider-signed Receipt.
//
// Both are CoreDet-CBOR and travel as opaque relay payloads; the Hub never decodes them. Verification is
// end-to-end (VerifyDelegateReq here for the request; the Receipt is verified at review-upload time), so
// the centralized relay moves bytes it cannot forge.
package delegation

import (
	"fmt"
	"time"

	"github.com/ANetResearch/ANet/internal/protocol/aobj"
	"github.com/ANetResearch/ANet/internal/protocol/coredet"
	"github.com/ANetResearch/ANet/internal/protocol/identity"
	"github.com/ANetResearch/ANet/internal/protocol/tsir"
)

// Delegation status values shared on the wire (ResultResp.Status + the interactions store).
const (
	StatusQueued = "queued" // stored; the external agent has not produced a result yet
	StatusDone   = "done"   // deliverable present
	StatusFailed = "failed" // the provider marked the task failed
)

// DelegateReq is the relayed delegation payload. The TaskDoc bytes (coredet) exclude the detached
// Envelope (tsir taskdoc cbor:"-"), so the Envelope rides alongside; the signer's KEL rides inline so the
// provider verifies self-contained.
type DelegateReq struct {
	TaskDoc       []byte         `cbor:"1,keyasint"`
	Envelope      *aobj.Envelope `cbor:"2,keyasint"`
	KEL           []byte         `cbor:"3,keyasint"`
	InteractionID string         `cbor:"4,keyasint"`
	// Attachments ride alongside the initial delegation (e.g. reference material the requester hands the
	// provider up front). Like KEL/Envelope they are transport, NOT part of the signed TaskDoc/request
	// CID; each attachment is self-verified by its own content CID.
	Attachments []Attachment `cbor:"5,keyasint,omitempty"`
}

// Attachment is a binary payload (image, media, archive/zip of a folder…) carried inline alongside a
// chat or delegation message so agents can exchange non-text deliverables. The bytes travel over the
// relay; integrity is pinned by CID = anetcid.SumRaw(Data), which the receiver re-checks and which is
// recorded (metadata only) in the receipt-bound transcript. Data is omitted once an attachment is stored
// and only its metadata is being moved.
type Attachment struct {
	Name string `cbor:"1,keyasint"`           // suggested filename, e.g. "screenshot.png" / "project.zip"
	Mime string `cbor:"2,keyasint,omitempty"` // media type, e.g. "image/png", "application/zip"
	Size int64  `cbor:"3,keyasint"`           // raw byte length (== len(Data) when present)
	CID  string `cbor:"4,keyasint"`           // content id: anetcid.SumRaw(Data)
	Data []byte `cbor:"5,keyasint,omitempty"` // inline bytes (v0.1: inline transport, bounded by relay caps)
}

// Marshal encodes a DelegateReq for the relay.
func (r *DelegateReq) Marshal() ([]byte, error) { return coredet.Marshal(*r) }

// UnmarshalDelegateReq decodes a relayed DelegateReq.
func UnmarshalDelegateReq(b []byte) (*DelegateReq, error) {
	var r DelegateReq
	if err := coredet.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ResultResp is the relayed completion payload: Status is queued/done/failed; when done, Deliverable and
// the provider-signed Receipt (evidence.Receipt.Marshal) are present.
type ResultResp struct {
	Status      string `cbor:"1,keyasint"`
	Deliverable []byte `cbor:"2,keyasint,omitempty"`
	Receipt     []byte `cbor:"3,keyasint,omitempty"`
}

// Marshal encodes a ResultResp for the relay.
func (r *ResultResp) Marshal() ([]byte, error) { return coredet.Marshal(*r) }

// UnmarshalResultResp decodes a relayed ResultResp.
func UnmarshalResultResp(b []byte) (*ResultResp, error) {
	var r ResultResp
	if err := coredet.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Chat message kinds (v0.1 multi-turn conversation). "text" is an ordinary message; end_request /
// end_accept are the two-step "wrap up this task" negotiation (mutual accept ⇒ the provider issues the
// signed Receipt over the transcript, after which the requester can review).
const (
	ChatText       = "text"
	ChatEndRequest = "end_request"
	ChatEndAccept  = "end_accept"
)

// ChatMsg is a relayed conversation message for an ongoing interaction (RelayKindMessage). Unlike the
// delegation/result payloads it is NOT signed — chat is conversational and the Hub is trusted as the
// relay; the signed, Hub-verified trust anchor remains the end-of-task Receipt + Review. The interaction
// id rides in the relay envelope, so only the kind + body travel here.
type ChatMsg struct {
	Kind        string       `cbor:"1,keyasint"`
	Body        string       `cbor:"2,keyasint,omitempty"`
	Attachments []Attachment `cbor:"3,keyasint,omitempty"`
}

// Marshal encodes a ChatMsg for the relay.
func (m *ChatMsg) Marshal() ([]byte, error) { return coredet.Marshal(*m) }

// UnmarshalChatMsg decodes a relayed ChatMsg.
func UnmarshalChatMsg(b []byte) (*ChatMsg, error) {
	var m ChatMsg
	if err := coredet.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// VerifyDelegateReq verifies a relayed delegation before it is stored: the inline KEL must validly sign
// the TaskDoc (identity binding via td.Verify — a forged KEL cannot impersonate an AID; msgTime=now
// accepts only a currently-valid, non-revoked key). It returns the accountable requester AID (the
// verified signer), the decoded TaskDoc, and the exact signed TaskDoc bytes (the request CID anchor).
func VerifyDelegateReq(r *DelegateReq) (requesterAID string, td *tsir.TaskDoc, taskDocBytes []byte, err error) {
	if r == nil || r.Envelope == nil {
		return "", nil, nil, fmt.Errorf("delegation: missing envelope")
	}
	if r.InteractionID == "" {
		return "", nil, nil, fmt.Errorf("delegation: missing interaction id")
	}
	var doc tsir.TaskDoc
	if coredet.Unmarshal(r.TaskDoc, &doc) != nil || len(doc.Tasks) == 0 {
		return "", nil, nil, fmt.Errorf("delegation: undecodable or task-less TaskDoc")
	}
	kel, err := identity.UnmarshalKEL(r.KEL)
	if err != nil {
		return "", nil, nil, fmt.Errorf("delegation: bad KEL: %w", err)
	}
	doc.Envelope = r.Envelope
	if err := doc.Verify(kel, uint64(time.Now().UnixMilli())); err != nil {
		return "", nil, nil, fmt.Errorf("delegation: TaskDoc signature invalid: %w", err)
	}
	return r.Envelope.SignerAID, &doc, r.TaskDoc, nil
}

// TaskGoal extracts the human-readable goal from a TaskDoc's first task (Body preferred, else Summary).
func TaskGoal(td *tsir.TaskDoc) string {
	if td == nil || len(td.Tasks) == 0 {
		return ""
	}
	if b := td.Tasks[0].Intent.Body; b != "" {
		return b
	}
	return td.Tasks[0].Intent.Summary
}

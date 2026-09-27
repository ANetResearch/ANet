package daemon

// taskview.go renders an interaction as an A2A Task (internal/a2ashape),
// following A2A-DESIGN §11.5 "Task 表示" [C21]:
//
//   - artifacts: a completed text task leads with the TextPart artifact
//     anet.reply (the provider's last message in the transcript the receipt
//     covers); a capability task leads with its deliverable as a DataPart.
//     Then anet.receipt (DataPart) and the reply's attachments (FilePart,
//     file name through safeName).
//   - status.message: the provider's latest message when it is newer than
//     the requester's latest turn (the question of an input-required task,
//     the progress of a working one, the reason of a failed one).
//   - history: the message table without control rows; the requester is
//     ROLE_USER and the provider ROLE_AGENT. An attachment is referenced by
//     a DataPart (anet.attachment) rather than repeated as bytes in every
//     read of the history; its bytes travel in the artifacts.
//   - metadata: anet.effect_status (capability tasks), anet.receipt_verified,
//     anet.request_cid, anet.result_cid, anet.peer_aid, anet.reason,
//     anet.cancel_requested, and anet.state_seq, the state sequence number
//     a caller passes back to /tasks/wait to wait for a newer state (C35).
//
// SI-6: "completed" is never merged with "the effect happened" or "the
// receipt checked out". A capability task in a terminal state always
// carries anet.effect_status and a completed text task always carries
// anet.receipt_verified; a value this node does not know is written as
// UNVERIFIED / unknown, not left out.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/transcript"
)

// Artifact ids. They are stable, so a client can tell an update of an
// artifact from a new one.
const (
	artifactReply   = "anet.reply"
	artifactResult  = "anet.result"
	artifactReceipt = "anet.receipt"
)

// viewOpts selects what a task view carries.
type viewOpts struct {
	// historyLen bounds the history to the newest n messages; nil is all
	// of it and 0 is none.
	historyLen *int
	// artifacts includes the artifacts (ListTasks leaves them out unless
	// asked, A2A-DESIGN §11.5).
	artifacts bool
}

// taskView renders ix as an A2A task.
func (d *Daemon) taskView(ix *interactions.Interaction, o viewOpts) (a2ashape.Task, error) {
	msgs, err := d.ix.Messages(ix.ID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	atts, err := d.ix.Attachments(ix.ID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	bySeq := map[int64][]*interactions.Attachment{}
	for i := range atts {
		bySeq[atts[i].MsgSeq] = append(bySeq[atts[i].MsgSeq], &atts[i])
	}
	requester := d.AID()
	if ix.Role == interactions.RoleInbound {
		requester = ix.PeerAID
	}
	v := &viewer{ix: ix, requester: requester, bySeq: bySeq}

	t := a2ashape.Task{ID: ix.ID, ContextID: ix.ContextID, Metadata: map[string]any{}}
	if t.ContextID == "" {
		// A row written before context ids existed. A2A requires one; the
		// task id is unique and stable.
		t.ContextID = ix.ID
	}
	state := a2ashape.StateFromANet(string(ix.State))
	ts := stateTime(ix)
	t.Status = a2ashape.TaskStatus{State: state, Timestamp: &ts}

	// History, and the latest turn of each side.
	var history []a2ashape.Message
	var lastRequester int64
	var lastProvider *interactions.Message
	canceledByMe := false
	for i := range msgs {
		m := &msgs[i]
		fromRequester := m.SenderAID == requester
		switch m.Kind {
		case interactions.MsgText, interactions.MsgPayment:
			history = append(history, v.message(m, t.ContextID))
			if fromRequester {
				lastRequester = m.Seq
			} else {
				lastProvider = m
			}
		case interactions.MsgStatus:
			if !fromRequester {
				lastProvider = m
			}
		case interactions.MsgCancel:
			if m.SenderAID == d.AID() {
				canceledByMe = true
			}
		}
	}
	if o.historyLen != nil {
		n := *o.historyLen
		if n < 0 {
			n = 0
		}
		if len(history) > n {
			history = history[len(history)-n:]
		}
	}
	if len(history) > 0 {
		t.History = history
	}

	// The deliverable, read once for status, artifacts and metadata.
	var capRes *capabilityResult
	if ix.IsCapability && len(ix.Result) > 0 {
		var r capabilityResult
		if json.Unmarshal(ix.Result, &r) == nil && r.Status != "" {
			capRes = &r
		}
	}

	// status.message
	switch {
	case state == a2ashape.StateCompleted:
		// The answer is the anet.reply / anet.result artifact.
	case lastProvider != nil && lastProvider.Seq > lastRequester:
		sm := v.message(lastProvider, t.ContextID)
		t.Status.Message = &sm
	case (state == a2ashape.StateFailed || state == a2ashape.StateRejected) && ix.Role == interactions.RoleOutbound:
		if why := failureText(ix, capRes); why != "" {
			t.Status.Message = &a2ashape.Message{MessageID: ix.ID + "-status", ContextID: t.ContextID, TaskID: ix.ID,
				Role: a2ashape.RoleAgent, Parts: []a2ashape.Part{a2ashape.TextPart(why)}}
		}
	}

	if o.artifacts {
		arts, err := d.taskArtifacts(ix, capRes)
		if err != nil {
			return a2ashape.Task{}, err
		}
		if len(arts) > 0 {
			t.Artifacts = arts
		}
	}

	// metadata
	md := t.Metadata
	md["anet.peer_aid"] = ix.PeerAID
	md["anet.role"] = string(ix.Role)
	md["anet.state_seq"] = ix.StateSeq
	if ix.RequestCID != "" {
		md["anet.request_cid"] = ix.RequestCID
	}
	if ix.ResultCID != "" {
		md["anet.result_cid"] = ix.ResultCID
	}
	if ix.Trust != "" {
		md["anet.trust"] = ix.Trust
	}
	if ix.IsCapability {
		if capID := capabilityOf(ix); capID != "" {
			md["anet.skill"] = capID
		}
		switch {
		case capRes != nil:
			md["anet.effect_status"] = capRes.Status
		case state == a2ashape.StateRejected:
			// Refused before it ran (policy, quota, pending expiry): no
			// effect.
			md["anet.effect_status"] = string(effect.Unavailable)
		case state.Terminal():
			// Ended without a result — canceled, failed without an answer,
			// never delivered. Whether the provider had started is not
			// known here, and "not known" is not "did not happen".
			md["anet.effect_status"] = string(effect.Unverified)
		}
		if capRes != nil && capRes.Message != "" && (state == a2ashape.StateFailed || state == a2ashape.StateRejected) {
			md["anet.reason"] = capRes.Message
		}
	}
	if len(ix.Receipt) > 0 || (!ix.IsCapability && state == a2ashape.StateCompleted) {
		md["anet.receipt_verified"] = receiptVerified(ix.ReceiptVerified)
	}
	if canceledByMe && !ix.IsTerminal() && ix.Role == interactions.RoleOutbound {
		// §4.2: a cancel after a submitted payment leaves the task open
		// until the provider answers.
		md["anet.cancel_requested"] = true
	}
	return t, nil
}

// viewer renders messages of one interaction.
type viewer struct {
	ix        *interactions.Interaction
	requester string
	bySeq     map[int64][]*interactions.Attachment
}

func (v *viewer) message(m *interactions.Message, contextID string) a2ashape.Message {
	out := a2ashape.Message{ContextID: contextID, TaskID: v.ix.ID, Role: a2ashape.RoleAgent, Parts: []a2ashape.Part{}}
	if m.SenderAID == v.requester {
		out.Role = a2ashape.RoleUser
	}
	meta := decodeMeta([]byte(m.Metadata))
	// The client's own message id, when it gave one, is the id it knows
	// the message by; otherwise the sender-minted id.
	switch cid, _ := meta[interactions.ClientMessageIDKey].(string); {
	case cid != "":
		out.MessageID = cid
	case m.MsgID != "":
		out.MessageID = m.MsgID
	default:
		out.MessageID = fmt.Sprintf("%s-m%d", v.ix.ID, m.Seq)
	}
	if len(meta) > 0 {
		out.Metadata = meta
	}
	if m.Body != "" {
		out.Parts = append(out.Parts, a2ashape.TextPart(m.Body))
	}
	for _, a := range v.bySeq[m.Seq] {
		p := a2ashape.DataPart(map[string]any{"name": safeName(a.Name), "mediaType": a.Mime, "size": a.Size, "cid": a.CID})
		p.Metadata = map[string]any{"anet.attachment": true}
		out.Parts = append(out.Parts, p)
	}
	return out
}

// taskArtifacts builds the artifacts of a task that carries a result.
func (d *Daemon) taskArtifacts(ix *interactions.Interaction, capRes *capabilityResult) ([]a2ashape.Artifact, error) {
	if len(ix.Result) == 0 && len(ix.Receipt) == 0 {
		return nil, nil
	}
	var out []a2ashape.Artifact
	var replyAtts []transcript.Attachment
	switch {
	case ix.IsCapability && len(ix.Result) > 0:
		p := a2ashape.Part{Data: json.RawMessage(ix.Result), MediaType: "application/json"}
		if capRes == nil && !json.Valid(ix.Result) {
			p = a2ashape.TextPart(printable(ix.Result))
		}
		out = append(out, a2ashape.Artifact{ArtifactID: artifactResult, Name: artifactResult, Parts: []a2ashape.Part{p}})
	case !ix.IsCapability && len(ix.Receipt) > 0:
		// The receipt covers the transcript; the reply is its last provider
		// line. A result without a receipt is a failure detail, not an
		// answer, and is reported through the status instead.
		if tr, err := transcript.Parse(ix.Result); err == nil {
			for i := len(tr.Messages) - 1; i >= 0; i-- {
				if tr.Messages[i].From == "provider" {
					out = append(out, a2ashape.Artifact{ArtifactID: artifactReply, Name: artifactReply,
						Parts: []a2ashape.Part{a2ashape.TextPart(tr.Messages[i].Body)}})
					replyAtts = tr.Messages[i].Attachments
					break
				}
			}
		}
	}
	if len(ix.Receipt) > 0 {
		out = append(out, a2ashape.Artifact{ArtifactID: artifactReceipt, Name: artifactReceipt,
			Parts: []a2ashape.Part{a2ashape.DataPart(map[string]any{
				"receipt":          base64.StdEncoding.EncodeToString(ix.Receipt),
				"request_cid":      ix.RequestCID,
				"result_cid":       ix.ResultCID,
				"receipt_verified": receiptVerified(ix.ReceiptVerified),
			})}})
	}
	for _, a := range replyAtts {
		att, err := d.ix.AttachmentData(ix.ID, a.CID)
		if err != nil {
			// The transcript names an attachment this node does not hold
			// (its bytes were refused on arrival); the fingerprint is all
			// there is.
			continue
		}
		out = append(out, a2ashape.Artifact{ArtifactID: "anet.attachment." + a.CID, Name: safeName(att.Name),
			Parts: []a2ashape.Part{a2ashape.FilePart(safeName(att.Name), att.Mime, att.Data)}})
	}
	return out, nil
}

// failureText is the reason a failed or rejected outbound task gives.
func failureText(ix *interactions.Interaction, capRes *capabilityResult) string {
	if capRes != nil {
		if capRes.Message != "" {
			return capRes.Message
		}
		return "effect " + capRes.Status
	}
	if len(ix.Result) > 0 && len(ix.Receipt) == 0 {
		return printable(ix.Result)
	}
	return ""
}

// printable is b as text when it is short UTF-8, else a description.
func printable(b []byte) string {
	if utf8.Valid(b) && len(b) <= 4096 {
		return strings.TrimSpace(string(b))
	}
	return fmt.Sprintf("(%d bytes of detail)", len(b))
}

// receiptVerified is the three-state receipt_verified value (SI-6).
func receiptVerified(v interactions.Verification) string {
	if v == interactions.VerificationUnknown {
		return "unknown"
	}
	return string(v)
}

// capabilityOf is the capability id of a capability task, from its signed
// request.
func capabilityOf(ix *interactions.Interaction) string {
	if len(ix.RequestDoc) == 0 {
		if ix.Trust == interactions.TrustPublicCap {
			return ix.Goal
		}
		return strings.TrimPrefix(ix.Goal, "invoke capability ")
	}
	td, err := decodeTaskDoc(ix.RequestDoc)
	if err != nil {
		return ""
	}
	id, _, _ := capabilityCall(td)
	return id
}

// stateTime is when the interaction entered its current state.
func stateTime(ix *interactions.Interaction) time.Time {
	if ix.StateAt > 0 {
		return ix.StateTime()
	}
	if t, err := time.Parse(time.RFC3339Nano, ix.UpdatedAt); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

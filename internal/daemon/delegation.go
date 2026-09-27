package daemon

// delegation.go holds the task lifecycle over the local interactions store (A2A-DESIGN §4):
// accepting inbound tasks (ingestDelegate), the conversation both ways (SendMessage /
// ingestMessage), completion (the provider completes on its own and signs a receipt over the
// transcript; a requester's end request makes the provider daemon complete), cancellation,
// status updates, landing results (ingestResult), and the requester-signed review
// (SubmitReview). anet runs no model — the deliverable bytes come from the operator's EXTERNAL
// agent.
//
// Every state write goes through the store's guarded transition (interactions.SetState /
// Finish), which refuses to leave a terminal state, and is published on the event bus after it
// commits. The state is written by the event that causes it (§4.1):
//
//	requester creates the task                         submitted
//	requester sends text or payment-submitted          working
//	requester sends cancel or payment-rejected         canceled (unless a payment was submitted)
//	provider sends a message without anet.state=working  input-required
//	provider sends anet.state=working, or a StatusMsg  the state it carries
//	result                                             completed / failed / rejected (§4.3)

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/transcript"
)

// ErrTaskTerminal is returned when new input is sent to an interaction in a
// terminal state (A2A UnsupportedOperationError).
var ErrTaskTerminal = errors.New("anet: the task has ended and takes no new input")

// ErrNotCancelable is returned by CancelTask for an interaction already in a
// terminal state (A2A TaskNotCancelableError).
var ErrNotCancelable = errors.New("anet: the task has ended and cannot be canceled")

// InboxItem is one inbound (delegated-to-us) task shown to the operator.
type InboxItem struct {
	InteractionID string `json:"interaction_id"`
	Requester     string `json:"requester"`
	Goal          string `json:"goal"`
	State         string `json:"state"`
	// Status carries the same value as State for readers written before
	// the state model (A2A-DESIGN §4.1).
	Status     string `json:"status"`
	Trust      string `json:"trust,omitempty"`
	Capability bool   `json:"capability,omitempty"`
	ContextID  string `json:"context_id,omitempty"`
	CreatedAt  string `json:"created_at"`
	StateAt    int64  `json:"state_at"`
}

// ResultItem is one outbound delegation that carries a result.
type ResultItem struct {
	InteractionID string `json:"interaction_id"`
	Provider      string `json:"provider"`
	Goal          string `json:"goal"`
	State         string `json:"state"`
	Result        string `json:"result"`
	RequestCID    string `json:"request_cid"`
	ResultCID     string `json:"result_cid"`
	ReceiptCID    string `json:"receipt_cid"`
	Receipt       string `json:"receipt"`
	Reviewed      bool   `json:"reviewed"`
	// ProviderKEL is the key history the receipt was verified against,
	// base64. Handed out so a holder of this result can re-check it
	// themselves rather than taking this daemon's word for it — which is
	// the whole point of a signed receipt.
	ProviderKEL string `json:"provider_kel,omitempty"`
	// ReceiptVerified is "verified", "unverified", or "" for a result
	// stored before this node recorded the distinction. Three states
	// because two would merge "we checked and it holds" with "we had no
	// way to check", and those differ most exactly where a caller is
	// about to act on the result.
	ReceiptVerified string `json:"receipt_verified,omitempty"`
}

// ReviewResult is the outcome of signing a review.
type ReviewResult struct {
	InteractionID string
	Subject       string
	Rating        int
}

// ThreadMsg is one conversation entry rendered for the chat console. From is "me"/"them" (placement).
type ThreadMsg struct {
	From        string          `json:"from"`
	Kind        string          `json:"kind"`
	Body        string          `json:"body"`
	MsgID       string          `json:"msg_id,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Attachments []ThreadAtt     `json:"attachments,omitempty"`
	CreatedAt   string          `json:"created_at"`
}

// ThreadAtt is one attachment's metadata as rendered for the console/CLI (bytes fetched separately via
// the /attachment endpoint or `anet pull`).
type ThreadAtt struct {
	Name string `json:"name"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
	CID  string `json:"cid"`
}

// Thread is one interaction rendered as a chat conversation, from EITHER side. Role is our side —
// "inbound" (someone delegated to us; Peer is the requester) or "outbound" (we delegated; Peer is the
// provider).
type Thread struct {
	InteractionID string `json:"interaction_id"`
	Role          string `json:"role"`
	Peer          string `json:"peer"`
	Goal          string `json:"goal"`
	State         string `json:"state"`
	// Status carries the same value as State for readers written before
	// the state model.
	Status       string      `json:"status"`
	StateSeq     int64       `json:"state_seq"`
	Trust        string      `json:"trust,omitempty"`
	IsCapability bool        `json:"capability,omitempty"`
	ContextID    string      `json:"context_id,omitempty"`
	Messages     []ThreadMsg `json:"messages"`
	// EndReqBy is "me", "them" or "": who asked for the task to end.
	EndReqBy  string `json:"end_req_by"`
	Reviewed  bool   `json:"reviewed"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// threadsLimit bounds Threads: the newest this many interactions.
const threadsLimit = 1000

// Threads returns the newest interactions in both roles, most recent state change first, each with its
// conversation log, for the console's chat view.
func (d *Daemon) Threads() ([]Thread, error) {
	p, err := d.ix.ListPage(interactions.ListFilter{Limit: threadsLimit})
	if err != nil {
		return nil, err
	}
	return d.buildThreads(p.Items)
}

// ActiveThreads returns every non-terminal interaction the auto-reply loop may act on: capability
// calls and public_cap interactions are excluded (A2A-DESIGN §5.2, §6), and terminal ones are
// filtered in SQL before their messages are loaded, so the loop stays O(active).
func (d *Daemon) ActiveThreads() ([]Thread, error) {
	list, err := d.ix.ListAll(interactions.ListFilter{Active: true, ExcludeCapability: true,
		ExcludeTrust: []string{interactions.TrustPublicCap}})
	if err != nil {
		return nil, err
	}
	return d.buildThreads(list)
}

// Thread returns one interaction rendered as a thread.
func (d *Daemon) Thread(interactionID string) (Thread, error) {
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		return Thread{}, err
	}
	ts, err := d.buildThreads([]*interactions.Interaction{ix})
	if err != nil {
		return Thread{}, err
	}
	return ts[0], nil
}

func (d *Daemon) buildThreads(list []*interactions.Interaction) ([]Thread, error) {
	me := d.AID()
	side := func(aid string) string {
		switch aid {
		case "":
			return ""
		case me:
			return "me"
		default:
			return "them"
		}
	}
	out := make([]Thread, 0, len(list))
	for _, ix := range list {
		msgs, err := d.ix.Messages(ix.ID)
		if err != nil {
			return nil, err
		}
		atts, err := d.ix.Attachments(ix.ID)
		if err != nil {
			return nil, err
		}
		bySeq := map[int64][]ThreadAtt{}
		for _, a := range atts {
			bySeq[a.MsgSeq] = append(bySeq[a.MsgSeq], ThreadAtt{Name: a.Name, Mime: a.Mime, Size: a.Size, CID: a.CID})
		}
		tmsgs := make([]ThreadMsg, 0, len(msgs))
		for _, m := range msgs {
			tm := ThreadMsg{From: side(m.SenderAID), Kind: m.Kind, Body: m.Body, MsgID: m.MsgID,
				Attachments: bySeq[m.Seq], CreatedAt: m.CreatedAt}
			if m.Metadata != "" && json.Valid([]byte(m.Metadata)) {
				tm.Metadata = json.RawMessage(m.Metadata)
			}
			tmsgs = append(tmsgs, tm)
		}
		out = append(out, Thread{
			InteractionID: ix.ID, Role: string(ix.Role), Peer: ix.PeerAID, Goal: ix.Goal,
			State: string(ix.State), Status: string(ix.State), StateSeq: ix.StateSeq,
			Trust: ix.Trust, IsCapability: ix.IsCapability, ContextID: ix.ContextID, Messages: tmsgs,
			EndReqBy: side(ix.EndReqBy), Reviewed: len(ix.Review) > 0,
			CreatedAt: ix.CreatedAt, UpdatedAt: ix.UpdatedAt,
		})
	}
	return out, nil
}

// newInteractionID mints a random shared interaction id (the requester generates it at delegate time).
func newInteractionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ix_" + hex.EncodeToString(b[:]), nil
}

// newContextID mints an A2A context id for a task whose client gave none.
func newContextID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ctx_" + hex.EncodeToString(b[:]), nil
}

// NonceContextKey is the TaskDoc context that carries the task nonce
// (A2A-DESIGN §2 X4). It is inside the TaskDoc's signed canonical preimage,
// so the request CID covers 16 random bytes and cannot be recomputed from a
// guess of the goal.
const NonceContextKey = "anet.nonce"

// newTaskNonce mints the 16-byte task nonce, base64url without padding.
func newTaskNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// nonceContext is the TaskDoc context entry for a nonce. It is private: it
// is for the two parties and for no one they later show a receipt to.
func nonceContext(nonce string) tsir.Context {
	return tsir.Context{Key: NonceContextKey, Value: nonce, Visibility: "private"}
}

// taskNonce reads the nonce a TaskDoc carries, or "".
func taskNonce(td *tsir.TaskDoc) string {
	if td == nil || len(td.Tasks) == 0 {
		return ""
	}
	for _, c := range td.Tasks[0].Contexts {
		if c.Key == NonceContextKey {
			return c.Value
		}
	}
	return ""
}

// Inbox lists inbound tasks, most recent state change first; pending=true keeps only the ones still
// open.
func (d *Daemon) Inbox(pending bool) ([]InboxItem, error) {
	list, err := d.ix.ListAll(interactions.ListFilter{Role: interactions.RoleInbound, Active: pending})
	if err != nil {
		return nil, err
	}
	out := make([]InboxItem, 0, len(list))
	for _, ix := range list {
		out = append(out, InboxItem{
			InteractionID: ix.ID, Requester: ix.PeerAID, Goal: ix.Goal,
			State: string(ix.State), Status: string(ix.State), Trust: ix.Trust,
			Capability: ix.IsCapability, ContextID: ix.ContextID,
			CreatedAt: ix.CreatedAt, StateAt: ix.StateAt,
		})
	}
	return out, nil
}

// SendMessage appends a chat message to an open interaction (from either side) and relays it to the
// peer. attachPaths are local files the daemon reads, pins, stores, and relays inline; a message may
// carry attachments with an empty body.
func (d *Daemon) SendMessage(ctx context.Context, interactionID, body string, attachPaths []string) error {
	atts, err := attachmentsFromPaths(attachPaths)
	if err != nil {
		return err
	}
	return d.SendMessageAtts(ctx, interactionID, body, atts)
}

// SendMessageAtts is SendMessage with attachments already assembled (from CLI file paths OR web uploads).
func (d *Daemon) SendMessageAtts(ctx context.Context, interactionID, body string, atts []delegation.Attachment) error {
	return d.SendMessageOpts(ctx, interactionID, body, atts, nil)
}

// SendMessageOpts is SendMessageAtts with message metadata (A2A Message.metadata). A provider that
// sends anet.state=working reports progress and leaves the task working; any other provider message
// makes it input-required. A requester message makes it working.
func (d *Daemon) SendMessageOpts(ctx context.Context, interactionID, body string, atts []delegation.Attachment,
	meta map[string]any) error {
	body = strings.TrimSpace(body)
	if body == "" && len(atts) == 0 {
		return fmt.Errorf("anet: empty message (pass text and/or --attach PATH)")
	}
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		return err
	}
	if ix.IsTerminal() {
		return fmt.Errorf("%w (%s is %s)", ErrTaskTerminal, interactionID, ix.State)
	}
	if ix.IsCapability {
		return fmt.Errorf("anet: %s is a capability call; it carries no conversation", interactionID)
	}
	var metaBytes []byte
	if len(meta) > 0 {
		if metaBytes, err = json.Marshal(meta); err != nil {
			return err
		}
	}
	msgID, err := newMessageID()
	if err != nil {
		return err
	}
	next := stateOnMessage(ix.Role == interactions.RoleOutbound, interactions.MsgText, metaBytes, ix.PayState)
	var seq int64
	err = d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		if seq, _, err = tx.AddMessageRecord(interactions.MessageRecord{InteractionID: interactionID,
			SenderAID: d.AID(), Kind: interactions.MsgText, Body: body, MsgID: msgID, Metadata: metaBytes}); err != nil {
			return err
		}
		if next != "" {
			_, err = tx.SetState(interactionID, next)
		}
		return err
	})
	if err != nil {
		return err
	}
	if err := d.storeMsgAttachments(interactionID, seq, atts); err != nil {
		return err
	}
	d.publishMessage(interactionID, seq, interactions.MsgText)
	d.publishState(interactionID)
	cm := &delegation.ChatMsg{Kind: delegation.ChatText, Body: body, Attachments: atts, MsgID: msgID, Metadata: metaBytes}
	payload, err := cm.Marshal()
	if err != nil {
		return err
	}
	return d.relaySend(ctx, ix.PeerAID, seal.TypeMessage, interactionID, payload)
}

// stateOnMessage is the state a message moves its task to (§4.1), or "" for
// no change. fromRequester says who sent it. A payment-rejected after a
// submitted payment does not cancel: the provider decides (§4.2).
func stateOnMessage(fromRequester bool, kind string, meta []byte, payState string) interactions.State {
	m := decodeMeta(meta)
	if fromRequester {
		switch kind {
		case interactions.MsgText:
			return interactions.StateWorking
		case interactions.MsgPayment:
			switch m["x402.payment.status"] {
			case "payment-submitted":
				return interactions.StateWorking
			case "payment-rejected":
				if payState != interactions.PaySubmitted {
					return interactions.StateCanceled
				}
			}
		}
		return ""
	}
	if kind == interactions.MsgText || kind == interactions.MsgPayment {
		if m["anet.state"] == string(interactions.StateWorking) {
			return interactions.StateWorking
		}
		return interactions.StateInputRequired
	}
	return ""
}

// decodeMeta reads a metadata object; anything else is an empty map.
func decodeMeta(b []byte) map[string]any {
	out := map[string]any{}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// hasPaymentStatus reports whether metadata carries x402.payment.status.
func hasPaymentStatus(meta []byte) bool {
	_, ok := decodeMeta(meta)["x402.payment.status"]
	return ok
}

// hasControlMeta reports whether metadata carries a key that makes a message a control message
// rather than a conversation turn (A2A-DESIGN §4.1 [C29]).
func hasControlMeta(meta string) bool {
	if meta == "" {
		return false
	}
	// One rule for the transcript a receipt covers and for the A2A history.
	return a2ashape.ControlMetadata(decodeMeta([]byte(meta)))
}

// RequestEnd ends a task from this side. For the provider it completes the task: the provider
// signs the receipt over the transcript and delivers it (CompleteTask). For the requester it asks
// the provider to complete (end_request); the provider daemon completes on receipt, without its
// agent.
func (d *Daemon) RequestEnd(ctx context.Context, interactionID string) error {
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		return err
	}
	if ix.IsTerminal() {
		return fmt.Errorf("%w (%s is %s)", ErrTaskTerminal, interactionID, ix.State)
	}
	if ix.Role == interactions.RoleInbound {
		if ix.IsCapability {
			return fmt.Errorf("anet: %s is a capability call; it completes when the capability answers", interactionID)
		}
		return d.CompleteTask(ctx, interactionID)
	}
	if ix.EndReqBy == d.AID() {
		return fmt.Errorf("anet: you already asked the provider to complete %s", interactionID)
	}
	msgID, err := newMessageID()
	if err != nil {
		return err
	}
	var seq int64
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		if seq, _, err = tx.AddMessageRecord(interactions.MessageRecord{InteractionID: interactionID,
			SenderAID: d.AID(), Kind: interactions.MsgEndRequest, MsgID: msgID}); err != nil {
			return err
		}
		return tx.SetEndRequested(interactionID, d.AID())
	}); err != nil {
		return err
	}
	d.publishMessage(interactionID, seq, interactions.MsgEndRequest)
	payload, err := (&delegation.ChatMsg{Kind: delegation.ChatEndRequest, MsgID: msgID}).Marshal()
	if err != nil {
		return err
	}
	return d.relaySend(ctx, ix.PeerAID, seal.TypeMessage, interactionID, payload)
}

// newMessageID mints the identity a receiver dedupes on. The sender is the
// only party present on every delivery path, so the sender mints it.
func newMessageID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "msg_" + hex.EncodeToString(b[:]), nil
}

// buildTranscript renders the interaction's conversation turns (text messages without control
// metadata, and their attachment fingerprints) as a v2 transcript — the provider's authoritative
// record. Its CID becomes the receipt's ResultCID. The nonce is the task's, or a fresh one for a
// task that carried none, so the result CID is never a function of the conversation alone.
func (d *Daemon) buildTranscript(ix *interactions.Interaction) ([]byte, error) {
	msgs, err := d.ix.Messages(ix.ID)
	if err != nil {
		return nil, err
	}
	atts, err := d.ix.Attachments(ix.ID)
	if err != nil {
		return nil, err
	}
	bySeq := map[int64][]transcript.Attachment{}
	for _, a := range atts {
		bySeq[a.MsgSeq] = append(bySeq[a.MsgSeq], transcript.Attachment{Name: a.Name, Mime: a.Mime, Size: a.Size, CID: a.CID})
	}
	provider := d.AID()
	if ix.Role == interactions.RoleOutbound {
		provider = ix.PeerAID
	}
	out := make([]transcript.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Kind != interactions.MsgText || hasControlMeta(m.Metadata) {
			continue
		}
		mAtts := bySeq[m.Seq]
		if m.Body == "" && len(mAtts) == 0 {
			continue
		}
		from := "requester"
		if m.SenderAID == provider {
			from = "provider"
		}
		out = append(out, transcript.Message{From: from, Body: m.Body, Attachments: mAtts})
	}
	nonce := ix.TaskNonce
	if nonce == "" {
		if nonce, err = newTaskNonce(); err != nil {
			return nil, err
		}
	}
	return transcript.EncodeV2(nonce, out)
}

// CompleteTask completes an inbound text task (A2A-DESIGN §4.2): the provider signs a receipt over
// the v2 transcript, stores it with state completed, and queues the result for delivery. The result
// write is guarded: if the task reached a terminal state first (a cancel), nothing is stored or
// sent, so no receipt exists for a canceled task.
func (d *Daemon) CompleteTask(ctx context.Context, interactionID string) error {
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		return err
	}
	if ix.Role != interactions.RoleInbound {
		return fmt.Errorf("anet: only the provider completes a task; ask it with `anet end`")
	}
	if ix.IsCapability {
		return fmt.Errorf("anet: %s is a capability call; it completes when the capability answers", interactionID)
	}
	if ix.IsTerminal() {
		return fmt.Errorf("%w (%s is %s)", ErrTaskTerminal, interactionID, ix.State)
	}
	tr, err := d.buildTranscript(ix)
	if err != nil {
		return err
	}
	resultCID, err := anetcid.Sum(tr)
	if err != nil {
		return err
	}
	rc := &evidence.Receipt{
		InteractionID: ix.ID,
		RequesterAID:  ix.PeerAID,
		ProviderAID:   d.AID(),
		RequestCID:    ix.RequestCID,
		ResultCID:     resultCID,
		CompletedAt:   uint64(nowMillis()),
	}
	if err := rc.Sign(d.self); err != nil {
		return err
	}
	receiptBytes, err := rc.Marshal()
	if err != nil {
		return err
	}
	selfKEL, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		return fmt.Errorf("anet: marshal KEL: %w", err)
	}
	meta, _ := json.Marshal(map[string]any{"anet.state": string(interactions.StateCompleted)})
	payload, err := (&delegation.ResultResp{Status: delegation.StatusDone, Deliverable: tr,
		Receipt: receiptBytes, KEL: selfKEL, Metadata: meta}).Marshal()
	if err != nil {
		return err
	}
	id, err := d.queueSend(ctx, ix.PeerAID, seal.TypeResult, ix.ID, payload, func(tx *interactions.Tx) error {
		// Our own signature over our own transcript.
		return tx.Finish(ix.ID, interactions.Finish{State: interactions.StateCompleted, Result: tr,
			ResultCID: resultCID, Receipt: receiptBytes, Verified: interactions.VerificationVerified})
	})
	if errors.Is(err, interactions.ErrTerminal) {
		return fmt.Errorf("%w (%s ended before it could be completed)", ErrTaskTerminal, interactionID)
	}
	if err != nil {
		return err
	}
	if _, lerr := d.ledger.Append(EvReceipt, map[string]any{
		"interaction_id": ix.ID, "requester_aid": ix.PeerAID, "result_cid": resultCID,
	}); lerr != nil {
		log.Printf("anet: receipt evidence ledger: %v", lerr)
	}
	d.publishResult(ix.ID)
	if err := d.deliverQueued(ctx, id); err != nil {
		log.Printf("anet: %s: completed; the result is queued for delivery (%v)", ix.ID, err)
	}
	return nil
}

// CancelTask cancels a task from this side (A2A CancelTask, A2A-DESIGN §4.2).
//
//   - Requester, no payment submitted: the task is canceled locally and the provider is sent a
//     cancel through the retry queue.
//   - Requester, a payment submitted: the local state does not change; the cancel is sent and the
//     provider's status or result decides (it does not cancel paid work). The returned interaction
//     is still open; its conversation log carries the cancel.
//   - Provider: the task is canceled, a running capability call is asked to stop, and the
//     requester is sent status{canceled} through the retry queue.
func (d *Daemon) CancelTask(ctx context.Context, interactionID string) (*interactions.Interaction, error) {
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		return nil, err
	}
	if ix.IsTerminal() {
		return ix, ErrNotCancelable
	}
	msgID, err := newMessageID()
	if err != nil {
		return nil, err
	}
	var typ string
	var payload []byte
	cancelState := true
	if ix.Role == interactions.RoleOutbound {
		typ = seal.TypeMessage
		payload, err = (&delegation.ChatMsg{Kind: delegation.KindCancel, MsgID: msgID}).Marshal()
		cancelState = ix.PayState != interactions.PaySubmitted
	} else {
		typ = seal.TypeStatus
		payload, err = (&delegation.StatusMsg{State: delegation.StateCanceled,
			Text: "the provider canceled the task", At: d.nowMS()}).Marshal()
	}
	if err != nil {
		return nil, err
	}
	var seq int64
	id, err := d.queueSend(ctx, ix.PeerAID, typ, ix.ID, payload, func(tx *interactions.Tx) error {
		var err error
		if seq, _, err = tx.AddMessageRecord(interactions.MessageRecord{InteractionID: ix.ID,
			SenderAID: d.AID(), Kind: interactions.MsgCancel, MsgID: msgID}); err != nil {
			return err
		}
		if cancelState {
			changed, err := tx.SetState(ix.ID, interactions.StateCanceled)
			if err != nil {
				return err
			}
			if !changed {
				return ErrNotCancelable // reached a terminal state since it was read
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotCancelable) {
			cur, _ := d.ix.Get(interactionID)
			return cur, ErrNotCancelable
		}
		return nil, err
	}
	d.stopRunning(ix.ID)
	d.publishMessage(ix.ID, seq, interactions.MsgCancel)
	d.publishState(ix.ID)
	if err := d.deliverQueued(ctx, id); err != nil {
		log.Printf("anet: %s: canceled; the notice to %s is queued for delivery (%v)", ix.ID, ix.PeerAID, err)
	}
	return d.ix.Get(interactionID)
}

// SendStatus sends a provider status update (anet.status/1) for an inbound task and moves the task
// to the state it carries. It goes through the retry queue. A terminal state is written with the
// same guard as every other state write.
func (d *Daemon) SendStatus(ctx context.Context, interactionID string, state interactions.State, text string,
	meta map[string]any) error {
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		return err
	}
	if ix.Role != interactions.RoleInbound {
		return fmt.Errorf("anet: only the provider sends task status")
	}
	if !delegation.ValidStatusState(string(state)) {
		return fmt.Errorf("anet: %q is not a status state", state)
	}
	if ix.IsTerminal() {
		return fmt.Errorf("%w (%s is %s)", ErrTaskTerminal, interactionID, ix.State)
	}
	var mb []byte
	if len(meta) > 0 {
		if mb, err = json.Marshal(meta); err != nil {
			return err
		}
	}
	msgID, err := newMessageID()
	if err != nil {
		return err
	}
	payload, err := (&delegation.StatusMsg{State: string(state), Text: text, Metadata: mb, At: d.nowMS()}).Marshal()
	if err != nil {
		return err
	}
	var seq int64
	id, err := d.queueSend(ctx, ix.PeerAID, seal.TypeStatus, ix.ID, payload, func(tx *interactions.Tx) error {
		var err error
		if seq, _, err = tx.AddMessageRecord(interactions.MessageRecord{InteractionID: ix.ID,
			SenderAID: d.AID(), Kind: interactions.MsgStatus, Body: text, MsgID: msgID, Metadata: mb}); err != nil {
			return err
		}
		changed, err := tx.SetState(ix.ID, state)
		if err != nil {
			return err
		}
		if !changed {
			cur, gerr := tx.Get(ix.ID)
			if gerr == nil && cur.IsTerminal() {
				return ErrTaskTerminal
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if state.IsTerminal() {
		d.stopRunning(ix.ID)
	}
	d.publishMessage(ix.ID, seq, interactions.MsgStatus)
	d.publishState(ix.ID)
	if err := d.deliverQueued(ctx, id); err != nil {
		log.Printf("anet: %s: status %s queued for delivery (%v)", ix.ID, state, err)
	}
	return nil
}

// SubmitReview signs a rating of the provider for a completed outbound interaction, anchored to the
// provider's receipt, and stores it locally (upload happens in the control handler).
func (d *Daemon) SubmitReview(interactionID string, rating int, comment string) (ReviewResult, error) {
	var zero ReviewResult
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		return zero, err
	}
	if ix.Role != interactions.RoleOutbound {
		return zero, fmt.Errorf("anet: %s is not an outbound delegation", interactionID)
	}
	if len(ix.Receipt) == 0 {
		return zero, fmt.Errorf("anet: no receipt for %s yet (run `results` first)", interactionID)
	}
	rc, err := evidence.UnmarshalReceipt(ix.Receipt)
	if err != nil {
		return zero, fmt.Errorf("anet: receipt corrupt: %w", err)
	}
	receiptCID, err := rc.CID()
	if err != nil {
		return zero, err
	}
	rv := &evidence.Review{
		InteractionID: interactionID,
		SubjectAID:    ix.PeerAID,
		ReviewerAID:   d.AID(),
		Rating:        rating,
		Comment:       comment,
		ReceiptCID:    receiptCID,
		CreatedAt:     uint64(nowMillis()),
	}
	if !rv.ValidRating() {
		return zero, fmt.Errorf("anet: rating must be %d..%d", evidence.RatingMin, evidence.RatingMax)
	}
	if err := rv.Sign(d.self); err != nil {
		return zero, err
	}
	rvBytes, err := rv.Marshal()
	if err != nil {
		return zero, err
	}
	if err := d.ix.SetReview(interactionID, rvBytes); err != nil {
		return zero, err
	}
	return ReviewResult{InteractionID: interactionID, Subject: ix.PeerAID, Rating: rating}, nil
}

// recordReceived writes anet.delegation.received for a delegation from a
// peer this node named (trust peer or approved). Public and public_cap
// acceptances are aggregated instead (noteReceivedPublic).
func (d *Daemon) recordReceived(ix, from, trust, requestCID, capID string) {
	if trust == interactions.TrustPublic || trust == interactions.TrustPublicCap {
		d.noteReceivedPublic(from, trust)
		return
	}
	if d.ledger == nil {
		return
	}
	ev := map[string]any{"interaction_id": ix, "requester_aid": from, "trust": trust, "request_cid": requestCID}
	if capID != "" {
		ev["capability"] = capID
	}
	if _, err := d.ledger.Append(EvDelegationReceived, ev); err != nil {
		log.Printf("anet: delegation evidence: %v", err)
	}
}

// ingestDelegate stores an inbound delegation that passed §3.6 steps 1-9 and
// the inbound policy of §5.2: the envelope is from m.from, the TaskDoc
// verified under the sender's resolved KEL at the message time and is
// signed by m.from, and m.ix is either new or an inbound interaction with
// the same peer.
//
// The interaction row, its opening message and the replay row commit in one
// transaction. A capability call is executed after the commit; if the
// daemon stops between the two, the redelivered envelope finds the replay
// row and redeliveredDelegate decides what to do.
//
// Step 9 read the interaction outside this transaction. Two copies of one
// delegation under different message ids (the requester sealed it twice)
// can both pass step 9 before either commits, so the transaction reads the
// interaction again under the store's write lock and creates it only when
// it is still absent.
func (d *Daemon) ingestDelegate(ctx context.Context, m *rxMsg) rxResult {
	if m.hold {
		return d.holdDelegate(m)
	}
	release := m.release
	m.release = nil
	released := false
	defer func() {
		if !released && release != nil {
			release()
		}
	}()
	requestCID, err := anetcid.Sum(m.tdBytes)
	if err != nil {
		return d.drop(dropBadTaskDoc, err)
	}
	capID, args, isCap := capabilityCall(m.td)
	goal := delegation.TaskGoal(m.td)
	publicCap := m.trust == interactions.TrustPublicCap
	if publicCap {
		// The Intent of a public capability call is not stored as a
		// conversation message: the caller is a stranger, and its text
		// is not something this node agreed to hold (A2A-DESIGN §5.2 row
		// 2, [C6]). The capability id is the goal.
		goal = capID
	}
	var peerKEL, peerKeys []byte
	if m.trust == interactions.TrustPublic || publicCap {
		// Not recorded in peer_identity (§3.8); kept on the interaction for
		// encrypting the reply, and deleted when it ends.
		peerKEL, _ = identity.MarshalKEL(m.kel)
		if m.noticeKeys != nil {
			peerKeys = m.noticeKeys.signed
		}
	}
	var seq int64
	redelivery := false
	res := d.commitRx(m, func(tx *interactions.Tx) error {
		prior, err := tx.Get(m.ix)
		switch {
		case err == nil:
			if prior.Role != interactions.RoleInbound || prior.PeerAID != m.from {
				return rxPermanent(dropIXCollision,
					fmt.Errorf("%s is %s with %s", m.ix, prior.Role, prior.PeerAID))
			}
			// The same interaction under a new message id: record the
			// message and treat it as a redelivery of the interaction.
			redelivery = true
			return nil
		case !errors.Is(err, interactions.ErrNotFound):
			return err
		}
		if err := tx.Create(interactions.New{ID: m.ix, Role: interactions.RoleInbound, PeerAID: m.from,
			Goal: goal, RequestCID: requestCID, RequestDoc: m.tdBytes, ContextID: m.dr.ContextID,
			Trust: m.trust, IsCapability: isCap, TaskNonce: taskNonce(m.td),
			PeerKEL: peerKEL, PeerKeys: peerKeys}); err != nil {
			return err
		}
		if publicCap {
			return nil
		}
		// Record the goal as the first conversation message.
		s, err := tx.AddMessage(m.ix, m.from, interactions.MsgText, goal)
		seq = s
		return err
	})
	if res.class != rxAccepted {
		return res
	}
	if redelivery {
		d.redeliveredDelegate(ctx, m)
		return res
	}
	if !publicCap {
		if err := d.storeMsgAttachments(m.ix, seq, m.dr.Attachments); err != nil {
			log.Printf("anet: store inbound attachments: %v", err)
		}
		d.publishMessage(m.ix, seq, interactions.MsgText)
	}
	d.publishState(m.ix)
	d.recordReceived(m.ix, m.from, m.trust, requestCID, capID)
	// A capability call is executed deterministically here; a
	// natural-language task waits for the operator's agent or auto-reply.
	if isCap {
		released = true
		d.runCapabilityCall(m.ix, capID, args, m.dr.Payment, release)
	}
	return res
}

// redeliveredDelegate handles a delegation this node already recorded.
//
// Delivery is at-least-once and cannot be otherwise; execution is what
// must not be repeated. Re-running is a second physical effect for a
// capability that has one, a second signed receipt for work that happened
// once, and a second entry on the chain.
//
//   - Answered already (a receipt exists): send the answer again. The
//     redelivery means the first may never have reached the requester, and
//     the receipt is signed over content that has not changed.
//   - Being executed by this process: nothing to do.
//   - Terminal without a receipt (canceled, or refused): nothing to do.
//   - A short capability call with no answer: the daemon stopped between
//     recording the delegation and answering it. Run it again
//     (at-least-once, as before wire 2).
//   - A long capability call with no answer: not run again (at-most-once;
//     see runCapabilityCall). Startup recovery reports it as interrupted.
func (d *Daemon) redeliveredDelegate(ctx context.Context, m *rxMsg) {
	prior, err := d.ix.Get(m.ix)
	if err != nil {
		return
	}
	if len(prior.Receipt) > 0 {
		log.Printf("anet: %s redelivered; re-sending the answer we already signed", m.ix)
		d.resendResult(m.ix, prior)
		return
	}
	if prior.IsTerminal() {
		return
	}
	if _, running := d.running.Load(m.ix); running {
		return
	}
	capID, args, ok := capabilityCall(m.td)
	if !ok {
		return
	}
	if d.longCall(capID) {
		log.Printf("anet: %s: long call %s was interrupted before it answered; not running it again", m.ix, capID)
		return
	}
	d.runCapabilityCall(m.ix, capID, args, m.dr.Payment, nil)
}

// longCall reports whether a capability runs off the receive path (see
// runCapabilityCall).
func (d *Daemon) longCall(capID string) bool {
	if d.providers == nil {
		return false
	}
	p, ok := d.providers.Resolve(capID)
	if !ok {
		return false
	}
	_, long := invokeBound(p, capID)
	return long
}

// runningCall is a capability call this process is executing. cancel stops
// its context (A2A-DESIGN §4.2: a cancel during execution is best effort).
type runningCall struct {
	cancel context.CancelFunc
}

// stopRunning asks a running capability call of ix to stop.
func (d *Daemon) stopRunning(ix string) {
	if v, ok := d.running.Load(ix); ok {
		if rc, ok := v.(*runningCall); ok && rc.cancel != nil {
			rc.cancel()
		}
	}
}

// runCapabilityCall invokes a capability, on this goroutine when it is
// quick and on its own when the provider says it will not be.
//
// The poll loop dispatches synchronously, which is right for work
// measured in milliseconds and wrong for work measured in hours: this
// call sits inside pollOnce holding pollMu, so an hour-long command is
// an hour in which the node collects no mail at all, acks nothing, and
// looks dead to everyone waiting on it.
//
// The message is acked as soon as a long call is accepted, not when it
// finishes. That is deliberate and it is the at-most-once choice: an
// unacked message is redelivered, and re-running a command that flashes
// firmware or applies a migration is worse than losing the record that
// it started. A long call is marked working when it starts; a node that
// dies mid-command finds it working with no result at the next start and
// reports it as failed, effect UNVERIFIED (recoverInterrupted).
//
// release, when not nil, frees the admission slot of a public capability
// call; it is called when the invocation ends.
func (d *Daemon) runCapabilityCall(interactionID, capID string, args map[string]any, payment []byte, release func()) bool {
	done := func() {
		if release != nil {
			release()
		}
	}
	if d.providers == nil {
		done()
		return true
	}
	// Marked for the whole execution, including the long path's goroutine,
	// so a redelivered delegation can tell work in progress from work a
	// crash interrupted (redeliveredDelegate), and a cancel can reach it.
	rc := &runningCall{}
	if _, busy := d.running.LoadOrStore(interactionID, rc); busy {
		done()
		return true
	}
	finish := func() { d.running.Delete(interactionID); done() }
	// Two copies of one delegation (a redelivery, or the same task sealed
	// twice) are handled on different goroutines. The other copy may have
	// finished and released the mark between this caller's receipt check
	// and the LoadOrStore above; its receipt is read again here, holding
	// the mark, so the capability is not executed a second time. A task
	// canceled before execution started is not executed.
	if ix, err := d.ix.Get(interactionID); err == nil && (len(ix.Receipt) > 0 || ix.IsTerminal()) {
		finish()
		return true
	}
	p, ok := d.providers.Resolve(capID)
	if !ok {
		defer finish()
		cctx, cancel := context.WithTimeout(d.ctx, capabilityInvokeTimeout)
		rc.cancel = cancel
		defer cancel()
		d.tryCapabilityPaid(cctx, interactionID, capID, args, payment)
		return true
	}
	bound, long := invokeBound(p, capID)
	if !long {
		defer finish()
		cctx, cancel := context.WithTimeout(d.ctx, bound)
		rc.cancel = cancel
		defer cancel()
		d.tryCapabilityPaid(cctx, interactionID, capID, args, payment)
		return true
	}
	select {
	case d.longCalls <- struct{}{}:
	default:
		defer finish()
		// Said, not queued. See maxConcurrentLongCalls.
		cctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
		defer cancel()
		ix, err := d.ix.Get(interactionID)
		if err != nil {
			return true
		}
		d.deliverCapabilityResult(cctx, interactionID, capID, ix, capabilityResult{
			Status: string(effect.Unavailable),
			Message: fmt.Sprintf("this node is already running %d long tasks, which is as many as it accepts",
				maxConcurrentLongCalls),
		}, nil, resultOpts{retryAfterMS: busyRetryAfterMS})
		return true
	}
	log.Printf("anet: %s: %s may take up to %s — running it off the poll loop", interactionID, capID, bound)
	if _, err := d.ix.SetState(interactionID, interactions.StateWorking); err != nil {
		log.Printf("anet: %s: mark working: %v", interactionID, err)
	}
	d.publishState(interactionID)
	cctx, cancel := context.WithTimeout(d.ctx, bound)
	rc.cancel = cancel
	d.longCallsWG.Add(1)
	go func() {
		defer d.longCallsWG.Done()
		defer finish()
		defer func() { <-d.longCalls }()
		defer cancel()
		d.tryCapabilityPaid(cctx, interactionID, capID, args, payment)
	}()
	return true
}

// busyRetryAfterMS is the retry hint given with an UNAVAILABLE answer that
// means "busy, try again" rather than "does not serve it".
const busyRetryAfterMS = 60_000

// ingestMessage lands a conversation message that passed §3.6 steps 1-9:
// it is from m.from, the peer of m.ix, and allowed on this interaction
// (a public_cap interaction takes only cancel, end_request and payment
// messages). The message, the state it causes and the replay row commit
// together.
func (d *Daemon) ingestMessage(ctx context.Context, m *rxMsg) rxResult {
	if m.pendingRoute {
		return d.routePendingMessage(m)
	}
	cm, ix := m.cm, m.existing
	fromRequester := ix.Role == interactions.RoleInbound
	switch cm.Kind {
	case delegation.ChatText:
		if ix.IsTerminal() {
			// §4.2: after a terminal state only new input is refused. The
			// message is acknowledged and not stored.
			d.count(dropAfterTerminal)
			return d.commitRx(m, nil)
		}
		kind := interactions.MsgText
		body, atts := cm.Body, cm.Attachments
		if hasPaymentStatus(cm.Metadata) {
			kind = interactions.MsgPayment
			if ix.Trust == interactions.TrustPublicCap || ix.IsCapability {
				// Only the payment metadata is kept on a capability call.
				body, atts = "", nil
			}
		}
		next := stateOnMessage(fromRequester, kind, cm.Metadata, ix.PayState)
		var seq int64
		var stored bool
		// The sender's id is what tells a redelivery from a repetition.
		// The replay table covers a redelivery of the same envelope; the
		// message id covers the same message sealed again.
		res := d.commitRx(m, func(tx *interactions.Tx) error {
			var err error
			seq, stored, err = tx.AddMessageRecord(interactions.MessageRecord{InteractionID: m.ix,
				SenderAID: m.from, Kind: kind, Body: body, MsgID: cm.MsgID, Metadata: cm.Metadata})
			if err != nil || !stored || next == "" {
				return err
			}
			_, err = tx.SetState(m.ix, next)
			return err
		})
		if res.class == rxAccepted && stored {
			if err := d.storeMsgAttachments(m.ix, seq, atts); err != nil {
				log.Printf("anet: store chat attachments: %v", err) // metadata stored; bytes rejected/failed
			}
			d.publishMessage(m.ix, seq, kind)
			d.publishState(m.ix)
			if kind == interactions.MsgText && !ix.IsCapability {
				d.kickAutoReply()
			}
		}
		return res
	case delegation.ChatEndRequest:
		if !fromRequester || ix.IsTerminal() {
			d.count(dropEndRequestIgnored)
			return d.commitRx(m, nil)
		}
		var seq int64
		res := d.commitRx(m, func(tx *interactions.Tx) error {
			var err error
			if seq, _, err = tx.AddMessageRecord(interactions.MessageRecord{InteractionID: m.ix,
				SenderAID: m.from, Kind: interactions.MsgEndRequest, MsgID: cm.MsgID}); err != nil {
				return err
			}
			return tx.SetEndRequested(m.ix, m.from)
		})
		if res.class != rxAccepted {
			return res
		}
		d.publishMessage(m.ix, seq, interactions.MsgEndRequest)
		fctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
		defer cancel()
		if ix.IsCapability {
			d.capabilityStopRequest(fctx, m.ix, false)
			return res
		}
		// The requester asked to end: the provider daemon completes the
		// task itself (A2A-DESIGN §4.2), without the provider's agent.
		if err := d.CompleteTask(fctx, m.ix); err != nil {
			log.Printf("anet: %s: complete on the requester's end request: %v", m.ix, err)
		}
		return res
	case delegation.KindCancel:
		if !fromRequester {
			d.count(dropCancelFromProvider)
			return d.commitRx(m, nil)
		}
		var seq int64
		res := d.commitRx(m, func(tx *interactions.Tx) error {
			var err error
			seq, _, err = tx.AddMessageRecord(interactions.MessageRecord{InteractionID: m.ix,
				SenderAID: m.from, Kind: interactions.MsgCancel, MsgID: cm.MsgID})
			return err
		})
		if res.class != rxAccepted {
			return res
		}
		d.publishMessage(m.ix, seq, interactions.MsgCancel)
		fctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
		defer cancel()
		if ix.IsCapability {
			d.capabilityStopRequest(fctx, m.ix, true)
		} else {
			d.providerCancel(fctx, m.ix)
		}
		return res
	case delegation.ChatEndAccept:
		// Wire 1's second step of the end negotiation. The provider now
		// completes on its own; the message is dropped (A2A-DESIGN §3.4).
		d.count(dropEndAccept)
		return d.commitRx(m, nil)
	default:
		// stream_preview is ephemeral and not stored.
		d.count("message-kind-ignored")
		return accepted()
	}
}

// providerCancel handles a requester's cancel on an inbound text task: the
// task is canceled and the requester is told status{canceled}. No receipt
// is signed. Nothing happens once the task is terminal or a payment has
// been submitted (the payment flow decides then).
func (d *Daemon) providerCancel(ctx context.Context, ixID string) {
	cur, err := d.ix.Get(ixID)
	if err != nil || cur.IsTerminal() {
		return
	}
	if cur.PayState == interactions.PaySubmitted || cur.PayState == interactions.PayCompleted {
		return
	}
	if _, err := d.CancelTask(ctx, ixID); err != nil && !errors.Is(err, ErrNotCancelable) {
		log.Printf("anet: %s: cancel at the requester's request: %v", ixID, err)
	}
}

// capabilityStopRequest applies the capability-call rules of A2A-DESIGN
// §4.2 to a cancel (isCancel) or an end_request:
//
//   - a payment submitted or settled: neither changes anything; the paid
//     work is completed and delivered;
//   - not executing (waiting for payment, or not started): both cancel the
//     task, with status{canceled} and no receipt;
//   - executing: a cancel stops the call's context (best effort) and the
//     result is delivered as it comes; an end_request is ignored.
func (d *Daemon) capabilityStopRequest(ctx context.Context, ixID string, isCancel bool) {
	cur, err := d.ix.Get(ixID)
	if err != nil || cur.IsTerminal() {
		return
	}
	if cur.PayState == interactions.PaySubmitted || cur.PayState == interactions.PayCompleted {
		return
	}
	if _, running := d.running.Load(ixID); running {
		if isCancel {
			d.stopRunning(ixID)
		}
		return
	}
	if _, err := d.CancelTask(ctx, ixID); err != nil && !errors.Is(err, ErrNotCancelable) {
		log.Printf("anet: %s: cancel capability call: %v", ixID, err)
	}
}

// resultState is the state a result moves an outbound task to (§4.3): the
// provider's anet.state when it names one a result may carry, otherwise
// derived from the result itself.
func resultState(ix *interactions.Interaction, rr *delegation.ResultResp) interactions.State {
	switch st := interactions.State(fmt.Sprint(decodeMeta(rr.Metadata)["anet.state"])); st {
	case interactions.StateCompleted, interactions.StateFailed, interactions.StateRejected, interactions.StateInputRequired:
		return st
	}
	if rr.Status == delegation.StatusFailed {
		return interactions.StateFailed
	}
	if ix.IsCapability {
		var res capabilityResult
		if json.Unmarshal(rr.Deliverable, &res) == nil && res.Status != "" {
			st, _ := stateForEffect(res.Status, res.Paid != nil)
			return st
		}
	}
	return interactions.StateCompleted
}

// ingestResult lands a result for an interaction this node started, from
// its provider (checked in step 9). A result that arrives after the task
// reached a terminal state here (a local cancel) is still verified and
// recorded — its deliverable, receipt and any settlement evidence — without
// changing the state (A2A-DESIGN §4.2).
func (d *Daemon) ingestResult(ctx context.Context, m *rxMsg) rxResult {
	rr, ix := m.rr, m.existing
	if rr.Status == delegation.StatusFailed && len(rr.Receipt) == 0 {
		if len(ix.Receipt) > 0 || ix.IsTerminal() {
			return d.commitRx(m, nil)
		}
		res := d.commitRx(m, func(tx *interactions.Tx) error {
			err := tx.SetFailed(m.ix, rr.Deliverable)
			if errors.Is(err, interactions.ErrTerminal) {
				return nil
			}
			return err
		})
		if res.class == rxAccepted {
			d.publishResult(m.ix)
		}
		return res
	}
	resultCID, err := anetcid.Sum(rr.Deliverable)
	if err != nil {
		return d.drop(dropBadBody, err)
	}

	// Have we already accepted this one?
	//
	// A result can arrive again under a new message id (the provider
	// re-sent it for a redelivered delegation). Storing identical content
	// twice is harmless; appending "result accepted" to the chain twice is
	// not.
	if len(ix.Receipt) > 0 {
		return d.commitRx(m, nil)
	}

	// Verify the receipt before accepting the work it certifies.
	//
	// VerifyResultWithKEL binds the receipt to this interaction, to us as
	// the requester, to the provider we actually delegated to, and to the
	// hash of the bytes in front of us. The provider KEL is the one resolved
	// from the envelope (§3.6 step 6), and the revocation gate is evaluated
	// at the message time (C4b, C4d).
	//
	// A failed check drops the result rather than storing it.
	verified := false
	switch _, verr := delegation.VerifyResultWithKEL(rr, m.kel, m.ix, d.AID(), ix.PeerAID, m.ts); {
	case verr == nil:
		verified = true
	case errors.Is(verr, delegation.ErrUnverifiable):
		log.Printf("anet: %s: result accepted UNVERIFIED (no provider KEL)", m.ix)
	default:
		log.Printf("anet: %s: REFUSING result: %v", m.ix, verr)
		return d.drop(dropResultRefused, verr)
	}
	seen := interactions.VerificationUnverified
	if verified {
		seen = interactions.VerificationVerified
	}
	state := resultState(ix, rr)
	// The result's metadata is kept with it: anet.reason and
	// anet.retry_after_ms are in no other place (A2A-DESIGN §4.3, §11.5).
	var resultMeta []byte
	if len(rr.Metadata) > 0 && json.Valid(rr.Metadata) {
		resultMeta = rr.Metadata
	}
	// The receipt check above read the interaction outside this
	// transaction. A second copy of the result under another message id can
	// have committed since, so it is read again under the write lock.
	already, late := false, false
	res := d.commitRx(m, func(tx *interactions.Tx) error {
		cur, err := tx.Get(m.ix)
		if err != nil {
			return err
		}
		if len(cur.Receipt) > 0 {
			already = true
			return nil
		}
		if cur.IsTerminal() {
			late = true
			return nil
		}
		return tx.Finish(m.ix, interactions.Finish{State: state, Result: rr.Deliverable, ResultCID: resultCID,
			Receipt: rr.Receipt, Verified: seen, Meta: resultMeta})
	})
	if res.class != rxAccepted || already {
		return res
	}
	if late {
		if _, err := d.ix.SetLateResult(m.ix, rr.Deliverable, resultCID, rr.Receipt, seen); err != nil {
			log.Printf("anet: %s: store the result that arrived after the task ended: %v", m.ix, err)
		}
	}
	// C5: the requester records the receipt it accepted on its own chain, so
	// both sides of a completed interaction carry evidence.
	ev := map[string]any{
		"interaction_id":   m.ix,
		"result_cid":       resultCID,
		"receipt_bytes":    len(rr.Receipt),
		"receipt_verified": verified,
		"state":            string(state),
	}
	if late {
		ev["after_terminal"] = true
	}
	if _, lerr := d.ledger.Append(EvResultAccepted, ev); lerr != nil {
		log.Printf("anet: result evidence ledger: %v", lerr)
	}
	d.recordSettlement(m.ix, rr.Deliverable)
	d.publishResult(m.ix)
	return res
}

// ingestStatus lands an anet.status/1 for an interaction this node
// started, from its provider (checked in step 9): the status is stored as
// a status message and the task moves to the state it carries. A status
// for a task already terminal here changes nothing.
func (d *Daemon) ingestStatus(_ context.Context, m *rxMsg) rxResult {
	sm, ix := m.sm, m.existing
	if ix.IsTerminal() {
		return d.commitRx(m, nil)
	}
	var meta []byte
	if len(sm.Metadata) > 0 && json.Valid(sm.Metadata) {
		meta = sm.Metadata
	}
	var seq int64
	res := d.commitRx(m, func(tx *interactions.Tx) error {
		var err error
		if seq, _, err = tx.AddMessageRecord(interactions.MessageRecord{InteractionID: m.ix, SenderAID: m.from,
			Kind: interactions.MsgStatus, Body: sm.Text, MsgID: "st_" + hex.EncodeToString(m.mid), Metadata: meta}); err != nil {
			return err
		}
		_, err = tx.SetState(m.ix, interactions.State(sm.State))
		return err
	})
	if res.class == rxAccepted {
		d.publishMessage(m.ix, seq, interactions.MsgStatus)
		d.publishState(m.ix)
	}
	return res
}

// recordSettlement puts the payer's half of a payment on the payer's own
// chain, having checked the hub's signature over it first.
//
// Failure here does not fail the result. The work arrived and is good;
// what is missing is our note about the payment, and dropping a delivered
// result over a bookkeeping problem would be the worse trade.
func (d *Daemon) recordSettlement(interactionID string, deliverable []byte) {
	if d.ledger == nil || len(deliverable) == 0 {
		return
	}
	var res struct {
		Paid *paidView `json:"paid"`
	}
	if err := json.Unmarshal(deliverable, &res); err != nil || res.Paid == nil {
		return
	}
	entry := map[string]any{
		"interaction_id": interactionID,
		"transaction":    res.Paid.Transaction,
		"amount":         res.Paid.Amount,
		"network":        res.Paid.Network,
	}
	// Verified is the point of the entry. "The provider told us it was
	// paid" and "the hub signed that it moved the credit" are different
	// facts, and a chain that cannot tell them apart is one that will be
	// read as claiming the stronger.
	verified := false
	if p := d.payer(); p != nil && res.Paid.Receipt != "" {
		if facts, ok := p.VerifyReceipt(res.Paid.Receipt, d.AID()); ok {
			verified = true
			entry["payee"] = facts.Payee
			entry["auth_id"] = facts.AuthID
			entry["receipt"] = res.Paid.Receipt
		} else {
			log.Printf("anet: %s: settlement receipt did not check out", interactionID)
		}
	}
	entry["verified"] = verified
	if _, err := d.ledger.Append(EvPaymentSettled, entry); err != nil {
		log.Printf("anet: settlement evidence: %v", err)
	}
}

// resendResult relays an answer this node already signed, for a
// redelivered delegation. It goes through the retry queue, so a failed
// attempt is retried rather than lost.
func (d *Daemon) resendResult(interactionID string, ix *interactions.Interaction) {
	selfKEL, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		return
	}
	meta, _ := json.Marshal(map[string]any{"anet.state": string(ix.State)})
	payload, err := (&delegation.ResultResp{
		Status: delegation.StatusDone, Deliverable: ix.Result,
		Receipt: ix.Receipt, KEL: selfKEL, Metadata: meta,
	}).Marshal()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
	defer cancel()
	id, err := d.queueSend(ctx, ix.PeerAID, seal.TypeResult, interactionID, payload, nil)
	if err != nil {
		log.Printf("anet: %s: queue the re-sent answer: %v", interactionID, err)
		return
	}
	if err := d.deliverQueued(ctx, id); err != nil {
		log.Printf("anet: %s: re-sending the answer: %v (queued)", interactionID, err)
	}
}

package daemon

// delegation.go holds the delegation lifecycle over the local interactions store: accepting inbound
// tasks pulled off the Hub relay (ingestDelegate), the multi-turn conversation both ways (SendMessage /
// ingestMessage), the two-step end-of-task negotiation (RequestEnd/AcceptEnd) whose mutual accept makes
// the provider issue the signed receipt over the transcript (maybeFinalize → result relayed back),
// landing pulled results (ingestResult), and the requester-signed review (SubmitReview). anet runs no
// model — the deliverable bytes come from the operator's EXTERNAL agent.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/protocol/anetcid"
	"github.com/ANetResearch/ANet/internal/protocol/delegation"
	"github.com/ANetResearch/ANet/internal/protocol/evidence"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// InboxItem is one inbound (delegated-to-us) task shown to the operator.
type InboxItem struct {
	InteractionID string `json:"interaction_id"`
	Requester     string `json:"requester"`
	Goal          string `json:"goal"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
}

// ResultItem is one completed outbound delegation (a pulled deliverable).
type ResultItem struct {
	InteractionID string `json:"interaction_id"`
	Provider      string `json:"provider"`
	Goal          string `json:"goal"`
	Result        string `json:"result"`
	RequestCID    string `json:"request_cid"`
	ResultCID     string `json:"result_cid"`
	ReceiptCID    string `json:"receipt_cid"`
	Receipt       string `json:"receipt"`
	Reviewed      bool   `json:"reviewed"`
}

// ReviewResult is the outcome of signing a review.
type ReviewResult struct {
	InteractionID string
	Subject       string
	Rating        int
}

// ThreadMsg is one conversation entry rendered for the chat console. From is "me"/"them" (placement),
// Kind is text / end_request / end_accept.
type ThreadMsg struct {
	From        string      `json:"from"`
	Kind        string      `json:"kind"`
	Body        string      `json:"body"`
	Attachments []ThreadAtt `json:"attachments,omitempty"`
	CreatedAt   string      `json:"created_at"`
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
// provider). Messages is the full multi-turn log; EndReqBy/EndAccBy ("me"/"them"/"") drive the
// end-of-task negotiation UI; Reviewed marks an outbound interaction we've already rated.
type Thread struct {
	InteractionID string      `json:"interaction_id"`
	Role          string      `json:"role"`
	Peer          string      `json:"peer"`
	Goal          string      `json:"goal"`
	Status        string      `json:"status"`
	Messages      []ThreadMsg `json:"messages"`
	EndReqBy      string      `json:"end_req_by"`
	EndAccBy      string      `json:"end_acc_by"`
	Reviewed      bool        `json:"reviewed"`
	CreatedAt     string      `json:"created_at"`
	UpdatedAt     string      `json:"updated_at"`
}

// Threads returns ALL interactions in both roles and all statuses, most-recently-updated first, each with
// its full conversation log, for the console's chat view. It does not poll the relay itself (the
// caller/handler does that first if desired).
func (d *Daemon) Threads() ([]Thread, error) { return d.threadsBuild(false) }

// ActiveThreads returns only NON-terminal interactions, skipping finished (done/failed) ones BEFORE their
// messages/attachments are loaded. The auto-reply loop runs every few seconds and only ever acts on
// active conversations, so paying to materialize the full message history of every finished interaction
// made each cycle O(all-history): as tasks accumulate the loop (and, via lock/IO contention, the control
// API and relay) slow to a crawl. Filtering on status first keeps the hot path O(active).
func (d *Daemon) ActiveThreads() ([]Thread, error) { return d.threadsBuild(true) }

// threadsBuild lists interactions for both roles and materializes Thread structs. When activeOnly is set,
// terminal interactions are skipped before their messages are loaded (see ActiveThreads).
func (d *Daemon) threadsBuild(activeOnly bool) ([]Thread, error) {
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
	out := make([]Thread, 0, 32)
	for _, role := range []interactions.Role{interactions.RoleInbound, interactions.RoleOutbound} {
		list, err := d.ix.List(role, interactions.Status(""), 0, 1000)
		if err != nil {
			return nil, err
		}
		for _, ix := range list {
			if activeOnly && (ix.Status == interactions.StatusDone || ix.Status == interactions.StatusFailed) {
				continue // finished — the auto-reply loop skips it anyway; don't pay to load its messages
			}
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
				tmsgs = append(tmsgs, ThreadMsg{From: side(m.SenderAID), Kind: m.Kind, Body: m.Body, Attachments: bySeq[m.Seq], CreatedAt: m.CreatedAt})
			}
			out = append(out, Thread{
				InteractionID: ix.ID, Role: string(ix.Role), Peer: ix.PeerAID, Goal: ix.Goal,
				Status: string(ix.Status), Messages: tmsgs,
				EndReqBy: side(ix.EndReqBy), EndAccBy: side(ix.EndAccBy),
				Reviewed: len(ix.Review) > 0, CreatedAt: ix.CreatedAt, UpdatedAt: ix.UpdatedAt,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
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

// Inbox lists inbound tasks; pending=true filters to the still-queued backlog.
func (d *Daemon) Inbox(pending bool) ([]InboxItem, error) {
	status := interactions.Status("")
	if pending {
		status = interactions.StatusQueued
	}
	list, err := d.ix.List(interactions.RoleInbound, status, 0, 0)
	if err != nil {
		return nil, err
	}
	out := make([]InboxItem, 0, len(list))
	for _, ix := range list {
		out = append(out, InboxItem{
			InteractionID: ix.ID, Requester: ix.PeerAID, Goal: ix.Goal,
			Status: string(ix.Status), CreatedAt: ix.CreatedAt,
		})
	}
	return out, nil
}

// SendMessage appends a chat message to an ACTIVE interaction (from either side) and relays it to the
// peer. Any number of messages may flow both ways until the task is mutually ended. attachPaths are local
// files (images/media/archives) the daemon reads, pins, stores, and relays inline; a message may carry
// attachments with an empty body.
func (d *Daemon) SendMessage(ctx context.Context, interactionID, body string, attachPaths []string) error {
	atts, err := attachmentsFromPaths(attachPaths)
	if err != nil {
		return err
	}
	return d.SendMessageAtts(ctx, interactionID, body, atts)
}

// SendMessageAtts is SendMessage with attachments already assembled (from CLI file paths OR web uploads).
func (d *Daemon) SendMessageAtts(ctx context.Context, interactionID, body string, atts []delegation.Attachment) error {
	body = strings.TrimSpace(body)
	if body == "" && len(atts) == 0 {
		return fmt.Errorf("anet: empty message (pass text and/or --attach PATH)")
	}
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		return err
	}
	if ix.Status == interactions.StatusDone {
		return fmt.Errorf("anet: interaction %s has ended", interactionID)
	}
	seq, err := d.ix.AddMessage(interactionID, d.AID(), interactions.MsgText, body)
	if err != nil {
		return err
	}
	if err := d.storeMsgAttachments(interactionID, seq, atts); err != nil {
		return err
	}
	return d.relayChat(ctx, ix.PeerAID, interactionID, delegation.ChatText, body, atts)
}

// RequestEnd proposes ending the task. If the OTHER side already proposed, this accepts it instead (so
// pressing "end" is always the right thing to do). Mutual agreement triggers the provider's receipt.
func (d *Daemon) RequestEnd(ctx context.Context, interactionID string) error {
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		return err
	}
	if ix.Status == interactions.StatusDone {
		return fmt.Errorf("anet: interaction %s has already ended", interactionID)
	}
	if ix.EndReqBy != "" && ix.EndReqBy != d.AID() {
		return d.AcceptEnd(ctx, interactionID) // the peer already proposed — accept it
	}
	if ix.EndReqBy == d.AID() {
		return fmt.Errorf("anet: you already proposed ending; waiting for the other side to accept")
	}
	if _, err := d.ix.AddMessage(interactionID, d.AID(), interactions.MsgEndRequest, ""); err != nil {
		return err
	}
	if err := d.ix.SetEndRequested(interactionID, d.AID()); err != nil {
		return err
	}
	return d.relayChat(ctx, ix.PeerAID, interactionID, delegation.ChatEndRequest, "", nil)
}

// AcceptEnd accepts the peer's end proposal. On mutual agreement the provider issues the signed Receipt
// over the transcript (maybeFinalize) and relays it back; the requester finalizes on receiving it.
func (d *Daemon) AcceptEnd(ctx context.Context, interactionID string) error {
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		return err
	}
	if ix.Status == interactions.StatusDone {
		return fmt.Errorf("anet: interaction %s has already ended", interactionID)
	}
	if ix.EndReqBy == "" {
		return fmt.Errorf("anet: no end proposal to accept (use `end` to propose ending)")
	}
	if ix.EndReqBy == d.AID() {
		return fmt.Errorf("anet: you proposed ending; the other side must accept")
	}
	if _, err := d.ix.AddMessage(interactionID, d.AID(), interactions.MsgEndAccept, ""); err != nil {
		return err
	}
	if err := d.ix.SetEndAccepted(interactionID, d.AID()); err != nil {
		return err
	}
	if err := d.relayChat(ctx, ix.PeerAID, interactionID, delegation.ChatEndAccept, "", nil); err != nil {
		return err
	}
	return d.maybeFinalize(ctx, interactionID)
}

// relayChat sends a ChatMsg (text or end negotiation, optionally with attachments) into the peer's Hub
// mailbox.
func (d *Daemon) relayChat(ctx context.Context, toAID, interactionID, kind, body string, atts []delegation.Attachment) error {
	cm := &delegation.ChatMsg{Kind: kind, Body: body, Attachments: atts}
	payload, err := cm.Marshal()
	if err != nil {
		return err
	}
	return d.relaySend(ctx, toAID, hubapi.RelayKindMessage, interactionID, payload)
}

// transcriptMsg is one line of the JSON transcript the provider signs (as the receipt's ResultCID) and
// the Hub later displays as verified interaction content. Only text messages are included; the end
// handshake is metadata. Attachments are recorded by metadata + content CID (not their bytes), so the
// receipt binds the delivered files without bloating the transcript / Hub review upload.
type transcriptMsg struct {
	From        string          `json:"from"` // "requester" | "provider"
	Body        string          `json:"body"`
	Attachments []transcriptAtt `json:"attachments,omitempty"`
}

// transcriptAtt is an attachment's receipt-bound fingerprint (name/mime/size + content CID).
type transcriptAtt struct {
	Name string `json:"name"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
	CID  string `json:"cid"`
}

// buildTranscript renders the interaction's text messages (and their attachment fingerprints) into
// canonical JSON — the provider's authoritative record. Its CID becomes the receipt's ResultCID; the
// exact bytes ride back to the requester and are later uploaded to the Hub, which re-hashes them against
// that anchor.
func (d *Daemon) buildTranscript(ix *interactions.Interaction) ([]byte, error) {
	msgs, err := d.ix.Messages(ix.ID)
	if err != nil {
		return nil, err
	}
	atts, err := d.ix.Attachments(ix.ID)
	if err != nil {
		return nil, err
	}
	bySeq := map[int64][]transcriptAtt{}
	for _, a := range atts {
		bySeq[a.MsgSeq] = append(bySeq[a.MsgSeq], transcriptAtt{Name: a.Name, Mime: a.Mime, Size: a.Size, CID: a.CID})
	}
	provider := d.AID()
	if ix.Role == interactions.RoleOutbound {
		provider = ix.PeerAID
	}
	out := make([]transcriptMsg, 0, len(msgs))
	for _, m := range msgs {
		if m.Kind != interactions.MsgText {
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
		out = append(out, transcriptMsg{From: from, Body: m.Body, Attachments: mAtts})
	}
	return json.Marshal(out)
}

// maybeFinalize issues the end-of-task Receipt once the interaction is mutually ended (one side proposed,
// the OTHER accepted). Only the PROVIDER issues it: it signs a Receipt anchoring the original request CID
// and the transcript CID, stores it (status → done), and relays the receipt + transcript to the
// requester (who finalizes on receipt via ingestResult). Idempotent and a no-op on the requester side.
func (d *Daemon) maybeFinalize(ctx context.Context, interactionID string) error {
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		return err
	}
	if ix.Status == interactions.StatusDone || len(ix.Receipt) != 0 {
		return nil
	}
	if ix.EndReqBy == "" || ix.EndAccBy == "" || ix.EndReqBy == ix.EndAccBy {
		return nil // not a mutual end yet
	}
	if ix.Role != interactions.RoleInbound {
		return nil // only the provider issues the receipt
	}
	transcript, err := d.buildTranscript(ix)
	if err != nil {
		return err
	}
	resultCID, err := anetcid.Sum(transcript)
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
	if err := d.ix.SetResult(ix.ID, transcript, resultCID, receiptBytes); err != nil {
		return err
	}
	rr := &delegation.ResultResp{Status: delegation.StatusDone, Deliverable: transcript, Receipt: receiptBytes}
	payload, err := rr.Marshal()
	if err != nil {
		return err
	}
	if err := d.relaySend(ctx, ix.PeerAID, hubapi.RelayKindResult, ix.ID, payload); err != nil {
		return fmt.Errorf("anet: ended locally but relaying the receipt failed: %w", err)
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

// ingestDelegate verifies a relayed delegation and (if this daemon accepts delegations) stores it as a
// queued inbound task for the external agent to complete. Returns whether to ack (drop) the message.
func (d *Daemon) ingestDelegate(payload []byte) bool {
	if !d.config().AcceptsDelegations() {
		return true // not accepting — drop
	}
	dr, err := delegation.UnmarshalDelegateReq(payload)
	if err != nil {
		log.Printf("anet: drop undecodable delegation: %v", err)
		return true
	}
	requesterAID, td, taskDocBytes, err := delegation.VerifyDelegateReq(dr)
	if err != nil {
		log.Printf("anet: drop unverifiable delegation: %v", err)
		return true
	}
	requestCID, err := anetcid.Sum(taskDocBytes)
	if err != nil {
		log.Printf("anet: delegation cid: %v", err)
		return false
	}
	goal := delegation.TaskGoal(td)
	if err := d.ix.Put(dr.InteractionID, interactions.RoleInbound, requesterAID, goal, requestCID, taskDocBytes); err != nil {
		log.Printf("anet: store inbound delegation: %v", err)
		return false
	}
	// Record the goal as the first conversation message (from the requester) so the chat starts with it,
	// carrying any attachments the requester sent up front.
	seq, err := d.ix.AddMessage(dr.InteractionID, requesterAID, interactions.MsgText, goal)
	if err != nil {
		log.Printf("anet: store inbound first message: %v", err)
	} else if err := d.storeMsgAttachments(dr.InteractionID, seq, dr.Attachments); err != nil {
		log.Printf("anet: store inbound attachments: %v", err)
	}
	return true
}

// ingestMessage lands a relayed conversation message (text or end negotiation) on the matching
// interaction; an end acceptance may trigger provider finalization (the signed receipt).
func (d *Daemon) ingestMessage(interactionID, fromAID string, payload []byte) bool {
	cm, err := delegation.UnmarshalChatMsg(payload)
	if err != nil {
		log.Printf("anet: drop undecodable chat message: %v", err)
		return true
	}
	if _, err := d.ix.Get(interactionID); err != nil {
		return true // unknown interaction — drop
	}
	switch cm.Kind {
	case delegation.ChatText:
		seq, err := d.ix.AddMessage(interactionID, fromAID, interactions.MsgText, cm.Body)
		if err != nil {
			log.Printf("anet: store chat message: %v", err)
			return false
		}
		if err := d.storeMsgAttachments(interactionID, seq, cm.Attachments); err != nil {
			log.Printf("anet: store chat attachments: %v", err) // metadata stored; bytes rejected/failed
		}
	case delegation.ChatEndRequest:
		if _, err := d.ix.AddMessage(interactionID, fromAID, interactions.MsgEndRequest, ""); err != nil {
			return false
		}
		if err := d.ix.SetEndRequested(interactionID, fromAID); err != nil && !errors.Is(err, interactions.ErrNotFound) {
			log.Printf("anet: set end requested: %v", err)
		}
	case delegation.ChatEndAccept:
		if _, err := d.ix.AddMessage(interactionID, fromAID, interactions.MsgEndAccept, ""); err != nil {
			return false
		}
		if err := d.ix.SetEndAccepted(interactionID, fromAID); err != nil && !errors.Is(err, interactions.ErrNotFound) {
			log.Printf("anet: set end accepted: %v", err)
		}
		ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
		defer cancel()
		if err := d.maybeFinalize(ctx, interactionID); err != nil {
			log.Printf("anet: finalize on end-accept: %v", err) // retriable on next poll/accept
		}
	default:
		// unknown kind — drop
	}
	return true
}

// ingestResult lands a relayed deliverable + provider receipt on the matching outbound interaction.
func (d *Daemon) ingestResult(interactionID string, payload []byte) bool {
	rr, err := delegation.UnmarshalResultResp(payload)
	if err != nil {
		log.Printf("anet: drop undecodable result: %v", err)
		return true
	}
	if rr.Status == delegation.StatusFailed {
		if err := d.ix.SetFailed(interactionID, rr.Deliverable); err != nil && !errors.Is(err, interactions.ErrNotFound) {
			log.Printf("anet: mark failed: %v", err)
			return false
		}
		return true
	}
	resultCID, err := anetcid.Sum(rr.Deliverable)
	if err != nil {
		log.Printf("anet: result cid: %v", err)
		return false
	}
	if err := d.ix.SetResult(interactionID, rr.Deliverable, resultCID, rr.Receipt); err != nil {
		if errors.Is(err, interactions.ErrNotFound) {
			return true // unknown interaction — drop
		}
		log.Printf("anet: store result: %v", err)
		return false
	}
	return true
}

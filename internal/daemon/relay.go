package daemon

// relay.go is the daemon's v0.1 transport: a thin client of the official Hub relay (wire types in
// internal/hubapi; the Hub itself is a separate closed-source service). All
// delegation traffic flows through it — a requester SENDS a signed delegation into the provider's Hub
// mailbox, the provider PULLS it (KEL-signed poll), completes it, and SENDS the result back to the
// requester's mailbox. A single background loop polls this daemon's mailbox and dispatches each message.
// There is no P2P (that is a later version); the relayed payloads are end-to-end verifiable, so the Hub
// cannot forge an interaction.

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/url"
	"time"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/protocol/anetcid"
	"github.com/ANetResearch/ANet/internal/protocol/delegation"
	"github.com/ANetResearch/ANet/internal/protocol/evidence"
	"github.com/ANetResearch/ANet/internal/protocol/identity"
	"github.com/ANetResearch/ANet/internal/protocol/relayauth"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// relayPollInterval is how often the background loop pulls this daemon's Hub mailbox. Kept short
// so a delegated task / reply is noticed quickly (the Hub is typically local; read commands also
// trigger a best-effort fresh pull via pollFresh, so this is mainly the idle-notice latency).
const relayPollInterval = 1 * time.Second

// relayCallTimeout bounds a single relay round-trip that may carry inline ATTACHMENTS (a poll response
// or a send can be tens of MiB). It is deliberately generous: on a constrained link a 64 MiB payload can
// take minutes, and if the transfer is cut short the message is neither acked nor delivered — it would be
// redelivered forever and wedge the mailbox (a "poison" message). The background loop and the attachment
// send paths use this; interactive freshness polls use the much shorter freshPollTimeout instead.
const relayCallTimeout = 15 * time.Minute

// freshPollTimeout bounds the best-effort poll a read command (inbox/thread/results) triggers, so a large
// inbound transfer never blocks the interactive command — big messages are left to the background loop.
const freshPollTimeout = 12 * time.Second

// HubRegister persists the Hub target + profile to config, registers with the Hub, and (re)starts the
// relay poll loop so delegations and results start flowing. guestMessages, when non-nil, updates this
// agent's guest-mode trial quota (persisted); acceptDelegations, when non-nil, updates whether this
// daemon stores inbound tasks; nil leaves the current setting unchanged.
func (d *Daemon) HubRegister(ctx context.Context, hubURL, name string, caps []string, guestMessages *int, acceptDelegations *bool) error {
	d.mu.Lock()
	if guestMessages != nil {
		d.cfg.GuestMessages = guestMessages
	}
	if acceptDelegations != nil {
		d.cfg.AcceptDelegations = acceptDelegations
	}
	quota := d.cfg.GuestQuota()
	d.mu.Unlock()
	if err := d.RegisterWithHub(ctx, hubURL, name, caps, quota); err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg.HubURL = hubURL
	d.cfg.Name = name
	if caps != nil {
		d.cfg.Caps = caps
	}
	// Don't clobber an auto_reply block that was hand-added to config.json after this daemon started
	// (its in-memory cfg wouldn't know about it): adopt the on-disk value before writing back.
	if d.cfg.AutoReply == nil {
		if prev, err := LoadConfig(d.layout); err == nil && prev.AutoReply != nil {
			d.cfg.AutoReply = prev.AutoReply
		}
	}
	cfg := d.cfg
	d.mu.Unlock()
	if err := SaveConfig(d.layout, cfg); err != nil {
		return err
	}
	// Re-publish any existing self-description so a fresh registration keeps the agent's profile.
	if cfg.Summary != "" || cfg.Readme != "" || cfg.Pricing != "" {
		if err := d.PublishProfile(ctx, hubURL, cfg.Summary, cfg.Readme, cfg.Pricing); err != nil {
			return err
		}
	}
	d.startRelayLoop(hubURL)
	return nil
}

// Find searches the Hub registry (substring over AID/name/caps).
func (d *Daemon) Find(ctx context.Context, query string) ([]hubapi.AgentView, error) {
	hub := d.config().HubURL
	if hub == "" {
		return nil, fmt.Errorf("anet: no hub configured (run `anet hub-register` first)")
	}
	var resp struct {
		Agents []hubapi.AgentView `json:"agents"`
	}
	q := url.Values{}
	if query != "" {
		q.Set("q", query)
	}
	if err := d.hubGet(ctx, hub, "/agents", q, &resp); err != nil {
		return nil, err
	}
	return resp.Agents, nil
}

// Delegate builds a signed TaskDoc for goal, stores the outbound interaction, and sends the delegation
// into the provider's Hub mailbox. It returns the shared interaction_id immediately (the provider may be
// offline; pull the result later with `results`).
func (d *Daemon) Delegate(ctx context.Context, providerAID, goal string, attachPaths []string) (string, error) {
	atts, err := attachmentsFromPaths(attachPaths)
	if err != nil {
		return "", err
	}
	return d.DelegateAtts(ctx, providerAID, goal, atts)
}

// DelegateAtts is Delegate with the attachments already assembled (from CLI file paths OR web uploads).
func (d *Daemon) DelegateAtts(ctx context.Context, providerAID, goal string, atts []delegation.Attachment) (string, error) {
	hub := d.config().HubURL
	if hub == "" {
		return "", fmt.Errorf("anet: no hub configured (run `anet hub-register` first)")
	}
	if providerAID == d.AID() {
		return "", fmt.Errorf("anet: cannot delegate to yourself")
	}
	doc, env, err := d.signTaskDoc(goal)
	if err != nil {
		return "", err
	}
	requestCID, err := anetcid.Sum(doc)
	if err != nil {
		return "", err
	}
	id, err := newInteractionID()
	if err != nil {
		return "", err
	}
	if err := d.ix.Put(id, interactions.RoleOutbound, providerAID, goal, requestCID, doc); err != nil {
		return "", err
	}
	// Record the goal as the first conversation message (from us, the requester), with any attachments.
	seq, err := d.ix.AddMessage(id, d.AID(), interactions.MsgText, goal)
	if err != nil {
		return "", err
	}
	if err := d.storeMsgAttachments(id, seq, atts); err != nil {
		return "", err
	}
	kelB, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		return "", err
	}
	dr := &delegation.DelegateReq{TaskDoc: doc, Envelope: env, KEL: kelB, InteractionID: id, Attachments: atts}
	payload, err := dr.Marshal()
	if err != nil {
		return "", err
	}
	if err := d.relaySend(ctx, providerAID, hubapi.RelayKindDelegate, id, payload); err != nil {
		return "", err
	}
	return id, nil
}

// Results pulls this daemon's mailbox once (so any pending deliverables land in the store) and then lists
// completed outbound delegations.
func (d *Daemon) Results(ctx context.Context) ([]ResultItem, error) {
	if d.config().HubURL == "" {
		return nil, fmt.Errorf("anet: no hub configured (run `anet hub-register` first)")
	}
	d.pollFresh(ctx) // best-effort freshness; the background loop delivers anything large
	var list []*interactions.Interaction
	var since int64
	for {
		batch, err := d.ix.List(interactions.RoleOutbound, interactions.StatusDone, since, 1000)
		if err != nil {
			return nil, err
		}
		list = append(list, batch...)
		if len(batch) < 1000 {
			break
		}
		since = batch[len(batch)-1].Seq
	}
	out := make([]ResultItem, 0, len(list))
	for _, ix := range list {
		rc, err := evidence.UnmarshalReceipt(ix.Receipt)
		if err != nil {
			return nil, fmt.Errorf("anet: receipt corrupt for %s: %w", ix.ID, err)
		}
		receiptCID, err := rc.CID()
		if err != nil {
			return nil, fmt.Errorf("anet: receipt CID for %s: %w", ix.ID, err)
		}
		out = append(out, ResultItem{
			InteractionID: ix.ID, Provider: ix.PeerAID, Goal: ix.Goal,
			Result: string(ix.Result), RequestCID: ix.RequestCID, ResultCID: ix.ResultCID,
			ReceiptCID: receiptCID, Receipt: base64.StdEncoding.EncodeToString(ix.Receipt),
			Reviewed: len(ix.Review) > 0,
		})
	}
	return out, nil
}

// --- relay HTTP client ---

// relaySend enqueues a message into toAID's Hub mailbox.
func (d *Daemon) relaySend(ctx context.Context, toAID, kind, interactionID string, payload []byte) error {
	hub := d.config().HubURL
	if hub == "" {
		return fmt.Errorf("anet: no hub configured")
	}
	body := map[string]any{
		"to_aid":         toAID,
		"from_aid":       d.AID(),
		"kind":           kind,
		"interaction_id": interactionID,
		"payload":        base64.StdEncoding.EncodeToString(payload),
	}
	return d.hubPost(ctx, hub, "/relay/send", body, nil)
}

// relayMsg is one message pulled from this daemon's mailbox.
type relayMsg struct {
	ID            int64  `json:"id"`
	FromAID       string `json:"from_aid"`
	Kind          string `json:"kind"`
	InteractionID string `json:"interaction_id"`
	Payload       string `json:"payload"`
}

// relayPoll pulls undelivered messages for this daemon (KEL-signed auth).
func (d *Daemon) relayPoll(ctx context.Context) ([]relayMsg, error) {
	hub := d.config().HubURL
	if hub == "" {
		return nil, fmt.Errorf("anet: no hub configured")
	}
	ts, seq, sig := d.signRelayAuth(relayauth.ActionPoll)
	body := map[string]any{"aid": d.AID(), "ts": ts, "key_state_seq": seq, "sig": sig, "limit": 100}
	var resp struct {
		Messages []relayMsg `json:"messages"`
	}
	if err := d.hubPost(ctx, hub, "/relay/poll", body, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// relayAck marks messages delivered so they are not redelivered.
func (d *Daemon) relayAck(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	hub := d.config().HubURL
	if hub == "" {
		return fmt.Errorf("anet: no hub configured")
	}
	ts, seq, sig := d.signRelayAuth(relayauth.ActionAck)
	body := map[string]any{"aid": d.AID(), "ts": ts, "key_state_seq": seq, "sig": sig, "ids": ids}
	return d.hubPost(ctx, hub, "/relay/ack", body, nil)
}

// signRelayAuth signs the mailbox challenge (relayauth.Preimage) with the current key.
func (d *Daemon) signRelayAuth(action string) (ts, seq uint64, sigB64 string) {
	ts = uint64(time.Now().UnixMilli())
	sig, s := d.self.Sign(relayauth.Preimage(action, d.AID(), ts))
	return ts, s, base64.StdEncoding.EncodeToString(sig)
}

// --- background poll loop ---

// startRelayLoop cancels any running loop and starts a fresh one against hubURL, under the daemon ctx.
func (d *Daemon) startRelayLoop(hubURL string) {
	d.mu.Lock()
	if d.relayStop != nil {
		d.relayStop()
	}
	loopCtx, cancel := context.WithCancel(d.ctx)
	d.relayStop = cancel
	d.mu.Unlock()
	go d.relayLoop(loopCtx)
}

func (d *Daemon) relayLoop(ctx context.Context) {
	t := time.NewTicker(relayPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Block-acquire pollMu so the loop always eventually polls (a brief freshness poll may hold
			// it). Use the generous relayCallTimeout so a large inbound attachment fully transfers rather
			// than being cut short and redelivered forever.
			d.pollMu.Lock()
			pc, cancel := context.WithTimeout(ctx, relayCallTimeout)
			if err := d.pollOnce(pc); err != nil && ctx.Err() == nil {
				log.Printf("anet: relay poll: %v", err)
			}
			cancel()
			d.pollMu.Unlock()
		}
	}
}

// pollFresh is a best-effort, non-blocking-if-busy mailbox poll for interactive read commands. If the
// background loop (or another handler) is already polling, it returns immediately and the caller serves
// whatever is already local — so an in-flight large transfer never blocks the command. Errors are
// intentionally swallowed (freshness is best-effort; the background loop retries).
func (d *Daemon) pollFresh(ctx context.Context) {
	if d.config().HubURL == "" {
		return
	}
	if !d.pollMu.TryLock() {
		return
	}
	defer d.pollMu.Unlock()
	pc, cancel := context.WithTimeout(ctx, freshPollTimeout)
	defer cancel()
	_ = d.pollOnce(pc)
}

// pollOnce pulls the mailbox, dispatches each message, and acks the ones it processed. If any inbound
// delegate/message landed (something that may owe an auto-reply), it wakes the auto-reply loop at once
// so a turn isn't stalled up to a full poll interval waiting for the next tick.
func (d *Daemon) pollOnce(ctx context.Context) error {
	msgs, err := d.relayPoll(ctx)
	if err != nil {
		return err
	}
	var acked []int64
	owesReply := false
	for _, m := range msgs {
		payload, derr := base64.StdEncoding.DecodeString(m.Payload)
		if derr != nil {
			acked = append(acked, m.ID) // undecodable — drop
			continue
		}
		if d.dispatch(m, payload) {
			acked = append(acked, m.ID)
			if m.Kind == hubapi.RelayKindDelegate || m.Kind == hubapi.RelayKindMessage {
				owesReply = true
			}
		}
	}
	if owesReply {
		d.kickAutoReply()
	}
	return d.relayAck(ctx, acked)
}

// kickAutoReply wakes the auto-reply loop immediately (non-blocking; coalesces bursts). No-op when the
// channel is full (a wake is already pending) or auto-reply is off (the loop just isn't selecting on it).
func (d *Daemon) kickAutoReply() {
	select {
	case d.autoReplyKick <- struct{}{}:
	default:
	}
}

// dispatch handles one mailbox message and reports whether it should be acked (removed). Malformed or
// unwanted messages are acked so they do not clog the mailbox; a transient store failure is NOT acked.
func (d *Daemon) dispatch(m relayMsg, payload []byte) (ack bool) {
	switch m.Kind {
	case hubapi.RelayKindDelegate:
		return d.ingestDelegate(payload)
	case hubapi.RelayKindResult:
		return d.ingestResult(m.InteractionID, payload)
	case hubapi.RelayKindMessage:
		return d.ingestMessage(m.InteractionID, m.FromAID, payload)
	default:
		return true // unknown kind — drop
	}
}

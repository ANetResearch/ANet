package daemon

// relay.go is the daemon's client of the Hub relay, wire 2 (A2A-DESIGN §3.7; wire types in
// internal/hubapi). A requester SENDS a sealed delegation envelope into the provider's Hub mailbox,
// the provider PULLS it (relayauth v2 signed poll), completes it, and SENDS a sealed result back to
// the requester's mailbox. A single background loop polls this daemon's mailbox and runs each
// envelope through the receive pipeline (receive.go). Every envelope is encrypted to its recipient and
// signed by its sender inside the encryption, so the Hub can neither read nor forge one; it sees the
// recipient AID, the authenticated sender at send time, the time and the size.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/provider"
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
// relay poll loop so delegations and results start flowing. Whether this node accepts delegations is
// the inbound policy (A2A-DESIGN §5), not a registration setting.
// invite carries an admission token to a hub that requires one; empty
// otherwise, and never written to config — see RegisterWithHub.
func (d *Daemon) HubRegister(ctx context.Context, hubURL, name string, caps []string, invite string) error {
	caps = withServedCapabilities(caps, d.providers)
	if err := d.RegisterWithHub(ctx, hubURL, name, caps, invite); err != nil {
		return err
	}
	d.mu.Lock()
	prevHub := d.cfg.HubURL
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
	if prevHub != hubURL && d.CardPublicationStatus().Sent {
		// The payment module learns the hub, and so the ledger its prices
		// are on, from the config, which is written only now: on a node
		// that had no hub, the card just sent carries no price list
		// (a2a_card.go). Publish once more with the config in place; a
		// card that comes out the same is answered "unchanged".
		d.cardInputsChanged()
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

// FindByCapability asks who serves a capability id.
//
// A different question from Find, and deliberately not a better-phrased
// version of it. A capability id is exact and structured, so this is a
// membership test: "cas.put" means that id, and "ptz.*" means that
// family. Find searches prose, which will happily return an agent that
// merely mentions the words.
func (d *Daemon) FindByCapability(ctx context.Context, capID string) ([]hubapi.AgentView, error) {
	hub := d.config().HubURL
	if hub == "" {
		return nil, fmt.Errorf("anet: no hub configured (run `anet hub-register` first)")
	}
	var resp struct {
		Agents []hubapi.AgentView `json:"agents"`
	}
	q := url.Values{}
	q.Set("cap", capID)
	if err := d.hubGet(ctx, hub, "/agents", q, &resp); err != nil {
		return nil, err
	}
	return resp.Agents, nil
}

// Find searches the Hub registry (substring over AID/name/summary/readme/caps).
//
// The free text never leaves this node (A2A-DESIGN §10.5): the daemon
// fetches the listing and matches locally (discover.go).
func (d *Daemon) Find(ctx context.Context, query string) ([]hubapi.AgentView, error) {
	hub := d.config().HubURL
	if hub == "" {
		return nil, fmt.Errorf("anet: no hub configured (run `anet hub-register` first)")
	}
	var resp struct {
		Agents []hubapi.AgentView `json:"agents"`
	}
	if err := d.hubGet(ctx, hub, "/agents", nil, &resp); err != nil {
		return nil, err
	}
	return matchAgents(resp.Agents, query), nil
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
	return d.DelegateIn(ctx, providerAID, goal, atts, "")
}

// DelegateIn is DelegateAtts under a given A2A context id. An empty
// contextID mints one: the requester daemon owns context ids for the tasks
// it creates (A2A-DESIGN §4.1).
func (d *Daemon) DelegateIn(ctx context.Context, providerAID, goal string, atts []delegation.Attachment, contextID string) (string, error) {
	id, err := newInteractionID()
	if err != nil {
		return "", err
	}
	if err := d.delegateInWithID(ctx, id, providerAID, goal, atts, contextID, nil); err != nil {
		return "", err
	}
	return id, nil
}

// delegateInWithID is DelegateIn under an interaction id the caller minted,
// so a caller learns the id of a task whose row was written even when the
// send then failed (the A2A task path marks such a task failed). meta is
// the first message's metadata (A2A Message.metadata, a JSON object or
// nil): stored on the goal message here and carried to the provider as
// DelegateReq.Metadata.
func (d *Daemon) delegateInWithID(ctx context.Context, id, providerAID, goal string, atts []delegation.Attachment,
	contextID string, meta []byte) error {
	hub := d.config().HubURL
	if hub == "" {
		return fmt.Errorf("anet: no hub configured (run `anet hub-register` first)")
	}
	if providerAID == d.AID() {
		return fmt.Errorf("anet: cannot delegate to yourself")
	}
	if contextID == "" {
		c, err := newContextID()
		if err != nil {
			return err
		}
		contextID = c
	}
	nonce, err := newTaskNonce()
	if err != nil {
		return err
	}
	doc, env, err := d.signTaskDoc(goal, nonce)
	if err != nil {
		return err
	}
	requestCID, err := anetcid.Sum(doc)
	if err != nil {
		return err
	}
	if err := d.ix.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: providerAID,
		Goal: goal, RequestCID: requestCID, RequestDoc: doc, ContextID: contextID, TaskNonce: nonce}); err != nil {
		return err
	}
	// Record the goal as the first conversation message (from us, the requester), with any
	// attachments. The requester keeps a message id for it too, so its history carries ids.
	msgID, err := newMessageID()
	if err != nil {
		return err
	}
	seq, _, err := d.ix.AddMessageRecord(interactions.MessageRecord{InteractionID: id, SenderAID: d.AID(),
		Kind: interactions.MsgText, Body: goal, MsgID: msgID, Metadata: meta})
	if err != nil {
		return err
	}
	if err := d.storeMsgAttachments(id, seq, atts); err != nil {
		return err
	}
	d.publishMessage(id, seq, interactions.MsgText)
	d.publishState(id)
	kelB, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		return err
	}
	dr := &delegation.DelegateReq{TaskDoc: doc, Envelope: env, KEL: kelB, InteractionID: id, Attachments: atts,
		ContextID: contextID, Metadata: meta}
	payload, err := dr.Marshal()
	if err != nil {
		return err
	}
	if err := d.relaySend(ctx, providerAID, seal.TypeDelegate, id, payload); err != nil {
		return err
	}
	// C5: a requester's chain should show what it asked for, not only what
	// it received.
	if _, lerr := d.ledger.Append(EvDelegationSent, map[string]any{
		"interaction_id": id,
		"provider_aid":   providerAID,
		"request_cid":    requestCID,
	}); lerr != nil {
		log.Printf("anet: delegation evidence ledger: %v", lerr)
	}
	return nil
}

// Results pulls this daemon's mailbox once (so any pending deliverables land in the store) and then lists
// outbound delegations that carry a result and its receipt, oldest first. The state tells a completed
// task from a failed or refused one; the deliverable of a capability call carries its effect status.
func (d *Daemon) Results(ctx context.Context) ([]ResultItem, error) {
	if d.config().HubURL == "" {
		return nil, fmt.Errorf("anet: no hub configured (run `anet hub-register` first)")
	}
	d.pollFresh(ctx) // best-effort freshness; the background loop delivers anything large
	var list []*interactions.Interaction
	var since int64
	for {
		batch, err := d.ix.List(interactions.RoleOutbound, "", since, 1000)
		if err != nil {
			return nil, err
		}
		for _, ix := range batch {
			if len(ix.Receipt) > 0 {
				list = append(list, ix)
			}
		}
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
			InteractionID: ix.ID, Provider: ix.PeerAID, Goal: ix.Goal, State: string(ix.State),
			Result: string(ix.Result), RequestCID: ix.RequestCID, ResultCID: ix.ResultCID,
			ReceiptCID: receiptCID, Receipt: base64.StdEncoding.EncodeToString(ix.Receipt),
			Reviewed: len(ix.Review) > 0,
			// The key the receipt was checked against travels with it, so a
			// holder can re-check rather than take this node's word for it.
			ProviderKEL: d.encodedPeerKEL(ix.PeerAID),
			// Per-result, unlike ProviderKEL: that is a per-peer record, so
			// one verified interaction with a provider would otherwise make
			// every later unverified one from it look checked.
			ReceiptVerified: string(ix.ReceiptVerified),
		})
	}
	return out, nil
}

// --- relay HTTP client (wire 2, A2A-DESIGN §3.7) ---

// relayPoll pulls undelivered envelopes for this daemon. The mailbox is the
// authenticated caller's; the request carries no AID of its own.
func (d *Daemon) relayPoll(ctx context.Context) ([]hubapi.RelayMessage, error) {
	hub := d.config().HubURL
	if hub == "" {
		return nil, fmt.Errorf("anet: no hub configured")
	}
	var resp hubapi.RelayPollResponse
	if err := d.hubSigned(ctx, hub, http.MethodPost, "/relay/poll", relayauth.ActionPoll,
		hubapi.RelayPollRequest{Limit: 100}, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// relayAck tells the hub these envelopes are handled; the hub deletes them.
func (d *Daemon) relayAck(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	hub := d.config().HubURL
	if hub == "" {
		return fmt.Errorf("anet: no hub configured")
	}
	return d.hubSigned(ctx, hub, http.MethodPost, "/relay/ack", relayauth.ActionAck,
		hubapi.RelayAckRequest{IDs: ids}, nil)
}

// --- background poll loop ---

// stopRelayLoop cancels the running loop, if any, and starts none.
//
// Separate from startRelayLoop because "poll a different hub" and "poll
// nothing" are different intentions, and the second one had no way to be
// expressed: a node that left its hub kept polling it.
func (d *Daemon) stopRelayLoop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.relayStop != nil {
		d.relayStop()
		d.relayStop = nil
	}
}

// startRelayLoop cancels any running loop and starts a fresh one against hubURL, under the daemon ctx.
func (d *Daemon) startRelayLoop(hubURL string) {
	d.mu.Lock()
	if d.relayStop != nil {
		d.relayStop()
	}
	loopCtx, cancel := context.WithCancel(d.ctx)
	d.relayStop = cancel
	d.mu.Unlock()
	d.goBackground(func() { d.relayLoop(loopCtx) })
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
			if err := d.pollOnce(pc); err != nil && ctx.Err() == nil && !errors.Is(err, errHubWire) {
				// A hub below wire 2 is reported once by checkHubWire;
				// repeating it every second would fill the log.
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

// pollOnce pulls the mailbox, runs each envelope through the receive
// pipeline (receive.go) and acks the ones the pipeline says to ack:
// accepted, or refused for a reason that will not change on retry. An
// envelope refused for a temporary reason (a store error, a message that
// arrived before its task) stays in the mailbox and is delivered again on a
// later poll.
func (d *Daemon) pollOnce(ctx context.Context) error {
	msgs, err := d.relayPoll(ctx)
	if err != nil {
		return err
	}
	var acked []int64
	for _, m := range msgs {
		env, derr := base64.StdEncoding.DecodeString(m.Envelope)
		if derr != nil {
			d.count(dropBadTransport)
			acked = append(acked, m.ID) // undecodable — drop
			continue
		}
		if d.receiveEnvelope(ctx, env).ack() {
			acked = append(acked, m.ID)
		}
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
	d.inFeed.wake() // and the modules' A2A backends (inbound_tasks.go)
}

// withServedCapabilities folds this node's actual capability ids into what
// it advertises.
//
// The advertised list and the served list were two separate things kept in
// step by hand, and nothing checked. A node could register "digest" while
// its provider answered "text.digest" — harmless while discovery searched
// prose, because "digest" is a substring of the goal text and the caller
// found it anyway. The moment discovery became exact, that node stopped
// being findable for the thing it actually does, and the directory was
// confidently wrong rather than vague.
//
// Operator labels are kept alongside rather than replaced. "coding" is not
// a capability id and is still how a person describes what they offer;
// what changes is that the ids a provider will actually answer are in
// there too, put there by the daemon that knows them.
func withServedCapabilities(declared []string, reg *provider.Registry) []string {
	if reg == nil {
		return declared
	}
	served := reg.Capabilities()
	if len(served) == 0 {
		return declared
	}
	seen := make(map[string]bool, len(declared)+len(served))
	out := make([]string, 0, len(declared)+len(served))
	for _, c := range append(append([]string{}, declared...), served...) {
		if c = strings.TrimSpace(c); c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

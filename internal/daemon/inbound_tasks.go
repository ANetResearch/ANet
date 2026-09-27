package daemon

// inbound_tasks.go is the kernel side of the provider-side A2A backends
// (A2A-DESIGN §11.6): module.InboundTaskHost, through which a module is
// handed the text tasks delegated to this node and answers them as this
// node.
//
// Which tasks is decided here and nowhere else (backendTakes):
//
//   - inbound and admitted (a held delegation is not an interaction until
//     the operator approves it), not a capability call, not public_cap, not
//     terminal;
//   - from a peer on the trust list and not on the deny list, both read at
//     the moment of the decision. A peer that is not trusted only when a
//     module declared a backend that accepts untrusted peers
//     (DeclareUntrustedBackend), which the configuration check refuses
//     together with policy open (§5.1), so such a peer is one on the allow
//     list or one the operator approved;
//   - with the requester's message the latest turn of the conversation. A
//     task comes again each time the requester adds a message.
//
// A task that goes to a backend does not also go to the auto-reply agent
// (autoReplyThread asks backendAnswers first): one of the two answers,
// never both. That holds while a module is subscribed; a daemon whose
// modules forward nothing answers as it always did. A forward that fails
// leaves the task in the inbox for the operator (MCP reply_task, the CLI),
// not for the auto-reply agent.
//
// What a module receives is the task's A2A projection with its files
// inline, anet.peer_aid and anet.trusted in its metadata, and a context id
// that is not the requester's (0017 Q23): backendContextID(peer, context).
// An agent behind a backend (Hermes, for one) keeps one conversation per
// context, and a context id is the requester's to choose. Two peers that
// name the same context, by accident or on purpose, would otherwise talk
// into one conversation, and each could read what the other said there.
// Derived from the peer as well, the id is the same for every task one peer
// sends in one context and never the same for two peers.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// inboundFeedInterval is how often a feed looks for tasks when nothing
// woke it; an arriving delegation or message wakes it at once
// (kickAutoReply). A variable so tests can shorten it.
var inboundFeedInterval = 3 * time.Second

// inboundFeedBuffer bounds the deliveries a subscriber may have unread.
const inboundFeedBuffer = 16

// inboundFeed is the set of subscriptions to inbound tasks, each with its
// own wake-up channel.
type inboundFeed struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func (f *inboundFeed) join() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = map[chan struct{}]struct{}{}
	}
	ch := make(chan struct{}, 1)
	f.subs[ch] = struct{}{}
	return ch
}

func (f *inboundFeed) leave(ch chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.subs, ch)
}

// active reports whether any module is subscribed.
func (f *inboundFeed) active() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs) > 0
}

// wake asks every subscription to look for tasks now. It never blocks.
func (f *inboundFeed) wake() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// backendTakes is the one decision of which inbound tasks a module's
// backend may have: ok says it may, trusted whether the peer is on the
// trust list. untrustedDeclared is the DeclareUntrustedBackend declaration.
func backendTakes(peer, trust string, isCapability bool, ps peerSets, untrustedDeclared bool) (trusted, ok bool) {
	if isCapability || trust == interactions.TrustPublicCap || peer == "" || ps.denied(peer) {
		return false, false
	}
	trusted = ps.trusted(peer)
	return trusted, trusted || untrustedDeclared
}

// backendAnswers reports whether a thread is one a module's backend is
// given, and so one the auto-reply agent leaves alone.
func (d *Daemon) backendAnswers(th Thread) bool {
	if th.Role != string(interactions.RoleInbound) || !d.inFeed.active() {
		return false
	}
	_, ok := backendTakes(th.Peer, th.Trust, th.IsCapability, d.readPeers(), d.untrustedBackend.Load())
	return ok
}

// backendContextID is the context id a backend sees for a task a peer sent
// in context contextID (0017 Q23): one per (peer, context), never shared
// between two peers, and not the id the requester chose.
func backendContextID(peer, contextID string) string {
	h := sha256.Sum256([]byte("anet/backend-context/1\x00" + peer + "\x00" + contextID))
	return "anet-ctx-" + hex.EncodeToString(h[:16])
}

// InboundTasks implements module.InboundTaskHost.
func (h moduleHost) InboundTasks(ctx context.Context) (<-chan module.Task, error) {
	return h.d.inboundTasks(ctx)
}

// ReplyTask implements module.InboundTaskHost.
func (h moduleHost) ReplyTask(ctx context.Context, taskID string, msg a2ashape.Message, state a2ashape.TaskState) (module.Task, error) {
	return h.d.replyInbound(ctx, taskID, msg, state)
}

var _ module.InboundTaskHost = moduleHost{}

// inboundTasks subscribes to inbound tasks until ctx or the daemon ends.
func (d *Daemon) inboundTasks(ctx context.Context) (<-chan module.Task, error) {
	out := make(chan module.Task, inboundFeedBuffer)
	kick := d.inFeed.join()
	if !d.goBackground(func() { d.runInboundFeed(ctx, kick, out) }) {
		d.inFeed.leave(kick)
		close(out)
		return nil, errors.New("anet: the daemon is shutting down")
	}
	return out, nil
}

func (d *Daemon) runInboundFeed(ctx context.Context, kick chan struct{}, out chan<- module.Task) {
	defer close(out)
	defer d.inFeed.leave(kick)
	// sent maps a task to what was last delivered of it (deliveryKey), so
	// a task is delivered once per message from the requester. It is in
	// memory: after a restart a task that still waits for an answer is
	// delivered again.
	sent := map[string]string{}
	t := time.NewTicker(inboundFeedInterval)
	defer t.Stop()
	for {
		if !d.feedInbound(ctx, out, sent) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
	}
}

// feedInbound delivers every task a backend may have that waits for an
// answer and was not delivered with its latest message yet. It returns
// false when the feed is to stop.
func (d *Daemon) feedInbound(ctx context.Context, out chan<- module.Task, sent map[string]string) bool {
	stopped := func() bool { return ctx.Err() != nil || d.ctx.Err() != nil }
	list, err := d.ix.ListAll(interactions.ListFilter{Role: interactions.RoleInbound, Active: true,
		ExcludeCapability: true, ExcludeTrust: []string{interactions.TrustPublicCap}})
	if err != nil {
		if !stopped() {
			log.Printf("anet: inbound tasks for modules: %v", err)
		}
		return !stopped()
	}
	ps := d.readPeers()
	untrusted := d.untrustedBackend.Load()
	live := make(map[string]bool, len(list))
	for _, ix := range list {
		trusted, ok := backendTakes(ix.PeerAID, ix.Trust, ix.IsCapability, ps, untrusted)
		if !ok {
			continue
		}
		live[ix.ID] = true
		view, err := d.taskView(ix, viewOpts{})
		if err != nil {
			continue
		}
		key, owed := deliveryKey(view)
		if !owed || sent[ix.ID] == key {
			continue
		}
		t, err := d.inboundView(ix, trusted, true)
		if err != nil {
			log.Printf("anet: inbound task %s for modules: %v", ix.ID, err)
			continue
		}
		select {
		case out <- t:
			sent[ix.ID] = key
		case <-ctx.Done():
			return false
		case <-d.ctx.Done():
			return false
		}
	}
	for id := range sent {
		if !live[id] {
			delete(sent, id)
		}
	}
	return true
}

// deliveryKey names a task's latest turn when it is the requester's: the
// length of the history and the message's id, so a requester that reuses a
// message id is not taken for one that sent nothing new.
func deliveryKey(t a2ashape.Task) (key string, owed bool) {
	n := len(t.History)
	if n == 0 || t.History[n-1].Role != a2ashape.RoleUser {
		return "", false
	}
	return strconv.Itoa(n) + ":" + t.History[n-1].ID, true
}

// inboundView is the task as a module sees it: the projection, anet.trusted,
// and the backend's context id in place of the requester's.
func (d *Daemon) inboundView(ix *interactions.Interaction, trusted, inline bool) (a2ashape.Task, error) {
	t, err := d.taskView(ix, viewOpts{inline: inline})
	if err != nil {
		return a2ashape.Task{}, err
	}
	t.Metadata[a2ashape.KeyPeerAID] = ix.PeerAID
	t.Metadata[a2ashape.KeyTrusted] = trusted
	cid := backendContextID(ix.PeerAID, t.ContextID)
	t.ContextID = cid
	for i := range t.History {
		t.History[i].ContextID = cid
	}
	if t.Status.Message != nil {
		m := *t.Status.Message
		m.ContextID = cid
		t.Status.Message = &m
	}
	return t, nil
}

// replyInbound answers a task a module was given (ReplyTask). Any other
// task — one sent by this node, a capability call, a peer the backend may
// not serve (any more) — is TaskNotFound, whichever it is.
func (d *Daemon) replyInbound(ctx context.Context, taskID string, msg a2ashape.Message, state a2ashape.TaskState) (a2ashape.Task, error) {
	notFound := a2ashape.Errorf(a2ashape.ErrTaskNotFound, "%s", taskID)
	if taskID == "" {
		return a2ashape.Task{}, notFound
	}
	ix, err := d.ix.Get(taskID)
	if errors.Is(err, interactions.ErrNotFound) {
		return a2ashape.Task{}, notFound
	}
	if err != nil {
		return a2ashape.Task{}, err
	}
	if ix.Role != interactions.RoleInbound {
		return a2ashape.Task{}, notFound
	}
	trusted, ok := backendTakes(ix.PeerAID, ix.Trust, ix.IsCapability, d.readPeers(), d.untrustedBackend.Load())
	if !ok {
		return a2ashape.Task{}, notFound
	}
	st, _ := a2ashape.StoreState(state)
	switch st {
	case interactions.StateInputRequired, interactions.StateCompleted, interactions.StateFailed, interactions.StateRejected:
	default:
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrInvalidParams,
			"an answer leaves the task input-required, completed, failed or rejected, not %q", state)
	}
	req := replyReq{TaskID: ix.ID, State: string(st)}
	if len(msg.Parts) > 0 {
		// The answer is its parts. Its metadata is not sent on (anet.skill
		// there would read as a capability call), and its ids are the
		// backend's, not this task's.
		m := a2ashape.Message{Role: msg.Role, Parts: msg.Parts}
		req.Message = &m
	}
	if _, err := d.replyTask(ctx, req); err != nil {
		return a2ashape.Task{}, err
	}
	cur, err := d.ix.Get(ix.ID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	return d.inboundView(cur, trusted, false)
}

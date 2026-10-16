package daemon

// taskseam.go is the daemon's A2A task surface (A2A-DESIGN §11.1, §11.5,
// §12). One implementation serves two callers:
//
//   - module/a2a, through TaskSeam, scoped to one remote agent. Every
//     operation that names a peer acts only on outbound interactions with
//     that peer; anything else — an inbound task, another agent's task, an
//     id that does not exist — is the same TaskNotFound, from one lookup
//     that compares role and peer as part of finding the row [C17].
//   - the control plane (/tasks/*, tasks_api.go), unscoped: the control
//     token is the node's full authority.
//
// SendMessage (sendTask):
//
//   - no taskId: a new task. A contextId the client gives is kept as it is
//     (C22); for a scoped caller it must not belong to another agent's
//     tasks (TaskNotFound, as for another agent's task id), and an unknown
//     one starts a new context. Without one the
//     daemon mints it. A client messageId makes the message a retry of an
//     earlier one under the same id, with the agent and the contextId (or,
//     without one, any of the agent's contexts: the retry cannot name the
//     context the daemon minted), when the content is the same and the
//     task is open — or ended moments ago after a call that was cut off
//     before its answer; the same id with other content on an open task is
//     refused, and on a finished one starts a new task (clientdedupe.go,
//     correctness-ux CUX-D1 to D5; an internal note item). Only this node's own
//     messages carry the key for it [redteam:F33]. The delegation
//     goes through the retry queue (an internal note item): once the task is recorded
//     the call answers submitted, and an unreachable provider or hub only
//     delays it. A delegation the queue
//     gives up on (expired, or refused for good by the hub) fails the task
//     with anet.reason=undeliverable, which was never run: a retry of its
//     message is a new attempt. The client's messageId is also kept in
//     message.metadata["a2a.messageId"] for the projection; the
//     envelope carries a daemon-minted id. One leniency (an internal note item,
//     taskseam_context.go): on the A2A interface, a contextId whose only
//     input-required task of this endpoint waits for it is continued
//     rather than given a new task.
//   - a taskId: a follow-up on that task. A client-chosen id for a task
//     that does not exist is TaskNotFound, never a new task. Input to a
//     terminal task is UnsupportedOperation (§4.2). A follow-up goes
//     through the retry queue: once recorded it will be delivered, so a
//     retry that finds it recorded can wait for the answer.
//   - metadata["anet.skill"] or a DataPart {skill, args} is a capability
//     call. Text parts are joined into the goal; a raw file part becomes an
//     attachment through attachmentFromBytes; a url part of any scheme is
//     InvalidParams — the daemon fetches nothing and reads no local path.
//   - unless returnImmediately, the call waits for a terminal or
//     interrupted (input-required) state whose state_seq is above the one
//     its own write left (C35), so a follow-up on an input-required task
//     does not return the old question as the answer. A client that gives
//     up waiting does not cancel the task: it runs on, and a retry with a
//     new messageId is a second task. ListTasks by contextId finds both.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// DaemonTaskSeam is the daemon's TaskSeam (A2A-DESIGN §11.1): the task
// operations scoped to one remote agent, for module/a2a. Every method
// refuses an empty peerAID with TaskNotFound; the unscoped form exists only
// on the control plane.
type DaemonTaskSeam struct{ d *Daemon }

// TaskSeam returns the daemon's task seam.
func (d *Daemon) TaskSeam() *DaemonTaskSeam { return &DaemonTaskSeam{d: d} }

// Send is A2A SendMessage to peerAID.
func (s *DaemonTaskSeam) Send(ctx context.Context, peerAID string, req module.TaskSend) (a2ashape.Task, error) {
	sc, err := peerScope(peerAID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	return s.d.sendTask(ctx, sc, peerAID, req, 0)
}

// PreviewAvailable implements module.TaskSeam (K-10): the local A2A
// interface activates and declares the preview extension only when this
// node has previews.
func (s *DaemonTaskSeam) PreviewAvailable() bool { return s.d.previewAvailable() }

// SendGate is nil when this node may send now, otherwise the refusal Send
// would give (*a2ashape.Refusal; LEC AD-6): the local A2A interface answers
// it before the binding, in the two-ErrorInfo form (ux UX-R10).
func (s *DaemonTaskSeam) SendGate(context.Context) error {
	if ref := s.d.sendGate(); ref != nil {
		return ref
	}
	return nil
}

// Get is A2A GetTask.
func (s *DaemonTaskSeam) Get(_ context.Context, peerAID, taskID string, historyLen *int) (a2ashape.Task, error) {
	sc, err := peerScope(peerAID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	return s.d.getTask(sc, taskID, historyLen)
}

// List is A2A ListTasks over this peer's tasks.
func (s *DaemonTaskSeam) List(_ context.Context, peerAID string, f module.TaskFilter) (a2ashape.TaskPage, error) {
	sc, err := peerScope(peerAID)
	if err != nil {
		return a2ashape.TaskPage{}, err
	}
	return s.d.listTasks(sc, taskListReq{TaskFilter: f})
}

// Cancel is A2A CancelTask.
func (s *DaemonTaskSeam) Cancel(ctx context.Context, peerAID, taskID string) (a2ashape.Task, error) {
	sc, err := peerScope(peerAID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	return s.d.cancelTask(ctx, sc, taskID)
}

// Watch returns the task and the events that follow it (SubscribeToTask,
// SendStreamingMessage). The snapshot and the subscription are taken
// atomically; the channel closes after the event of a terminal state, or
// when ctx ends.
func (s *DaemonTaskSeam) Watch(ctx context.Context, peerAID, taskID string, opts module.WatchOpts) (a2ashape.Task, <-chan a2ashape.TaskEvent, error) {
	sc, err := peerScope(peerAID)
	if err != nil {
		return a2ashape.Task{}, nil, err
	}
	return s.d.watchTask(ctx, sc, taskID, opts)
}

// Agents lists agents of the network (A2A-DESIGN §10.5).
func (s *DaemonTaskSeam) Agents(ctx context.Context, q module.AgentQuery) ([]module.RemoteAgent, error) {
	agents, _, err := s.d.listAgents(ctx, q)
	return agents, err
}

// Card returns one agent's network card and this node's verification of it.
func (s *DaemonTaskSeam) Card(ctx context.Context, aid string) (module.RemoteAgent, error) {
	return s.d.agentCard(ctx, aid)
}

// ExtendedCard is the local A2A GetExtendedAgentCard (HPP §4.7, CORE-K23):
// the agent's extended card through the same fetch as /agents/card, or the
// refusal of the extended_card_* code its outcome is. A request that could
// not go because this node may not send (it is not registered at its hub,
// or its identity is stopped) is refused with that state's sentence.
func (s *DaemonTaskSeam) ExtendedCard(ctx context.Context, aid string) (module.ExtendedAgent, error) {
	ra, res, err := s.d.agentCardWithPrices(ctx, aid)
	if err != nil {
		return module.ExtendedAgent{}, err
	}
	if res.Outcome == extNotSent {
		if ref := s.d.sendGate(); ref != nil {
			return module.ExtendedAgent{}, ref
		}
	}
	if ref := extendedRefusal(res); ref != nil {
		return module.ExtendedAgent{}, ref
	}
	return module.ExtendedAgent{Agent: ra, Card: append([]byte(nil), res.Card.Bytes...)}, nil
}

// CanPay is TaskSeam.CanPay (K-11): this node can sign a payment and settle
// it now. Worked out at each call. The PaymentSeam is granted as soon as a
// daemon exists, so holding it says nothing; what makes a payment possible is
// a payer, a hub address and the hub's AID, and the last two appear when the
// node registers, after start.
func (s *DaemonTaskSeam) CanPay() bool {
	return s.d.canPay()
}

// canPay is the K-11 condition.
func (d *Daemon) canPay() bool {
	return d.payer() != nil && strings.TrimSpace(d.config().HubURL) != "" && d.hubAID() != ""
}

// Pay answers a payment-required task at the agent tier (A2A-DESIGN §8.7).
func (s *DaemonTaskSeam) Pay(ctx context.Context, peerAID, taskID string, decision module.PayDecision) (a2ashape.Task, error) {
	sc, err := peerScope(peerAID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	ix, err := s.d.scopedTask(sc, taskID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	answer, err := s.d.taskPay(ctx, ix, decision)
	if err != nil {
		return a2ashape.Task{}, err
	}
	cur, err := s.d.ix.Get(ix.ID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	t, err := s.d.taskView(cur, viewOpts{artifacts: true, inline: sc.inline()})
	if err == nil && answer != nil {
		t = answer.apply(t)
	}
	return t, err
}

var _ module.TaskSeam = (*DaemonTaskSeam)(nil)

// taskScope limits a task operation. The zero value matches nothing.
type taskScope struct {
	// peer limits the operation to outbound interactions with this peer.
	peer string
	// all is the control plane: every interaction, either role.
	all bool
}

func peerScope(peerAID string) (taskScope, error) {
	if peerAID == "" {
		return taskScope{}, a2ashape.Errorf(a2ashape.ErrTaskNotFound, "no agent named")
	}
	return taskScope{peer: peerAID}, nil
}

var controlScope = taskScope{all: true}

// inline says whether one task read for this scope carries attachment
// bytes (see viewOpts.inline): the A2A interface does, up to the inline
// limit and never in the history, and the control plane does not. A list
// and a stream never do, for either (an internal note item).
func (sc taskScope) inline() bool { return !sc.all }

// scopedTask finds a task the scope may see.
func (d *Daemon) scopedTask(sc taskScope, id string) (*interactions.Interaction, error) {
	var ix *interactions.Interaction
	var err error
	switch {
	case id == "":
		return nil, a2ashape.Errorf(a2ashape.ErrInvalidParams, "task id required")
	case sc.all:
		ix, err = d.ix.Get(id)
	case sc.peer != "":
		ix, err = d.ix.GetFor(id, interactions.RoleOutbound, sc.peer)
	default:
		err = interactions.ErrNotFound
	}
	if errors.Is(err, interactions.ErrNotFound) {
		return nil, d.noLocalTask(id)
	}
	return ix, err
}

// noLocalTask is the answer for a task this node does not have, whether it
// never did, belongs to another agent's scope, or was deleted by the
// retention (or by the loss of its data key): one sentence, no_local_task
// (FS-U5), that says it may have been deleted and where the evidence is.
// It is a TaskNotFoundError on the local A2A interface and a 404 on the
// control plane (errors.Is(err, a2ashape.ErrTaskNotFound) holds).
func (d *Daemon) noLocalTask(id string) error {
	return a2ashape.Refuse("no_local_task", a2ashape.Args{"task": id, "retention": d.ContentRetention().String()})
}

func (d *Daemon) getTask(sc taskScope, id string, historyLen *int) (a2ashape.Task, error) {
	if err := checkHistoryLen(historyLen); err != nil {
		return a2ashape.Task{}, err
	}
	ix, err := d.scopedTask(sc, id)
	if err != nil {
		return a2ashape.Task{}, err
	}
	return d.taskView(ix, viewOpts{historyLen: historyLen, artifacts: true, inline: sc.inline()})
}

func checkHistoryLen(n *int) error {
	if n != nil && *n < 0 {
		return a2ashape.Errorf(a2ashape.ErrInvalidParams, "historyLength must not be negative")
	}
	return nil
}

// --- SendMessage ---

// sendLocks serializes sends that share a client messageId, so two copies of
// one client message sent at once create one task. Keys carry this node's AID
// (tests run several daemons in one process) and the remote agent.
var sendLocks keyedLocks

// lockSend takes the dedupe lock of one client message. The returned
// function releases it and may be called more than once: a send releases it
// as soon as the message is recorded, before it waits for the answer, and a
// deferred call covers the error paths.
func lockSend(scope, contextID, messageID string) func() {
	release := sendLocks.lock(scope + "\x00" + contextID + "\x00" + messageID)
	var once sync.Once
	return func() { once.Do(release) }
}

// taskWaitMax is how long the control plane blocks a send or a wait when
// the caller names no limit. The A2A interface has no limit of its own: the
// client's request context ends the wait.
const taskWaitMax = 60 * time.Second

// withoutPreview is req with the request for previews taken out: the
// anet.preview_mode it would set, and the extension in the activated list.
func withoutPreview(req module.TaskSend) module.TaskSend {
	req.Preview = ""
	if slices.Contains(req.Extensions, module.ExtPreviewURI) {
		req.Extensions = slices.DeleteFunc(slices.Clone(req.Extensions), func(u string) bool { return u == module.ExtPreviewURI })
	}
	return req
}

// sendTask is SendMessage. to names the remote agent of a new task (for a
// scoped caller it is the scope's peer). wait bounds a blocking call; ≤ 0
// waits until ctx ends. When the bound elapses the task is returned as it
// is, not an error: it is still running.
func (d *Daemon) sendTask(ctx context.Context, sc taskScope, to string, req module.TaskSend, wait time.Duration) (a2ashape.Task, error) {
	if err := checkHistoryLen(req.HistoryLength); err != nil {
		return a2ashape.Task{}, err
	}
	if !d.previewAvailable() {
		// No preview module: a message never asks for one, and the
		// extension is not echoed in the stored service parameters
		// (11-modularity §5.4, MOD-F4).
		req = withoutPreview(req)
	}
	msg := req.Message
	if msg.TaskID != "" {
		if ref := d.sendGate(); ref != nil {
			return a2ashape.Task{}, ref
		}
		return d.appendTask(ctx, sc, req, wait)
	}
	peer := to
	if !sc.all {
		peer = sc.peer
	}
	if peer == "" {
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrInvalidParams, "the remote agent (to) is required for a new task")
	}
	if peer == d.AID() {
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrInvalidParams, "cannot send a task to this node itself")
	}
	if !validAgentID(peer) {
		// Refused before anything is written: a malformed id would only
		// make a task that fails at the hub.
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrInvalidParams, "%q is not an agent id", peer)
	}
	in, err := parseTaskInput(msg, a2ashape.RoleUser)
	if err != nil {
		return a2ashape.Task{}, err
	}
	// Until the hub registered this node a send fails at once with the
	// join state's sentence, and no outbound row is written (LEC AD-6, ux
	// UX-R7): the control plane, MCP and the local A2A interface all come
	// through here. A request malformed in itself is told so first.
	if ref := d.sendGate(); ref != nil {
		return a2ashape.Task{}, ref
	}
	contextID := msg.ContextID
	if contextID != "" && !sc.all {
		// §11.1: a context belongs to the agent its tasks were sent to.
		// Only outbound tasks count: an inbound task's context id was
		// chosen by a remote requester and says nothing about this
		// client's contexts.
		peers, err := d.ix.ContextPeers(interactions.RoleOutbound, contextID)
		if err != nil {
			return a2ashape.Task{}, err
		}
		for _, p := range peers {
			if p != peer {
				// The same answer as for another agent's task id: a client
				// scoped to one agent learns nothing about the others.
				return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrTaskNotFound, "context %s", contextID)
			}
		}
	}
	release := func() {}
	cm, err := newClientMark(msg.ID, inputContent(in, msg, req.Extensions))
	if err != nil {
		return a2ashape.Task{}, err
	}
	var ans func(ix string) *answered
	if cm != nil {
		// Without a contextId the key is (agent, messageId): the lock key
		// then has an empty context, and the lookup spans the agent's
		// contexts. (The key is taken now: contextID is minted below.)
		lockCtx := contextID
		lock := func() func() { return lockSend(d.AID()+"\x00"+peer, lockCtx, msg.ID) }
		release = lock()
		defer release()
		ans = func(ix string) *answered { return &answered{lock: lock, ix: ix, key: cm.key} }
		m, err := d.judgeClientMessage(cm, interactions.ClientKeyQuery{PeerAID: peer, ContextID: contextID})
		if err != nil {
			return a2ashape.Task{}, err
		}
		switch m.verdict {
		case clientRetry:
			// A retry of a message that already made a task: that task,
			// and nothing is sent again.
			release()
			return dedupedTask(d.finishSend(ctx, sc, m.ix.ID, 0, req, wait, ans(m.ix.ID)))
		case clientReusedOpen, clientReusedFollowUp:
			return a2ashape.Task{}, reusedRefusal(m)
		}
	}
	// an internal note item: a contextId-only message may continue the one task of this
	// endpoint that waits for input there (taskseam_context.go).
	if cont, err := d.continuedTask(sc, peer, contextID, in, msg.Metadata); err != nil {
		return a2ashape.Task{}, err
	} else if cont != "" {
		release() // appendTask takes the same dedupe lock
		req.Message.TaskID = cont
		return d.appendTask(ctx, sc, req, wait)
	}
	if contextID == "" {
		if contextID, err = newContextID(); err != nil {
			return a2ashape.Task{}, err
		}
	}
	if d.config().HubURL == "" {
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrUnavailable, "this node has no hub (run `anet hub-register` first)")
	}
	id, err := newInteractionID()
	if err != nil {
		return a2ashape.Task{}, err
	}
	meta := clientMeta(req)
	if in.capID != "" {
		delete(meta, metaPreviewMode) // previews are for text tasks (CUX-P4)
	}
	sctx, cancel := context.WithTimeout(ctx, relayCallTimeout)
	defer cancel()
	if in.capID != "" {
		_, err = d.delegateCapabilityCtx(sctx, id, peer, in.capID, in.args, in.atts, contextID, cm)
	} else {
		var mb []byte
		if len(meta) > 0 {
			if mb, err = json.Marshal(meta); err != nil {
				return a2ashape.Task{}, err
			}
		}
		err = d.delegateInWithID(sctx, id, peer, in.goal(), in.atts, contextID, mb, cm)
	}
	var mark *answered
	if ans != nil {
		mark = ans(id)
	}
	switch {
	case errors.Is(err, errUndeliverable):
		// Recorded, and refused for good by the hub on the first attempt:
		// the task is failed (anet.reason=undeliverable) and answered as it
		// is. A retry of the same message is a new attempt.
		release()
		return d.finishSend(ctx, sc, id, 0, req, wait, mark)
	case err != nil:
		// Nothing was recorded: a recipient no hub knows or that does not
		// verify (a refusal of the reason table, engine ENG-U5), or the
		// store.
		var ref *a2ashape.Refusal
		if errors.As(err, &ref) {
			return a2ashape.Task{}, ref
		}
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrUnavailable, "%v", err)
	}
	if in.capID != "" && len(meta) > 0 {
		// The capability path writes its first message without metadata;
		// the client's message id is added to it here, still under the send
		// lock, so a concurrent retry either sees it or waits for it. It
		// stays local: a capability call carries its input as args.
		if first, ok := d.firstOwnMessage(id); ok {
			if err := d.ix.MergeMessageMeta(id, first, meta); err != nil {
				return a2ashape.Task{}, err
			}
		}
	}
	// Recorded: a retry now finds the task, so it need not wait for this
	// call to finish waiting.
	release()
	// A new task is written at state_seq 1 and nothing but the provider's
	// answer moves it on.
	return d.finishSend(ctx, sc, id, 1, req, wait, mark)
}

// dedupedTask is a retry's answer: the task, marked as the answer to a retry
// (anet.deduplicated, CUX-D3).
func dedupedTask(t a2ashape.Task, err error) (a2ashape.Task, error) {
	if err != nil {
		return t, err
	}
	return a2ashape.Deduplicated(t), nil
}

// appendTask is SendMessage with a taskId: a follow-up on a task.
func (d *Daemon) appendTask(ctx context.Context, sc taskScope, req module.TaskSend, wait time.Duration) (a2ashape.Task, error) {
	msg := req.Message
	ix, err := d.scopedTask(sc, msg.TaskID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	if ix.Role != interactions.RoleOutbound {
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrInvalidParams, "%s was delegated to this node; answer it with /tasks/reply", ix.ID)
	}
	if msg.ContextID != "" && msg.ContextID != ix.ContextID {
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrInvalidParams, "task %s is in context %q, not %q", ix.ID, ix.ContextID, msg.ContextID)
	}
	payment := hasX402Meta(msg.Metadata)
	// The content a messageId is held to (CUX-D1): a payment message's
	// decision (CUX-D6), or the input of any other.
	var in *taskInput
	var content clientContent
	if payment {
		content = paymentContent(msg, req.Extensions)
	} else {
		if in, err = parseTaskInput(msg, a2ashape.RoleUser); err != nil {
			return a2ashape.Task{}, err
		}
		content = inputContent(in, msg, req.Extensions)
	}
	cm, err := newClientMark(msg.ID, content)
	if err != nil {
		return a2ashape.Task{}, err
	}
	release := func() {}
	var mark *answered
	if cm != nil {
		lock := func() func() { return lockSend(d.AID()+"\x00"+ix.PeerAID, ix.ContextID, msg.ID) }
		release = lock()
		defer release()
		mark = &answered{lock: lock, ix: ix.ID, key: cm.key}
		m, err := d.judgeClientMessage(cm, interactions.ClientKeyQuery{TaskID: ix.ID})
		if err != nil {
			return a2ashape.Task{}, err
		}
		switch m.verdict {
		case clientRetry:
			release()
			return dedupedTask(d.finishSend(ctx, sc, ix.ID, 0, req, wait, mark))
		case clientReusedOpen, clientReusedFollowUp:
			return a2ashape.Task{}, reusedRefusal(m)
		}
	}
	if payment {
		// A payment decision on the task (A2A-DESIGN §8.7). This is the one
		// path a local client's payment message takes, so its messageId is
		// held to its content above like any other message's: the same
		// decision again is signed once, submit and reject under one id are
		// refused (CUX-D6).
		after, answer, err := d.taskPaymentMessage(ctx, ix, msg, cm)
		if err != nil {
			return a2ashape.Task{}, err
		}
		release()
		if answer != nil {
			// Nothing was signed or sent: the task as it is, told why
			// (a refusal, §8.7) or who decides now (the operator, §8.3).
			cur, err := d.ix.Get(ix.ID)
			if err != nil {
				return a2ashape.Task{}, err
			}
			t, err := d.taskView(cur, viewOpts{historyLen: req.HistoryLength, artifacts: true, inline: sc.inline()})
			if err != nil {
				return a2ashape.Task{}, err
			}
			return answer.apply(t), nil
		}
		return d.finishSend(ctx, sc, ix.ID, after, req, wait, mark)
	}
	if ix.IsTerminal() {
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s is %s and takes no new input", ix.ID, ix.State)
	}
	if ix.IsCapability {
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s is a capability call; it takes no follow-up message", ix.ID)
	}
	if in.capID != "" {
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrInvalidParams, "a capability call starts a new task; send it without a taskId")
	}
	sctx, cancel := context.WithTimeout(ctx, relayCallTimeout)
	defer cancel()
	after, err := d.sendMessageMarked(sctx, ix.ID, in.goal(), in.atts, clientMeta(req), cm)
	switch {
	case errors.Is(err, ErrTaskTerminal):
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "%v", err)
	case errors.Is(err, errUndeliverable):
		// Recorded, and refused for good by the hub on the first attempt:
		// the task is failed (anet.reason=undeliverable) and answered as it
		// is, as for a new task (sendTask).
	case err != nil:
		// Delivery is queued and cannot fail here otherwise; this is the
		// store.
		return a2ashape.Task{}, err
	}
	release()
	return d.finishSend(ctx, sc, ix.ID, after, req, wait, mark)
}

// finishSend answers a send: at once, or once the task is terminal or
// interrupted with a state_seq above after. after 0 on a retry means the
// seq of the original write is not known; any terminal or interrupted
// state then ends the wait.
//
// mark is the client message the call stands for (nil: the client named no
// messageId). When the call's request has ended by the time the answer is
// ready — the client went away or gave up — the message is marked
// unanswered, so that a retry of it is still given this task for a while
// after it ends (CUX-D4, D5). An answer the client did get leaves it
// unmarked.
func (d *Daemon) finishSend(ctx context.Context, sc taskScope, id string, after int64, req module.TaskSend,
	wait time.Duration, mark *answered) (a2ashape.Task, error) {
	var ix *interactions.Interaction
	var err error
	if req.ReturnImmediately {
		ix, err = d.ix.Get(id)
	} else {
		ix, _, err = d.waitTask(ctx, id, after, wait)
	}
	if ctx.Err() != nil {
		d.noteUnanswered(mark)
	}
	if err != nil {
		return a2ashape.Task{}, err
	}
	return d.taskView(ix, viewOpts{historyLen: req.HistoryLength, artifacts: true, inline: sc.inline()})
}

// firstOwnMessage is the seq of the first message this node stored on id.
func (d *Daemon) firstOwnMessage(id string) (int64, bool) {
	msgs, err := d.ix.Messages(id)
	if err != nil {
		return 0, false
	}
	for _, m := range msgs {
		if m.SenderAID == d.AID() {
			return m.Seq, true
		}
	}
	return 0, false
}

// clientMeta is the metadata stored and sent with a client's message: the
// client's own keys, minus the reserved anet.*, x402.* and a2a.* namespaces;
// its message id under a2a.messageId; and the extensions it activated as
// a2a.serviceParameters, the relay's stand-in for the A2A-Extensions header
// (A2A-DESIGN §3.4).
func clientMeta(req module.TaskSend) map[string]any {
	m := req.Message
	out := map[string]any{}
	for k, v := range m.Metadata {
		if strings.HasPrefix(k, "anet.") || strings.HasPrefix(k, "x402.") || strings.HasPrefix(k, "a2a.") {
			continue
		}
		out[k] = v
	}
	if m.ID != "" {
		out[a2ashape.KeyMessageID] = m.ID
	}
	if mode := normalPreviewMode(req.Preview); mode != "" {
		// What the round asks of the provider (CUX-P10); the provider reads
		// it with the message, before anet.* is stripped for storage.
		out[metaPreviewMode] = mode
	}
	if len(req.Extensions) > 0 {
		out[a2ashape.KeyServiceParameters] = map[string]any{"A2A-Extensions": req.Extensions, "A2A-Version": "1.0"}
	}
	return out
}

// peerMessageMeta is the metadata a peer sent with its first message, as
// stored: a JSON object without the reserved anet.* and x402.* keys, which
// only this node's own code writes (a requester that set anet.state on its
// goal would take its goal out of the transcript the receipt covers). nil
// when nothing is left.
func peerMessageMeta(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	for k := range m {
		if strings.HasPrefix(k, "anet.") || strings.HasPrefix(k, "x402.") {
			delete(m, k)
		}
	}
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

func hasX402Meta(m map[string]any) bool {
	for k := range m {
		if strings.HasPrefix(k, "x402.") {
			return true
		}
	}
	return false
}

// taskInput is a client message read by the input part rules [C44].
type taskInput struct {
	texts []string
	atts  []delegation.Attachment
	capID string
	args  map[string]any
}

// goal is the text of the message; a message of attachments only names
// them, because a task needs a goal.
func (in *taskInput) goal() string {
	g := strings.TrimSpace(strings.Join(in.texts, "\n"))
	if g == "" && len(in.atts) > 0 {
		names := make([]string, len(in.atts))
		for i, a := range in.atts {
			names[i] = a.Name
		}
		g = "attachments: " + strings.Join(names, ", ")
	}
	return g
}

// parseTaskInput applies the input part rules (A2A-DESIGN §11.5 [C44]).
// role is the side the message speaks for: the user for a requester's
// message, the agent for a provider's reply. A message may leave its role
// unset; one that names the other side is refused.
func parseTaskInput(m a2ashape.Message, role a2ashape.Role) (*taskInput, error) {
	bad := func(format string, a ...any) error {
		return a2ashape.Errorf(a2ashape.ErrInvalidParams, format, a...)
	}
	if m.Role != role && m.Role != a2ashape.RoleUnspecified {
		return nil, bad("role must be %s", role)
	}
	in := &taskInput{}
	if v, ok := m.Metadata[a2ashape.KeySkill]; ok {
		id, _ := v.(string)
		if strings.TrimSpace(id) == "" {
			return nil, bad("metadata %s must be a capability id", a2ashape.KeySkill)
		}
		in.capID = id
		if a, ok := m.Metadata["anet.args"]; ok && a != nil {
			obj, ok := a.(map[string]any)
			if !ok {
				return nil, bad("metadata anet.args must be an object")
			}
			in.args = obj
		}
	}
	var loose []map[string]any // data objects that are not {skill, args}
	var looseJSON []string
	for i, p := range m.Parts {
		switch p.Kind {
		case a2ashape.PartURL:
			// Refused whatever the scheme: the daemon does not fetch, and
			// does not read local paths (file:), for anyone.
			return nil, bad("part %d: url parts are not accepted (the daemon fetches nothing and reads no local file); send the bytes as a raw part", i)
		case a2ashape.PartText:
			in.texts = append(in.texts, p.Text)
		case a2ashape.PartRaw:
			att, err := attachmentFromBytes(p.Filename, p.Raw)
			if err != nil {
				return nil, bad("part %d: %v", i, err)
			}
			if mt, _, err := mime.ParseMediaType(p.MediaType); err == nil && p.MediaType != "" {
				att.Mime = mt
			}
			in.atts = append(in.atts, att)
		case a2ashape.PartData:
			v := p.Data
			obj, isObj := v.(map[string]any)
			if isObj {
				if sk, ok := obj["skill"]; ok {
					id, _ := sk.(string)
					if strings.TrimSpace(id) == "" {
						return nil, bad("part %d: skill must be a capability id", i)
					}
					if in.capID != "" && in.capID != id {
						return nil, bad("part %d: the message names two capabilities (%s, %s)", i, in.capID, id)
					}
					in.capID = id
					if a, ok := obj["args"]; ok && a != nil {
						ao, ok := a.(map[string]any)
						if !ok {
							return nil, bad("part %d: args must be an object", i)
						}
						in.args = ao
					}
					continue
				}
				loose = append(loose, obj)
			}
			b, err := json.Marshal(v)
			if err != nil {
				return nil, bad("part %d: data: %v", i, err)
			}
			looseJSON = append(looseJSON, string(b))
		default:
			return nil, bad("part %d: exactly one of text, raw or data is required", i)
		}
	}
	if in.capID != "" {
		if len(in.texts) > 0 {
			return nil, bad("a capability call takes its input as args and files, not as text")
		}
		switch {
		case len(looseJSON) == 0:
		case in.args == nil && len(looseJSON) == 1 && len(loose) == 1:
			// metadata anet.skill with the args as a DataPart.
			in.args = loose[0]
		default:
			return nil, bad("a capability call carries one args object")
		}
		if in.args == nil {
			in.args = map[string]any{}
		}
		// Keys that start with "$" are anet's (CUX-F2): refused here, before
		// anything is written.
		if reservedArgsKey(in.args) != "" {
			return nil, a2ashape.Refuse("args_reserved_key", nil)
		}
		return in, nil
	}
	in.texts = append(in.texts, looseJSON...)
	if strings.TrimSpace(strings.Join(in.texts, "")) == "" && len(in.atts) == 0 {
		return nil, bad("the message has no content")
	}
	return in, nil
}

// --- waiting ---

// stopsWait reports whether a state ends a blocking send or a wait: a
// terminal state, or one in which the other side waits for this node. On a
// task this node sent, that is input-required (the agent asks); on a task
// delegated to it, any state but input-required (the requester answered,
// or the task is new).
func stopsWait(role interactions.Role, st interactions.State) bool {
	if st.IsTerminal() {
		return true
	}
	if role == interactions.RoleInbound {
		return st != interactions.StateInputRequired
	}
	return st == interactions.StateInputRequired
}

// waitTask blocks until the task is terminal, or waits for this node at a
// state_seq above after, or ctx ends, or max elapses (max ≤ 0: no bound). It
// returns the task as it then is; timedOut says the bound — or ctx's
// deadline — elapsed first: the task runs on (§11.5), and the caller
// answers it as it is. A canceled ctx is an error: nobody is waiting.
//
// It compares sequence numbers, not states (C35): after answering an
// input-required task the requester waits for the next input-required, and
// the one it answered has a lower state_seq. A terminal task ends the wait
// whatever its state_seq, because nothing newer can follow it.
func (d *Daemon) waitTask(ctx context.Context, id string, after int64, max time.Duration) (ix *interactions.Interaction, timedOut bool, err error) {
	snap, events, cancel, err := d.Watch(id)
	if err != nil {
		return nil, false, err
	}
	defer func() { cancel() }()
	role := snap.Role
	done := func(ix *interactions.Interaction) bool {
		return ix.IsTerminal() || (ix.StateSeq > after && stopsWait(role, ix.State))
	}
	if done(snap) {
		return snap, false, nil
	}
	var timeout <-chan time.Time
	if max > 0 {
		t := time.NewTimer(max)
		defer t.Stop()
		timeout = t.C
	}
	for {
		select {
		case e, ok := <-events:
			if !ok {
				// Dropped for falling behind: watch again from a new
				// snapshot, which holds every write so far.
				cancel()
				if snap, events, cancel, err = d.Watch(id); err != nil {
					return nil, false, err
				}
				if done(snap) {
					return snap, false, nil
				}
				continue
			}
			if e.Kind != EventState || !(e.State.IsTerminal() || (e.StateSeq > after && stopsWait(role, e.State))) {
				continue
			}
			cur, err := d.ix.Get(id)
			if err != nil {
				return nil, false, err
			}
			return cur, false, nil
		case <-ctx.Done():
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, false, ctx.Err()
			}
			cur, err := d.ix.Get(id)
			if err != nil {
				return nil, false, err
			}
			return cur, true, nil
		case <-timeout:
			cur, err := d.ix.Get(id)
			if err != nil {
				return nil, false, err
			}
			return cur, true, nil
		}
	}
}

// --- ListTasks ---

// taskListReq is a ListTasks request. The fields beyond TaskFilter are the
// control plane's; a scoped caller cannot set them.
type taskListReq struct {
	module.TaskFilter
	Role    interactions.Role
	PeerAID string
	// MaxTaskBytes, when above 0, holds each task of the page to about
	// that many bytes as JSON (a2ashape.TaskWithin): the control plane's
	// max_task_bytes, which MCP list_tasks sets so that one large message
	// does not ride along in every page that lists its task.
	MaxTaskBytes int
}

func (d *Daemon) listTasks(sc taskScope, r taskListReq) (a2ashape.TaskPage, error) {
	f := r.TaskFilter
	bad := func(format string, a ...any) error {
		return a2ashape.Errorf(a2ashape.ErrInvalidParams, "%s", fmt.Sprintf(format, a...))
	}
	size, err := a2ashape.PageSize(f.PageSize)
	if err != nil {
		return a2ashape.TaskPage{}, err
	}
	if err := checkHistoryLen(f.HistoryLen); err != nil {
		return a2ashape.TaskPage{}, err
	}
	lf := interactions.ListFilter{ContextID: f.ContextID, Cursor: f.PageToken, Limit: size}
	switch {
	case sc.all:
		lf.Role, lf.PeerAID = r.Role, r.PeerAID
	case sc.peer != "":
		lf.Role, lf.PeerAID = interactions.RoleOutbound, sc.peer
	default:
		return a2ashape.TaskPage{Tasks: []a2ashape.Task{}, PageSize: size}, nil
	}
	if f.State != "" {
		st, ok := storeState(f.State)
		switch {
		case ok:
			lf.States = []interactions.State{st}
		case a2ashape.TaskState(f.State).Valid():
			// An A2A state no task here is ever in (auth-required): the
			// filter is well formed and matches nothing.
			return a2ashape.TaskPage{Tasks: []a2ashape.Task{}, PageSize: size}, nil
		default:
			return a2ashape.TaskPage{}, bad("unknown status %q", f.State)
		}
	}
	if f.UpdatedAfter != nil {
		// At or after (A2A: "greater than or equal to"). state_at is in
		// whole milliseconds and the store's bound is strict, so the bound
		// is the first millisecond not before the time, minus one.
		ms := f.UpdatedAfter.UnixMilli()
		if f.UpdatedAfter.After(time.UnixMilli(ms)) {
			ms++
		}
		lf.UpdatedAfter = ms - 1
		if lf.UpdatedAfter <= 0 {
			// Every state_at is after the epoch; 0 means "no bound".
			lf.UpdatedAfter = 0
		}
	}
	page, err := d.ix.ListPage(lf)
	if errors.Is(err, interactions.ErrBadCursor) {
		return a2ashape.TaskPage{}, bad("pageToken was not issued by this node")
	}
	if err != nil {
		return a2ashape.TaskPage{}, err
	}
	total, err := d.ix.Count(lf)
	if err != nil {
		return a2ashape.TaskPage{}, err
	}
	out := a2ashape.TaskPage{Tasks: make([]a2ashape.Task, 0, len(page.Items)), TotalSize: total,
		PageSize: size, NextPageToken: page.Next}
	for _, ix := range page.Items {
		// A page carries no file bytes: up to a hundred tasks, each with
		// files a peer chose (an internal note item). A client reads one with GetTask.
		t, err := d.taskView(ix, viewOpts{historyLen: f.HistoryLen, artifacts: f.IncludeArtifacts})
		if errors.Is(err, a2ashape.ErrTaskNotFound) {
			continue // deleted by the retention since the page was read
		}
		if err != nil {
			return a2ashape.TaskPage{}, err
		}
		if r.MaxTaskBytes > 0 {
			t = a2ashape.TaskWithin(t, r.MaxTaskBytes)
		}
		out.Tasks = append(out.Tasks, t)
	}
	return out, nil
}

// --- CancelTask ---

func (d *Daemon) cancelTask(ctx context.Context, sc taskScope, id string) (a2ashape.Task, error) {
	ix, err := d.scopedTask(sc, id)
	if err != nil {
		return a2ashape.Task{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	cur, err := d.CancelTask(cctx, ix.ID)
	switch {
	case errors.Is(err, ErrNotCancelable):
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrTaskNotCancelable, "task %s is %s", ix.ID, stateName(cur, ix))
	case err != nil:
		return a2ashape.Task{}, err
	}
	return d.taskView(cur, viewOpts{artifacts: true, inline: sc.inline()})
}

// storeState reads a state filter in either spelling: the A2A enum name
// (TASK_STATE_WORKING) or the daemon's (working). ok is false for any
// other value, and for a state the store never holds (auth-required).
func storeState(s string) (interactions.State, bool) {
	if st := interactions.State(s); st.Valid() {
		return st, true
	}
	return a2ashape.StoreState(a2ashape.TaskState(s))
}

func stateName(cur, fallback *interactions.Interaction) interactions.State {
	if cur != nil {
		return cur.State
	}
	return fallback.State
}

// --- Watch ---

// watchTask is SubscribeToTask: the task now, then its changes. It reads
// the row again on every event rather than trusting the event's content,
// so events that arrive out of order or coalesced still produce each new
// state once, in order. A new provider message sends the anet.reply
// artifact before the status update that announces it, and the status
// event of a terminal state is preceded by all the task's artifacts
// (CUX-A2, §11.5). A subscriber that opted in (opts.Preview) also receives
// the task's preview frames as artifact updates (CUX-A3).
func (d *Daemon) watchTask(ctx context.Context, sc taskScope, id string, opts module.WatchOpts) (a2ashape.Task, <-chan a2ashape.TaskEvent, error) {
	ix, err := d.scopedTask(sc, id)
	if err != nil {
		return a2ashape.Task{}, nil, err
	}
	snap, events, cancel, err := d.Watch(ix.ID)
	if err != nil {
		cancel()
		return a2ashape.Task{}, nil, err
	}
	// The newest peer message is read before the first view, which reads
	// the messages again: one stored in between is then in the view and
	// also news to the pump (sent twice), never in neither.
	lastMsg := d.lastPeerMessage(snap)
	// A stream carries files as metadata, never bytes, the snapshot
	// included (an internal note item): an event is one SSE line, and a file inline in
	// it would be sent again with every event that repeats it.
	first, err := d.taskView(snap, viewOpts{artifacts: true})
	if err != nil {
		cancel()
		return a2ashape.Task{}, nil, err
	}
	out := make(chan a2ashape.TaskEvent, eventBufferSize)
	if snap.IsTerminal() {
		cancel()
		close(out)
		return first, out, nil
	}
	go d.pumpTaskEvents(ctx, snap, lastMsg, first.Artifacts, events, cancel, out, opts)
	return first, out, nil
}

// metaPreviewFlag marks the artifact update of a preview frame (CUX-A3).
const metaPreviewFlag = "anet.preview"

// previewUpdate is the artifact update of one preview frame: anet.reply
// replaced whole, not final. A frame that omitted its head starts with an
// ellipsis. Reasoning is never part of it (CUX-P17).
func previewUpdate(ix *interactions.Interaction, contextID string, f previewFrame) a2ashape.TaskArtifactUpdateEvent {
	text := f.Text
	if f.Omitted > 0 {
		text = "…" + text
	}
	meta := map[string]any{metaPreviewFlag: true}
	return a2ashape.TaskArtifactUpdateEvent{
		Artifact: a2ashape.Artifact{ID: a2ashape.ArtifactReply, Name: a2ashape.ArtifactReply,
			Parts: []a2ashape.Part{a2ashape.TextPart(text)}, Metadata: map[string]any{metaPreviewFlag: true}},
		ContextID: contextID, TaskID: ix.ID, Metadata: meta,
	}
}

// artifactKey identifies an artifact's content, to send it again only when
// it changed.
func artifactKey(a a2ashape.Artifact) string {
	b, _ := json.Marshal(a)
	return string(b)
}

func (d *Daemon) pumpTaskEvents(ctx context.Context, snap *interactions.Interaction, lastMsg int64, sentArtifacts []a2ashape.Artifact,
	events <-chan Event, cancel func(), out chan<- a2ashape.TaskEvent, opts module.WatchOpts) {
	defer func() { cancel(); close(out) }()
	lastSeq := snap.StateSeq
	sent := map[string]string{}
	for _, a := range sentArtifacts {
		sent[a.ID] = artifactKey(a)
	}
	send := func(e a2ashape.TaskEvent) bool {
		select {
		case out <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}
	sendPreview := func(f previewFrame) bool {
		u := previewUpdate(snap, snap.ContextID, f)
		return send(a2ashape.TaskEvent{ArtifactUpdate: &u})
	}
	if opts.Preview {
		// A subscriber that comes in the middle of a round is shown the
		// preview the task has now (always replacing, never appending).
		if f, ok := d.previews().Current(snap.ID); ok && !sendPreview(f) {
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				// Dropped for falling behind; resubscribe. The new snapshot
				// is compared below like any event.
				cancel()
				var err error
				if _, events, cancel, err = d.Watch(snap.ID); err != nil {
					return
				}
			} else if ev.Kind == EventPreview {
				if f, ok := d.previews().Current(snap.ID); opts.Preview && ok && !sendPreview(f) {
					return
				}
				continue
			} else if ev.Kind == EventDelivery {
				continue // the control plane's stream only (UX-C8)
			}
		}
		cur, err := d.ix.Get(snap.ID)
		if err != nil {
			return
		}
		msg := d.lastPeerMessage(cur)
		if cur.StateSeq <= lastSeq && msg <= lastMsg {
			continue
		}
		t, err := d.taskView(cur, viewOpts{historyLen: new(int), artifacts: true})
		if err != nil {
			return
		}
		for _, a := range a2ashape.ArtifactUpdates(t) {
			// Every artifact before a terminal status; before any other
			// only what is new, so a progress note does not repeat the
			// reply (CUX-A2).
			k := artifactKey(a.Artifact)
			if !cur.IsTerminal() && sent[a.Artifact.ID] == k {
				continue
			}
			sent[a.Artifact.ID] = k
			if !send(a2ashape.TaskEvent{ArtifactUpdate: &a}) {
				return
			}
		}
		su := a2ashape.StatusUpdate(t)
		if !send(a2ashape.TaskEvent{StatusUpdate: &su}) {
			return
		}
		lastSeq, lastMsg = cur.StateSeq, msg
		if cur.IsTerminal() {
			return
		}
	}
}

// lastPeerMessage is the seq of the newest message the other party stored
// on ix (a provider's progress note does not move the state, and is still
// news to a watcher).
func (d *Daemon) lastPeerMessage(ix *interactions.Interaction) int64 {
	msgs, err := d.ix.Messages(ix.ID)
	if err != nil {
		return 0
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].SenderAID != d.AID() {
			return msgs[i].Seq
		}
	}
	return 0
}

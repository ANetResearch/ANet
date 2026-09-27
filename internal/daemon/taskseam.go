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
//     daemon mints it. (contextId, client messageId) is the dedupe key: a
//     retry returns the task the first attempt created. The client's
//     messageId is stored in message.metadata["a2a.messageId"]; the
//     envelope carries a daemon-minted id.
//   - a taskId: a follow-up on that task. A client-chosen id for a task
//     that does not exist is TaskNotFound, never a new task. Input to a
//     terminal task is UnsupportedOperation (§4.2).
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
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// DaemonTaskSeam is the daemon's TaskSeam (A2A-DESIGN §11.1): the task
// operations scoped to one remote agent, for module/a2a. Every method
// refuses an empty peerAID with TaskNotFound; the unscoped form exists only
// on the control plane.
type DaemonTaskSeam struct{ d *Daemon }

// TaskSeam returns the daemon's task seam.
func (d *Daemon) TaskSeam() *DaemonTaskSeam { return &DaemonTaskSeam{d: d} }

// Send is A2A SendMessage to peerAID.
func (s *DaemonTaskSeam) Send(ctx context.Context, peerAID string, req a2ashape.TaskSend) (a2ashape.Task, error) {
	sc, err := peerScope(peerAID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	return s.d.sendTask(ctx, sc, peerAID, req, 0)
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
func (s *DaemonTaskSeam) List(_ context.Context, peerAID string, f a2ashape.TaskFilter) (a2ashape.TaskPage, error) {
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
func (s *DaemonTaskSeam) Watch(ctx context.Context, peerAID, taskID string) (a2ashape.Task, <-chan a2ashape.TaskEvent, error) {
	sc, err := peerScope(peerAID)
	if err != nil {
		return a2ashape.Task{}, nil, err
	}
	return s.d.watchTask(ctx, sc, taskID)
}

// Agents lists agents of the network (A2A-DESIGN §10.5).
func (s *DaemonTaskSeam) Agents(ctx context.Context, q a2ashape.AgentQuery) ([]a2ashape.RemoteAgent, error) {
	agents, _, err := s.d.listAgents(ctx, q)
	return agents, err
}

// Card returns one agent's network card and this node's verification of it.
func (s *DaemonTaskSeam) Card(ctx context.Context, aid string) (a2ashape.RemoteAgent, error) {
	return s.d.agentCard(ctx, aid)
}

// taskScope limits a task operation. The zero value matches nothing.
type taskScope struct {
	// peer limits the operation to outbound interactions with this peer.
	peer string
	// all is the control plane: every interaction, either role.
	all bool
}

func peerScope(peerAID string) (taskScope, error) {
	if peerAID == "" {
		return taskScope{}, fmt.Errorf("%w: no agent named", a2ashape.ErrTaskNotFound)
	}
	return taskScope{peer: peerAID}, nil
}

var controlScope = taskScope{all: true}

// scopedTask finds a task the scope may see.
func (d *Daemon) scopedTask(sc taskScope, id string) (*interactions.Interaction, error) {
	var ix *interactions.Interaction
	var err error
	switch {
	case id == "":
		return nil, fmt.Errorf("%w: task id required", a2ashape.ErrInvalidParams)
	case sc.all:
		ix, err = d.ix.Get(id)
	case sc.peer != "":
		ix, err = d.ix.GetFor(id, interactions.RoleOutbound, sc.peer)
	default:
		err = interactions.ErrNotFound
	}
	if errors.Is(err, interactions.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", a2ashape.ErrTaskNotFound, id)
	}
	return ix, err
}

func (d *Daemon) getTask(sc taskScope, id string, historyLen *int) (a2ashape.Task, error) {
	if err := checkHistoryLen(historyLen); err != nil {
		return a2ashape.Task{}, err
	}
	ix, err := d.scopedTask(sc, id)
	if err != nil {
		return a2ashape.Task{}, err
	}
	return d.taskView(ix, viewOpts{historyLen: historyLen, artifacts: true})
}

func checkHistoryLen(n *int) error {
	if n != nil && *n < 0 {
		return fmt.Errorf("%w: historyLength must not be negative", a2ashape.ErrInvalidParams)
	}
	return nil
}

// --- SendMessage ---

// sendLocks serializes sends that share a dedupe key, so two copies of one
// client message sent at once create one task. Keys carry this node's AID
// (tests run several daemons in one process).
var sendLocks keyedLocks

// taskWaitMax is how long the control plane blocks a send or a wait when
// the caller names no limit. The A2A interface has no limit of its own: the
// client's request context ends the wait.
const taskWaitMax = 60 * time.Second

// sendTask is SendMessage. to names the remote agent of a new task (for a
// scoped caller it is the scope's peer). wait bounds a blocking call; ≤ 0
// waits until ctx ends. When the bound elapses the task is returned as it
// is, not an error: it is still running.
func (d *Daemon) sendTask(ctx context.Context, sc taskScope, to string, req a2ashape.TaskSend, wait time.Duration) (a2ashape.Task, error) {
	if err := checkHistoryLen(req.HistoryLength); err != nil {
		return a2ashape.Task{}, err
	}
	msg := req.Message
	if msg.TaskID != "" {
		return d.appendTask(ctx, sc, req, wait)
	}
	peer := to
	if !sc.all {
		peer = sc.peer
	}
	if peer == "" {
		return a2ashape.Task{}, fmt.Errorf("%w: the remote agent (to) is required for a new task", a2ashape.ErrInvalidParams)
	}
	if peer == d.AID() {
		return a2ashape.Task{}, fmt.Errorf("%w: cannot send a task to this node itself", a2ashape.ErrInvalidParams)
	}
	in, err := parseTaskInput(msg)
	if err != nil {
		return a2ashape.Task{}, err
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
				return a2ashape.Task{}, fmt.Errorf("%w: context %s", a2ashape.ErrTaskNotFound, contextID)
			}
		}
	}
	if contextID != "" && msg.MessageID != "" {
		unlock := sendLocks.lock(d.AID() + "\x00" + contextID + "\x00" + msg.MessageID)
		defer unlock()
		prior, err := d.ix.FindByClientMessage(interactions.ClientMessageQuery{Role: interactions.RoleOutbound,
			ContextID: contextID, PeerAID: peer, ClientMsgID: msg.MessageID})
		switch {
		case err == nil:
			// A retry of a message that already made a task.
			return d.finishSend(ctx, prior.ID, 0, req, wait)
		case !errors.Is(err, interactions.ErrNotFound):
			return a2ashape.Task{}, err
		}
	}
	if contextID == "" {
		if contextID, err = newContextID(); err != nil {
			return a2ashape.Task{}, err
		}
	}
	if d.config().HubURL == "" {
		return a2ashape.Task{}, fmt.Errorf("%w: this node has no hub (run `anet hub-register` first)", a2ashape.ErrUnavailable)
	}
	id, err := newInteractionID()
	if err != nil {
		return a2ashape.Task{}, err
	}
	meta := clientMeta(msg)
	sctx, cancel := context.WithTimeout(ctx, relayCallTimeout)
	defer cancel()
	if in.capID != "" {
		_, err = d.delegateCapabilityCtx(sctx, id, peer, in.capID, in.args, nil, contextID)
	} else {
		var mb []byte
		if len(meta) > 0 {
			if mb, err = json.Marshal(meta); err != nil {
				return a2ashape.Task{}, err
			}
		}
		err = d.delegateInWithID(sctx, id, peer, in.goal(), in.atts, contextID, mb)
	}
	if err != nil {
		d.failUndelivered(id, err)
		return a2ashape.Task{}, fmt.Errorf("%w: %v", a2ashape.ErrUnavailable, err)
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
	// A new task is written at state_seq 1 and nothing but the provider's
	// answer moves it on.
	return d.finishSend(ctx, id, 1, req, wait)
}

// appendTask is SendMessage with a taskId: a follow-up on a task.
func (d *Daemon) appendTask(ctx context.Context, sc taskScope, req a2ashape.TaskSend, wait time.Duration) (a2ashape.Task, error) {
	msg := req.Message
	ix, err := d.scopedTask(sc, msg.TaskID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	if ix.Role != interactions.RoleOutbound {
		return a2ashape.Task{}, fmt.Errorf("%w: %s was delegated to this node; answer it with /tasks/reply", a2ashape.ErrInvalidParams, ix.ID)
	}
	if msg.ContextID != "" && msg.ContextID != ix.ContextID {
		return a2ashape.Task{}, fmt.Errorf("%w: task %s is in context %q, not %q", a2ashape.ErrInvalidParams, ix.ID, ix.ContextID, msg.ContextID)
	}
	if msg.MessageID != "" {
		unlock := sendLocks.lock(d.AID() + "\x00" + ix.ContextID + "\x00" + msg.MessageID)
		defer unlock()
		if _, err := d.ix.FindByClientMessage(interactions.ClientMessageQuery{Role: interactions.RoleOutbound,
			ContextID: ix.ContextID, TaskID: ix.ID, ClientMsgID: msg.MessageID}); err == nil {
			return d.finishSend(ctx, ix.ID, 0, req, wait)
		} else if !errors.Is(err, interactions.ErrNotFound) {
			return a2ashape.Task{}, err
		}
	}
	if hasX402Meta(msg.Metadata) {
		// A payment decision on the task (A2A-DESIGN §8.7).
		after, err := d.taskPaymentMessage(ctx, ix, msg)
		if err != nil {
			return a2ashape.Task{}, err
		}
		return d.finishSend(ctx, ix.ID, after, req, wait)
	}
	if ix.IsTerminal() {
		return a2ashape.Task{}, fmt.Errorf("%w: task %s is %s and takes no new input", a2ashape.ErrUnsupportedOperation, ix.ID, ix.State)
	}
	if ix.IsCapability {
		return a2ashape.Task{}, fmt.Errorf("%w: task %s is a capability call; it takes no follow-up message", a2ashape.ErrUnsupportedOperation, ix.ID)
	}
	in, err := parseTaskInput(msg)
	if err != nil {
		return a2ashape.Task{}, err
	}
	if in.capID != "" {
		return a2ashape.Task{}, fmt.Errorf("%w: a capability call starts a new task; send it without a taskId", a2ashape.ErrInvalidParams)
	}
	sctx, cancel := context.WithTimeout(ctx, relayCallTimeout)
	defer cancel()
	after, err := d.sendMessage(sctx, ix.ID, in.goal(), in.atts, clientMeta(msg))
	switch {
	case errors.Is(err, ErrTaskTerminal):
		return a2ashape.Task{}, fmt.Errorf("%w: %v", a2ashape.ErrUnsupportedOperation, err)
	case err != nil && after == 0:
		return a2ashape.Task{}, err
	case err != nil:
		// Stored here and not delivered; the peer will not see it.
		return a2ashape.Task{}, fmt.Errorf("%w: the message was stored but not delivered: %v", a2ashape.ErrUnavailable, err)
	}
	return d.finishSend(ctx, ix.ID, after, req, wait)
}

// finishSend answers a send: at once, or once the task is terminal or
// interrupted with a state_seq above after. after 0 on a retry means the
// seq of the original write is not known; any terminal or interrupted
// state then ends the wait.
func (d *Daemon) finishSend(ctx context.Context, id string, after int64, req a2ashape.TaskSend, wait time.Duration) (a2ashape.Task, error) {
	var ix *interactions.Interaction
	var err error
	if req.ReturnImmediately {
		ix, err = d.ix.Get(id)
	} else {
		ix, _, err = d.waitTask(ctx, id, after, wait)
	}
	if err != nil {
		return a2ashape.Task{}, err
	}
	return d.taskView(ix, viewOpts{historyLen: req.HistoryLength, artifacts: true})
}

// failUndelivered marks a task whose delegation could not be sent. The row
// exists (it is written before the send), and left as submitted it would
// look like work in progress forever.
func (d *Daemon) failUndelivered(id string, cause error) {
	if _, err := d.ix.Get(id); err != nil {
		return
	}
	if err := d.ix.SetFailed(id, []byte("not delivered: "+cause.Error())); err == nil {
		d.publishResult(id)
	}
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
// client's own keys, minus the reserved anet.* and x402.* namespaces, and
// its message id under a2a.messageId.
func clientMeta(m a2ashape.Message) map[string]any {
	out := map[string]any{}
	for k, v := range m.Metadata {
		if strings.HasPrefix(k, "anet.") || strings.HasPrefix(k, "x402.") || k == interactions.ClientMessageIDKey {
			continue
		}
		out[k] = v
	}
	if m.MessageID != "" {
		out[interactions.ClientMessageIDKey] = m.MessageID
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
func parseTaskInput(m a2ashape.Message) (*taskInput, error) {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", a2ashape.ErrInvalidParams, fmt.Sprintf(format, a...))
	}
	in := &taskInput{}
	if v, ok := m.Metadata["anet.skill"]; ok {
		id, _ := v.(string)
		if strings.TrimSpace(id) == "" {
			return nil, bad("metadata anet.skill must be a capability id")
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
		// A url part is refused whatever else the part carries: the daemon
		// does not fetch, and does not read local paths (file:), for anyone.
		if p.URL != "" {
			return nil, bad("part %d: url parts are not accepted (the daemon fetches nothing and reads no local file); send the bytes as a raw part", i)
		}
		n := 0
		if p.Text != nil {
			n++
		}
		if len(p.Raw) > 0 {
			n++
		}
		if len(p.Data) > 0 {
			n++
		}
		if n != 1 {
			return nil, bad("part %d: exactly one of text, raw or data is required", i)
		}
		switch {
		case p.Text != nil:
			in.texts = append(in.texts, *p.Text)
		case len(p.Raw) > 0:
			att, err := attachmentFromBytes(p.Filename, p.Raw)
			if err != nil {
				return nil, bad("part %d: %v", i, err)
			}
			if mt, _, err := mime.ParseMediaType(p.MediaType); err == nil && p.MediaType != "" {
				att.Mime = mt
			}
			in.atts = append(in.atts, att)
		default:
			var v any
			if err := json.Unmarshal(p.Data, &v); err != nil {
				return nil, bad("part %d: data is not JSON: %v", i, err)
			}
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
			b, _ := json.Marshal(v)
			looseJSON = append(looseJSON, string(b))
		}
	}
	if in.capID != "" {
		if len(in.texts) > 0 || len(in.atts) > 0 {
			return nil, bad("a capability call takes its input as args, not as text or files")
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
// terminal state, or one in which the agent waits for the client.
func stopsWait(st interactions.State) bool {
	return st.IsTerminal() || st == interactions.StateInputRequired
}

// waitTask blocks until the task is terminal or input-required at a
// state_seq above after, ctx ends, or max elapses (max ≤ 0: no bound). It
// returns the task as it then is; timedOut says the bound elapsed first.
//
// It compares sequence numbers, not states (C35): after answering an
// input-required task the requester waits for the next input-required, and
// the one it answered has a lower state_seq.
func (d *Daemon) waitTask(ctx context.Context, id string, after int64, max time.Duration) (ix *interactions.Interaction, timedOut bool, err error) {
	done := func(ix *interactions.Interaction) bool { return ix.StateSeq > after && stopsWait(ix.State) }
	snap, events, cancel, err := d.Watch(id)
	if err != nil {
		return nil, false, err
	}
	defer func() { cancel() }()
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
			if e.Kind != EventState || e.StateSeq <= after || !stopsWait(e.State) {
				continue
			}
			cur, err := d.ix.Get(id)
			if err != nil {
				return nil, false, err
			}
			return cur, false, nil
		case <-ctx.Done():
			return nil, false, ctx.Err()
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
	a2ashape.TaskFilter
	Role    interactions.Role
	PeerAID string
}

// Page sizes (A2A ListTasks: 1–100, default 50).
const (
	listPageDefault = 50
	listPageMax     = 100
)

func (d *Daemon) listTasks(sc taskScope, r taskListReq) (a2ashape.TaskPage, error) {
	f := r.TaskFilter
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", a2ashape.ErrInvalidParams, fmt.Sprintf(format, a...))
	}
	size := f.PageSize
	switch {
	case size == 0:
		size = listPageDefault
	case size < 0 || size > listPageMax:
		return a2ashape.TaskPage{}, bad("pageSize must be between 1 and %d", listPageMax)
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
		st, ok := a2ashape.ANetState(f.State)
		if !ok {
			return a2ashape.TaskPage{}, bad("unknown status %q", f.State)
		}
		lf.States = []interactions.State{interactions.State(st)}
	}
	if f.UpdatedAfter != nil {
		lf.UpdatedAfter = f.UpdatedAfter.UnixMilli()
		if lf.UpdatedAfter <= 0 {
			// Every state_at is after the epoch; 0 would mean "no bound".
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
		t, err := d.taskView(ix, viewOpts{historyLen: f.HistoryLen, artifacts: f.IncludeArtifacts})
		if err != nil {
			return a2ashape.TaskPage{}, err
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
		return a2ashape.Task{}, fmt.Errorf("%w: task %s is %s", a2ashape.ErrTaskNotCancelable, ix.ID, stateName(cur, ix))
	case err != nil:
		return a2ashape.Task{}, err
	}
	return d.taskView(cur, viewOpts{artifacts: true})
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
// state once, in order. Before the status event of a terminal state it
// sends the task's artifacts (anet.reply first, §11.5).
func (d *Daemon) watchTask(ctx context.Context, sc taskScope, id string) (a2ashape.Task, <-chan a2ashape.TaskEvent, error) {
	ix, err := d.scopedTask(sc, id)
	if err != nil {
		return a2ashape.Task{}, nil, err
	}
	snap, events, cancel, err := d.Watch(ix.ID)
	if err != nil {
		cancel()
		return a2ashape.Task{}, nil, err
	}
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
	go d.pumpTaskEvents(ctx, snap, events, cancel, out)
	return first, out, nil
}

func (d *Daemon) pumpTaskEvents(ctx context.Context, snap *interactions.Interaction, events <-chan Event,
	cancel func(), out chan<- a2ashape.TaskEvent) {
	defer func() { cancel(); close(out) }()
	lastSeq := snap.StateSeq
	lastMsg := d.lastPeerMessage(snap)
	send := func(e a2ashape.TaskEvent) bool {
		select {
		case out <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				// Dropped for falling behind; resubscribe. The new snapshot
				// is compared below like any event.
				cancel()
				var err error
				if _, events, cancel, err = d.Watch(snap.ID); err != nil {
					return
				}
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
		t, err := d.taskView(cur, viewOpts{historyLen: new(int), artifacts: cur.IsTerminal()})
		if err != nil {
			return
		}
		if cur.IsTerminal() {
			for _, a := range t.Artifacts {
				if !send(a2ashape.TaskEvent{ArtifactUpdate: &a2ashape.TaskArtifactUpdateEvent{TaskID: t.ID,
					ContextID: t.ContextID, Artifact: a, LastChunk: true}}) {
					return
				}
			}
		}
		if !send(a2ashape.TaskEvent{StatusUpdate: &a2ashape.TaskStatusUpdateEvent{TaskID: t.ID,
			ContextID: t.ContextID, Status: t.Status, Metadata: t.Metadata}}) {
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

//go:build !no_a2a

package a2a

// handler.go is the A2A RequestHandler behind both bindings: each A2A
// operation mapped to the kernel's TaskSeam (A2A-DESIGN §11.5), always with
// the remote agent named in the request's path.
//
// The kernel owns the semantics that need the task store — the contextId
// belonging to this agent's tasks, the (contextId, messageId) dedupe, the
// input part rules, waiting on state_seq rather than on the state [C35] —
// and this file owns what belongs to the A2A face: the service parameters,
// the tenant, the refusals that need no store (a url part, a push config),
// and the shape of a stream. A local client's payment message (§8.7) is a
// message like any other here: the kernel recognises it on Send, so its
// messageId is deduplicated with the rest, and makes the decision.
//
// One thing depends on the request rather than the task: a client that did
// not activate a2a-x402 cannot answer a quote itself, so where the kernel
// says a task waits for a payment decision (needs_operator_approval) such
// a client is told payment_extension_not_activated (§8.7, forClient).

import (
	"context"
	"encoding/json"
	"iter"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// handler implements a2asrv.RequestHandler over a TaskSeam.
type handler struct {
	seam module.TaskSeam
}

// begin checks what every operation shares: the agent from the path, the
// version the client asked for, and a tenant, if the body names one, that
// is the same agent.
func (h *handler) begin(ctx context.Context, tenant string) (*reqInfo, error) {
	info := infoFrom(ctx)
	if info == nil || info.aid == "" {
		return nil, a2a.NewError(a2a.ErrInternalError, "no agent in the request path")
	}
	if info.versionErr != nil {
		return nil, info.versionErr
	}
	if tenant != "" && tenant != info.aid {
		return nil, a2a.NewError(a2a.ErrInvalidParams, "tenant names an agent other than the one in the path")
	}
	return info, nil
}

// taskSend reads a SendMessage request into the kernel's form.
func (h *handler) taskSend(info *reqInfo, r *a2a.SendMessageRequest) (module.TaskSend, error) {
	msg, err := shapeMessage(r.Message)
	if err != nil {
		return module.TaskSend{}, err
	}
	for i, p := range msg.Parts {
		if p.Kind == a2ashape.PartURL {
			// Refused whatever the scheme — file:, http(s):, data: —
			// because the daemon fetches nothing on a client's behalf and
			// reads no local path for it [C44]. The kernel refuses it too;
			// this is the same answer without a round trip.
			return module.TaskSend{}, a2a.NewError(a2a.ErrInvalidParams,
				"part "+strconv.Itoa(i)+": url parts are not accepted; send the file's bytes as a raw part")
		}
	}
	if msg.Role == a2ashape.RoleAgent {
		return module.TaskSend{}, a2a.NewError(a2a.ErrInvalidParams, "a client's message has the user role")
	}
	req := module.TaskSend{Message: msg, Metadata: r.Metadata, Extensions: info.active}
	if c := r.Config; c != nil {
		if c.PushConfig != nil {
			return module.TaskSend{}, a2a.NewError(a2a.ErrPushNotificationNotSupported, "push notifications are not supported")
		}
		req.ReturnImmediately = c.ReturnImmediately
		req.HistoryLength = c.HistoryLength
		req.AcceptedOutputModes = c.AcceptedOutputModes
	}
	if req.HistoryLength != nil && *req.HistoryLength < 0 {
		return module.TaskSend{}, a2a.NewError(a2a.ErrInvalidParams, "historyLength must not be negative")
	}
	return req, nil
}

// SendMessage is a new task, a message on a task, or a payment decision
// (§8.7). Unless the client asked to return immediately, it answers once
// the task is terminal or waits for the client (§11.5 [C22]); a client
// that gives up waiting does not cancel the task.
func (h *handler) SendMessage(ctx context.Context, r *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	info, err := h.begin(ctx, r.Tenant)
	if err != nil {
		return nil, err
	}
	req, err := h.taskSend(info, r)
	if err != nil {
		return nil, err
	}
	// A payment message (§8.7) goes the same way as any message: the
	// kernel's TaskSeam recognises it, deduplicates its messageId, makes
	// the decision (PayTask) and waits like any send. Its answer is given
	// as the kernel made it: a payment the operator has to approve says
	// so, whether or not the extension was activated.
	t, err := h.seam.Send(ctx, info.aid, req)
	if err != nil {
		return nil, toSDKError(err)
	}
	if !isPaymentMessage(req.Message) {
		t = forClient(info, t)
	}
	return sdkTask(t)
}

// SendStreamingMessage sends like SendMessage and then streams the task:
// the Task first, then its status and artifact updates, until it is
// terminal or waits for the client again. Over HTTP the send was carried
// out before the stream opened (precheck.go), so that a refusal is an
// ordinary error; the task it answered is streamed here.
func (h *handler) SendStreamingMessage(ctx context.Context, r *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		info := infoFrom(ctx)
		var req module.TaskSend
		var t module.Task
		if sent, ok := sentFrom(ctx); ok && info != nil {
			req, t = sent.req, sent.task
		} else {
			s, err := h.sendForStream(ctx, r)
			if err != nil {
				yield(nil, toSDKError(err))
				return
			}
			info, req, t = infoFrom(ctx), s.req, s.task
		}
		if isPaymentMessage(req.Message) && (a2ashape.IsPaymentRefusal(t) || a2ashape.IsPaymentHold(t)) {
			// The kernel refused the payment before signing (§8.7), or
			// held it for the operator above the agent tier (§8.3):
			// nothing was sent, and nothing follows until someone acts.
			// A stream event: files as metadata (0017 Q12).
			if ev, err := sdkTask(a2ashape.ByReference(t)); err != nil {
				yield(nil, err)
			} else {
				yield(ev, nil)
			}
			return
		}
		after := stateSeq(t.Metadata)
		if req.Message.TaskID == "" && after > 1 {
			// A new task: the task comes back as it is when Send returns,
			// and a question the agent asked before that is already in it.
			// The send's own write is the task's first state (state_seq 1);
			// waiting past the question instead would hold the stream open
			// for an answer that has come.
			after = 1
		}
		h.stream(ctx, info, t.ID, after, false, yield)
	}
}

// sendForStream is the send of a streaming call: the checks SendMessage
// makes, then the kernel's Send, answering at once.
func (h *handler) sendForStream(ctx context.Context, r *a2a.SendMessageRequest) (*sentStream, error) {
	info, err := h.begin(ctx, r.Tenant)
	if err != nil {
		return nil, err
	}
	req, err := h.taskSend(info, r)
	if err != nil {
		return nil, err
	}
	req.ReturnImmediately = true
	t, err := h.seam.Send(ctx, info.aid, req)
	if err != nil {
		return nil, err
	}
	return &sentStream{req: req, task: t}, nil
}

// checkSubscribe is what SubscribeToTask checks before it streams: the
// request, and a task of this agent's that has not ended.
func (h *handler) checkSubscribe(ctx context.Context, r *a2a.SubscribeToTaskRequest) error {
	info, err := h.begin(ctx, r.Tenant)
	if err != nil {
		return err
	}
	if r.ID == "" {
		return a2a.NewError(a2a.ErrInvalidParams, "task id is required")
	}
	zero := 0
	t, err := h.seam.Get(ctx, info.aid, string(r.ID), &zero)
	if err != nil {
		return err
	}
	if t.Status.State.Terminal() {
		return errEnded
	}
	return nil
}

// errEnded answers a subscription to a task that has ended.
var errEnded = a2a.NewError(a2a.ErrUnsupportedOperation, "the task has ended; read it with GetTask")

// SubscribeToTask streams a task that has not ended. A terminal task is
// UnsupportedOperation (A2A §3.1.6).
func (h *handler) SubscribeToTask(ctx context.Context, r *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		info, err := h.begin(ctx, r.Tenant)
		if err != nil {
			yield(nil, err)
			return
		}
		if r.ID == "" {
			yield(nil, a2a.NewError(a2a.ErrInvalidParams, "task id is required"))
			return
		}
		h.stream(ctx, info, string(r.ID), 0, true, yield)
	}
}

// GetTask reads one of this agent's tasks.
func (h *handler) GetTask(ctx context.Context, r *a2a.GetTaskRequest) (*a2a.Task, error) {
	info, err := h.begin(ctx, r.Tenant)
	if err != nil {
		return nil, err
	}
	if r.ID == "" {
		return nil, a2a.NewError(a2a.ErrInvalidParams, "task id is required")
	}
	t, err := h.seam.Get(ctx, info.aid, string(r.ID), r.HistoryLength)
	if err != nil {
		return nil, toSDKError(err)
	}
	return sdkTask(forClient(info, t))
}

// ListTasks lists this node's tasks with the agent, most recent state
// change first. Artifacts are left out unless includeArtifacts is set.
func (h *handler) ListTasks(ctx context.Context, r *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	info, err := h.begin(ctx, r.Tenant)
	if err != nil {
		return nil, err
	}
	state := string(r.Status)
	if r.Status == a2a.TaskStateUnspecified || state == "TASK_STATE_UNSPECIFIED" {
		state = ""
	}
	page, err := h.seam.List(ctx, info.aid, module.TaskFilter{
		ContextID: r.ContextID, State: state, PageSize: r.PageSize, PageToken: r.PageToken,
		HistoryLen: r.HistoryLength, UpdatedAfter: r.StatusTimestampAfter, IncludeArtifacts: r.IncludeArtifacts,
	})
	if err != nil {
		return nil, toSDKError(err)
	}
	for i := range page.Tasks {
		if !r.IncludeArtifacts {
			page.Tasks[i].Artifacts = nil
		}
		page.Tasks[i] = forClient(info, page.Tasks[i])
	}
	out, err := convert[a2a.ListTasksResponse](page)
	if err != nil {
		return nil, a2a.NewError(a2a.ErrInternalError, "the task list could not be encoded")
	}
	if out.Tasks == nil {
		out.Tasks = []*a2a.Task{}
	}
	return &out, nil
}

// CancelTask cancels one of this agent's tasks (A2A-DESIGN §4.2): a task
// whose payment was already submitted stays working, with
// anet.cancel_requested, and the provider's answer decides.
func (h *handler) CancelTask(ctx context.Context, r *a2a.CancelTaskRequest) (*a2a.Task, error) {
	info, err := h.begin(ctx, r.Tenant)
	if err != nil {
		return nil, err
	}
	if r.ID == "" {
		return nil, a2a.NewError(a2a.ErrInvalidParams, "task id is required")
	}
	t, err := h.seam.Cancel(ctx, info.aid, string(r.ID))
	if err != nil {
		return nil, toSDKError(err)
	}
	return sdkTask(forClient(info, t))
}

// The push-notification operations: this interface has no way to reach a
// client that is not connected, and its cards say so
// (pushNotifications: false).

func (h *handler) GetTaskPushConfig(context.Context, *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	return nil, errNoPush
}

func (h *handler) ListTaskPushConfigs(context.Context, *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	return nil, errNoPush
}

func (h *handler) CreateTaskPushConfig(context.Context, *a2a.PushConfig) (*a2a.PushConfig, error) {
	return nil, errNoPush
}

func (h *handler) DeleteTaskPushConfig(context.Context, *a2a.DeleteTaskPushConfigRequest) error {
	return errNoPush
}

var errNoPush = a2a.NewError(a2a.ErrPushNotificationNotSupported, "push notifications are not supported")

// GetExtendedAgentCard is not offered: the cards declare no extended card,
// and the specification answers that with UnsupportedOperation (A2A §3.3.4).
func (h *handler) GetExtendedAgentCard(context.Context, *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	return nil, a2a.NewError(a2a.ErrUnsupportedOperation, "this agent has no extended card")
}

// --- streams ---

// stream sends a task and what happens to it. The Task comes first — the
// snapshot the kernel took together with the subscription, so nothing
// falls between them — then the events. Before the status update of a
// terminal state come the task's artifacts, anet.reply first, so a client
// that stops reading at the terminal state already has the answer
// (A2A-DESIGN §11.5).
//
// A send's stream ends at a terminal state or at an interrupted one newer
// than its own write (after, a state_seq); a subscription ends at a
// terminal state or at an interrupted state newer than the one it found.
//
// No event carries a file's bytes, only its metadata (0017 Q12): the
// kernel's stream views carry none, and every event is passed through
// a2ashape.EventByReference on its way out as well, so that an event is
// never one SSE line longer than a client can read (a2a-go's reader stops
// at 10 MB) whatever a peer sent.
func (h *handler) stream(ctx context.Context, info *reqInfo, id string, after int64, subscribe bool, yield func(a2a.Event, error) bool) {
	peer := info.aid
	snap, events, err := h.seam.Watch(ctx, peer, id)
	if err != nil {
		yield(nil, toSDKError(err))
		return
	}
	if subscribe {
		if snap.Status.State.Terminal() {
			// Ended since precheckStream looked.
			yield(nil, errEnded)
			return
		}
		after = stateSeq(snap.Metadata)
	}
	seen := map[string]bool{}
	for _, a := range snap.Artifacts {
		seen[a.ID] = true
	}
	first, err := sdkTask(forClient(info, a2ashape.ByReference(snap)))
	if err != nil {
		yield(nil, err)
		return
	}
	if !yield(first, nil) || ends(snap.Status.State, stateSeq(snap.Metadata), after, false) {
		return
	}
	for resubscribed := 0; ; {
		var ev module.TaskEvent
		var ok bool
		select {
		case <-ctx.Done():
			return
		case ev, ok = <-events:
		}
		if !ok {
			// The kernel dropped this watcher (it fell behind) or the task
			// ended as the channel closed. Watch again: the new snapshot
			// holds every change so far, sent as one status update.
			if ctx.Err() != nil || resubscribed >= maxResubscribe {
				return
			}
			resubscribed++
			if snap, events, err = h.seam.Watch(ctx, peer, id); err != nil {
				yield(nil, toSDKError(err))
				return
			}
			su := a2ashape.StatusUpdate(snap)
			ev = module.TaskEvent{StatusUpdate: &su}
		}
		state, seq, isStatus := eventState(ev)
		if ev.ArtifactUpdate != nil {
			seen[ev.ArtifactUpdate.Artifact.ID] = true
		}
		if isStatus && state.Terminal() && !h.flushArtifacts(ctx, peer, id, seen, yield) {
			return
		}
		out, err := sdkEvent(eventForClient(info, a2ashape.EventByReference(ev)))
		if err != nil {
			yield(nil, err)
			return
		}
		if !yield(out, nil) || (isStatus && ends(state, seq, after, true)) {
			return
		}
	}
}

// maxResubscribe bounds how often one stream watches again after being
// dropped: a client that keeps falling behind is not kept up forever.
const maxResubscribe = 64

// flushArtifacts sends the artifacts of a task that the stream has not
// sent yet. The kernel sends them itself before the terminal status; this
// covers a kernel event that arrives without them, at the cost of one read.
func (h *handler) flushArtifacts(ctx context.Context, peer, id string, seen map[string]bool, yield func(a2a.Event, error) bool) bool {
	zero := 0
	t, err := h.seam.Get(ctx, peer, id, &zero)
	if err != nil {
		return true // the status update still goes out
	}
	for _, au := range a2ashape.ArtifactUpdates(t) {
		if seen[au.Artifact.ID] {
			continue
		}
		seen[au.Artifact.ID] = true
		out, err := sdkEvent(a2ashape.EventByReference(module.TaskEvent{ArtifactUpdate: &au}))
		if err != nil {
			yield(nil, err)
			return false
		}
		if !yield(out, nil) {
			return false
		}
	}
	return true
}

// eventState is the state an event reports, if it reports one.
func eventState(ev module.TaskEvent) (a2ashape.TaskState, int64, bool) {
	switch {
	case ev.StatusUpdate != nil:
		return ev.StatusUpdate.Status.State, stateSeq(ev.StatusUpdate.Metadata), true
	case ev.Task != nil:
		return ev.Task.Status.State, stateSeq(ev.Task.Metadata), true
	}
	return "", 0, false
}

// ends reports whether a state ends a send or a wait: a terminal state, or
// an interrupted one newer than after. An event whose sequence number is
// unknown (0) is newer by being an event.
func ends(state a2ashape.TaskState, seq, after int64, event bool) bool {
	if state.Terminal() {
		return true
	}
	return state.Interrupted() && (seq > after || (event && seq == 0))
}

// stateSeq reads anet.state_seq, the kernel's count of state writes.
func stateSeq(meta map[string]any) int64 {
	switch v := meta[a2ashape.KeyStateSeq].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// --- payments (A2A-DESIGN §8.7) ---

// forClient is t as this request's client is told it: a client that did
// not activate a2a-x402 reads payment_extension_not_activated where the
// kernel says the task waits for a payment decision (§8.7). The kernel
// pays within the automatic tier either way; above it, such a client
// cannot answer the quote until it activates the extension.
func forClient(info *reqInfo, t module.Task) module.Task {
	if x402a2a.Activated(info.requested) {
		return t
	}
	return a2ashape.WithoutX402Extension(t)
}

// eventForClient is forClient for one stream event.
func eventForClient(info *reqInfo, ev module.TaskEvent) module.TaskEvent {
	if x402a2a.Activated(info.requested) {
		return ev
	}
	switch {
	case ev.StatusUpdate != nil:
		su := a2ashape.StatusWithoutX402Extension(*ev.StatusUpdate)
		ev.StatusUpdate = &su
	case ev.Task != nil:
		t := a2ashape.WithoutX402Extension(*ev.Task)
		ev.Task = &t
	}
	return ev
}

// isPaymentMessage reports a message that carries a2a-x402 metadata: a
// local client answering a payment-required task.
func isPaymentMessage(m a2ashape.Message) bool {
	for k := range m.Metadata {
		if strings.HasPrefix(k, "x402.") {
			return true
		}
	}
	return false
}

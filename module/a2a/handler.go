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
// the payment message of a local client (§8.7), and the shape of a stream.

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
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
	var t module.Task
	if isPaymentMessage(req.Message) {
		var settled bool
		t, settled, err = h.pay(ctx, info.aid, req)
		if err == nil && !settled && !req.ReturnImmediately {
			t, err = h.await(ctx, info.aid, t.ID, stateSeq(t.Metadata), req.HistoryLength)
		}
	} else {
		t, err = h.seam.Send(ctx, info.aid, req)
	}
	if err != nil {
		return nil, toSDKError(err)
	}
	return sdkTask(t)
}

// SendStreamingMessage sends like SendMessage and then streams the task:
// the Task first, then its status and artifact updates, until it is
// terminal or waits for the client again.
func (h *handler) SendStreamingMessage(ctx context.Context, r *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		info, err := h.begin(ctx, r.Tenant)
		if err != nil {
			yield(nil, err)
			return
		}
		req, err := h.taskSend(info, r)
		if err != nil {
			yield(nil, err)
			return
		}
		req.ReturnImmediately = true
		var t module.Task
		if isPaymentMessage(req.Message) {
			var settled bool
			t, settled, err = h.pay(ctx, info.aid, req)
			if err == nil && settled {
				// A refusal answered here: nothing was sent, and nothing
				// will follow.
				if ev, err := sdkTask(t); err != nil {
					yield(nil, err)
				} else {
					yield(ev, nil)
				}
				return
			}
		} else {
			t, err = h.seam.Send(ctx, info.aid, req)
		}
		if err != nil {
			yield(nil, toSDKError(err))
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
		h.stream(ctx, info.aid, t.ID, after, false, yield)
	}
}

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
		h.stream(ctx, info.aid, string(r.ID), 0, true, yield)
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
	return sdkTask(t)
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
	if !r.IncludeArtifacts {
		for i := range page.Tasks {
			page.Tasks[i].Artifacts = nil
		}
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
	return sdkTask(t)
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
func (h *handler) stream(ctx context.Context, peer, id string, after int64, subscribe bool, yield func(a2a.Event, error) bool) {
	snap, events, err := h.seam.Watch(ctx, peer, id)
	if err != nil {
		yield(nil, toSDKError(err))
		return
	}
	if subscribe {
		if snap.Status.State.Terminal() {
			yield(nil, a2a.NewError(a2a.ErrUnsupportedOperation, "the task has ended; read it with GetTask"))
			return
		}
		after = stateSeq(snap.Metadata)
	}
	seen := map[string]bool{}
	for _, a := range snap.Artifacts {
		seen[a.ID] = true
	}
	first, err := sdkTask(snap)
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
		out, err := sdkEvent(ev)
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
		out, err := sdkEvent(module.TaskEvent{ArtifactUpdate: &au})
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

// await waits for a task to end or to wait for the client again, with a
// state newer than after, and returns it. The client's context bounds the
// wait; ending it does not cancel the task.
func (h *handler) await(ctx context.Context, peer, id string, after int64, historyLen *int) (module.Task, error) {
	for range maxResubscribe {
		snap, events, err := h.seam.Watch(ctx, peer, id)
		if err != nil {
			return module.Task{}, err
		}
		if ends(snap.Status.State, stateSeq(snap.Metadata), after, false) {
			return h.seam.Get(ctx, peer, id, historyLen)
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return module.Task{}, ctx.Err()
			case ev, ok := <-events:
				if !ok {
					break wait
				}
				if state, seq, isStatus := eventState(ev); isStatus && ends(state, seq, after, true) {
					return h.seam.Get(ctx, peer, id, historyLen)
				}
			}
		}
		if ctx.Err() != nil {
			return module.Task{}, ctx.Err()
		}
	}
	return h.seam.Get(ctx, peer, id, historyLen)
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

// pay carries out a local client's payment message. This node is the
// signing service of a2a-x402 §5.1: the client says payment-submitted and,
// when the quote offers more than one, which option (anet.payment.accept,
// copied from x402.payment.required.accepts); the kernel signs within the
// agent-tier limits and forwards the payment inside the E2E envelope.
//
// settled is true when the answer is a refusal made here, which is final
// for this message: a payload of the client's own (its payer is not this
// node, so it could not settle even if forwarded), or an option the quote
// did not offer. The task is returned as it is, with a status message
// saying payment-failed.
func (h *handler) pay(ctx context.Context, peer string, req module.TaskSend) (t module.Task, settled bool, err error) {
	m := req.Message
	if m.TaskID == "" {
		return module.Task{}, false, a2a.NewError(a2a.ErrInvalidParams, "a payment message answers a task: taskId is required")
	}
	if _, ok := m.Metadata[a2ashape.KeyX402Payload]; ok {
		t, err := h.seam.Get(ctx, peer, m.TaskID, req.HistoryLength)
		if err != nil {
			return module.Task{}, false, err
		}
		return paymentFailed(t, reasonClientPayloadUnsupported,
			"this node signs payments itself; a payload made by the client is not forwarded"), true, nil
	}
	status, _ := m.Metadata[a2ashape.KeyX402Status].(string)
	var d module.PayDecision
	switch status {
	case a2ashape.PaymentSubmitted:
		d.Decision = module.PaySubmit
		if v, ok := m.Metadata[a2ashape.KeyPaymentAccept]; ok && v != nil {
			b, err := json.Marshal(v)
			if err != nil {
				return module.Task{}, false, a2a.NewError(a2a.ErrInvalidParams, a2ashape.KeyPaymentAccept+" is not JSON")
			}
			d.Accept = b
			t, err := h.seam.Get(ctx, peer, m.TaskID, req.HistoryLength)
			if err != nil {
				return module.Task{}, false, err
			}
			if !offered(t, b) {
				return paymentFailed(t, reasonOptionNotOffered,
					"the chosen option is not one of those in x402.payment.required.accepts"), true, nil
			}
		}
	case a2ashape.PaymentRejected:
		d.Decision = module.PayReject
	default:
		return module.Task{}, false, a2a.NewError(a2a.ErrInvalidParams,
			"a client's "+a2ashape.KeyX402Status+" is "+a2ashape.PaymentSubmitted+" or "+a2ashape.PaymentRejected)
	}
	t, err = h.seam.Pay(ctx, peer, m.TaskID, d)
	return t, false, err
}

// Reasons of a refusal made here (internal/x402a2a names the same).
const (
	reasonClientPayloadUnsupported = "client_payload_unsupported"
	reasonOptionNotOffered         = "option_not_offered"
)

// codeSettlementFailed is the a2a-x402 error code both refusals carry.
const codeSettlementFailed = "SETTLEMENT_FAILED"

// paymentFailed is t answered with payment-failed: a status message from
// the agent's side carrying the a2a-x402 keys and anet.reason. The task
// itself is unchanged — the quote still stands and may be paid properly.
func paymentFailed(t module.Task, reason, text string) module.Task {
	msg := a2ashape.Message{
		ID: a2a.NewMessageID(), ContextID: t.ContextID, TaskID: t.ID, Role: a2ashape.RoleAgent,
		Parts: []a2ashape.Part{a2ashape.TextPart("payment not submitted: " + text)},
		Metadata: map[string]any{
			a2ashape.KeyX402Status: a2ashape.PaymentFailed,
			a2ashape.KeyX402Error:  codeSettlementFailed,
			a2ashape.KeyReason:     reason,
		},
	}
	t.Status.Message = &msg
	return t
}

// offered reports whether accept is one of the options of the task's
// payment requirements, compared as canonical JSON (the kernel compares
// again against the requirements it stored). A task whose requirements are
// not visible here is left to the kernel.
func offered(t module.Task, accept []byte) bool {
	req := paymentRequired(t)
	if req == nil {
		return true
	}
	accepts, _ := req["accepts"].([]any)
	want, ok := canonicalJSON(accept)
	if !ok {
		return false
	}
	for _, a := range accepts {
		b, err := json.Marshal(a)
		if err != nil {
			continue
		}
		if got, ok := canonicalJSON(b); ok && bytes.Equal(got, want) {
			return true
		}
	}
	return false
}

// paymentRequired finds x402.payment.required on the task's status message
// or, failing that, its metadata.
func paymentRequired(t module.Task) map[string]any {
	if m := t.Status.Message; m != nil {
		if r, ok := m.Metadata[a2ashape.KeyX402Required].(map[string]any); ok {
			return r
		}
	}
	r, _ := t.Metadata[a2ashape.KeyX402Required].(map[string]any)
	return r
}

// canonicalJSON re-encodes a JSON value with sorted keys and numbers as
// written.
func canonicalJSON(b []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	out, err := json.Marshal(v)
	return out, err == nil
}

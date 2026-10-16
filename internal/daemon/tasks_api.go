package daemon

// tasks_api.go is the control plane's task routes (A2A-DESIGN §12):
//
//	POST /tasks/send    SendMessage (new task or follow-up)
//	POST /tasks/get     GetTask
//	POST /tasks/list    ListTasks, with role and peer filters
//	POST /tasks/cancel  CancelTask
//	POST /tasks/events  one task as server-sent events: task, preview and delivery (tasks_events.go)
//	POST /tasks/wait    wait until the task is terminal or waits for this node (newer than after_seq)
//	POST /tasks/reply   the provider's answer on a task delegated to this node
//	POST /agents/list   discovery: verified network cards, skill, tag and free text matched here
//	POST /agents/card   one agent's network card (original bytes), this node's verification, and its signed prices
//	POST /agents/kel    one agent's key history (anet verify), from this node's record or the directory
//
// They return the internal/a2ashape projection and share taskseam.go with
// the A2A interface, without the per-agent scope: the control token is the
// node's full authority. (/tasks/pay and /tasks/pay-manual belong to the
// payment work package.)
//
// All of them are bearer-only but /agents/list and /agents/card, which the
// console reads (ctlsec.go sessionRoutes, sealed-sender SSR-D17);
// TestEveryRouteIsAllowlistedOrRefusedForASession names them explicitly.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// taskWaitCap bounds the timeout a caller may ask a control-plane call to
// block for.
const taskWaitCap = time.Hour

// taskBodyMax bounds a /tasks/send or /tasks/reply body, which may carry
// files as base64 raw parts: the local A2A interface's limit (A2A-DESIGN
// §11.4), well under the control plane's general one. Each file is held to
// maxAttachmentBytes on its own.
const taskBodyMax = 96 << 20

// keyWait marks a /tasks/wait answer whose bound elapsed first: the task
// is returned as it is, still running.
const keyWait = "anet.wait"

// registerTaskRoutes adds the /tasks/* routes to the control plane.
func (d *Daemon) registerTaskRoutes(api *routeMux) {
	api.HandleFunc("POST /tasks/send", d.hTasksSend)
	api.HandleFunc("POST /tasks/get", d.hTasksGet)
	api.HandleFunc("POST /tasks/list", d.hTasksList)
	api.HandleFunc("POST /tasks/cancel", d.hTasksCancel)
	api.HandleFunc("POST /tasks/wait", d.hTasksWait)
	api.HandleFunc("POST /tasks/events", d.hTasksEvents)
	api.HandleFunc("POST /tasks/reply", d.hTasksReply)
	api.HandleFunc("POST /tasks/preview", d.hTasksPreview) // bearer only (preview_source.go)
	api.HandleFunc("POST /agents/list", d.hAgentsList)
	api.HandleFunc("POST /agents/card", d.hAgentsCard)
	api.HandleFunc("POST /agents/kel", d.hAgentsKEL)
}

// writeTaskError answers a task operation's error with a status code and
// the A2A error name.
func writeTaskError(w http.ResponseWriter, err error) {
	var ref *a2ashape.Refusal
	if errors.As(err, &ref) {
		// A refusal of the reason table (the send gate, LEC AD-6; a strict
		// send, engine ENG-U5): its sentence, code and command (ux UX-R9).
		writeRefusal(w, refusalStatus(ref), ref)
		return
	}
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, a2ashape.ErrTaskNotFound):
		code = http.StatusNotFound
	case errors.Is(err, a2ashape.ErrInvalidParams):
		code = http.StatusBadRequest
	case errors.Is(err, a2ashape.ErrUnsupportedOperation), errors.Is(err, a2ashape.ErrTaskNotCancelable):
		code = http.StatusConflict
	case errors.Is(err, a2ashape.ErrUnavailable):
		code = http.StatusServiceUnavailable
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		code = http.StatusGatewayTimeout
	}
	writeJSON(w, code, map[string]string{"error": err.Error(), "code": a2ashape.ErrorName(err)})
}

// refusalStatus is the control plane's status for a refusal: a send gate's
// is 409 (LEC AD-6); a strict send's is the REST status its row gives
// (engine ENG-U5: 400 for a recipient no hub knows, 503 for one that does
// not verify).
func refusalStatus(ref *a2ashape.Refusal) int {
	switch ref.Code() {
	case reasonRecipientUnknown, reasonRecipientUnverifiable:
		return ref.A2A(a2ashape.Lang()).HTTPStatus
	}
	return http.StatusConflict
}

func badTaskRequest(w http.ResponseWriter, format string, a ...any) {
	writeTaskError(w, a2ashape.Errorf(a2ashape.ErrInvalidParams, "%s", fmt.Sprintf(format, a...)))
}

// waitBound turns a request's timeout_ms into a wait bound.
func waitBound(ms int64) (time.Duration, error) {
	switch {
	case ms < 0:
		return 0, a2ashape.Errorf(a2ashape.ErrInvalidParams, "timeout_ms must not be negative")
	case ms == 0:
		return taskWaitMax, nil
	}
	d := time.Duration(ms) * time.Millisecond
	if d > taskWaitCap {
		d = taskWaitCap
	}
	return d, nil
}

// tasksSendReq is the /tasks/send body. message is an A2A Message; when it
// is absent the message is built from the short fields (text, skill, args,
// context_id, task_id, message_id, metadata), which is what the CLI and
// MCP send.
type tasksSendReq struct {
	To        string            `json:"to"`
	Message   *a2ashape.Message `json:"message"`
	Text      string            `json:"text"`
	Skill     string            `json:"skill"`
	Args      map[string]any    `json:"args"`
	ContextID string            `json:"context_id"`
	TaskID    string            `json:"task_id"`
	MessageID string            `json:"message_id"`
	Metadata  map[string]any    `json:"metadata"`
	// ReturnImmediately returns once the task is created or the message
	// sent. Otherwise the call blocks until the task is terminal or
	// input-required, or timeout_ms (default 60 s) elapses; the task is then
	// returned as it is, still running.
	ReturnImmediately bool `json:"return_immediately"`
	Configuration     *struct {
		ReturnImmediately bool `json:"returnImmediately"`
		HistoryLength     *int `json:"historyLength"`
	} `json:"configuration"`
	HistoryLength *int  `json:"history_length"`
	TimeoutMS     int64 `json:"timeout_ms"`
	// MaxTaskBytes bounds the task returned (maxTaskBytes).
	MaxTaskBytes int `json:"max_task_bytes"`
	// Preview asks the provider for previews of the round this message
	// opens (CUX-P10): "off" (the default), "on" or "with_reasoning".
	Preview string `json:"preview"`
}

// readJSONNumbers is readJSON keeping the numbers of the message, its
// metadata and a capability call's args as the client wrote them
// (json.Number): a retry is judged on the literal content (correctness-ux
// CUX-D1), and args go to the provider as written. A number no double holds
// (1e400) is refused, as it is everywhere else: a client of the projection
// that reads numbers as doubles could not read the task.
func readJSONNumbers(r *http.Request, req *tasksSendReq) error {
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(req); err != nil {
		return err
	}
	var msgMeta map[string]any
	if req.Message != nil {
		msgMeta = req.Message.Metadata
	}
	return doubleNumbers(req.Args, req.Metadata, msgMeta)
}

// doubleNumbers is an error naming the first number in vs that no double
// holds, nil when there is none.
func doubleNumbers(vs ...any) error {
	for _, v := range vs {
		switch x := v.(type) {
		case json.Number:
			if _, err := x.Float64(); err != nil {
				return fmt.Errorf("the number %s is out of range", x)
			}
		case map[string]any:
			for _, e := range x {
				if err := doubleNumbers(e); err != nil {
					return err
				}
			}
		case []any:
			for _, e := range x {
				if err := doubleNumbers(e); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// maxTaskBytes is the optional max_task_bytes of a route that returns one
// task: above 0 the task is held to about that many bytes as JSON, what is
// cut replaced by a notice of its size that says how to read it whole
// (a2ashape.TaskReadWithin); 0, the default, returns it whole. MCP sets it
// on every tool that returns a task, so that a peer's multi-megabyte reply
// is not a tool result its client refuses (an internal note §7.4).
func maxTaskBytes(n int) error {
	if n < 0 {
		return a2ashape.Errorf(a2ashape.ErrInvalidParams, "max_task_bytes must not be negative")
	}
	return nil
}

// boundTask is t held to max bytes (maxTaskBytes); t itself when max is 0.
func boundTask(t a2ashape.Task, max int) a2ashape.Task {
	if max <= 0 {
		return t
	}
	return a2ashape.TaskReadWithin(t, max)
}

func (r *tasksSendReq) message() a2ashape.Message {
	if r.Message != nil {
		return *r.Message
	}
	m := a2ashape.Message{ID: r.MessageID, ContextID: r.ContextID, TaskID: r.TaskID,
		Role: a2ashape.RoleUser, Metadata: r.Metadata}
	if r.Text != "" {
		m.Parts = append(m.Parts, a2ashape.TextPart(r.Text))
	}
	if r.Skill != "" {
		call := map[string]any{"skill": r.Skill}
		if r.Args != nil {
			call["args"] = r.Args
		}
		m.Parts = append(m.Parts, a2ashape.DataPart(call))
	}
	return m
}

func (d *Daemon) hTasksSend(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, taskBodyMax)
	var req tasksSendReq
	if err := readJSONNumbers(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	wait, err := waitBound(req.TimeoutMS)
	if err == nil {
		err = maxTaskBytes(req.MaxTaskBytes)
	}
	switch req.Preview {
	case "", "off", previewModeOn, previewModeWithReasoning:
	default:
		if err == nil {
			err = a2ashape.Errorf(a2ashape.ErrInvalidParams, "preview must be off, on or with_reasoning")
		}
	}
	if err != nil {
		writeTaskError(w, err)
		return
	}
	ts := module.TaskSend{Message: req.message(), ReturnImmediately: req.ReturnImmediately, HistoryLength: req.HistoryLength,
		Preview: req.Preview}
	if c := req.Configuration; c != nil {
		ts.ReturnImmediately = ts.ReturnImmediately || c.ReturnImmediately
		if ts.HistoryLength == nil {
			ts.HistoryLength = c.HistoryLength
		}
	}
	t, err := d.sendTask(r.Context(), controlScope, req.To, ts, wait)
	if err != nil {
		writeTaskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, boundTask(t, req.MaxTaskBytes))
}

func (d *Daemon) hTasksGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID        string `json:"task_id"`
		HistoryLength *int   `json:"history_length"`
		MaxTaskBytes  int    `json:"max_task_bytes"` // maxTaskBytes
	}
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	if err := maxTaskBytes(req.MaxTaskBytes); err != nil {
		writeTaskError(w, err)
		return
	}
	d.pollFresh(r.Context())
	t, err := d.getTask(controlScope, req.TaskID, req.HistoryLength)
	if err != nil {
		writeTaskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, boundTask(t, req.MaxTaskBytes))
}

func (d *Daemon) hTasksList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ContextID string `json:"context_id"`
		// Status and State are the same filter; State is the MCP name.
		Status               string `json:"status"`
		State                string `json:"state"`
		Role                 string `json:"role"`
		Peer                 string `json:"peer"`
		PageSize             int    `json:"page_size"`
		PageToken            string `json:"page_token"`
		HistoryLength        *int   `json:"history_length"`
		StatusTimestampAfter string `json:"status_timestamp_after"`
		IncludeArtifacts     bool   `json:"include_artifacts"`
		// MaxTaskBytes bounds each task of the page as JSON
		// (a2ashape.TaskWithin); 0, the default, leaves them whole. MCP
		// list_tasks sets it.
		MaxTaskBytes int `json:"max_task_bytes"`
	}
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	if req.MaxTaskBytes < 0 {
		badTaskRequest(w, "max_task_bytes must not be negative")
		return
	}
	lr := taskListReq{TaskFilter: module.TaskFilter{ContextID: req.ContextID, State: req.Status,
		PageSize: req.PageSize, PageToken: req.PageToken, HistoryLen: req.HistoryLength,
		IncludeArtifacts: req.IncludeArtifacts}, PeerAID: req.Peer, MaxTaskBytes: req.MaxTaskBytes}
	if lr.State == "" {
		lr.State = req.State
	}
	switch interactions.Role(req.Role) {
	case "", interactions.RoleInbound, interactions.RoleOutbound:
		lr.Role = interactions.Role(req.Role)
	default:
		badTaskRequest(w, "role must be inbound or outbound")
		return
	}
	if req.StatusTimestampAfter != "" {
		t, err := time.Parse(time.RFC3339Nano, req.StatusTimestampAfter)
		if err != nil {
			badTaskRequest(w, "status_timestamp_after: %v", err)
			return
		}
		lr.UpdatedAfter = &t
	}
	d.pollFresh(r.Context())
	page, err := d.listTasks(controlScope, lr)
	if err != nil {
		writeTaskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (d *Daemon) hTasksCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID       string `json:"task_id"`
		MaxTaskBytes int    `json:"max_task_bytes"` // maxTaskBytes
	}
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	if err := maxTaskBytes(req.MaxTaskBytes); err != nil {
		writeTaskError(w, err)
		return
	}
	t, err := d.cancelTask(r.Context(), controlScope, req.TaskID)
	if err != nil {
		writeTaskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, boundTask(t, req.MaxTaskBytes))
}

// hTasksWait blocks until the task is terminal, or waits for this node at
// a state_seq above after_seq (every task view carries its state_seq as
// metadata anet.state_seq), or timeout_ms elapses. Waiting for this node is
// input-required on a task it sent, and any other state on a task
// delegated to it (the requester answered). Without after_seq such a task
// returns at once. A timed-out wait answers the task as it is, with
// metadata anet.wait = "timed_out".
func (d *Daemon) hTasksWait(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID        string `json:"task_id"`
		AfterSeq      int64  `json:"after_seq"`
		TimeoutMS     int64  `json:"timeout_ms"`
		HistoryLength *int   `json:"history_length"`
		MaxTaskBytes  int    `json:"max_task_bytes"` // maxTaskBytes
	}
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	wait, err := waitBound(req.TimeoutMS)
	if err == nil {
		err = maxTaskBytes(req.MaxTaskBytes)
	}
	if err != nil {
		writeTaskError(w, err)
		return
	}
	if err := checkHistoryLen(req.HistoryLength); err != nil {
		writeTaskError(w, err)
		return
	}
	ix, err := d.scopedTask(controlScope, req.TaskID)
	if err != nil {
		writeTaskError(w, err)
		return
	}
	cur, timedOut, err := d.waitTask(r.Context(), ix.ID, req.AfterSeq, wait)
	if err != nil {
		writeTaskError(w, err)
		return
	}
	t, err := d.taskView(cur, viewOpts{historyLen: req.HistoryLength, artifacts: true})
	if err != nil {
		writeTaskError(w, err)
		return
	}
	t = boundTask(t, req.MaxTaskBytes)
	if timedOut {
		t.Metadata[keyWait] = "timed_out"
	}
	writeJSON(w, http.StatusOK, t)
}

// replyReq is the /tasks/reply body: the provider's answer on a task
// delegated to this node.
type replyReq struct {
	TaskID string `json:"task_id"`
	// Text, or Message for parts (files as raw parts).
	Text    string            `json:"text"`
	Message *a2ashape.Message `json:"message"`
	// State is what the answer does to the task:
	//   input-required (default)  a message; the requester is asked to answer
	//   working                   a progress message; the task stays working
	//   completed                 completion: a receipt is signed over the
	//                             transcript, which ends with the message
	//                             (if any); the result carries it as its
	//                             reply (completeWithReply, CUX-R4)
	//   failed, rejected          a status with the text as the reason
	//   canceled                  the provider cancels
	State string `json:"state"`
	// MaxTaskBytes bounds the task returned (maxTaskBytes).
	MaxTaskBytes int `json:"max_task_bytes"`
}

func (d *Daemon) hTasksReply(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, taskBodyMax)
	var req replyReq
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	if err := maxTaskBytes(req.MaxTaskBytes); err != nil {
		writeTaskError(w, err)
		return
	}
	t, err := d.replyTask(r.Context(), req)
	if err != nil {
		writeTaskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, boundTask(t, req.MaxTaskBytes))
}

// replyTask carries out a provider's answer (see replyReq).
func (d *Daemon) replyTask(ctx context.Context, req replyReq) (a2ashape.Task, error) {
	bad := func(format string, a ...any) error {
		return a2ashape.Errorf(a2ashape.ErrInvalidParams, "%s", fmt.Sprintf(format, a...))
	}
	ix, err := d.scopedTask(controlScope, req.TaskID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	if ix.Role != interactions.RoleInbound {
		return a2ashape.Task{}, bad("%s is a task this node sent; follow it up with /tasks/send", ix.ID)
	}
	if ix.IsCapability {
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrUnsupportedOperation,
			"%s is a capability call; it completes when the capability answers", ix.ID)
	}
	if ix.IsTerminal() {
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s is %s", ix.ID, ix.State)
	}
	state := interactions.StateInputRequired
	if req.State != "" {
		s, ok := storeState(req.State)
		if !ok {
			return a2ashape.Task{}, bad("unknown state %q", req.State)
		}
		state = s
	}
	var in *taskInput
	if req.Message != nil || req.Text != "" {
		m := a2ashape.Message{Parts: []a2ashape.Part{a2ashape.TextPart(req.Text)}}
		if req.Message != nil {
			m = *req.Message
		}
		if in, err = parseTaskInput(m, a2ashape.RoleAgent); err != nil {
			return a2ashape.Task{}, err
		}
		if in.capID != "" {
			return a2ashape.Task{}, bad("a reply cannot be a capability call")
		}
	}
	text := ""
	if in != nil {
		text = in.goal()
	}
	sctx, cancel := context.WithTimeout(ctx, relayCallTimeout)
	defer cancel()
	switch state {
	case interactions.StateInputRequired:
		if in == nil {
			return a2ashape.Task{}, bad("a reply that asks for input needs text or files")
		}
		_, err = d.sendMessage(sctx, ix.ID, text, in.atts, nil)
	case interactions.StateWorking:
		if in == nil {
			err = d.SendStatus(sctx, ix.ID, interactions.StateWorking, "", nil)
		} else {
			_, err = d.sendMessage(sctx, ix.ID, text, in.atts, map[string]any{a2ashape.KeyState: string(interactions.StateWorking)})
		}
	case interactions.StateCompleted:
		if in == nil {
			err = d.CompleteTask(sctx, ix.ID)
			break
		}
		// The reply and the completion are one write and one message to
		// the requester (CUX-R4, R5): the result carries the reply, so the
		// requester's blocking SendMessage returns completed with it.
		err = d.completeWithReply(sctx, ix.ID, text, in.atts)
	case interactions.StateFailed, interactions.StateRejected:
		err = d.SendStatus(sctx, ix.ID, state, text, nil)
	case interactions.StateCanceled:
		_, err = d.CancelTask(sctx, ix.ID)
		if errors.Is(err, ErrNotCancelable) {
			err = a2ashape.Errorf(a2ashape.ErrTaskNotCancelable, "%v", err)
		}
	default:
		return a2ashape.Task{}, bad("a reply cannot set the state %s", state)
	}
	if errors.Is(err, ErrTaskTerminal) {
		return a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "%v", err)
	}
	if err != nil {
		return a2ashape.Task{}, err
	}
	cur, err := d.ix.Get(ix.ID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	return d.taskView(cur, viewOpts{artifacts: true})
}

// hAgentsList is discovery (MCP list_agents). q is free text; it is matched
// against the cards here and never sent to the hub (A2A-DESIGN §10.5).
func (d *Daemon) hAgentsList(w http.ResponseWriter, r *http.Request) {
	var q module.AgentQuery
	if err := readJSON(r, &q); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	agents, next, err := d.listAgents(r.Context(), q)
	if err != nil {
		writeTaskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents, "nextCursor": next})
}

// hAgentsCard returns one agent's network card (MCP get_agent_card, anet
// card), with the prices of its extended card when CORE-K23 rule 2 holds
// (HPP §2.7, PAY-C10 case 1): from the cache, or asked of the agent, which
// may take up to 10 seconds. "prices": false leaves the agent alone — what
// a confirmation prompt asks when it wants only the agent's name.
func (d *Daemon) hAgentsCard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AID    string `json:"aid"`
		Prices *bool  `json:"prices"`
	}
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	var ra module.RemoteAgent
	var err error
	if req.Prices != nil && !*req.Prices {
		ra, err = d.agentCard(r.Context(), req.AID)
	} else {
		ra, _, err = d.agentCardWithPrices(r.Context(), req.AID)
	}
	if err != nil {
		writeTaskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ra)
}

// fillStatusText sets each thread's StatusText (threadStatusText).
func (d *Daemon) fillStatusText(ts []Thread) {
	for i := range ts {
		switch interactions.State(ts[i].State) {
		case interactions.StateFailed, interactions.StateRejected, interactions.StateCanceled,
			interactions.StateInputRequired:
		default:
			continue
		}
		ix, err := d.ix.Get(ts[i].InteractionID)
		if err != nil {
			continue
		}
		ts[i].StatusText = d.threadStatusText(ix)
	}
}

// threadStatusText is the text of a task's status.message as its A2A
// projection words it (ux UX-W1, UX-C6): the reason of a failed, rejected or
// canceled task, and the payment sentence of a quote that waits for a
// decision. The console shows it as it is and words no reason of its own;
// when the projection's wording changes, the console follows. "" for any
// other state, and for the question of an input-required task that is not
// about payment: the conversation shows that already.
func (d *Daemon) threadStatusText(ix *interactions.Interaction) string {
	none := 0
	t, err := d.taskView(ix, viewOpts{historyLen: &none})
	if err != nil || t.Status.Message == nil {
		return ""
	}
	if ix.State == interactions.StateInputRequired && t.Status.Message.Metadata[a2ashape.KeyX402Status] == nil {
		return ""
	}
	var parts []string
	for _, p := range t.Status.Message.Parts {
		if p.Kind == a2ashape.PartText && strings.TrimSpace(p.Text) != "" {
			parts = append(parts, strings.TrimSpace(p.Text))
		}
	}
	return strings.Join(parts, " ")
}

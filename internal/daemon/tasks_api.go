package daemon

// tasks_api.go is the control plane's task routes (A2A-DESIGN §12):
//
//	POST /tasks/send    SendMessage (new task or follow-up)
//	POST /tasks/get     GetTask
//	POST /tasks/list    ListTasks, with role and peer filters
//	POST /tasks/cancel  CancelTask
//	POST /tasks/wait    wait for a terminal or input-required state newer than after_seq
//	POST /tasks/reply   the provider's answer on a task delegated to this node
//	POST /agents/list   discovery: verified network cards by skill/tag, free text matched here
//	POST /agents/card   one agent's network card (original bytes) and this node's verification
//
// They return the internal/a2ashape projection and share taskseam.go with
// the A2A interface, without the per-agent scope: the control token is the
// node's full authority. (/tasks/pay and /tasks/pay-manual belong to the
// payment work package.)
//
// All of them are bearer-only. None is on the console session allowlist
// (ctlsec.go, §7.3), and TestEveryRouteIsAllowlistedOrRefusedForASession
// names them explicitly.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// taskWaitCap bounds the timeout a caller may ask a control-plane call to
// block for.
const taskWaitCap = time.Hour

// registerTaskRoutes adds the /tasks/* routes to the control plane.
func (d *Daemon) registerTaskRoutes(api *routeMux) {
	api.HandleFunc("POST /tasks/send", d.hTasksSend)
	api.HandleFunc("POST /tasks/get", d.hTasksGet)
	api.HandleFunc("POST /tasks/list", d.hTasksList)
	api.HandleFunc("POST /tasks/cancel", d.hTasksCancel)
	api.HandleFunc("POST /tasks/wait", d.hTasksWait)
	api.HandleFunc("POST /tasks/reply", d.hTasksReply)
	api.HandleFunc("POST /agents/list", d.hAgentsList)
	api.HandleFunc("POST /agents/card", d.hAgentsCard)
}

// writeTaskError answers a task operation's error with a status code and
// the A2A error name.
func writeTaskError(w http.ResponseWriter, err error) {
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
	var req tasksSendReq
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	wait, err := waitBound(req.TimeoutMS)
	if err != nil {
		writeTaskError(w, err)
		return
	}
	ts := module.TaskSend{Message: req.message(), ReturnImmediately: req.ReturnImmediately, HistoryLength: req.HistoryLength}
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
	writeJSON(w, http.StatusOK, t)
}

func (d *Daemon) hTasksGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID        string `json:"task_id"`
		HistoryLength *int   `json:"history_length"`
	}
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	d.pollFresh(r.Context())
	t, err := d.getTask(controlScope, req.TaskID, req.HistoryLength)
	if err != nil {
		writeTaskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
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
	}
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	lr := taskListReq{TaskFilter: module.TaskFilter{ContextID: req.ContextID, State: req.Status,
		PageSize: req.PageSize, PageToken: req.PageToken, HistoryLen: req.HistoryLength,
		IncludeArtifacts: req.IncludeArtifacts}, PeerAID: req.Peer}
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
		TaskID string `json:"task_id"`
	}
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	t, err := d.cancelTask(r.Context(), controlScope, req.TaskID)
	if err != nil {
		writeTaskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// hTasksWait blocks until the task is terminal or input-required at a
// state_seq above after_seq (every task view carries its state_seq as
// metadata anet.state_seq), or timeout_ms elapses. Without after_seq a task
// already waiting for input returns at once. A timed-out wait answers the
// task as it is, with metadata anet.wait = "timed_out".
func (d *Daemon) hTasksWait(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID        string `json:"task_id"`
		AfterSeq      int64  `json:"after_seq"`
		TimeoutMS     int64  `json:"timeout_ms"`
		HistoryLength *int   `json:"history_length"`
	}
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	wait, err := waitBound(req.TimeoutMS)
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
	if timedOut {
		t.Metadata["anet.wait"] = "timed_out"
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
	//   completed                 the message (if any), then completion: a
	//                             receipt is signed over the transcript
	//   failed, rejected          a status with the text as the reason
	//   canceled                  the provider cancels
	State string `json:"state"`
}

func (d *Daemon) hTasksReply(w http.ResponseWriter, r *http.Request) {
	var req replyReq
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	t, err := d.replyTask(r.Context(), req)
	if err != nil {
		writeTaskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
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
		if in, err = parseTaskInput(m); err != nil {
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
			_, err = d.sendMessage(sctx, ix.ID, text, in.atts, map[string]any{"anet.state": string(interactions.StateWorking)})
		}
	case interactions.StateCompleted:
		if in != nil {
			// A conversation turn like any other, so the transcript the
			// receipt covers ends with it and it becomes the anet.reply
			// artifact. (A progress note, anet.state=working, is not a turn
			// and would be left out.) The requester sees input-required
			// for the moment between this message and the result, as with
			// an auto-reply that completes.
			if _, err = d.sendMessage(sctx, ix.ID, text, in.atts, nil); err != nil {
				break
			}
		}
		err = d.CompleteTask(sctx, ix.ID)
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

// hAgentsCard returns one agent's network card (MCP get_agent_card).
func (d *Daemon) hAgentsCard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AID string `json:"aid"`
	}
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	ra, err := d.agentCard(r.Context(), req.AID)
	if err != nil {
		writeTaskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ra)
}

// Package a2ashape is the A2A JSON projection of the daemon's task model
// (A2A-DESIGN §2 "本机 A2A 接口形态", §11.1). It has no build tag and does
// not import a2a-go: the control plane, MCP and module/a2a all speak these
// types, and module/a2a converts them to a2a-go types by a JSON round trip.
//
// PROVISIONAL. This file was written by the C5 daemon work package
// (wp/tasksd) while wp/a2ashape, which owns this package, had not yet
// committed. It defines only what the daemon's TaskSeam and the /tasks/*
// control routes need. When wp/a2ashape is merged, its definitions win:
// delete this file and adapt the daemon to the names it chose.
//
// The JSON of every type here matches a2a-go v2.6.0 (a2a.Task, a2a.Message,
// a2a.Part, a2a.Artifact, the two update events and ListTasksResponse).
package a2ashape

import (
	"encoding/json"
	"errors"
	"time"
)

// Errors a TaskSeam returns, wrapped with detail. module/a2a maps them to
// the a2a-go errors of the same names; the control plane maps them to HTTP
// status codes and an A2A error name.
var (
	ErrTaskNotFound         = errors.New("task not found")
	ErrTaskNotCancelable    = errors.New("task cannot be canceled")
	ErrUnsupportedOperation = errors.New("this operation is not supported")
	ErrInvalidParams        = errors.New("invalid params")
	// ErrUnavailable is a temporary failure to reach the network (no hub,
	// hub unreachable); the operation may succeed later.
	ErrUnavailable = errors.New("unavailable")
)

// ErrorName is the A2A error name of err (TaskNotFoundError, …), or
// "InternalError".
func ErrorName(err error) string {
	switch {
	case errors.Is(err, ErrTaskNotFound):
		return "TaskNotFoundError"
	case errors.Is(err, ErrTaskNotCancelable):
		return "TaskNotCancelableError"
	case errors.Is(err, ErrUnsupportedOperation):
		return "UnsupportedOperationError"
	case errors.Is(err, ErrInvalidParams):
		return "InvalidParamsError"
	case errors.Is(err, ErrUnavailable):
		return "UnavailableError"
	}
	return "InternalError"
}

// TaskState is an A2A task state in its protobuf enum spelling, which is
// what a2a-go v2 writes.
type TaskState string

const (
	StateUnspecified   TaskState = "TASK_STATE_UNSPECIFIED"
	StateSubmitted     TaskState = "TASK_STATE_SUBMITTED"
	StateWorking       TaskState = "TASK_STATE_WORKING"
	StateInputRequired TaskState = "TASK_STATE_INPUT_REQUIRED"
	StateAuthRequired  TaskState = "TASK_STATE_AUTH_REQUIRED"
	StateCompleted     TaskState = "TASK_STATE_COMPLETED"
	StateFailed        TaskState = "TASK_STATE_FAILED"
	StateCanceled      TaskState = "TASK_STATE_CANCELED"
	StateRejected      TaskState = "TASK_STATE_REJECTED"
)

var fromANet = map[string]TaskState{
	"submitted":      StateSubmitted,
	"working":        StateWorking,
	"input-required": StateInputRequired,
	"auth-required":  StateAuthRequired,
	"completed":      StateCompleted,
	"failed":         StateFailed,
	"canceled":       StateCanceled,
	"rejected":       StateRejected,
}

// StateFromANet maps a daemon state (interactions.State, lowercase-hyphen)
// to its A2A name. An unknown value is StateUnspecified.
func StateFromANet(s string) TaskState {
	if st, ok := fromANet[s]; ok {
		return st
	}
	return StateUnspecified
}

// ANetState maps an A2A state name back to the daemon's spelling. It
// accepts the enum name ("TASK_STATE_WORKING") and the daemon's own
// spelling ("working"); ok is false for anything else.
func ANetState(s string) (string, bool) {
	if _, ok := fromANet[s]; ok {
		return s, true
	}
	for k, v := range fromANet {
		if string(v) == s {
			return k, true
		}
	}
	return "", false
}

// Terminal reports whether s is one of the four terminal states.
func (s TaskState) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCanceled, StateRejected:
		return true
	}
	return false
}

// Interrupted reports whether s is a state in which the agent waits for the
// client (A2A "interrupted": input-required, auth-required).
func (s TaskState) Interrupted() bool {
	return s == StateInputRequired || s == StateAuthRequired
}

// Role is the sender role of a message.
type Role string

const (
	RoleUser  Role = "ROLE_USER"
	RoleAgent Role = "ROLE_AGENT"
)

// Part is one content part: exactly one of Text, Raw, Data, URL is set.
type Part struct {
	Text *string `json:"text,omitempty"`
	// Raw is file bytes (base64 in JSON).
	Raw []byte `json:"raw,omitempty"`
	// Data is structured content (any JSON value).
	Data      json.RawMessage `json:"data,omitempty"`
	URL       string          `json:"url,omitempty"`
	Filename  string          `json:"filename,omitempty"`
	MediaType string          `json:"mediaType,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
}

// TextPart is a text part.
func TextPart(s string) Part { return Part{Text: &s} }

// DataPart is a data part holding v's JSON. A value that does not marshal
// becomes JSON null.
func DataPart(v any) Part {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte("null")
	}
	return Part{Data: b}
}

// FilePart is a file part carrying bytes.
func FilePart(name, mediaType string, raw []byte) Part {
	return Part{Raw: raw, Filename: name, MediaType: mediaType}
}

// Message is an A2A message.
type Message struct {
	MessageID        string         `json:"messageId"`
	ContextID        string         `json:"contextId,omitempty"`
	TaskID           string         `json:"taskId,omitempty"`
	Role             Role           `json:"role"`
	Parts            []Part         `json:"parts"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	Extensions       []string       `json:"extensions,omitempty"`
	ReferenceTaskIDs []string       `json:"referenceTaskIds,omitempty"`
}

// TaskStatus is a task's state at a point in time.
type TaskStatus struct {
	State     TaskState  `json:"state"`
	Message   *Message   `json:"message,omitempty"`
	Timestamp *time.Time `json:"timestamp,omitempty"`
}

// Artifact is an output of a task.
type Artifact struct {
	ArtifactID  string         `json:"artifactId"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parts       []Part         `json:"parts"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Extensions  []string       `json:"extensions,omitempty"`
}

// Task is an A2A task. Its id is the anet interaction id.
type Task struct {
	ID        string         `json:"id"`
	ContextID string         `json:"contextId"`
	Status    TaskStatus     `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// TaskStatusUpdateEvent is a status change pushed to a watcher.
type TaskStatusUpdateEvent struct {
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId"`
	Status    TaskStatus     `json:"status"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// TaskArtifactUpdateEvent is an artifact pushed to a watcher.
type TaskArtifactUpdateEvent struct {
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId"`
	Artifact  Artifact       `json:"artifact"`
	Append    bool           `json:"append,omitempty"`
	LastChunk bool           `json:"lastChunk,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// TaskEvent is one event of a Watch: exactly one field is set. Its JSON is
// the A2A StreamResponse wrapper.
type TaskEvent struct {
	StatusUpdate   *TaskStatusUpdateEvent   `json:"statusUpdate,omitempty"`
	ArtifactUpdate *TaskArtifactUpdateEvent `json:"artifactUpdate,omitempty"`
}

// TaskSend is a SendMessage request made to one remote agent.
type TaskSend struct {
	Message Message
	// ReturnImmediately returns as soon as the task is created or the
	// message is sent. Otherwise Send waits for a terminal or interrupted
	// state (A2A-DESIGN §11.5 [C22]).
	ReturnImmediately bool
	// HistoryLength bounds the history in the returned task (nil = all).
	HistoryLength *int
}

// TaskFilter is a ListTasks request (A2A-DESIGN §11.1).
type TaskFilter struct {
	ContextID string
	// State is an A2A state name or the daemon's spelling; empty = any.
	State string
	// PageSize is 1–100; 0 means 50.
	PageSize         int
	PageToken        string
	HistoryLen       *int
	UpdatedAfter     *time.Time
	IncludeArtifacts bool
}

// TaskPage is a ListTasks answer. NextPageToken is empty on the last page.
type TaskPage struct {
	Tasks         []Task `json:"tasks"`
	TotalSize     int    `json:"totalSize"`
	PageSize      int    `json:"pageSize"`
	NextPageToken string `json:"nextPageToken"`
}

// AgentQuery is a discovery request (A2A-DESIGN §10.5). Skill and Tag are
// sent to the hub; Query is free text matched locally and never sent.
type AgentQuery struct {
	Skill  string `json:"skill,omitempty"`
	Tag    string `json:"tag,omitempty"`
	Query  string `json:"q,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// RemoteAgent is one agent of the network, as this node sees it.
type RemoteAgent struct {
	AID string `json:"aid"`
	// Card is the agent's network card, the exact bytes the hub served
	// (empty when the agent publishes none).
	Card json.RawMessage `json:"card,omitempty"`
	// Verification is this node's own check of Card: "VERIFIED", or
	// "UNVERIFIED" with VerificationError saying why. The hub's statement
	// is HubVerification; it is not a substitute.
	Verification      string `json:"verification"`
	VerificationError string `json:"verificationError,omitempty"`
	HubVerification   string `json:"hubVerification,omitempty"`
	Name              string `json:"name,omitempty"`
	HomeHub           string `json:"homeHub,omitempty"`
	LastSeen          string `json:"lastSeen,omitempty"`
	Quiet             bool   `json:"quiet,omitempty"`
	ReviewCount       int    `json:"reviewCount,omitempty"`
	AvgRating         any    `json:"avgRating,omitempty"`
}

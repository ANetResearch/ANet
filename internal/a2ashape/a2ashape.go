// Package a2ashape is the A2A JSON projection of this node's tasks
// (A2A-DESIGN §11.1, §11.5, §12): the Task, Message, Part and Artifact of the
// A2A v1.0 data model, the two streaming events, and a ListTasks page — as
// plain Go types whose encoding/json form is the A2A JSON form.
//
// It exists so that the kernel can speak A2A without importing an A2A SDK.
// The control plane and MCP return these values directly, and module/a2a
// (the local A2A interface, the one place that imports a2a-go) converts them
// to the SDK's types by a JSON round trip. That conversion is only safe if the
// two encodings agree, so every type here mirrors a2a-go v2.6.0 field for
// field — same JSON names, same field order, same omitempty choices, the same
// flattened Part — and the contract tests in this package decode what it
// emits with a2a-go, encode it again, and compare. The package must never
// import a2a-go itself (SI-8): a no_a2a build, and the dependency closure of
// internal/daemon and internal/mcpserv, would otherwise contain it.
//
// Enum values are the proto names the specification requires on the JSON
// wire (TASK_STATE_COMPLETED, ROLE_USER); timestamps are RFC 3339 in UTC.
package a2ashape

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// TaskState is an A2A task state, in its JSON form (the proto enum name).
type TaskState string

// Task states. Unspecified is the zero value and never appears in a
// projection.
const (
	TaskStateUnspecified   TaskState = ""
	TaskStateSubmitted     TaskState = "TASK_STATE_SUBMITTED"
	TaskStateWorking       TaskState = "TASK_STATE_WORKING"
	TaskStateCompleted     TaskState = "TASK_STATE_COMPLETED"
	TaskStateFailed        TaskState = "TASK_STATE_FAILED"
	TaskStateCanceled      TaskState = "TASK_STATE_CANCELED"
	TaskStateInputRequired TaskState = "TASK_STATE_INPUT_REQUIRED"
	TaskStateRejected      TaskState = "TASK_STATE_REJECTED"
	TaskStateAuthRequired  TaskState = "TASK_STATE_AUTH_REQUIRED"
)

const taskStateUnspecifiedJSON = "TASK_STATE_UNSPECIFIED"

// String returns the JSON name; the zero value is TASK_STATE_UNSPECIFIED.
func (s TaskState) String() string {
	if s == TaskStateUnspecified {
		return taskStateUnspecifiedJSON
	}
	return string(s)
}

// MarshalJSON writes the proto enum name, as a2a-go does.
func (s TaskState) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// UnmarshalJSON reads the proto enum name.
func (s *TaskState) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if v == taskStateUnspecifiedJSON {
		v = ""
	}
	*s = TaskState(v)
	return nil
}

// Terminal reports the four states after which a task takes no more input.
func (s TaskState) Terminal() bool {
	switch s {
	case TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected:
		return true
	}
	return false
}

// Interrupted reports the two states in which the task waits for the
// client. A blocking SendMessage returns at a terminal or interrupted state.
func (s TaskState) Interrupted() bool {
	return s == TaskStateInputRequired || s == TaskStateAuthRequired
}

// Valid reports whether s is one of the eight defined states.
func (s TaskState) Valid() bool {
	switch s {
	case TaskStateSubmitted, TaskStateWorking, TaskStateCompleted, TaskStateFailed,
		TaskStateCanceled, TaskStateInputRequired, TaskStateRejected, TaskStateAuthRequired:
		return true
	}
	return false
}

// Role is the sender of a message, in its JSON form. The requester of a task
// is the user and the provider is the agent, on both sides of it.
type Role string

// Message roles.
const (
	RoleUnspecified Role = ""
	RoleUser        Role = "ROLE_USER"
	RoleAgent       Role = "ROLE_AGENT"
)

const roleUnspecifiedJSON = "ROLE_UNSPECIFIED"

// String returns the JSON name; the zero value is ROLE_UNSPECIFIED.
func (r Role) String() string {
	if r == RoleUnspecified {
		return roleUnspecifiedJSON
	}
	return string(r)
}

// MarshalJSON writes the proto enum name.
func (r Role) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

// UnmarshalJSON reads the proto enum name.
func (r *Role) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if v == roleUnspecifiedJSON {
		v = ""
	}
	*r = Role(v)
	return nil
}

// Task is an A2A Task. The id is the interaction id.
type Task struct {
	ID        string         `json:"id"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	ContextID string         `json:"contextId"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Status    TaskStatus     `json:"status"`
}

// TaskStatus is a task's state at a point in time, with an optional message
// saying why.
type TaskStatus struct {
	Message   *Message   `json:"message,omitempty"`
	State     TaskState  `json:"state"`
	Timestamp *time.Time `json:"timestamp,omitempty"`
}

// Message is one A2A message.
type Message struct {
	ID             string         `json:"messageId"`
	ContextID      string         `json:"contextId,omitempty"`
	Extensions     []string       `json:"extensions,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	Parts          []Part         `json:"parts"`
	ReferenceTasks []string       `json:"referenceTaskIds,omitempty"`
	Role           Role           `json:"role"`
	TaskID         string         `json:"taskId,omitempty"`
}

// Artifact is one output of a task.
type Artifact struct {
	ID          string         `json:"artifactId"`
	Description string         `json:"description,omitempty"`
	Extensions  []string       `json:"extensions,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Name        string         `json:"name,omitempty"`
	Parts       []Part         `json:"parts"`
}

// PartKind says which content a Part carries. A2A's Part is a oneof of
// text, raw bytes, a URL and structured data; the JSON form flattens the
// oneof into the object, so the kind is not written, only the one field.
type PartKind uint8

// Part kinds. The zero value is no content, which does not encode.
const (
	PartText PartKind = iota + 1
	PartRaw
	PartURL
	PartData
)

// Part is one piece of a message or an artifact: exactly one of text, raw
// bytes (a file carried inline), a URL (a file carried by reference) or
// structured data. Filename, MediaType and Metadata apply to every kind.
type Part struct {
	Kind      PartKind
	Text      string // PartText
	Raw       []byte // PartRaw; base64 on the wire
	URL       string // PartURL
	Data      any    // PartData: any JSON value
	Filename  string
	MediaType string
	Metadata  map[string]any
}

// TextPart is a text part.
func TextPart(s string) Part { return Part{Kind: PartText, Text: s} }

// DataPart is a structured-data part carrying v.
func DataPart(v any) Part { return Part{Kind: PartData, Data: v, MediaType: "application/json"} }

// RawPart is a file carried inline.
func RawPart(b []byte, filename, mediaType string) Part {
	return Part{Kind: PartRaw, Raw: b, Filename: filename, MediaType: mediaType}
}

// URLPart is a file carried by reference.
func URLPart(u, filename, mediaType string) Part {
	return Part{Kind: PartURL, URL: u, Filename: filename, MediaType: mediaType}
}

// partJSON is the wire form of a Part: the oneof flattened into the object.
// Field order and omitempty follow a2a-go's part type, so the bytes agree.
type partJSON struct {
	Text      *string          `json:"text,omitempty"`
	Raw       *[]byte          `json:"raw,omitempty"`
	Data      *json.RawMessage `json:"data,omitempty"`
	URL       *string          `json:"url,omitempty"`
	Filename  string           `json:"filename,omitempty"`
	MediaType string           `json:"mediaType,omitempty"`
	Metadata  map[string]any   `json:"metadata,omitempty"`
}

// MarshalJSON writes the flattened form. A part with no content, or a URL
// part with an empty URL (which the wire cannot tell from no URL), is an
// error rather than an object a reader would refuse.
func (p Part) MarshalJSON() ([]byte, error) {
	w := partJSON{Filename: p.Filename, MediaType: p.MediaType, Metadata: p.Metadata}
	switch p.Kind {
	case PartText:
		w.Text = &p.Text
	case PartRaw:
		raw := p.Raw
		if raw == nil {
			raw = []byte{}
		}
		w.Raw = &raw
	case PartURL:
		if p.URL == "" {
			return nil, errors.New("a2ashape: url part with an empty url")
		}
		w.URL = &p.URL
	case PartData:
		b, err := json.Marshal(p.Data)
		if err != nil {
			return nil, fmt.Errorf("a2ashape: data part: %w", err)
		}
		if string(b) == "null" {
			// Both readers take "data":null for no data at all.
			return nil, errors.New("a2ashape: data part with null data")
		}
		rm := json.RawMessage(b)
		w.Data = &rm
	default:
		return nil, errors.New("a2ashape: part has no content")
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads the flattened form. Exactly one of text, raw, data
// and url must be present, as a2a-go requires; an empty url and a null data
// count as absent there, so they do here. Numbers in data are kept as
// json.Number, so an integer is not rounded through float64.
func (p *Part) UnmarshalJSON(b []byte) error {
	var w partJSON
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	out := Part{Filename: w.Filename, MediaType: w.MediaType, Metadata: w.Metadata}
	n := 0
	if w.Text != nil {
		out.Kind, out.Text = PartText, *w.Text
		n++
	}
	if w.Raw != nil {
		out.Kind, out.Raw = PartRaw, *w.Raw
		n++
	}
	if w.Data != nil {
		v, err := decodeJSON(*w.Data)
		if err != nil {
			return fmt.Errorf("a2ashape: data part: %w", err)
		}
		out.Kind, out.Data = PartData, v
		n++
	}
	if w.URL != nil && *w.URL != "" {
		out.Kind, out.URL = PartURL, *w.URL
		n++
	}
	switch {
	case n == 0:
		return errors.New("a2ashape: part has none of text, raw, data or url")
	case n > 1:
		return fmt.Errorf("a2ashape: part has %d of text, raw, data and url; exactly one is allowed", n)
	}
	*p = out
	return nil
}

// TaskStatusUpdateEvent reports a change of a task's status.
type TaskStatusUpdateEvent struct {
	ContextID string         `json:"contextId"`
	Status    TaskStatus     `json:"status"`
	TaskID    string         `json:"taskId"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// TaskArtifactUpdateEvent reports an artifact of a task. The projection
// sends each artifact whole: Append is false and LastChunk true.
type TaskArtifactUpdateEvent struct {
	Append    bool           `json:"append,omitempty"`
	Artifact  Artifact       `json:"artifact"`
	ContextID string         `json:"contextId"`
	LastChunk bool           `json:"lastChunk,omitempty"`
	TaskID    string         `json:"taskId"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// TaskEvent is one item of a task's stream (A2A StreamResponse): exactly
// one field is set. A stream starts with the Task and continues with status and
// artifact updates until the task is terminal.
type TaskEvent struct {
	Message        *Message                 `json:"message,omitempty"`
	Task           *Task                    `json:"task,omitempty"`
	StatusUpdate   *TaskStatusUpdateEvent   `json:"statusUpdate,omitempty"`
	ArtifactUpdate *TaskArtifactUpdateEvent `json:"artifactUpdate,omitempty"`
}

// count is how many of the event's fields are set.
func (e TaskEvent) count() int {
	n := 0
	if e.Message != nil {
		n++
	}
	if e.Task != nil {
		n++
	}
	if e.StatusUpdate != nil {
		n++
	}
	if e.ArtifactUpdate != nil {
		n++
	}
	return n
}

// MarshalJSON refuses an event that is not exactly one thing, which a
// reader would refuse anyway.
func (e TaskEvent) MarshalJSON() ([]byte, error) {
	if n := e.count(); n != 1 {
		return nil, fmt.Errorf("a2ashape: event has %d payloads; exactly one is allowed", n)
	}
	type plain TaskEvent
	return json.Marshal(plain(e))
}

// UnmarshalJSON reads a stream item and checks it is exactly one thing.
func (e *TaskEvent) UnmarshalJSON(b []byte) error {
	type plain TaskEvent
	var v plain
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if n := TaskEvent(v).count(); n != 1 {
		return fmt.Errorf("a2ashape: event has %d payloads; exactly one is allowed", n)
	}
	*e = TaskEvent(v)
	return nil
}

// TaskPage is one page of ListTasks (A2A ListTasksResponse). NextPageToken
// is empty on the last page.
type TaskPage struct {
	Tasks         []Task `json:"tasks"`
	TotalSize     int    `json:"totalSize"`
	PageSize      int    `json:"pageSize"`
	NextPageToken string `json:"nextPageToken"`
}

// MarshalJSON writes an empty page's tasks as [] rather than null: the
// field is required, and a reader that ranges over it should not have to
// tell the two apart.
func (p TaskPage) MarshalJSON() ([]byte, error) {
	type plain TaskPage
	if p.Tasks == nil {
		p.Tasks = []Task{}
	}
	return json.Marshal(plain(p))
}

// ListTasks page-size bounds (A2A ListTasksRequest.page_size).
const (
	DefaultPageSize = 50
	MaxPageSize     = 100
)

// PageSize applies the ListTasks bounds: 0 (unset) is the default, 1 to 100
// is taken as given, anything else is InvalidParams.
func PageSize(n int) (int, error) {
	switch {
	case n == 0:
		return DefaultPageSize, nil
	case n < 1 || n > MaxPageSize:
		return 0, Errorf(ErrInvalidParams, "pageSize must be between 1 and %d, got %d", MaxPageSize, n)
	}
	return n, nil
}

// StatusUpdate is the status event for a projected task: its status and its
// metadata, so a subscriber sees anet.effect_status and the x402 keys
// change with the state rather than only on the next GetTask.
func StatusUpdate(t Task) TaskStatusUpdateEvent {
	return TaskStatusUpdateEvent{ContextID: t.ContextID, Status: t.Status, TaskID: t.ID, Metadata: t.Metadata}
}

// ArtifactUpdates are the artifact events for a projected task, one per
// artifact, in the task's order. A stream sends them before the terminal
// status update (A2A-DESIGN §11.5), so a client that stops reading at the
// terminal state has the reply already.
func ArtifactUpdates(t Task) []TaskArtifactUpdateEvent {
	out := make([]TaskArtifactUpdateEvent, 0, len(t.Artifacts))
	for _, a := range t.Artifacts {
		out = append(out, TaskArtifactUpdateEvent{Artifact: a, ContextID: t.ContextID, LastChunk: true, TaskID: t.ID})
	}
	return out
}

// decodeJSON decodes one JSON value keeping numbers as json.Number.
func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the JSON value")
	}
	return finite(v), nil
}

// finite replaces a number too large for a float64 (1e400) with its text.
// Numbers are otherwise kept as written, but a reader that decodes into
// float64 — a2a-go, which module/a2a converts to — refuses such a number
// outright, and the value may be a peer's (message metadata, a
// deliverable): one of them would make the task, and any ListTasks page
// holding it, unreadable to the local A2A client.
func finite(v any) any {
	switch x := v.(type) {
	case json.Number:
		if _, err := x.Float64(); err != nil {
			return x.String()
		}
	case map[string]any:
		for k, e := range x {
			x[k] = finite(e)
		}
	case []any:
		for i, e := range x {
			x[i] = finite(e)
		}
	}
	return v
}

// decodeObject decodes a JSON object (numbers as json.Number). Anything
// else, including an empty input, is nil.
func decodeObject(b []byte) map[string]any {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	v, err := decodeJSON(b)
	if err != nil {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

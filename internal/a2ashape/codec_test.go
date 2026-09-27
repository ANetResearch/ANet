package a2ashape_test

// codec_test.go pins the projection's JSON against a2a-go v2.6.0, the SDK
// module/a2a converts it to. The test may import a2a-go; the package must
// not (SI-8), and TestNoSDKImport checks that it does not.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/build"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return b
}

// viaSDK decodes b into the a2a-go type sdk points to and encodes it again.
func viaSDK(t *testing.T, b []byte, sdk any) []byte {
	t.Helper()
	if err := json.Unmarshal(b, sdk); err != nil {
		t.Fatalf("a2a-go cannot read %s: %v", b, err)
	}
	return mustJSON(t, sdk)
}

// sameBytes asserts the SDK re-encodes the projection byte for byte.
func sameBytes(t *testing.T, ours []byte, sdk any) {
	t.Helper()
	if again := viaSDK(t, ours, sdk); !bytes.Equal(ours, again) {
		t.Fatalf("a2a-go re-encodes differently:\nours: %s\nsdk:  %s", ours, again)
	}
}

func ts() *time.Time {
	t := time.UnixMilli(1790000000123).UTC()
	return &t
}

// fullTask exercises every field and every part kind.
func fullTask() a2ashape.Task {
	raw := a2ashape.RawPart([]byte{0xff, 0xfe, 0x00, 'x'}, "a.png", "image/png")
	raw.Metadata = map[string]any{a2ashape.KeyCID: "bafkq", a2ashape.KeySize: 4}
	msg := a2ashape.Message{
		ID: "msg_1", ContextID: "ctx_1", TaskID: "ix_1", Role: a2ashape.RoleUser,
		Extensions:     []string{a2ashape.X402ExtensionURI},
		ReferenceTasks: []string{"ix_0"},
		Metadata:       map[string]any{a2ashape.KeyMessageID: "client-1", "n": 1.5, "html": "<a&b>"},
		Parts: []a2ashape.Part{
			a2ashape.TextPart("hello, 世界"),
			raw,
			a2ashape.URLPart("anet:attachment?interaction_id=ix_1&cid=bafkq", "b.pdf", "application/pdf"),
			a2ashape.DataPart(map[string]any{"skill": "cas.put", "args": map[string]any{"k": []any{"v", true, nil}}}),
			{Kind: a2ashape.PartText, Text: ""},
		},
	}
	agent := a2ashape.Message{ID: "msg_2", ContextID: "ctx_1", TaskID: "ix_1", Role: a2ashape.RoleAgent,
		Parts: []a2ashape.Part{a2ashape.TextPart("done")}}
	return a2ashape.Task{
		ID: "ix_1", ContextID: "ctx_1",
		History: []a2ashape.Message{msg, agent},
		Artifacts: []a2ashape.Artifact{
			{ID: a2ashape.ArtifactReply, Name: a2ashape.ArtifactReply, Description: "the reply",
				Parts: []a2ashape.Part{a2ashape.TextPart("done")}},
			{ID: a2ashape.ArtifactReceipt, Extensions: []string{"x"}, Metadata: map[string]any{"k": "v"},
				Parts: []a2ashape.Part{a2ashape.DataPart(map[string]any{"receipt": "AAEC", "completed_at": 1790000000123})}},
		},
		Metadata: map[string]any{
			a2ashape.KeyEffectStatus: "UNVERIFIED", a2ashape.KeyReceiptVerified: a2ashape.ReceiptVerified,
			a2ashape.KeyRetryAfterMS: 60000, a2ashape.KeyCancelRequested: true,
		},
		Status: a2ashape.TaskStatus{State: a2ashape.TaskStateCompleted, Timestamp: ts(), Message: &agent},
	}
}

// A task the projection writes is re-encoded by a2a-go to the same bytes,
// and a2a-go's encoding of it reads back into the projection unchanged.
func TestTaskBytesAgreeWithSDK(t *testing.T) {
	ours := mustJSON(t, fullTask())
	sameBytes(t, ours, &a2a.Task{})

	var back a2ashape.Task
	if err := json.Unmarshal(ours, &back); err != nil {
		t.Fatal(err)
	}
	if again := mustJSON(t, back); !bytes.Equal(ours, again) {
		t.Fatalf("projection does not round-trip itself:\n%s\n%s", ours, again)
	}
}

// The other direction: what a2a-go writes, the projection reads and writes
// back identically. module/a2a converts client messages this way.
func TestSDKTaskReadsBack(t *testing.T) {
	now := time.UnixMilli(1790000000456).UTC()
	m := a2a.NewMessageForTask(a2a.MessageRoleUser, a2a.TaskInfo{TaskID: "ix_9", ContextID: "c"},
		a2a.NewTextPart("hi"), a2a.NewRawPart([]byte("bytes")), a2a.NewFileURLPart("https://x/y", "text/plain"),
		a2a.NewDataPart(map[string]any{"a": 1.0}))
	m.ID = "fixed"
	sdk := &a2a.Task{ID: "ix_9", ContextID: "c", History: []*a2a.Message{m},
		Status:    a2a.TaskStatus{State: a2a.TaskStateInputRequired, Timestamp: &now, Message: m},
		Artifacts: []*a2a.Artifact{{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart("x")}}},
		Metadata:  map[string]any{"k": "v"}}
	b := mustJSON(t, sdk)
	var ours a2ashape.Task
	if err := json.Unmarshal(b, &ours); err != nil {
		t.Fatal(err)
	}
	if again := mustJSON(t, ours); !bytes.Equal(b, again) {
		t.Fatalf("\nsdk:  %s\nours: %s", b, again)
	}
	if ours.Status.State != a2ashape.TaskStateInputRequired || ours.History[0].Parts[1].Kind != a2ashape.PartRaw ||
		string(ours.History[0].Parts[1].Raw) != "bytes" || ours.History[0].Parts[2].URL != "https://x/y" {
		t.Fatalf("decoded %+v", ours)
	}
}

// Every state and role constant has the SDK's JSON spelling, and the zero
// values spell "unspecified" the same way.
func TestEnumSpellings(t *testing.T) {
	states := map[a2ashape.TaskState]a2a.TaskState{
		a2ashape.TaskStateUnspecified:   a2a.TaskStateUnspecified,
		a2ashape.TaskStateSubmitted:     a2a.TaskStateSubmitted,
		a2ashape.TaskStateWorking:       a2a.TaskStateWorking,
		a2ashape.TaskStateCompleted:     a2a.TaskStateCompleted,
		a2ashape.TaskStateFailed:        a2a.TaskStateFailed,
		a2ashape.TaskStateCanceled:      a2a.TaskStateCanceled,
		a2ashape.TaskStateInputRequired: a2a.TaskStateInputRequired,
		a2ashape.TaskStateRejected:      a2a.TaskStateRejected,
		a2ashape.TaskStateAuthRequired:  a2a.TaskStateAuthRequired,
	}
	for ours, sdk := range states {
		if a, b := mustJSON(t, ours), mustJSON(t, sdk); !bytes.Equal(a, b) {
			t.Errorf("state %q: ours %s, sdk %s", ours, a, b)
		}
		if ours.Terminal() != sdk.Terminal() {
			t.Errorf("state %q: terminal disagrees", ours)
		}
		var back a2ashape.TaskState
		if err := json.Unmarshal(mustJSON(t, sdk), &back); err != nil || back != ours {
			t.Errorf("state %q read back as %q (%v)", sdk, back, err)
		}
	}
	roles := map[a2ashape.Role]a2a.MessageRole{
		a2ashape.RoleUnspecified: a2a.MessageRoleUnspecified,
		a2ashape.RoleUser:        a2a.MessageRoleUser,
		a2ashape.RoleAgent:       a2a.MessageRoleAgent,
	}
	for ours, sdk := range roles {
		if a, b := mustJSON(t, ours), mustJSON(t, sdk); !bytes.Equal(a, b) {
			t.Errorf("role %q: ours %s, sdk %s", ours, a, b)
		}
	}
}

// The part forms of a2a-go's own codec test, reproduced byte for byte.
func TestPartShapes(t *testing.T) {
	cases := []struct {
		part a2ashape.Part
		want string
	}{
		{a2ashape.TextPart("hello, world"), `{"text":"hello, world"}`},
		{a2ashape.Part{Kind: a2ashape.PartData, Data: map[string]any{"foo": "bar"}}, `{"data":{"foo":"bar"}}`},
		{a2ashape.Part{Kind: a2ashape.PartURL, URL: "https://cats.com/1.png", Filename: "foo"}, `{"url":"https://cats.com/1.png","filename":"foo"}`},
		{a2ashape.RawPart([]byte{0xFF, 0xFE}, "foo", "image/png"), `{"raw":"//4=","filename":"foo","mediaType":"image/png"}`},
		{a2ashape.Part{Kind: a2ashape.PartText, Text: "42", Metadata: map[string]any{"foo": "bar"}}, `{"text":"42","metadata":{"foo":"bar"}}`},
		{a2ashape.RawPart(nil, "", ""), `{"raw":""}`},
	}
	for _, c := range cases {
		got := mustJSON(t, c.part)
		if string(got) != c.want {
			t.Errorf("got %s, want %s", got, c.want)
		}
		var sdk a2a.Part
		sameBytes(t, got, &sdk)
	}
}

func TestPartRefusals(t *testing.T) {
	for _, p := range []a2ashape.Part{{}, {Kind: a2ashape.PartURL}, a2ashape.DataPart(nil)} {
		if _, err := json.Marshal(p); err == nil {
			t.Errorf("encoded a part with no content: %+v", p)
		}
	}
	for _, s := range []string{`{}`, `{"url":""}`, `{"data":null}`, `{"text":"a","raw":"AA=="}`, `{"text":"a","data":1}`} {
		var p a2ashape.Part
		if err := json.Unmarshal([]byte(s), &p); err == nil {
			t.Errorf("read %s as %+v", s, p)
		}
		// The SDK refuses the same inputs.
		var sdk a2a.Part
		if err := json.Unmarshal([]byte(s), &sdk); err == nil {
			t.Errorf("a2a-go accepts %s; the two codecs disagree", s)
		}
	}
}

// A data part keeps an integer exact: the projection reads numbers as
// json.Number, not float64.
func TestDataNumbersExact(t *testing.T) {
	var p a2ashape.Part
	if err := json.Unmarshal([]byte(`{"data":{"n":12345678901234567890}}`), &p); err != nil {
		t.Fatal(err)
	}
	if got := string(mustJSON(t, p)); got != `{"data":{"n":12345678901234567890}}` {
		t.Fatalf("got %s", got)
	}
}

// A number no float64 holds cannot reach a2a-go, which refuses it and with
// it the whole task; the projection carries it as text.
func TestDataNumbersOutOfRange(t *testing.T) {
	var p a2ashape.Part
	if err := json.Unmarshal([]byte(`{"data":{"big":1e400,"list":[-1e999,1],"tiny":1e-400}}`), &p); err != nil {
		t.Fatal(err)
	}
	b := mustJSON(t, p)
	if string(b) != `{"data":{"big":"1e400","list":["-1e999",1],"tiny":1e-400}}` {
		t.Fatalf("got %s", b)
	}
	var sdk a2a.Part
	if err := json.Unmarshal(b, &sdk); err != nil {
		t.Fatalf("a2a-go cannot read %s: %v", b, err)
	}
}

// The streaming events and their wrapper agree with a2a-go's.
func TestEventsAgreeWithSDK(t *testing.T) {
	task := fullTask()
	st := a2ashape.StatusUpdate(task)
	sameBytes(t, mustJSON(t, st), &a2a.TaskStatusUpdateEvent{})
	arts := a2ashape.ArtifactUpdates(task)
	if len(arts) != len(task.Artifacts) || !arts[0].LastChunk || arts[0].Append || arts[0].TaskID != "ix_1" {
		t.Fatalf("artifact updates %+v", arts)
	}
	for _, a := range arts {
		sameBytes(t, mustJSON(t, a), &a2a.TaskArtifactUpdateEvent{})
	}
	events := []a2ashape.TaskEvent{
		{Task: &task},
		{Message: task.Status.Message},
		{StatusUpdate: &st},
		{ArtifactUpdate: &arts[0]},
	}
	for _, e := range events {
		b := mustJSON(t, e)
		var sr a2a.StreamResponse
		sameBytes(t, b, &sr)
		var back a2ashape.TaskEvent
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
	}
	var sr a2a.StreamResponse
	_ = viaSDK(t, mustJSON(t, a2ashape.TaskEvent{StatusUpdate: &st}), &sr)
	if _, ok := sr.Event.(*a2a.TaskStatusUpdateEvent); !ok {
		t.Fatalf("a2a-go read a status update as %T", sr.Event)
	}
	if _, err := json.Marshal(a2ashape.TaskEvent{}); err == nil {
		t.Error("encoded an empty event")
	}
	if _, err := json.Marshal(a2ashape.TaskEvent{Task: &task, StatusUpdate: &st}); err == nil {
		t.Error("encoded an event with two payloads")
	}
	var e a2ashape.TaskEvent
	if err := json.Unmarshal([]byte(`{"task":{"id":"a","contextId":"b","status":{"state":"TASK_STATE_WORKING"}},"message":{"messageId":"m","parts":[],"role":"ROLE_USER"}}`), &e); err == nil {
		t.Error("read an event with two payloads")
	}
}

func TestPageAgreesWithSDK(t *testing.T) {
	p := a2ashape.TaskPage{Tasks: []a2ashape.Task{fullTask()}, TotalSize: 3, PageSize: 1, NextPageToken: "1790000000123.7"}
	sameBytes(t, mustJSON(t, p), &a2a.ListTasksResponse{})
	empty := mustJSON(t, a2ashape.TaskPage{PageSize: 50})
	if string(empty) != `{"tasks":[],"totalSize":0,"pageSize":50,"nextPageToken":""}` {
		t.Fatalf("empty page %s", empty)
	}
	sameBytes(t, empty, &a2a.ListTasksResponse{})
}

func TestPageSize(t *testing.T) {
	for in, want := range map[int]int{0: 50, 1: 1, 100: 100} {
		if got, err := a2ashape.PageSize(in); err != nil || got != want {
			t.Errorf("PageSize(%d) = %d, %v", in, got, err)
		}
	}
	for _, in := range []int{-1, 101, 150} {
		if _, err := a2ashape.PageSize(in); !errors.Is(err, a2ashape.ErrInvalidParams) {
			t.Errorf("PageSize(%d) = %v, want InvalidParams", in, err)
		}
	}
}

func TestErrorsMatchByName(t *testing.T) {
	err := a2ashape.Errorf(a2ashape.ErrTaskNotFound, "ix_1")
	if !errors.Is(err, a2ashape.ErrTaskNotFound) || errors.Is(err, a2ashape.ErrTaskNotCancelable) {
		t.Fatalf("errors.Is wrong for %v", err)
	}
	if err.Error() != "TaskNotFoundError: ix_1" {
		t.Fatalf("message %q", err)
	}
	wrapped := fmt.Errorf("get: %w", err)
	if a2ashape.ErrorName(wrapped) != "TaskNotFoundError" || a2ashape.ErrorName(errors.New("x")) != "InternalError" ||
		!errors.Is(fmt.Errorf("%w: ix", a2ashape.ErrInvalidParams), a2ashape.ErrInvalidParams) {
		t.Fatalf("ErrorName / wrapping")
	}
}

// The strings other parts of the system (module/a2a, MCP descriptions,
// clients) match on. A change here is a wire change.
func TestKeyStrings(t *testing.T) {
	pinned := map[string]string{
		a2ashape.KeyEffectStatus:      "anet.effect_status",
		a2ashape.KeyReceiptVerified:   "anet.receipt_verified",
		a2ashape.KeyRequestCID:        "anet.request_cid",
		a2ashape.KeyResultCID:         "anet.result_cid",
		a2ashape.KeyPeerAID:           "anet.peer_aid",
		a2ashape.KeyReason:            "anet.reason",
		a2ashape.KeyRetryAfterMS:      "anet.retry_after_ms",
		a2ashape.KeyCancelRequested:   "anet.cancel_requested",
		a2ashape.KeyStateSeq:          "anet.state_seq",
		a2ashape.KeyTrust:             "anet.trust",
		a2ashape.KeySkill:             "anet.skill",
		a2ashape.KeyRole:              "anet.role",
		a2ashape.KeyInbound:           "anet.inbound",
		a2ashape.KeyState:             "anet.state",
		a2ashape.KeyA2AError:          "anet.a2aError",
		a2ashape.KeyServiceParameters: "a2a.serviceParameters",
		a2ashape.KeyMessageID:         "a2a.messageId",
		a2ashape.KeyX402Status:        "x402.payment.status",
		a2ashape.KeyX402Required:      "x402.payment.required",
		a2ashape.KeyX402Payload:       "x402.payment.payload",
		a2ashape.KeyX402Receipts:      "x402.payment.receipts",
		a2ashape.KeyX402Error:         "x402.payment.error",
		a2ashape.KeyPaymentAccept:     "anet.payment.accept",
		a2ashape.X402ExtensionURI:     "https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2",
	}
	for got, want := range pinned {
		if got != want {
			t.Errorf("key %q, want %q", got, want)
		}
	}
}

// SI-8: the package itself imports no A2A SDK. Tests may.
func TestNoSDKImport(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		if strings.Contains(imp, "a2aproject") {
			t.Errorf("a2ashape imports %s", imp)
		}
	}
}

// Decoding and re-encoding through a2a-go leaves the projection's JSON
// meaning the same (keys, values, order-insensitive), which is the weaker
// property the byte comparison implies; kept separate for the projection
// tests, whose numbers may be json.Number.
func semanticallySame(t *testing.T, ours []byte, sdk any) {
	t.Helper()
	again := viaSDK(t, ours, sdk)
	var a, b any
	if err := json.Unmarshal(ours, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(again, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("a2a-go changed the meaning:\nours: %s\nsdk:  %s", ours, again)
	}
}

package a2ashape

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// jsonLen is the size encoding/json writes for a string, escapes included:
// the budget is only as good as this count.
func TestJSONLenIsWhatEncodingJSONWrites(t *testing.T) {
	for _, s := range []string{"", "plain", `"quoted\"`, "<a href='x'>&amp;</a>", "tab\tnew\nline\r",
		"\x00\x01\x1f", "  ", "中文 émoji 🙂", "bad \xff\xfe utf-8", strings.Repeat("<", 1000)} {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if got := jsonLen(s); got != len(b) {
			t.Errorf("jsonLen(%q) = %d, encoding/json writes %d", s, got, len(b))
		}
	}
}

// A task within the limit is carried as it is; one past it comes out
// within the limit as encoded, with what was cut marked.
func TestTaskForStreamHoldsAnEventToTheLimit(t *testing.T) {
	small := Task{ID: "t", ContextID: "c", Status: TaskStatus{State: TaskStateInputRequired,
		Message: &Message{ID: "m", Role: RoleAgent, Parts: []Part{TextPart("a question")}}},
		History:  []Message{{ID: "h", Role: RoleUser, Parts: []Part{TextPart("hi")}}},
		Metadata: map[string]any{"anet.peer_aid": "x"}}
	if got := TaskForStream(small); !reflect.DeepEqual(got, small) {
		t.Fatalf("a small task was changed: %+v", got)
	}

	huge := strings.Repeat("<&>", 1<<20) // 3 MiB, 18 MB as JSON
	history := make([]Message, 200)
	for i := range history {
		history[i] = Message{ID: "h", Role: RoleAgent, Parts: []Part{TextPart(strings.Repeat("x", 64<<10))}}
	}
	big := Task{ID: "t", ContextID: "c",
		Status: TaskStatus{State: TaskStateCompleted, Message: &Message{ID: "m", Role: RoleAgent,
			Parts: []Part{TextPart(huge)}, Metadata: map[string]any{KeyState: "working"}}},
		Artifacts: []Artifact{{ID: ArtifactReply, Parts: []Part{TextPart(huge)}},
			{ID: "anet.receipt", Parts: []Part{DataPart(map[string]any{"request_cid": "bafy"})}}},
		History:  history,
		Metadata: map[string]any{"anet.peer_aid": "x", "note": huge}}
	got := TaskForStream(big)
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > MaxStreamEventBytes {
		t.Fatalf("the event is %d bytes as JSON, over %d", len(b), MaxStreamEventBytes)
	}
	if got.Metadata[KeyTruncated] != true || got.Status.Message.Metadata[KeyTruncated] != true ||
		got.Status.Message.Metadata[KeyState] != "working" || got.Artifacts[0].Metadata[KeyTruncated] != true {
		t.Fatalf("what was cut is not marked: task %v status %v reply %v", got.Metadata,
			got.Status.Message.Metadata, got.Artifacts[0].Metadata)
	}
	if len(got.Artifacts) != 2 || got.Artifacts[1].Metadata[KeyTruncated] == true {
		t.Fatalf("the receipt, which fits, was cut: %+v", got.Artifacts)
	}
	if n := len(got.History); n == 0 || n == len(history) {
		t.Fatalf("history of %d: want the newest part of it", n)
	}
	if len(big.History) != 200 || big.Status.Message.Parts[0].Text != huge {
		t.Fatal("the task given was changed")
	}
}

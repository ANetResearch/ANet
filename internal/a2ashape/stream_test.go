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

// TaskWithin holds a listed task to its bound: a small one is carried as
// it is; a big latest message is replaced by a notice that says how large
// it was, and the task is marked (docs/notes/0035: one 3 MiB reply made
// every MCP list_tasks page megabytes long).
func TestTaskWithinHoldsAListedTaskToItsBound(t *testing.T) {
	small := Task{ID: "t", ContextID: "c", Status: TaskStatus{State: TaskStateCompleted},
		History:  []Message{{ID: "h", Role: RoleAgent, Parts: []Part{TextPart("echo: hi")}}},
		Metadata: map[string]any{"anet.peer_aid": "x"}}
	if got := TaskWithin(small, 8<<10); !reflect.DeepEqual(got, small) {
		t.Fatalf("a small task was changed: %+v", got)
	}
	huge := strings.Repeat("<", 3<<20)
	big := Task{ID: "t", ContextID: "c", Status: TaskStatus{State: TaskStateCompleted},
		History: []Message{{ID: "old", Role: RoleUser, Parts: []Part{TextPart("the question")}},
			{ID: "new", Role: RoleAgent, Parts: []Part{TextPart(huge)}, Metadata: map[string]any{KeyState: "working"}}},
		Metadata: map[string]any{"anet.peer_aid": "x"}}
	for _, max := range []int{8 << 10, 1} {
		got := TaskWithin(big, max)
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		bound := max
		if bound < MinTaskBytes {
			bound = MinTaskBytes
		}
		if len(b) > bound {
			t.Fatalf("max %d: the task is %d bytes as JSON", max, len(b))
		}
		if len(got.History) != 1 || got.History[0].ID != "new" || got.History[0].Metadata[KeyTruncated] != true ||
			got.History[0].Metadata[KeyState] != "working" || got.Metadata[KeyTruncated] != true ||
			got.Metadata["anet.peer_aid"] != "x" {
			t.Fatalf("max %d: history %+v, metadata %v: want the newest message as a notice, the task marked", max,
				got.History, got.Metadata)
		}
		size, _ := got.History[0].Parts[0].Metadata[KeySize].(int)
		if size < len(huge) || !strings.Contains(got.History[0].Parts[0].Text, "left out of this list") {
			t.Fatalf("max %d: notice %+v", max, got.History[0].Parts[0])
		}
	}
	if big.History[1].Parts[0].Text != huge {
		t.Fatal("the task given was changed")
	}
}

// TaskReadWithin holds one task read to its bound the way TaskWithin holds
// a listed one, and its notice says where the whole task is read — `anet
// task get <id> --full` — naming the id only when it is plainly an id: a
// task sent to this node has the id its requester chose, and a model may run
// what the notice says (docs/notes/0035 §7.4: a 3 MiB reply returned whole
// by MCP get_task was a tool result Claude Code refused).
func TestTaskReadWithinSaysHowToReadTheWholeTask(t *testing.T) {
	huge := strings.Repeat("字", 1<<20)
	mk := func(id string) Task {
		return Task{ID: id, ContextID: "c", Status: TaskStatus{State: TaskStateCompleted},
			History: []Message{{ID: "q", Role: RoleUser, Parts: []Part{TextPart("the question")}},
				{ID: "a", Role: RoleAgent, Parts: []Part{TextPart(huge)}}},
			Artifacts: []Artifact{{ID: ArtifactReply, Parts: []Part{TextPart(huge)}}},
			Metadata:  map[string]any{"anet.role": "requester"}}
	}
	small := Task{ID: "ix_1", Status: TaskStatus{State: TaskStateWorking}, Metadata: map[string]any{"k": "v"}}
	if got := TaskReadWithin(small, 24<<10); !reflect.DeepEqual(got, small) {
		t.Fatalf("a small task was changed: %+v", got)
	}
	for _, tc := range []struct{ id, named string }{
		{"ix_0123abcd", "anet task get ix_0123abcd --full"},
		{"x; rm -rf ~ #", "anet task get <task id> --full"},
		{"--help", "anet task get <task id> --full"},
	} {
		got := TaskReadWithin(mk(tc.id), 24<<10)
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > 24<<10 {
			t.Fatalf("%q: %d bytes as JSON, over 24 KiB", tc.id, len(b))
		}
		if got.Metadata[KeyTruncated] != true || got.Metadata["anet.role"] != "requester" {
			t.Fatalf("%q: metadata %v", tc.id, got.Metadata)
		}
		if len(got.Artifacts) != 1 || got.Artifacts[0].Metadata[KeyTruncated] != true {
			t.Fatalf("%q: artifacts %+v", tc.id, got.Artifacts)
		}
		notice := got.Artifacts[0].Parts[0]
		if size, _ := notice.Metadata[KeySize].(int); size < len(huge) || !strings.Contains(notice.Text, tc.named) {
			t.Fatalf("%q: notice %q (size %v), want it to name %q", tc.id, notice.Text, notice.Metadata[KeySize], tc.named)
		}
	}
}

// A peer's large value in a task's metadata — here a 40 KB anet.reason
// and a long list of claimed receipts — does not take this node's
// statements about the task out of a bounded read or a listing: the effect
// status, the role, the peer, the state sequence and the receipt check stay
// (SI-6); only the values too large to keep are dropped, and the task is
// marked anet.truncated. Before, a cut kept nothing but the mark.
func TestACutKeepsWhatThisNodeSaysAboutTheTask(t *testing.T) {
	var claimed []any
	for i := 0; i < 400; i++ {
		claimed = append(claimed, map[string]any{"network": "base", "transaction": strings.Repeat("ab", 32)})
	}
	task := Task{ID: "ix_1", ContextID: "c", Status: TaskStatus{State: TaskStateFailed},
		Metadata: map[string]any{
			KeyEffectStatus: "UNVERIFIED", KeyRole: "requester", KeyPeerAID: "bafypeer", KeyStateSeq: 7,
			KeyReceiptVerified: "unverified", KeyTrust: "trusted", KeySkill: "lamp",
			KeyReason:             strings.Repeat("r", 40<<10),
			KeyUnverifiedReceipts: claimed,
			"x.small":             "kept",
		}}
	for name, got := range map[string]Task{"read": TaskReadWithin(task, 24<<10), "list": TaskWithin(task, 8<<10),
		"smallest": TaskWithin(task, 0)} {
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		bound := map[string]int{"read": 24 << 10, "list": 8 << 10, "smallest": MinTaskBytes}[name]
		if len(b) > bound {
			t.Fatalf("%s: %d bytes as JSON, over %d", name, len(b), bound)
		}
		m := got.Metadata
		if m[KeyTruncated] != true || m[KeyEffectStatus] != "UNVERIFIED" || m[KeyRole] != "requester" ||
			m[KeyPeerAID] != "bafypeer" || m[KeyStateSeq] != 7 || m[KeyReceiptVerified] != "unverified" {
			t.Fatalf("%s: metadata %v", name, m)
		}
		if _, ok := m[KeyReason]; ok {
			t.Fatalf("%s: the 40 KB reason was kept", name)
		}
		if _, ok := m[KeyUnverifiedReceipts]; ok {
			t.Fatalf("%s: the claimed receipts were kept", name)
		}
	}
	if m := TaskReadWithin(task, 24<<10).Metadata; m["x.small"] != "kept" || m[KeySkill] != "lamp" || m[KeyTrust] != "trusted" {
		t.Fatalf("read: the small keys were not all kept: %v", m)
	}
}

// A bounded read stays within its bound when a peer fills everything a cut
// keeps: a long anet.reason in the task's metadata, and 4 KB of metadata on
// both its status message and its message, each with a huge text. A first
// fit came to 31 KB for a 24 KiB bound; the task is fitted again with less
// room, and the effect status is still there.
func TestABoundedReadHoldsWhenEveryCutCarriesMetadata(t *testing.T) {
	meta4k := map[string]any{"x.pad": strings.Repeat("m", 3900)}
	huge := strings.Repeat("字", 1<<18)
	task := Task{ID: "ix_1", ContextID: "c", Status: TaskStatus{State: TaskStateInputRequired,
		Message: &Message{ID: "s", Role: RoleAgent, Parts: []Part{TextPart(huge)}, Metadata: meta4k}},
		Metadata: map[string]any{KeyEffectStatus: "UNVERIFIED", KeyRole: "requester", KeyReason: strings.Repeat("r", 22<<10)},
		History:  []Message{{ID: "q", Role: RoleUser, Parts: []Part{TextPart(huge)}, Metadata: meta4k}}}
	got := TaskReadWithin(task, 24<<10)
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 24<<10 {
		t.Fatalf("%d bytes as JSON, over 24 KiB", len(b))
	}
	if got.Metadata[KeyEffectStatus] != "UNVERIFIED" || got.Metadata[KeyTruncated] != true {
		t.Fatalf("metadata %v", got.Metadata)
	}
}

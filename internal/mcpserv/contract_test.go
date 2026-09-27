//go:build !no_mcp

package mcpserv_test

// contract_test.go is the SI-6 contract through MCP (A2A-DESIGN §1, §12):
// a task the daemon projects, as the model receives it from the MCP tools,
// decodes as an a2a-go a2a.Task, and in it A2A COMPLETED stands beside the
// effect status and the receipt verification instead of replacing them.
//
// It is an external test package so that a2a-go stays out of the package
// itself: `go list -deps ./internal/mcpserv` must not contain a2aproject
// (SI-8), and test imports are not in that closure.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/mcpserv"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

const (
	self = "did:anet:self"
	peer = "did:anet:peer"
	now  = int64(1790000000123)
)

// daemon answers each path with fixed JSON, as the control plane would.
type daemon map[string][]byte

func (d daemon) Call(_ context.Context, path string, _, out any) error {
	b, ok := d[path]
	if !ok {
		return &mcpserv.DaemonError{Status: 404, Message: "no route " + path}
	}
	return json.Unmarshal(b, out)
}

func connect(t *testing.T, c mcpserv.Control) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := mcpserv.New(c, "test").Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "contract"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// callJSON calls a tool and returns what the model reads: the JSON text.
func callJSON(t *testing.T, sess *mcp.ClientSession, tool string, args map[string]any) []byte {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError {
		t.Fatalf("%s: %s", tool, text)
	}
	return []byte(text)
}

// checkSI6 is the invariant as a client can check it from the A2A task
// alone: a terminal capability task names its effect status, a completed
// task names its receipt verification in one of three values, and the two
// are separate keys that the state does not replace.
func checkSI6(task *a2a.Task) error {
	_, isCap := task.Metadata[a2ashape.KeySkill]
	_, hasES := task.Metadata[a2ashape.KeyEffectStatus]
	if isCap && task.Status.State.Terminal() && !hasES {
		return fmt.Errorf("terminal capability task %s has no %s", task.Status.State, a2ashape.KeyEffectStatus)
	}
	if task.Status.State == a2a.TaskStateCompleted {
		rv, ok := task.Metadata[a2ashape.KeyReceiptVerified].(string)
		if !ok {
			return fmt.Errorf("completed task has no %s", a2ashape.KeyReceiptVerified)
		}
		switch rv {
		case a2ashape.ReceiptVerified, a2ashape.ReceiptUnverified, a2ashape.ReceiptUnknown:
		default:
			return fmt.Errorf("%s=%q", a2ashape.KeyReceiptVerified, rv)
		}
	}
	return nil
}

// The checker has teeth: a projection that folded either fact into the
// state, or dropped it, is refused.
func TestSI6CheckerRefuses(t *testing.T) {
	for name, task := range map[string]*a2a.Task{
		"capability without effect": {Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
			Metadata: map[string]any{a2ashape.KeySkill: "x", a2ashape.KeyReceiptVerified: "verified"}},
		"text without verification": {Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}},
		"verification as a bool": {Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
			Metadata: map[string]any{a2ashape.KeyReceiptVerified: true}},
	} {
		if checkSI6(task) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func capDoc(t *testing.T, capID string) []byte {
	t.Helper()
	td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{{
		Intent:   tsir.Intent{Summary: "invoke capability " + capID, Body: "invoke capability " + capID},
		Requires: []tsir.Require{{ID: capID, Type: "capability", Necessity: "must"}},
		Contexts: []tsir.Context{{Key: "args", Value: `{}`, Format: "json"}, {Key: "anet.nonce", Value: "n", Visibility: "private"}},
	}}}
	b, err := coredet.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func receipt(t *testing.T) []byte {
	t.Helper()
	b, err := (&evidence.Receipt{InteractionID: "ix", RequesterAID: self, ProviderAID: peer,
		RequestCID: "bafyrequest", ResultCID: "bafyresult", CompletedAt: uint64(now)}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every task shape the daemon projects — both kinds, every state, every
// answer — reaches the model through get_task, wait_task, send_message,
// cancel_task and list_tasks as an a2a.Task that satisfies SI-6; and the
// case the invariant exists for, completed with effect UNVERIFIED, stays
// COMPLETED with UNVERIFIED beside it rather than being read as OK.
func TestTasksThroughMCPAreA2ATasksThatKeepSI6(t *testing.T) {
	states := []interactions.State{interactions.StateSubmitted, interactions.StateWorking, interactions.StateInputRequired,
		interactions.StateCompleted, interactions.StateFailed, interactions.StateCanceled, interactions.StateRejected}
	answers := []string{"", `{"status":"OK"}`, `{"status":"UNVERIFIED"}`, `{"status":"FAILED"}`, `{"status":"UNAVAILABLE"}`}
	tools := []struct {
		name string
		path string
		args map[string]any
	}{
		{"get_task", "/tasks/get", map[string]any{"task_id": "ix"}},
		{"wait_task", "/tasks/wait", map[string]any{"task_id": "ix"}},
		{"send_message", "/tasks/send", map[string]any{"task_id": "ix", "text": "more"}},
		{"cancel_task", "/tasks/cancel", map[string]any{"task_id": "ix"}},
	}
	sawUnverified := false
	for _, capability := range []bool{true, false} {
		for _, st := range states {
			for _, ans := range answers {
				ix := &interactions.Interaction{ID: "ix", Role: interactions.RoleOutbound, PeerAID: peer, State: st,
					StateAt: now, StateSeq: 3, IsCapability: capability, ContextID: "c",
					ReceiptVerified: interactions.VerificationUnknown, Goal: "g"}
				if capability {
					ix.RequestDoc = capDoc(t, "x.y")
				}
				if ans != "" {
					ix.Result, ix.Receipt = []byte(ans), receipt(t)
				}
				task := a2ashape.Project(a2ashape.Source{Interaction: ix}, a2ashape.Options{Artifacts: true})
				raw, err := json.Marshal(task)
				if err != nil {
					t.Fatal(err)
				}
				page, _ := json.Marshal(a2ashape.TaskPage{Tasks: []a2ashape.Task{task}, TotalSize: 1, PageSize: 1})
				d := daemon{"/tasks/list": page}
				for _, tl := range tools {
					d[tl.path] = raw
				}
				sess := connect(t, d)

				var got []a2a.Task
				for _, tl := range tools {
					var sdk a2a.Task
					if err := json.Unmarshal(callJSON(t, sess, tl.name, tl.args), &sdk); err != nil {
						t.Fatalf("%s: a2a-go cannot read it: %v", tl.name, err)
					}
					got = append(got, sdk)
				}
				var sdkPage struct {
					Tasks []a2a.Task `json:"tasks"`
				}
				if err := json.Unmarshal(callJSON(t, sess, "list_tasks", map[string]any{}), &sdkPage); err != nil || len(sdkPage.Tasks) != 1 {
					t.Fatalf("list_tasks: %v %d", err, len(sdkPage.Tasks))
				}
				got = append(got, sdkPage.Tasks[0])

				for i := range got {
					sdk := &got[i]
					if err := checkSI6(sdk); err != nil {
						t.Fatalf("cap=%v %s %q: SI-6: %v", capability, st, ans, err)
					}
					if sdk.Status.State != a2a.TaskState(a2ashape.StateOf(st)) {
						t.Fatalf("cap=%v %s %q: state %s", capability, st, ans, sdk.Status.State)
					}
					if capability && st == interactions.StateCompleted && ans == `{"status":"UNVERIFIED"}` {
						sawUnverified = true
						if sdk.Status.State != a2a.TaskStateCompleted || sdk.Metadata[a2ashape.KeyEffectStatus] != "UNVERIFIED" {
							t.Fatalf("completed+UNVERIFIED reached the model as %s / %v", sdk.Status.State, sdk.Metadata[a2ashape.KeyEffectStatus])
						}
						if sdk.Metadata[a2ashape.KeyReceiptVerified] != a2ashape.ReceiptUnknown {
							t.Fatalf("receipt verification merged or lost: %v", sdk.Metadata)
						}
					}
				}
			}
		}
	}
	if !sawUnverified {
		t.Fatal("the completed+UNVERIFIED case was never exercised")
	}
}

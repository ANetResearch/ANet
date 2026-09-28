//go:build !no_a2a

package a2a

// a2aif_redteam_test.go: adversarial PoCs for the local A2A interface's
// HTTP face. Each test asserts that the attack SUCCEEDS: a passing test
// means the defect is present.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// The kernel puts a provider's files inline in every task view it gives the
// local A2A interface, stream events included (internal/daemon
// TestRedteamA2AIF_ProviderAttachmentsInlinedWithoutQ12Cap). One file of
// ~8 MiB from the remote provider makes the stream's first event — the Task
// snapshot — a single SSE data line longer than a2a-go's client accepts
// (internal/sse MaxSSETokenSize, 10 MB): SubscribeToTask and
// SendStreamingMessage fail for every a2a-go client for as long as the file
// is in the task, while Q12 said stream events carry only metadata.
func TestRedteamA2AIF_ProviderFileBreaksA2AGoStreams(t *testing.T) {
	for _, binding := range bindings {
		t.Run(string(binding), func(t *testing.T) {
			e := newEnv(t)
			cl, ctx := e.client(agentA, binding)
			e.seam.quote = map[string]any{"accepts": []any{}} // leaves the task waiting (input-required)
			res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("q")})
			if err != nil {
				t.Fatal(err)
			}
			id := string(res.(*a2a.Task).ID)
			// The provider's question, with the file inline, as the kernel
			// projects it for this interface.
			e.seam.mu.Lock()
			ft := e.seam.tasks[id]
			q := a2ashape.Message{ID: "p1", Role: a2ashape.RoleAgent, TaskID: id, ContextID: ft.t.ContextID,
				Parts: []a2ashape.Part{a2ashape.TextPart("see file"),
					a2ashape.RawPart(bytes.Repeat([]byte{'z'}, 8<<20), "big.bin", "application/octet-stream")}}
			e.seam.setState(ft, a2ashape.TaskStateInputRequired, &q)
			e.seam.mu.Unlock()

			var streamErr error
			for _, err := range cl.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: a2a.TaskID(id)}) {
				if err != nil {
					streamErr = err
					break
				}
				t.Fatal("the stream delivered an event: defect absent")
			}
			if streamErr == nil || !strings.Contains(streamErr.Error(), "too long") {
				t.Fatalf("stream error %v, want the SSE line limit", streamErr)
			}
			t.Logf("%s: a2a-go client stream fails: %v", binding, streamErr)
		})
	}
}

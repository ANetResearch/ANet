//go:build !no_a2a

package a2a

// Red-team finding F32, on review: Q12 keeps file bytes out of stream
// events, but the rest of an event was as large as a peer made it. A
// provider's text reply of 2 MiB of '<' (six bytes each once JSON escapes
// it) made one SSE line longer than a2a-go's reader takes (10 MB), and
// SubscribeToTask and SendStreamingMessage failed for every a2a-go client
// exactly as with an 8 MiB file; so did large message metadata, tens of
// thousands of attachments (each a placeholder), or a long history in the
// first event. Every event is held to a2ashape.MaxStreamEventBytes: what
// does not fit is cut and marked anet.truncated, and GetTask has it all.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

func TestAStreamStaysReadableWhateverAPeerSends(t *testing.T) {
	escaped := strings.Repeat("<", 2<<20) // 2 MiB, 12 MB as JSON
	manyFiles := make([]a2ashape.Part, 60000)
	for i := range manyFiles {
		manyFiles[i] = a2ashape.Placeholder(a2ashape.Part{Filename: fmt.Sprintf("f%05d.bin", i),
			MediaType: "application/octet-stream", Metadata: map[string]any{a2ashape.KeySize: 1}},
			"task1", fmt.Sprintf("bafkfile%05d", i))
	}
	longHistory := make([]a2ashape.Message, 120) // 12 MiB in all
	for i := range longHistory {
		longHistory[i] = a2ashape.Message{ID: fmt.Sprintf("h%03d", i), Role: a2ashape.RoleAgent,
			Parts: []a2ashape.Part{a2ashape.TextPart(strings.Repeat("a", 100<<10))}}
	}
	for _, c := range []struct {
		name    string
		parts   []a2ashape.Part
		meta    map[string]any
		history []a2ashape.Message
	}{
		{name: "text", parts: []a2ashape.Part{a2ashape.TextPart(escaped)}},
		{name: "metadata", parts: []a2ashape.Part{a2ashape.TextPart("q")}, meta: map[string]any{"note": escaped}},
		{name: "files", parts: manyFiles},
		{name: "history", parts: []a2ashape.Part{a2ashape.TextPart("q")}, history: longHistory},
	} {
		for _, binding := range bindings {
			t.Run(c.name+"/"+string(binding), func(t *testing.T) {
				e := newEnv(t)
				cl, ctx := e.client(agentA, binding)
				e.seam.quote = map[string]any{"accepts": []any{}} // leaves the task waiting
				res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("q")})
				if err != nil {
					t.Fatal(err)
				}
				id := string(res.(*a2a.Task).ID)
				e.seam.mu.Lock()
				ft := e.seam.tasks[id]
				q := a2ashape.Message{ID: "p1", Role: a2ashape.RoleAgent, TaskID: id, ContextID: ft.t.ContextID,
					Parts: c.parts, Metadata: c.meta}
				ft.t.History = append(c.history, q)
				e.seam.setState(ft, a2ashape.TaskStateInputRequired, &q)
				e.seam.mu.Unlock()

				var first *a2a.Task
				for ev, err := range cl.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: a2a.TaskID(id)}) {
					if err != nil {
						t.Fatalf("the stream failed: %v", err)
					}
					first, _ = ev.(*a2a.Task)
					break
				}
				if first == nil || first.Status.Message == nil {
					t.Fatalf("first event %+v", first)
				}
				if first.Metadata[a2ashape.KeyTruncated] != true {
					t.Fatalf("the task does not say it was cut: %v", first.Metadata)
				}
				if c.history != nil {
					n := len(first.History)
					if n == 0 || n > len(c.history) || first.History[n-1].ID != "p1" {
						t.Fatalf("history of %d, want the newest part of it", n)
					}
					return
				}
				if first.Status.Message.Metadata[a2ashape.KeyTruncated] != true {
					t.Fatalf("the status message is not marked cut: %v", first.Status.Message.Metadata)
				}
			})
		}
	}
}

// The same for an event after the first: a status update carrying a
// provider's long reply arrives cut, and the stream goes on.
func TestAStatusUpdateStaysReadableWhateverAPeerSends(t *testing.T) {
	for _, binding := range bindings {
		t.Run(string(binding), func(t *testing.T) {
			e := newEnv(t)
			cl, ctx := e.client(agentA, binding)
			e.seam.quote = map[string]any{"accepts": []any{}}
			res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("q")})
			if err != nil {
				t.Fatal(err)
			}
			id := string(res.(*a2a.Task).ID)
			n := 0
			for ev, err := range cl.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: a2a.TaskID(id)}) {
				if err != nil {
					t.Fatalf("event %d: the stream failed: %v", n, err)
				}
				n++
				if n == 1 {
					e.seam.mu.Lock()
					ft := e.seam.tasks[id]
					q := a2ashape.Message{ID: "p2", Role: a2ashape.RoleAgent, TaskID: id, ContextID: ft.t.ContextID,
						Parts: []a2ashape.Part{a2ashape.TextPart(strings.Repeat("&", 2<<20))}}
					e.seam.setState(ft, a2ashape.TaskStateWorking, &q)
					su := a2ashape.StatusUpdate(copyTask(ft.t))
					e.seam.publish(id, module.TaskEvent{StatusUpdate: &su})
					e.seam.mu.Unlock()
					continue
				}
				su, ok := ev.(*a2a.TaskStatusUpdateEvent)
				if !ok || su.Status.Message == nil || su.Status.Message.Metadata[a2ashape.KeyTruncated] != true {
					t.Fatalf("event %d: %T %+v", n, ev, ev)
				}
				return
			}
			t.Fatal("the stream ended before the status update")
		})
	}
}

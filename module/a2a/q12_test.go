//go:build !no_a2a

package a2a

// Red-team finding F32 (0017 Q12), at the interface: a stream event carries
// a file's metadata, never its bytes. One file of ~8 MiB inline in the
// stream's first event made a single SSE data line longer than a2a-go's
// client reads (internal/sse MaxSSETokenSize, 10 MB), and SubscribeToTask
// and SendStreamingMessage failed for every a2a-go client for as long as
// the file was in the task. Whatever the kernel hands it, the interface
// passes every event through a2ashape.EventByReference.

import (
	"bytes"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

func TestAStreamCarriesFilesAsMetadataOnly(t *testing.T) {
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
			// The provider's question, with its file inline, as a kernel
			// that did not hold to Q12 would project it.
			e.seam.mu.Lock()
			ft := e.seam.tasks[id]
			big := a2ashape.RawPart(bytes.Repeat([]byte{'z'}, 8<<20), "big.bin", "application/octet-stream")
			big.Metadata = map[string]any{a2ashape.KeyCID: "bafkbig", a2ashape.KeySize: 8 << 20}
			q := a2ashape.Message{ID: "p1", Role: a2ashape.RoleAgent, TaskID: id, ContextID: ft.t.ContextID,
				Parts: []a2ashape.Part{a2ashape.TextPart("see file"), big}}
			e.seam.setState(ft, a2ashape.TaskStateInputRequired, &q)
			e.seam.mu.Unlock()

			var first *a2a.Task
			for ev, err := range cl.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: a2a.TaskID(id)}) {
				if err != nil {
					t.Fatalf("the stream failed: %v", err)
				}
				first, _ = ev.(*a2a.Task)
				break // the task waits for the client: the stream would stay open
			}
			if first == nil || first.Status.Message == nil || len(first.Status.Message.Parts) != 2 {
				t.Fatalf("first event %+v", first)
			}
			file := first.Status.Message.Parts[1]
			if file.Raw() != nil || file.Metadata[a2ashape.KeyAttachmentCID] != "bafkbig" ||
				file.URL() != a2a.URL(a2ashape.AttachmentURI(id, "bafkbig")) || file.Filename != "big.bin" {
				t.Fatalf("the file in a stream event: %+v", file)
			}
		})
	}
}

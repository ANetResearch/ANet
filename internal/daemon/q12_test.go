package daemon

// Red-team finding F32 (0017 Q12): the A2A interface inlined every
// attachment a remote provider sent, with no limit — in the history, the
// status message, every ListTasks page and every stream event — so a peer
// the local client talked to could make each read load and copy gigabytes
// in this daemon. Q12: the history and stream events carry a file's
// metadata only (name, type, size, CID, anet.attachment_cid); one task read
// (GetTask, a SendMessage answer) inlines at most 8 MiB in all, the rest as
// placeholders; a list inlines nothing.

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// rawBytesIn counts the attachment bytes messages carry inline (raw parts),
// and the placeholders (anet.attachment_cid) among their parts.
func rawBytesIn(msgs ...*a2ashape.Message) (raw, placeholders int) {
	for _, m := range msgs {
		if m == nil {
			continue
		}
		for _, p := range m.Parts {
			switch {
			case p.Kind == a2ashape.PartRaw:
				raw += len(p.Raw)
			case p.Metadata[a2ashape.KeyAttachmentCID] != nil:
				placeholders++
			}
		}
	}
	return raw, placeholders
}

// taskRawBytes is rawBytesIn over the whole task: history, status message
// and artifacts.
func taskRawBytes(t a2ashape.Task) (history, rest int) {
	for i := range t.History {
		n, _ := rawBytesIn(&t.History[i])
		history += n
	}
	rest, _ = rawBytesIn(t.Status.Message)
	for _, a := range t.Artifacts {
		for _, p := range a.Parts {
			if p.Kind == a2ashape.PartRaw {
				rest += len(p.Raw)
			}
		}
	}
	return history, rest
}

func TestTheA2AInterfaceHoldsToTheQ12InlineLimit(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	seam := req.TaskSeam()
	task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: textMsg("hello", "ctx-q12", "m-q12-1"), ReturnImmediately: true})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the provider has the task", func() bool {
		_ = prov.pollOnce(ctx)
		_, err := prov.ix.Get(task.ID)
		return err == nil
	})

	// The provider answers with three 4 MiB files, one per message: 12 MiB,
	// over the 8 MiB a task read may inline.
	const each = 4 << 20
	const n = 3
	for i := 0; i < n; i++ {
		att, err := attachmentFromBytes(fmt.Sprintf("blob%d.bin", i), bytes.Repeat([]byte{byte('a' + i)}, each))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prov.sendMessage(ctx, task.ID, fmt.Sprintf("part %d", i), []delegation.Attachment{att}, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitUntil(t, "the requester stored the provider's files", func() bool {
		_ = req.pollOnce(ctx)
		atts, _ := req.ix.Attachments(task.ID)
		return len(atts) == n
	})

	// GetTask: the history is metadata only; the question the task waits
	// on (status.message) has its file inline, within the limit.
	got, err := seam.Get(ctx, prov.AID(), task.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	history, rest := taskRawBytes(got)
	if history != 0 {
		t.Fatalf("GetTask history carries %d bytes inline; Q12: metadata only", history)
	}
	if rest != each || rest > a2ashape.MaxInlineBytes {
		t.Fatalf("GetTask inlines %d bytes beyond the history, want the question's %d (limit %d)", rest, each, a2ashape.MaxInlineBytes)
	}
	var placeholders int
	for i := range got.History {
		_, ph := rawBytesIn(&got.History[i])
		placeholders += ph
	}
	if placeholders != n {
		t.Fatalf("the history gives %d of %d files as placeholders", placeholders, n)
	}

	// ListTasks: nothing inline, with or without artifacts.
	for _, arts := range []bool{false, true} {
		page, err := seam.List(ctx, prov.AID(), module.TaskFilter{IncludeArtifacts: arts})
		if err != nil || len(page.Tasks) != 1 {
			t.Fatalf("list: %+v %v", page, err)
		}
		if h, r := taskRawBytes(page.Tasks[0]); h+r != 0 {
			t.Fatalf("ListTasks (artifacts %v) inlines %d bytes", arts, h+r)
		}
	}

	// A stream: the snapshot and the status update for the provider's next
	// message (a 9 MiB file) carry metadata only.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	snap, events, err := seam.Watch(wctx, prov.AID(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h, r := taskRawBytes(snap); h+r != 0 {
		t.Fatalf("the stream's snapshot inlines %d bytes", h+r)
	}
	big, err := attachmentFromBytes("big.bin", bytes.Repeat([]byte{'z'}, 9<<20))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prov.sendMessage(ctx, task.ID, "one more", []delegation.Attachment{big}, nil); err != nil {
		t.Fatal(err)
	}
	go func() { _ = req.pollOnce(ctx) }()
	for ev := range events {
		if ev.StatusUpdate == nil || ev.StatusUpdate.Status.Message == nil {
			continue
		}
		raw, ph := rawBytesIn(ev.StatusUpdate.Status.Message)
		if raw != 0 {
			t.Fatalf("a stream status-update event carries %d bytes inline", raw)
		}
		if ph == 1 {
			break // the message with the big file, as metadata
		}
	}

	// And a read past the limit: the 9 MiB file of the question the task
	// now waits on does not fit, so it too is a placeholder.
	got, err = seam.Get(ctx, prov.AID(), task.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h, r := taskRawBytes(got); h+r != 0 {
		t.Fatalf("a file over the limit was inlined: %d bytes", h+r)
	}
	if _, ph := rawBytesIn(got.Status.Message); ph != 1 {
		t.Fatalf("the question's file is not a placeholder: %+v", got.Status.Message)
	}
}

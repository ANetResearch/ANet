package a2ashape_test

import (
	"bytes"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/transcript"
)

// 0017 Q12: a task read inlines at most InlineLimit bytes of files in all —
// the artifacts first, then status.message — and the history none; a file
// past the limit is a placeholder, and its bytes are not even read.
func TestInlineFilesStopAtTheLimit(t *testing.T) {
	st := openStore(t)
	const ix = "ix_lim"
	must(t, st.Create(interactions.New{ID: ix, Role: interactions.RoleOutbound, PeerAID: peer, Goal: "g", ContextID: "c"}))
	msg(t, st, ix, self, interactions.MsgText, "g", "m1", nil)
	reply := msg(t, st, ix, peer, interactions.MsgText, "here", "m2", nil)
	files := []interactions.Attachment{
		{Name: "a.bin", Mime: "application/octet-stream", Size: 3, CID: "bafka", Data: []byte("aaa")},
		{Name: "b.bin", Mime: "application/octet-stream", Size: 3, CID: "bafkb", Data: []byte("bbb")},
	}
	var trAtts []transcript.Attachment
	for _, a := range files {
		must(t, st.AddAttachment(ix, reply, a))
		trAtts = append(trAtts, transcript.Attachment{Name: a.Name, Mime: a.Mime, Size: a.Size, CID: a.CID})
	}
	tr, err := transcript.EncodeV2("n", []transcript.Message{{From: "requester", Body: "g"},
		{From: "provider", Body: "here", Attachments: trAtts}})
	must(t, err)
	must(t, st.Finish(ix, interactions.Finish{State: interactions.StateCompleted, Result: tr, ResultCID: "bafyr",
		Receipt: receipt(t, ix, self, peer, "bafyr"), Verified: interactions.VerificationVerified}))

	loaded := map[string]int{}
	opt := a2ashape.Options{Artifacts: true, InlineFiles: true, InlineLimit: 4, SafeName: safe,
		LoadFile: func(cid string) ([]byte, error) {
			loaded[cid]++
			a, err := st.AttachmentData(ix, cid)
			if err != nil {
				return nil, err
			}
			return a.Data, nil
		}}
	task, err := a2ashape.ProjectStored(st, ix, opt)
	must(t, err)
	parts := task.Artifacts[0].Parts
	if len(parts) != 3 || parts[1].Kind != a2ashape.PartRaw || !bytes.Equal(parts[1].Raw, []byte("aaa")) {
		t.Fatalf("the first file fits and is inline: %+v", parts)
	}
	if p := parts[2]; p.Kind != a2ashape.PartURL || p.Raw != nil || p.Metadata[a2ashape.KeyAttachmentCID] != "bafkb" ||
		p.URL != a2ashape.AttachmentURI(ix, "bafkb") || p.Filename != "safe-b.bin" || p.Metadata[a2ashape.KeySize] != int64(3) {
		t.Fatalf("the second is past the limit and a placeholder: %+v", p)
	}
	for _, m := range task.History {
		for _, p := range m.Parts {
			if p.Kind == a2ashape.PartRaw {
				t.Fatalf("the history carries bytes: %+v", p)
			}
		}
	}
	if loaded["bafkb"] != 0 || loaded["bafka"] != 1 {
		t.Fatalf("bytes read: %v; a file past the limit, or in the history, is not read", loaded)
	}

	// ByReference (a stream event's form) leaves no bytes anywhere.
	byRef := a2ashape.ByReference(task)
	if p := byRef.Artifacts[0].Parts[1]; p.Kind != a2ashape.PartURL || p.Metadata[a2ashape.KeyAttachmentCID] != "bafka" {
		t.Fatalf("ByReference: %+v", p)
	}
	if task.Artifacts[0].Parts[1].Kind != a2ashape.PartRaw {
		t.Fatal("ByReference changed the task it was given")
	}
}

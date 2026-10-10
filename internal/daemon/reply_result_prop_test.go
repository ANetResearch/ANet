package daemon

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/transcript"
)

// requesterOnly is the requester's entries of a conversation, in order.
func requesterOnly(conv []transcript.Message) []transcript.Message {
	var out []transcript.Message
	for _, e := range conv {
		if e.From == transcriptRequester {
			out = append(out, e)
		}
	}
	return out
}

// TestRequesterEntriesMatchProperty is P-CUX5: for a random conversation
// and a random edit of the transcript the provider signs, the requester's
// check reports a mismatch if and only if the edit inserted or changed a
// requester entry; pure deletions (of any entry) and changes to the
// provider's entries are not a mismatch. The requester may also have sent
// messages the conversation never had in it.
func TestRequesterEntriesMatchProperty(t *testing.T) {
	r := rand.New(rand.NewSource(20261010))
	fresh := 0
	newBody := func() string { fresh++; return fmt.Sprintf("m-%d", fresh) }
	newFile := func() transcript.Attachment {
		fresh++
		return transcript.Attachment{Name: fmt.Sprintf("f%d.txt", fresh), Mime: "text/plain", Size: int64(fresh), CID: fmt.Sprintf("cid-%d", fresh)}
	}
	for iter := 0; iter < 20000; iter++ {
		n := r.Intn(8)
		var conv []transcript.Message
		for i := 0; i < n; i++ {
			e := transcript.Message{From: transcriptRequester, Body: newBody()}
			if r.Intn(2) == 0 {
				e.From = transcriptProvider
			}
			for k := r.Intn(3); k > 0; k-- {
				e.Attachments = append(e.Attachments, newFile())
			}
			conv = append(conv, e)
		}
		// What this node sent: its entries, with messages the transcript
		// never covered spliced in.
		var sent []transcript.Message
		for _, e := range requesterOnly(conv) {
			if r.Intn(4) == 0 {
				sent = append(sent, transcript.Message{From: transcriptRequester, Body: newBody()})
			}
			sent = append(sent, e)
		}
		tr := append([]transcript.Message(nil), conv...)
		for k := r.Intn(4); k > 0; k-- {
			switch op := r.Intn(5); {
			case op == 0 && len(tr) > 0: // delete any entry
				i := r.Intn(len(tr))
				tr = append(tr[:i:i], tr[i+1:]...)
			case op == 1: // insert a requester entry
				i := r.Intn(len(tr) + 1)
				e := transcript.Message{From: transcriptRequester, Body: newBody()}
				tr = append(tr[:i:i], append([]transcript.Message{e}, tr[i:]...)...)
			case op == 2 && len(tr) > 0: // change an entry's body
				i := r.Intn(len(tr))
				tr[i].Body = newBody()
			case op == 3 && len(tr) > 0: // change an entry's files
				i := r.Intn(len(tr))
				tr[i].Attachments = append(append([]transcript.Attachment(nil), tr[i].Attachments...), newFile())
			case op == 4: // insert a provider entry
				i := r.Intn(len(tr) + 1)
				e := transcript.Message{From: transcriptProvider, Body: newBody()}
				tr = append(tr[:i:i], append([]transcript.Message{e}, tr[i:]...)...)
			}
		}
		// The edit inserted or changed a requester entry when the final
		// transcript has one that is none of the original ones (bodies
		// and files are never reused, so a changed entry equals none).
		want := false
		for _, e := range requesterOnly(tr) {
			found := false
			for _, o := range requesterOnly(conv) {
				found = found || sameEntry(e, o)
			}
			want = want || !found
		}
		if got := !requesterEntriesMatch(sent, tr); got != want {
			t.Fatalf("iteration %d: mismatch %v, want %v\nsent %+v\ntranscript %+v", iter, got, want, sent, tr)
		}
	}
}

// A reorder of the requester's own words is a change, not a deletion.
func TestRequesterEntriesMatchOrder(t *testing.T) {
	a := transcript.Message{From: transcriptRequester, Body: "a"}
	b := transcript.Message{From: transcriptRequester, Body: "b"}
	if requesterEntriesMatch([]transcript.Message{a, b}, []transcript.Message{b, a}) {
		t.Fatal("a reordered transcript matched")
	}
	if !requesterEntriesMatch([]transcript.Message{a, b}, []transcript.Message{b}) {
		t.Fatal("a deletion did not match")
	}
}

// FuzzReplyAgainstTranscript fuzzes the requester's two checks of a result
// (CUX-R2, CUX-R6 item 4) with arbitrary transcript bytes: neither panics; a
// reply that passes is the transcript's last provider entry; a transcript's
// own requester entries match it, and so do they after any one of its
// entries is left out.
func FuzzReplyAgainstTranscript(f *testing.F) {
	good, _ := transcript.EncodeV2("bm9uY2U", []transcript.Message{{From: transcriptRequester, Body: "q"},
		{From: transcriptProvider, Body: "a", Attachments: []transcript.Attachment{{Name: "x", Mime: "text/plain", Size: 1, CID: "c"}}}})
	f.Add(good, "a", "x", "text/plain", int64(1), "c", uint(1))
	f.Add([]byte(`[{"from":"provider","body":"a"}]`), "a", "", "", int64(0), "", uint(0))
	f.Add([]byte(`{"v":2,"nonce":"n","messages":[{"from":"requester","body":"q"}]}`), "q", "", "", int64(0), "", uint(0))
	f.Add([]byte(`{"v":3}`), "", "", "", int64(0), "", uint(0))
	f.Add([]byte(`not json`), "", "", "", int64(0), "", uint(0))
	f.Fuzz(func(t *testing.T, deliverable []byte, body, name, mime string, size int64, cid string, drop uint) {
		reply := &delegation.ReplyMsg{Kind: delegation.ChatText, Body: body, MsgID: "m"}
		if name != "" || cid != "" {
			reply.Attachments = []delegation.Attachment{{Name: name, Mime: mime, Size: size, CID: cid}}
		}
		err := replyMatches(deliverable, reply)
		tr, perr := transcript.Parse(deliverable)
		if perr != nil {
			if err == nil {
				t.Fatal("a reply matched a transcript that does not parse")
			}
			return
		}
		if err == nil {
			var last *transcript.Message
			for i := len(tr.Messages) - 1; i >= 0; i-- {
				if tr.Messages[i].From == transcriptProvider {
					last = &tr.Messages[i]
					break
				}
			}
			if last == nil || last.Body != body || !sameFiles(last.Attachments, wireFiles(reply.Attachments)) {
				t.Fatalf("a reply matched that is not the last provider entry: %+v", tr.Messages)
			}
		}
		own := requesterOnly(tr.Messages)
		if !requesterEntriesMatch(own, tr.Messages) {
			t.Fatal("a transcript does not match its own requester entries")
		}
		if len(tr.Messages) > 0 {
			i := int(drop % uint(len(tr.Messages)))
			cut := append(append([]transcript.Message(nil), tr.Messages[:i]...), tr.Messages[i+1:]...)
			if !requesterEntriesMatch(own, cut) {
				t.Fatal("leaving an entry out made a mismatch")
			}
		}
	})
}

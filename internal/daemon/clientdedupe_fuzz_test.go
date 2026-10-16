package daemon

import (
	"bytes"
	"encoding/json"
	"testing"
	"unicode/utf8"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// FuzzClientCanon: any client message the entries accept has a canonical
// content, which is valid UTF-8 JSON, is its own canonical form (read back
// with its literals and written again, the same bytes), and does not depend
// on how the message was sent — its messageId, context and task.
func FuzzClientCanon(f *testing.F) {
	for _, s := range []string{
		`{"messageId":"m","role":"ROLE_USER","parts":[{"text":"hi"}],"metadata":{"b":[1,2.50,true,null],"a2a.x":1}}`,
		`{"messageId":"m","parts":[{"data":{"skill":"calc.add","args":{"a":9007199254740993,"\ud83d\ude00":-0,"\ue000":1e2}}}]}`,
		`{"messageId":"m","parts":[{"text":"x\u0001\"\\"},{"raw":"aGk=","filename":"a/b.png","mediaType":"IMAGE/PNG; q=1"}]}`,
		`{"messageId":"m","taskId":"ix","parts":[],"metadata":{"x402.payment.status":"payment-submitted","anet.payment.accept":{"amount":"3","n":1.0}}}`,
		`{"messageId":"m","parts":[{"data":[1,{"a":"b"}]},{"data":{"k":1e400}}]}`,
	} {
		f.Add([]byte(s), "https://example.org/ext/v1")
	}
	f.Fuzz(func(t *testing.T, raw []byte, ext string) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var m a2ashape.Message
		if dec.Decode(&m) != nil || doubleNumbers(m.Metadata) != nil {
			return // refused by every entry
		}
		var c clientContent
		if hasX402Meta(m.Metadata) {
			c = paymentContent(m, []string{ext, previewExtensionURI})
		} else {
			in, err := parseTaskInput(m, a2ashape.RoleUser)
			if err != nil {
				return
			}
			c = inputContent(in, m, []string{ext, previewExtensionURI})
		}
		canon, err := clientCanon(c)
		if err != nil {
			return // a value with no JSON (none of the entries gives one)
		}
		if !utf8.Valid(canon) || !json.Valid(canon) {
			t.Fatalf("canon is not UTF-8 JSON: %q", canon)
		}
		d := json.NewDecoder(bytes.NewReader(canon))
		d.UseNumber()
		var back any
		if err := d.Decode(&back); err != nil {
			t.Fatal(err)
		}
		again, err := canonJSON(back)
		if err != nil || !bytes.Equal(again, canon) {
			t.Fatalf("not its own canonical form:\n %s\n %s (%v)", canon, again, err)
		}
		m.ID, m.ContextID, m.TaskID = m.ID+"x", "ctx-other", "task-other"
		var c2 clientContent
		if hasX402Meta(m.Metadata) {
			c2 = paymentContent(m, []string{previewExtensionURI, ext, ext})
		} else {
			in, err := parseTaskInput(m, a2ashape.RoleUser)
			if err != nil {
				t.Fatalf("the same parts read once and not twice: %v", err)
			}
			c2 = inputContent(in, m, []string{previewExtensionURI, ext, ext})
		}
		if c2b, err := clientCanon(c2); err != nil || !bytes.Equal(c2b, canon) {
			t.Fatalf("how it was sent changed the content:\n %s\n %s", canon, c2b)
		}
	})
}

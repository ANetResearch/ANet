package transcript_test

import (
	"testing"

	"github.com/ANetResearch/ANet/internal/transcript"
)

// Both versions read back to the same messages; an unknown version and a
// value of another shape are refused.
func TestParseReadsBothVersions(t *testing.T) {
	msgs := []transcript.Message{{From: "requester", Body: "hi"}, {From: "provider", Body: "done",
		Attachments: []transcript.Attachment{{Name: "a.png", Mime: "image/png", Size: 3, CID: "bafy"}}}}
	v1 := []byte(`[{"from":"requester","body":"hi"},{"from":"provider","body":"done","attachments":[{"name":"a.png","mime":"image/png","size":3,"cid":"bafy"}]}]`)
	got1, err := transcript.Parse(v1)
	if err != nil || got1.Version != 1 || len(got1.Messages) != 2 || got1.Messages[1].Attachments[0].CID != "bafy" {
		t.Fatalf("v1: %+v %v", got1, err)
	}
	b2, err := transcript.EncodeV2("bm9uY2U", msgs)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := transcript.Parse(b2)
	if err != nil || got2.Version != 2 || got2.Nonce != "bm9uY2U" || len(got2.Messages) != 2 || got2.Messages[0].Body != "hi" {
		t.Fatalf("v2: %+v %v", got2, err)
	}
	for _, bad := range []string{`{"v":3,"nonce":"x","messages":[]}`, `"text"`, ``, `{"v":2,"messages":"x"}`} {
		if _, err := transcript.Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := transcript.EncodeV2("", msgs); err == nil {
		t.Error("v2 without a nonce accepted")
	}
	empty, _ := transcript.EncodeV2("n", nil)
	if string(empty) != `{"v":2,"nonce":"n","messages":[]}` {
		t.Errorf("empty v2 = %s", empty)
	}
}

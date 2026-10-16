package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// UX-10, CUX-P30: the public card declares the preview extension when
// stream_preview is not off — the same for on and with_reasoning, not
// required, no params — and a fresh install does not.
func TestPublicCardPreviewDeclaration(t *testing.T) {
	h := newFakeHub(t)
	d := lampNode(t)
	extOf := func() []map[string]any {
		t.Helper()
		raw, _, err := d.networkCard(h.URL)
		if err != nil {
			t.Fatal(err)
		}
		caps, _ := decodeCard(t, raw)["capabilities"].(map[string]any)
		var out []map[string]any
		for _, e := range caps["extensions"].([]any) {
			if m := e.(map[string]any); m["uri"] == module.ExtPreviewURI {
				out = append(out, m)
			}
		}
		return out
	}
	if got := extOf(); len(got) != 0 {
		t.Fatalf("a fresh install declares previews: %v", got)
	}
	var first map[string]any
	for _, mode := range []string{"on", "with_reasoning"} {
		d.streamPreviewFn = func() string { return mode }
		got := extOf()
		if len(got) != 1 {
			t.Fatalf("%s: %v", mode, got)
		}
		if _, ok := got[0]["required"]; ok {
			t.Fatalf("%s: required is written: %v", mode, got[0])
		}
		if _, ok := got[0]["params"]; ok {
			t.Fatalf("%s: params are written: %v", mode, got[0])
		}
		if first == nil {
			first = got[0]
		} else if a, b := string(mustJSON(t, first)), string(mustJSON(t, got[0])); a != b {
			t.Fatalf("on and with_reasoning differ: %s %s", a, b)
		}
	}
	d.streamPreviewFn = func() string { return "bogus" }
	if got := extOf(); len(got) != 0 {
		t.Fatalf("an unknown value declares previews: %v", got)
	}
	d.streamPreviewFn = func() string { return "off" }
	if got := extOf(); len(got) != 0 {
		t.Fatalf("off declares previews: %v", got)
	}
}

// The declaration is in the extended cards too (it is made in
// cardContributions, which both use).
func TestPreviewDeclarationInContributions(t *testing.T) {
	d := lampNode(t)
	cc := module.CardContext{AID: d.AID(), Skills: []string{lampCap}}
	if exts, _ := d.cardContributions(cc); len(exts) != 0 {
		t.Fatalf("off: %v", exts)
	}
	d.streamPreviewFn = func() string { return "on" }
	exts, _ := d.cardContributions(cc)
	if len(exts) != 1 || exts[0]["uri"] != module.ExtPreviewURI {
		t.Fatalf("on: %v", exts)
	}
}

// Only a change between off and not off alters the card.
func TestStreamPreviewChangeRepublishesOnlyAcrossOff(t *testing.T) {
	for _, tc := range []struct {
		old, cur string
		want     bool
	}{{"off", "on", true}, {"on", "off", true}, {"on", "with_reasoning", false}, {"with_reasoning", "on", false}, {"off", "off", false}, {"", "on", true}} {
		old, cur := normalPreviewMode(tc.old), normalPreviewMode(tc.cur)
		if got := (old == "") != (cur == ""); got != tc.want {
			t.Errorf("%s → %s: republish %v, want %v", tc.old, tc.cur, got, tc.want)
		}
	}
}

// CUX-P10: a /tasks/send with preview asks the provider for previews of
// the round (anet.preview_mode on the message it sends); off and a
// capability call do not; an unknown value is refused.
func TestTasksSendPreviewField(t *testing.T) {
	_, req, prov := registeredPair(t)
	p := newPlaneFor(t, req, "tok")
	send := func(body string) (int, map[string]any) {
		resp, raw := p.req(t, "POST", "/tasks/send", body, p.bearer)
		out := map[string]any{}
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	modeOf := func(id string) string {
		msgs, err := req.ix.Messages(id)
		if err != nil || len(msgs) == 0 {
			t.Fatalf("messages of %s: %v", id, err)
		}
		return previewModeOf([]byte(msgs[0].Metadata))
	}
	for _, mode := range []string{"on", "with_reasoning"} {
		code, out := send(`{"to":"` + prov.AID() + `","text":"hi ` + mode + `","preview":"` + mode + `","return_immediately":true}`)
		if code != 200 {
			t.Fatalf("%s: %d %v", mode, code, out)
		}
		if got := modeOf(out["id"].(string)); got != mode {
			t.Errorf("%s: the message asks for %q", mode, got)
		}
	}
	for i, body := range []string{`"preview":"off"`, `"preview":""`, `"x":1`} {
		code, out := send(`{"to":"` + prov.AID() + `","text":"hi off ` + string(rune('a'+i)) + `",` + body + `,"return_immediately":true}`)
		if code != 200 {
			t.Fatalf("%s: %d %v", body, code, out)
		}
		if got := modeOf(out["id"].(string)); got != "" {
			t.Errorf("%s: the message asks for %q", body, got)
		}
	}
	if code, out := send(`{"to":"` + prov.AID() + `","text":"hi","preview":"loud","return_immediately":true}`); code != 400 {
		t.Fatalf("an unknown value: %d %v", code, out)
	}
	// A follow-up message may ask too.
	_, first := send(`{"to":"` + prov.AID() + `","text":"first","return_immediately":true}`)
	id := first["id"].(string)
	if err := prov.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(context.Background(), id, "which?", nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code, out := send(`{"task_id":"` + id + `","text":"this one","preview":"on","return_immediately":true}`); code != 200 {
		t.Fatalf("follow-up: %d %v", code, out)
	}
	msgs, _ := req.ix.Messages(id)
	last := msgs[len(msgs)-1]
	if previewModeOf([]byte(last.Metadata)) != "on" {
		t.Errorf("the follow-up does not ask: %s", last.Metadata)
	}
	_ = a2ashape.ArtifactReply
}

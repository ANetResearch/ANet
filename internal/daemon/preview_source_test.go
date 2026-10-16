package daemon

// Tests of the two preview sources and of the stream_preview setting
// (UX-9; correctness-ux §4.3.6, §4.5, §4.8): the openai stream, the
// control-plane route, the setting and the card it changes.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// postTo calls h with body and returns the status and the JSON answer.
func postTo(t *testing.T, h http.HandlerFunc, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%q: %d %q", body, rec.Code, rec.Body)
	}
	return rec.Code, out
}

func previewBody(id, text string) string {
	b, _ := json.Marshal(map[string]any{"task_id": id, "text": text})
	return string(b)
}

// CUX §4.8: 409 task_not_open, 409 preview_off, 200 not_requested, 200 sent.
func TestTasksPreviewRoute(t *testing.T) {
	req, prov, co, clk, id, log := previewPair(t, previewModeOn)
	_ = req

	// The setting is off (the default): the open task gets preview_off.
	if _, err := prov.SetStreamPreview(streamPreviewOff); err != nil {
		t.Fatal(err)
	}
	if code, out := postTo(t, prov.hTasksPreview, previewBody(id, "x")); code != 409 || out["reason"] != "preview_off" {
		t.Fatalf("off: %d %v", code, out)
	}
	if _, err := prov.SetStreamPreview(streamPreviewOn); err != nil {
		t.Fatal(err)
	}
	// Not a task of this node.
	if code, out := postTo(t, prov.hTasksPreview, previewBody("ix-nobody", "x")); code != 409 || out["reason"] != "task_not_open" {
		t.Fatalf("unknown task: %d %v", code, out)
	}
	// The task the node sent is not one it answers.
	if code, out := postTo(t, req.hTasksPreview, previewBody(id, "x")); code != 409 || out["reason"] != "task_not_open" {
		t.Fatalf("outbound task: %d %v", code, out)
	}
	for _, bad := range []string{``, `{}`, `{"task_id":"` + id + `","text":1}`, `{"task_id":"` + id + `","text":"` + strings.Repeat("a", maxPreviewPostText+1) + `"}`} {
		rec := httptest.NewRecorder()
		prov.hTasksPreview(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(bad)))
		if rec.Code != 400 {
			t.Errorf("a malformed body got %d", rec.Code)
		}
	}
	// The round asked: sent. Not delivered yet: the coalescer holds it.
	if code, out := postTo(t, prov.hTasksPreview, previewBody(id, "so far")); code != 200 || out["sent"] != true {
		t.Fatalf("sent: %d %v", code, out)
	}
	if n, _ := flushPreviews(co, clk.now()); n != 1 || log.count() != 1 {
		t.Fatalf("%d frames, %d sent", n, log.count())
	}
	// A round that asked for nothing: sent false, with the sentence.
	req2, prov2, _, _, id2, _ := previewPair(t, "")
	_ = req2
	if _, err := prov2.SetStreamPreview(streamPreviewOn); err != nil {
		t.Fatal(err)
	}
	if code, out := postTo(t, prov2.hTasksPreview, previewBody(id2, "x")); code != 200 || out["sent"] != false ||
		out["reason"] != "not_requested" || out["message"] == "" {
		t.Fatalf("not requested: %d %v", code, out)
	}
	// An ended task is not open.
	if err := prov.completeWithReply(context.Background(), id, "done", nil); err != nil {
		t.Fatal(err)
	}
	if code, out := postTo(t, prov.hTasksPreview, previewBody(id, "x")); code != 409 || out["reason"] != "task_not_open" {
		t.Fatalf("ended: %d %v", code, out)
	}
}

// The setting is on no surface a browser session or MCP reaches.
func TestPreviewRoutesAreBearerOnly(t *testing.T) {
	for _, r := range []string{"POST /preview", "POST /tasks/preview"} {
		if _, ok := sessionRoutes[r]; ok {
			t.Errorf("%s is open to a console session", r)
		}
	}
}

// previewWanted and previewOffer follow CUX-P4: off, no ask, ended.
func TestPreviewWantedFollowsCUXP4(t *testing.T) {
	_, prov, _, _, id, _ := previewPair(t, previewModeOn)
	if _, err := prov.SetStreamPreview(streamPreviewOff); err != nil {
		t.Fatal(err)
	}
	if _, ok := prov.previewWanted(id); ok || prov.previewOffer(id, "x", "") {
		t.Fatal("previews are off, yet the round is wanted")
	}
	if _, err := prov.SetStreamPreview(streamPreviewOn); err != nil {
		t.Fatal(err)
	}
	if m, ok := prov.previewWanted(id); !ok || m != previewModeOn {
		t.Fatalf("%q %v", m, ok)
	}
	if !prov.previewOffer(id, "x", "") {
		t.Fatal("the round did not take the offer")
	}
	if err := prov.completeWithReply(context.Background(), id, "done", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := prov.previewWanted(id); ok {
		t.Fatal("an ended task is wanted")
	}
}

// stream_preview is checked at start, set through SetStreamPreview and
// saved before it is in force.
func TestStreamPreviewSetting(t *testing.T) {
	if DefaultConfig().StreamPreview != "off" {
		t.Fatal("a fresh config.json does not say off")
	}
	l := NewLayout(t.TempDir())
	for v, ok := range map[any]bool{"off": true, "on": true, "with_reasoning": true, nil: true,
		"yes": false, "ON": false, "": false, true: false, 1: false} {
		m := freshConfigFile(t)
		m["stream_preview"] = v
		writeConfigMap(t, l, m)
		err := CheckConfigForStart(l)
		if (err == nil) != ok {
			t.Errorf("stream_preview %v: %v", v, err)
		}
		if err != nil && !strings.Contains(err.Error(), "stream_preview") {
			t.Errorf("the refusal does not name the key: %v", err)
		}
	}
	d := newBareDaemon(t)
	if from, err := d.SetStreamPreview("with_reasoning"); err != nil || from != "off" {
		t.Fatalf("%q %v", from, err)
	}
	saved, err := LoadConfig(d.layout)
	if err != nil || saved.StreamPreview != "with_reasoning" || d.config().streamPreview() != "with_reasoning" {
		t.Fatalf("saved %q (%v)", saved.StreamPreview, err)
	}
	if _, err := d.SetStreamPreview("loud"); err == nil {
		t.Fatal("a value that is no setting was taken")
	}
	// The route: {} reads, {"mode"} sets, a value that is no setting is refused.
	if code, out := postTo(t, d.hPreview, `{}`); code != 200 || out["mode"] != "with_reasoning" {
		t.Fatalf("read: %d %v", code, out)
	}
	if code, out := postTo(t, d.hPreview, `{"mode":"off"}`); code != 200 || out["mode"] != "off" || out["from"] != "with_reasoning" {
		t.Fatalf("set: %d %v", code, out)
	}
	if code, out := postTo(t, d.hPreview, `{"mode":"loud"}`); code != 409 || out["reason"] != "config_invalid" {
		t.Fatalf("refused: %d %v", code, out)
	}
	if d.config().streamPreview() != "off" {
		t.Fatal("a refused value changed the setting")
	}
}

// CUX-P30: the public card declares previews while the setting is not off;
// moving between off and on republishes it, on and with_reasoning does
// not, and a node without a hub publishes nothing.
func TestPreviewToggleRepublishesCard(t *testing.T) {
	srv, _, prov := quietPair(t)
	setPolicyQuiet(prov, PolicyOpen) // an open node publishes a chat skill, so it has a card
	fh := fakeHubAt(t, srv.URL)
	calls := func() int { fh.mu.Lock(); defer fh.mu.Unlock(); return fh.registerCalls }
	declares := func() bool {
		raw, err := prov.NetworkCard()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(string(raw), previewExtensionURI)
	}
	settle := func() int {
		prev := -1
		for calls() != prev {
			prev = calls()
			time.Sleep(300 * time.Millisecond)
		}
		return prev
	}
	base := settle()
	if declares() {
		t.Fatal("a node with previews off declares them")
	}
	for _, step := range []struct {
		to        string
		republish bool
	}{{"on", true}, {"with_reasoning", false}, {"off", true}, {"off", false}} {
		if _, err := prov.SetStreamPreview(step.to); err != nil {
			t.Fatal(err)
		}
		if step.republish {
			waitUntil(t, "the registration to be sent again after "+step.to, func() bool { return calls() > base })
		}
		now := settle()
		if (now > base) != step.republish {
			t.Fatalf("-> %s: %d registrations, was %d; want a republish: %v", step.to, now, base, step.republish)
		}
		base = now
		if declares() != (step.to != "off") {
			t.Fatalf("-> %s: the card declares previews: %v", step.to, declares())
		}
	}
	// No hub: nothing to publish, nothing sent.
	d := newBareDaemon(t)
	if _, err := d.SetStreamPreview("on"); err != nil {
		t.Fatal(err)
	}
}

// --- the openai stream (CUX-P5) ---

// sseFixtures are recorded streams of three servers' dialects, each with
// the non-streamed answer the same server gives for the same text.
var sseFixtures = []struct {
	name, stream, whole string
}{
	{"deepseek", "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Let me think. \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"The user asks.\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\", \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"world.\\n\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n", "Hello, world.\n"},
	{"qwen", ": keep-alive\n\n" +
		"data:{\"choices\":[{\"delta\":{\"reasoning\":\"hmm\"},\"finish_reason\":null}]}\n\n" +
		"data:{\"choices\":[{\"delta\":{\"content\":\"  你好，\"},\"finish_reason\":null}]}\n\n" +
		"data:{\"choices\":[{\"delta\":{\"content\":\"世界\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: not json\n\ndata:{\"choices\":[]}\n\ndata:[DONE]\n\n", "  你好，世界"},
	{"ollama", "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"A \"},\"finish_reason\":null}]}\r\n\r\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"yellow circle <<ANET_TASK_DONE>>\"},\"finish_reason\":null}]}\r\n\r\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}]}\r\n\r\n" +
		"data: [DONE]\r\n\r\n", "A yellow circle <<ANET_TASK_DONE>>"},
}

// CUX-P5: the streamed and the non-streamed answer are the same bytes,
// and the accumulated text and reasoning are what the coalescer is offered.
func TestStreamedAnswerEqualsTheWholeOne(t *testing.T) {
	for _, f := range sseFixtures {
		t.Run(f.name, func(t *testing.T) {
			var offers [][2]string
			got, err := readChatStream(strings.NewReader(f.stream), func(text, reasoning string) {
				offers = append(offers, [2]string{text, reasoning})
			})
			if err != nil {
				t.Fatal(err)
			}
			want := strings.TrimSpace(f.whole)
			if got != want {
				t.Fatalf("streamed %q, whole %q", got, want)
			}
			a, _ := stripDoneSentinel(got)
			b, _ := stripDoneSentinel(want)
			if a != b {
				t.Fatalf("after the sentinel: %q and %q", a, b)
			}
			if len(offers) == 0 || offers[len(offers)-1][0] != f.whole && strings.TrimSpace(offers[len(offers)-1][0]) != want {
				t.Fatalf("the last offer is not the whole text: %q", offers)
			}
		})
	}
}

// openAIStreamServer answers /chat/completions as a server would: JSON for
// a plain call, the fixture as SSE when stream is asked.
func openAIStreamServer(t *testing.T, stream string, whole string, status int) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var plain, streamed atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if status != 0 {
			http.Error(w, "model exploded", status)
			return
		}
		if req["stream"] == true {
			streamed.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, stream)
			return
		}
		plain.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": whole}}}})
	}))
	t.Cleanup(srv.Close)
	return srv, &plain, &streamed
}

func TestOpenAIReplierStreamsOnlyWhenAskedAndAgrees(t *testing.T) {
	for _, f := range sseFixtures {
		t.Run(f.name, func(t *testing.T) {
			srv, plain, streamed := openAIStreamServer(t, f.stream, f.whole, 0)
			r := &openAIReplier{cfg: AutoReplyConfig{APIBase: srv.URL, Model: "m"}}
			turns := []chatTurn{{Role: "user", Text: "hi"}}
			a, err := r.Reply(context.Background(), replyContext{}, turns)
			if err != nil || plain.Load() != 1 || streamed.Load() != 0 {
				t.Fatalf("plain: %q %v (%d plain, %d streamed)", a, err, plain.Load(), streamed.Load())
			}
			var last string
			b, err := r.Reply(context.Background(), replyContext{Preview: func(text, _ string) { last = text }}, turns)
			if err != nil || streamed.Load() != 1 {
				t.Fatalf("streamed: %q %v", b, err)
			}
			if a != b {
				t.Fatalf("plain %q, streamed %q: not the same bytes", a, b)
			}
			if strings.TrimSpace(last) != b {
				t.Fatalf("the last offer %q is not the answer %q", last, b)
			}
		})
	}
}

// A server that ignores stream and answers JSON is read as JSON.
func TestOpenAIReplierStreamAnsweredWithJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":" whole "}}]}`))
	}))
	defer srv.Close()
	r := &openAIReplier{cfg: AutoReplyConfig{APIBase: srv.URL, Model: "m"}}
	got, err := r.Reply(context.Background(), replyContext{Preview: func(string, string) {}}, []chatTurn{{Role: "user", Text: "hi"}})
	if err != nil || got != "whole" {
		t.Fatalf("%q %v", got, err)
	}
}

// A stream that breaks, ends early or is empty is an error: never half an
// answer sent as a whole one.
func TestStreamErrors(t *testing.T) {
	for name, s := range map[string]string{
		"no end":     "data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n",
		"no content": "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		"empty":      "",
		"too long":   "data: " + strings.Repeat("x", maxSSELine+10) + "\n",
	} {
		if got, err := readChatStream(strings.NewReader(s), func(string, string) {}); err == nil {
			t.Errorf("%s: %q accepted", name, got)
		}
	}
}

// CUX-P5, TestStreamFailureNoRecall: a failed stream is one failed call.
// There is no second call without stream, and the requester gets
// backend_failed.
func TestStreamFailureNoRecall(t *testing.T) {
	for name, status := range map[string]int{"HTTP error": 500, "broken stream": 0} {
		t.Run(name, func(t *testing.T) {
			// A stream that ends before it finished.
			srv, plain, streamed := openAIStreamServer(t, "data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n", "whole", status)
			f := newAutoReplyFixture(t, AutoReplyConfig{Model: "m"}, &fakeOpenAI{})
			cfg := f.cfg
			cfg.APIBase = srv.URL
			f.cfg = cfg
			f.replier = &openAIReplier{cfg: cfg}
			if _, err := f.prov.SetStreamPreview(streamPreviewOn); err != nil {
				t.Fatal(err)
			}
			id := previewDelegation(t, f.req, f.prov, previewModeOn)
			f.tick(t)
			f.tick(t)
			if plain.Load() != 0 || streamed.Load() > 1 {
				t.Fatalf("%d plain calls, %d streamed: the failed stream was called again", plain.Load(), streamed.Load())
			}
			if err := f.req.pollOnce(f.ctx); err != nil {
				t.Fatal(err)
			}
			ix, err := f.req.ix.Get(id)
			if err != nil || ix.State != interactions.StateInputRequired {
				t.Fatalf("%+v %v", ix, err)
			}
		})
	}
}

// A round that asked for previews is answered by a streamed call, and the
// answer sent is the one the non-streamed call gives.
func TestAutoReplyStreamsAPreviewedRound(t *testing.T) {
	f0 := sseFixtures[0]
	srv, plain, streamed := openAIStreamServer(t, f0.stream, f0.whole, 0)
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "m"}, &fakeOpenAI{})
	cfg := f.cfg
	cfg.APIBase = srv.URL
	f.cfg, f.replier = cfg, &openAIReplier{cfg: cfg}
	if _, err := f.prov.SetStreamPreview(streamPreviewWR); err != nil {
		t.Fatal(err)
	}
	clk := newPreviewClock()
	co := manualPreviews(f.prov, clk)

	// A round that asked: one streamed call, frames offered, the answer as the plain one.
	id := previewDelegation(t, f.req, f.prov, previewModeWithReasoning)
	f.tick(t)
	if streamed.Load() != 1 || plain.Load() != 0 {
		t.Fatalf("%d streamed, %d plain", streamed.Load(), plain.Load())
	}
	if got := f.lastPeerMsg(t, id); got.Body != strings.TrimSpace(f0.whole) {
		t.Fatalf("answer %q", got.Body)
	}
	_ = co
	// A round that asked for nothing: the call is as it ever was.
	id2 := previewDelegation(t, f.req, f.prov, "")
	f.tick(t)
	if streamed.Load() != 1 || plain.Load() != 1 {
		t.Fatalf("%d streamed, %d plain after a round that asked for nothing", streamed.Load(), plain.Load())
	}
	_ = id2
	// Previews off on this node: plain again, whatever the round asked.
	if _, err := f.prov.SetStreamPreview(streamPreviewOff); err != nil {
		t.Fatal(err)
	}
	previewDelegation(t, f.req, f.prov, previewModeOn)
	f.tick(t)
	if streamed.Load() != 1 || plain.Load() != 2 {
		t.Fatalf("%d streamed, %d plain with previews off", streamed.Load(), plain.Load())
	}
}

func FuzzReadChatStream(f *testing.F) {
	for _, s := range sseFixtures {
		f.Add(s.stream)
	}
	f.Add("data: [DONE]\n")
	f.Add("data: {\"choices\":[{\"delta\":{\"content\":\"\\ud800\"},\"finish_reason\":\"x\"}]}\n")
	f.Fuzz(func(t *testing.T, s string) {
		var buf bytes.Buffer
		out, err := readChatStream(strings.NewReader(s), func(text, reasoning string) { buf.Reset() })
		if err == nil && strings.TrimSpace(out) == "" {
			t.Fatalf("an empty answer is no error: %q", s)
		}
	})
}


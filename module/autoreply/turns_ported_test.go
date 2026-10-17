package autoreply

// turns_ported_test.go: the turn-level tests that drove the auto-reply loop through the daemon's hub
// fixture before the module moved out of internal/daemon, on the module's stand-in node (turnfx_test.go).
// What the kernel decides (who is owed a turn, the deny list at every send, how a completion is stored and
// shown to the requester) is asserted by the kernel's own tests in internal/daemon; here the stand-in node
// models that decision and the tests assert what the module does with it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/module"
)

// attachImage puts an image on the first message of ix, as a peer's delegation with a picture.
func attachImage(fx *turnFx, ix, cid, mime string, data []byte) {
	fx.h.mu.Lock()
	defer fx.h.mu.Unlock()
	x := fx.h.ixs[ix]
	x.files = map[string][]byte{cid: data}
	x.turn.Messages[0].Attachments = []module.TurnAttachment{{CID: cid, Name: "pic.png", Mime: mime, Size: int64(len(data))}}
}

// The openai backend answers a delegation with an image: the image rides inline as a base64 data URL, the
// conversation is mapped to chat messages, a second scan does not answer again, and a follow-up keeps the
// context, image included.
func TestAutoReplyVisionRoundTrip(t *testing.T) {
	api := &fakeOpenAI{reply: "a yellow circle on blue"}
	fx := newTurnFx(t, AutoReplyConfig{Model: "test-vl", SystemPrompt: "you are a vision service"}, api)
	ix := fx.deliver(fxPeerA, false, "describe this image")
	attachImage(fx, ix, "cid-pic", "image/png", []byte("\x89PNG\r\n\x1a\n-not-really-a-png"))

	fx.tick()
	if got, _ := fx.lastSent(ix); got != "a yellow circle on blue" {
		t.Fatalf("reply = %q, want the backend answer", got)
	}
	body, _ := api.lastBody.Load().(string)
	for _, want := range []string{"you are a vision service", "describe this image", "data:image/png;base64,", "image_url"} {
		if !strings.Contains(body, want) {
			t.Fatalf("backend request missing %q:\n%s", want, body)
		}
	}
	fx.tick() // the last word is ours: no second reply
	if got := api.calls.Load(); got != 1 {
		t.Fatalf("backend called %d times, want 1 (no double reply)", got)
	}
	fx.say(ix, "what text is in it?")
	fx.tick()
	if got := api.calls.Load(); got != 2 {
		t.Fatalf("backend called %d times after the follow-up, want 2", got)
	}
	body, _ = api.lastBody.Load().(string)
	for _, want := range []string{"what text is in it?", "a yellow circle on blue", "data:image/png;base64,"} {
		if !strings.Contains(body, want) {
			t.Fatalf("follow-up request missing %q", want)
		}
	}
	if x := fx.ix(ix); x.ended != 0 || x.refusals != 0 {
		t.Fatalf("an ordinary conversation ended or refused: %+v", x)
	}
}

// An image the host cannot read is a failed backend, not a reply without the image.
func TestAnAttachmentThatCannotBeReadFailsTheTurn(t *testing.T) {
	api := &fakeOpenAI{reply: "unused"}
	fx := newTurnFx(t, AutoReplyConfig{Model: "m"}, api)
	ix := fx.deliver(fxPeerA, false, "look")
	attachImage(fx, ix, "cid-pic", "image/png", []byte("x"))
	fx.h.mu.Lock()
	fx.h.ixs[ix].files = nil
	fx.h.mu.Unlock()
	fx.tick()
	if api.calls.Load() != 0 {
		t.Fatal("the model was called without the image")
	}
	if _, sent := fx.lastSent(ix); sent {
		t.Fatal("a reply went out")
	}
}

// Auto-reply is symmetric: when this node delegated a task and the peer answers, the node answers too.
func TestAutoReplyOutbound(t *testing.T) {
	api := &fakeOpenAI{reply: "sure, here is more detail"}
	fx := newTurnFx(t, AutoReplyConfig{Model: "test"}, api)
	ix := fx.deliverOutbound(fxPeerA, false, "can you look into X?", "yes, what exactly do you need?")
	fx.tick()
	if got := api.calls.Load(); got != 1 {
		t.Fatalf("backend called %d times on an outbound thread, want 1", got)
	}
	if got, _ := fx.lastSent(ix); got != "sure, here is more detail" {
		t.Fatalf("outbound auto-reply = %q", got)
	}
}

// The runaway guard: once the node has auto-sent the most messages one interaction allows, it proposes the
// end once instead of replying again, so two autopilots cannot loop for ever.
func TestAutoReplyCapProposesEnd(t *testing.T) {
	prev := autoReplyMaxReplies
	autoReplyMaxReplies = 1
	t.Cleanup(func() { autoReplyMaxReplies = prev })
	api := &fakeOpenAI{reply: "reply"}
	fx := newTurnFx(t, AutoReplyConfig{Model: "test"}, api)
	ix := fx.deliver(fxPeerA, false, "q1")
	fx.tick() // the one reply allowed
	if got := api.calls.Load(); got != 1 {
		t.Fatalf("want 1 backend call, got %d", got)
	}
	fx.say(ix, "q2")
	fx.tick()
	fx.tick()
	if got := api.calls.Load(); got != 1 {
		t.Fatalf("cap breached: %d backend calls, want 1", got)
	}
	if x := fx.ix(ix); x.ended != 1 || len(x.sent) != 1 {
		t.Fatalf("at the cap the node proposed the end %d times and sent %d messages, want once and 1", x.ended, len(x.sent))
	}
}

// Completion-awareness: when the backend appends the task-done marker, the module strips it and completes
// the task with the surrounding text as its result; a reply without the marker is an ordinary turn.
func TestAutoReplyDoneSentinelCompletesTheTask(t *testing.T) {
	api := &fakeOpenAI{reply: "here is your function: def f(): pass\n" + autoReplyDoneSentinel}
	fx := newTurnFx(t, AutoReplyConfig{Model: "test"}, api)
	ix := fx.deliver(fxPeerA, false, "write f()")
	fx.tick()
	x := fx.ix(ix)
	if len(x.sent) != 1 || x.sent[0].Text != "here is your function: def f(): pass" || !x.sent[0].Final || x.state != "completed" {
		t.Fatalf("done reply: %+v state %s", x.sent, x.state)
	}
	if strings.Contains(x.sent[0].Text, autoReplyDoneSentinel) {
		t.Fatalf("the marker was delivered: %q", x.sent[0].Text)
	}

	api2 := &fakeOpenAI{reply: "which language?"}
	fx2 := newTurnFx(t, AutoReplyConfig{Model: "test"}, api2)
	ix2 := fx2.deliver(fxPeerA, false, "write f()")
	fx2.tick()
	if x := fx2.ix(ix2); len(x.sent) != 1 || x.sent[0].Final || x.sent[0].Text != "which language?" || x.state == "completed" {
		t.Fatalf("a question completed the task: %+v state %s", x.sent, x.state)
	}
}

// A done reply to a task this node delegated, or to one whose end the node itself asked for, is a message,
// and the end is proposed after it.
func TestADoneReplyOnAnOutboundTurnProposesTheEnd(t *testing.T) {
	api := &fakeOpenAI{reply: "thanks, that settles it\n" + autoReplyDoneSentinel}
	fx := newTurnFx(t, AutoReplyConfig{Model: "test"}, api)
	ix := fx.deliverOutbound(fxPeerA, false, "a question", "an answer")
	fx.tick()
	x := fx.ix(ix)
	if len(x.sent) != 1 || x.sent[0].Final || x.sent[0].Text != "thanks, that settles it" || x.ended != 1 {
		t.Fatalf("outbound done: %+v ended %d", x.sent, x.ended)
	}
}

// A backend delivers files by writing them to $ANET_OUTBOX: the module sends them with the reply text as
// one message.
func TestAutoReplyOutboxAttachment(t *testing.T) {
	fx := newTurnFx(t, AutoReplyConfig{Model: "test"}, &fakeOpenAI{reply: "unused"})
	fx.replier = &outboxReplier{text: "here is your file", file: "game.html", data: []byte("<html>hi</html>")}
	ix := fx.deliver(fxPeerA, false, "make a file")
	fx.tick()
	x := fx.ix(ix)
	if len(x.sent) != 1 || x.sent[0].Text != "here is your file" {
		t.Fatalf("sent %+v", x.sent)
	}
	if atts := x.sent[0].Atts; len(atts) != 1 || atts[0].Name != "game.html" || !bytes.Equal(atts[0].Data, []byte("<html>hi</html>")) {
		t.Fatalf("want exactly one attachment game.html, got %+v", atts)
	}
}

// A misconfigured block fails loudly instead of silently not replying.
func TestNewAutoReplierValidation(t *testing.T) {
	env := newBareDaemon(t).mod.env
	if _, err := newAutoReplier(AutoReplyConfig{}, env); err == nil {
		t.Fatal("want error for missing api_base/model")
	}
	if _, err := newAutoReplier(AutoReplyConfig{Backend: "quantum"}, env); err == nil {
		t.Fatal("want error for unknown backend")
	}
	if _, err := newAutoReplier(AutoReplyConfig{Backend: "exec"}, env); err == nil {
		t.Fatal("want error for exec without agent")
	}
}

// The exec backend runs the local agent for a trusted peer and sends what it prints.
func TestAutoReplyExecRoundTrip(t *testing.T) {
	stub := writeStub(t, "#!/bin/sh\necho \"stub received prompt\" >&2\necho \"EXEC-OK: processed delegation\"\n")
	setExecCommand(t, stub)
	fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "cursor"}, nil)
	ix := fx.deliver(fxPeerA, true, "say hello via exec")
	fx.tick()
	if got, _ := fx.lastSent(ix); got != "EXEC-OK: processed delegation" {
		t.Fatalf("reply = %q, want the stub output", got)
	}
	// A peer that is not trusted does not reach the agent unconfined (A2A-DESIGN section 6).
	other := fx.deliver("bafyreistranger", false, "say hello via exec")
	fx.tick()
	if _, sent := fx.lastSent(other); sent {
		t.Fatal("an untrusted peer reached the exec agent")
	}
}

// A first turn the backend is slow to answer is announced to the requester as working before the reply, so
// that a turn longer than the requester's no_response_after is not failed there; a quick turn costs no
// extra message.
func TestASlowAutoReplyTurnIsAnnouncedAsWorking(t *testing.T) {
	old := slowTurnAfter
	slowTurnAfter = 50 * time.Millisecond
	t.Cleanup(func() { slowTurnAfter = old })

	fx := newTurnFx(t, AutoReplyConfig{Model: "test"}, &fakeOpenAI{reply: "quick"})
	fx.replier = sleepyReplier{after: 400 * time.Millisecond}
	ix := fx.deliver(fxPeerA, false, "a long piece of work")
	fx.tick()
	if got, _ := fx.lastSent(ix); got != "done, after a while" {
		t.Fatalf("reply %q", got)
	}
	if x := fx.ix(ix); x.working != 1 || len(x.sent) != 1 {
		t.Fatalf("the requester was told working %d times and got %d replies, want 1 and 1", x.working, len(x.sent))
	}

	quick := newTurnFx(t, AutoReplyConfig{Model: "test"}, &fakeOpenAI{reply: "quick"})
	ix2 := quick.deliver(fxPeerA, false, "a small question")
	quick.tick()
	time.Sleep(150 * time.Millisecond)
	if got, _ := quick.lastSent(ix2); got != "quick" {
		t.Fatalf("reply %q", got)
	}
	if x := quick.ix(ix2); x.working != 0 {
		t.Fatalf("a quick turn was announced as working %d times", x.working)
	}
}

// A2A-DESIGN section 5.1: the deny list is read on every turn whatever the backend. A peer denied since its
// message landed is not answered, before any revocation sweep.
func TestTheOpenAIBackendReadsTheDenyListEveryTurn(t *testing.T) {
	api := &fakeOpenAI{reply: "hello"}
	fx := newTurnFx(t, AutoReplyConfig{Model: "test"}, api)
	ix := fx.deliver(fxPeerA, false, "first question")
	fx.tick()
	if n := api.calls.Load(); n != 1 {
		t.Fatalf("backend called %d times for an allowed peer, want 1", n)
	}
	fx.say(ix, "second question")
	fx.deny(ix, true)
	fx.tick()
	if n := api.calls.Load(); n != 1 {
		t.Fatalf("backend called %d times after the deny, want still 1", n)
	}
}

// Section 5.1: a peer denied while the backend was producing its reply is not sent that reply.
func TestAReplyIsNotSentToAPeerDeniedWhileTheBackendWorked(t *testing.T) {
	api := &fakeOpenAI{reply: "hello"}
	fx := newTurnFx(t, AutoReplyConfig{Model: "test"}, api)
	ix := fx.deliver(fxPeerA, false, "a question")
	fx.replier = denyDuring{inner: fx.replier, deny: func() { fx.deny(ix, true) }}
	fx.tick()
	if n := api.calls.Load(); n != 1 {
		t.Fatalf("backend called %d times, want 1", n)
	}
	if x := fx.ix(ix); len(x.sent) != 0 || x.refusals != 0 || x.ended != 0 {
		t.Fatalf("something went to the peer denied during the call: %+v", x)
	}
}

// --- the openai stream (CUX-P5) ---

// sseFixtures are recorded streams of three servers' dialects, each with the non-streamed answer the same
// server gives for the same text.
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

// openAIStreamServer answers /chat/completions as a server would: JSON for a plain call, the fixture as SSE
// when stream is asked. A status other than 0 fails every call with it.
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
			_, _ = w.Write([]byte(stream))
			return
		}
		plain.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + jsonString(whole) + `}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &plain, &streamed
}

// CUX-P5: the streamed and the non-streamed answer are the same bytes, and the accumulated text and
// reasoning are what the coalescer is offered.
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

// CUX-P5, TestStreamFailureNoRecall: a failed stream is one failed call. There is no second call without
// stream, and the requester gets backend_failed.
func TestStreamFailureNoRecall(t *testing.T) {
	for name, status := range map[string]int{"HTTP error": 500, "broken stream": 0} {
		t.Run(name, func(t *testing.T) {
			// A stream that ends before it finished.
			srv, plain, streamed := openAIStreamServer(t, "data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n", "whole", status)
			fx := newTurnFx(t, AutoReplyConfig{Model: "m", APIBase: srv.URL}, nil)
			ix := fx.deliver(fxPeerA, false, "a task with previews")
			fx.previewed(ix, true)
			fx.tick()
			fx.tick()
			if plain.Load() != 0 || streamed.Load() > 1 {
				t.Fatalf("%d plain calls, %d streamed: the failed stream was called again", plain.Load(), streamed.Load())
			}
			if x := fx.ix(ix); x.state != "input-required" || x.reason != module.ReasonBackendFailed || len(x.sent) != 0 {
				t.Fatalf("the requester was told state %q reason %q, %d messages", x.state, x.reason, len(x.sent))
			}
		})
	}
}

// A round that asked for previews is answered by a streamed call, and the answer sent is the one the
// non-streamed call gives; a round that asked for nothing, or on a node without previews, is as it ever was.
func TestAutoReplyStreamsAPreviewedRound(t *testing.T) {
	f0 := sseFixtures[0]
	srv, plain, streamed := openAIStreamServer(t, f0.stream, f0.whole, 0)
	fx := newTurnFx(t, AutoReplyConfig{Model: "m", APIBase: srv.URL}, nil)

	id := fx.deliver(fxPeerA, false, "a task with previews")
	fx.previewed(id, true)
	fx.tick()
	if streamed.Load() != 1 || plain.Load() != 0 {
		t.Fatalf("%d streamed, %d plain", streamed.Load(), plain.Load())
	}
	x := fx.ix(id)
	if got, _ := fx.lastSent(id); got != strings.TrimSpace(f0.whole) {
		t.Fatalf("answer %q", got)
	}
	if len(x.offers) == 0 || strings.TrimSpace(x.offers[len(x.offers)-1]) != strings.TrimSpace(f0.whole) {
		t.Fatalf("the preview sink was offered %q", x.offers)
	}

	id2 := fx.deliver(fxPeerA, false, "a task that asked for nothing")
	fx.tick()
	if streamed.Load() != 1 || plain.Load() != 1 {
		t.Fatalf("%d streamed, %d plain after a round that asked for nothing", streamed.Load(), plain.Load())
	}
	if x := fx.ix(id2); len(x.offers) != 0 {
		t.Fatalf("a round that asked for nothing was offered %q", x.offers)
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

// MOD-3 (6), TestDarwinPendingFailsClosed: with the macOS sandbox not part of the build (seatbeltEnabled
// false, stood in for on this host), a turn that would run in the sandbox is turned away before the agent
// starts with sandbox_unavailable and the local reason platform_unsupported.pending; the direct and openai
// paths are not touched; and the constant is false in 0.3.0.
func TestDarwinPendingFailsClosed(t *testing.T) {
	if seatbeltEnabled {
		t.Fatal("seatbeltEnabled is false in 0.3.0 (MOD-V3)")
	}
	prev := sandboxPrepareForTest
	sandboxPrepareForTest = func(*sandboxPlan) error { return seatbeltGate(false) }
	t.Cleanup(func() { sandboxPrepareForTest = prev })
	runs := execStub(t) // the agent, were it started
	cfg := AutoReplyConfig{Backend: "exec", Agent: "claude", Untrusted: UntrustedSandbox, APIKey: "k", APIBase: "https://api.anthropic.com"}
	fx := newTurnFx(t, cfg, nil)
	ix := fx.deliver(fxPeerA, false, "needs the sandbox")
	fx.tick()
	if x := fx.ix(ix); x.state != "rejected" || x.reason != module.ReasonSandboxUnavailable || len(x.sent) != 0 {
		t.Fatalf("state %s reason %q sent %d", x.state, x.reason, len(x.sent))
	}
	if runs() != 0 {
		t.Fatal("the agent started")
	}
	// The local reason is the one a status or doctor says.
	err := seatbeltGate(false)
	var pe *sandboxProbeError
	if !errors.As(err, &pe) || pe.Code != "platform_unsupported.pending" {
		t.Fatalf("gate = %v", err)
	}
	// The openai backend and a direct run are not affected by the switch.
	if g := gateFor(AutoReplyConfig{Backend: "exec", Agent: "claude"}, true); g.mode != modeDirect {
		t.Errorf("a trusted peer's turn: %+v", g)
	}
	if g := gateFor(AutoReplyConfig{Model: "m", APIBase: "http://x"}, false); g.mode != modeOpenAI {
		t.Errorf("openai turn: %+v", g)
	}
}

// MOD-3 (3), TestModuleStopWaitsForRuns: Stop returns only when the turns under way have ended and
// recorded, which is before the evidence ledger closes, and no run starts after it.
func TestModuleStopWaitsForRuns(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once, runs atomic.Int64
	sandboxRunnerForTest = func(ctx context.Context, req sandboxRequest, workDir string) (string, *egressSummary, error) {
		runs.Add(1)
		if once.Add(1) == 1 {
			close(started)
		}
		<-release
		return "a late answer", &egressSummary{}, nil
	}
	t.Cleanup(func() { sandboxRunnerForTest = nil })
	cfg := AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Untrusted: UntrustedSandbox}
	fx := newTurnFx(t, cfg, nil)
	if err := fx.m.startLoop(fx.cfg); err != nil { // the real loop
		t.Fatal(err)
	}
	fx.deliver(fxPeerA, false, "a long task")
	fx.wakeLoop()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not start")
	}
	done := make(chan struct{})
	go func() { _ = fx.m.Stop(context.Background()); close(done) }()
	select {
	case <-done:
		t.Fatal("Stop returned while a run was under way")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return after the run ended")
	}
	if got := withExit(fx.invoked(), exitOK); len(got) != 1 {
		t.Fatalf("the run was not recorded before Stop returned: %v", fx.invoked())
	}
	// Nothing starts after Stop.
	fx.deliver(fxPeerA, false, "a second task")
	fx.wakeLoop()
	time.Sleep(200 * time.Millisecond)
	if n := runs.Load(); n != 1 {
		t.Fatalf("%d runs; one started after Stop", n)
	}
	if err := fx.m.startLoop(cfg); err != nil {
		t.Fatal(err)
	}
	fx.wakeLoop()
	time.Sleep(200 * time.Millisecond)
	if n := runs.Load(); n != 1 {
		t.Fatalf("a stopped module started a loop again (%d runs)", n)
	}
}

// K-5: the sandbox's never-bind list holds the default C1 socket whatever the modules of the node declare,
// and the node's own state directory.
func TestNeverBindHoldsTheDefaultC1Socket(t *testing.T) {
	n := newBareDaemon(t)
	never := n.sandboxNeverBind()
	found := false
	for _, p := range never {
		if strings.HasSuffix(p, "anetlink/c1.sock") {
			found = true
		}
	}
	if !found {
		t.Errorf("the never-bind list lacks the default C1 socket: %v", never)
	}
	if !strings.Contains(strings.Join(never, "\n"), filepath.Clean(n.layout.Root)) {
		t.Errorf("the never-bind list lacks the data directory %s: %v", n.layout.Root, never)
	}
	if _, err := os.Stat(n.layout.Root); err != nil {
		t.Fatal(err)
	}
}

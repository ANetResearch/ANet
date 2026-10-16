//go:build mod4_pending

package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// fakeOpenAI is a minimal OpenAI-compatible /chat/completions endpoint that records the last request
// body and returns a fixed reply. calls counts invocations (to assert contract violations SKIP the API).
type fakeOpenAI struct {
	calls    atomic.Int64
	lastBody atomic.Value // string
	reply    string
	fail     bool
}

func (f *fakeOpenAI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		f.calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		f.lastBody.Store(string(b))
		if f.fail {
			http.Error(w, "model exploded", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": f.reply}}},
		})
	})
}

// autoReplyFixture wires a hub + requester + auto-replying provider, with both background loops stopped
// so the test drives pollOnce / autoReplyOnce deterministically.
type autoReplyFixture struct {
	ctx     context.Context
	req     *Daemon
	prov    *Daemon
	api     *fakeOpenAI
	cfg     AutoReplyConfig
	replier autoReplier
}

func newAutoReplyFixture(t *testing.T, cfg AutoReplyConfig, api *fakeOpenAI) *autoReplyFixture {
	t.Helper()
	hub := newFakeHub(t)
	apiSrv := httptest.NewServer(api.handler())
	t.Cleanup(apiSrv.Close)
	ctx := context.Background()

	req := newTestDaemon(t, hub.URL, false)
	prov := newTestDaemon(t, hub.URL, true)
	if err := req.RegisterWithHub(ctx, hub.URL, "Alice", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, hub.URL, "Vision Bot", []string{"vision"}); err != nil {
		t.Fatal(err)
	}
	// Re-stop the relay loops HubRegister restarted; the test polls explicitly.
	for _, d := range []*Daemon{req, prov} {
		d.mu.Lock()
		if d.relayStop != nil {
			d.relayStop()
			d.relayStop = nil
		}
		d.mu.Unlock()
	}

	// The fake service is the openai backend's and a chat agent's model service. A sandboxed claude speaks
	// only the Anthropic API, and any other api_base would be refused (LEC UP-2): it is given none.
	if cfg.Backend != "exec" || cfg.Agent != agentClaude {
		cfg.APIBase = apiSrv.URL + "/v1"
	}
	replier, err := newAutoReplier(cfg, testEnv(prov.layout))
	if err != nil {
		t.Fatal(err)
	}
	return &autoReplyFixture{ctx: ctx, req: req, prov: prov, api: api, cfg: cfg, replier: replier}
}

// tick simulates one auto-reply scan on the provider after pulling its mailbox.
func (f *autoReplyFixture) tick(t *testing.T) {
	t.Helper()
	if err := f.prov.pollOnce(f.ctx); err != nil {
		t.Fatalf("provider poll: %v", err)
	}
	f.prov.autoReplyOnce(f.ctx, f.cfg, f.replier)
}

// lastPeerMsg pulls the requester's mailbox and returns the last message the provider sent it.
func (f *autoReplyFixture) lastPeerMsg(t *testing.T, id string) ThreadMsg {
	t.Helper()
	if err := f.req.pollOnce(f.ctx); err != nil {
		t.Fatalf("requester poll: %v", err)
	}
	ts, err := f.req.Threads()
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range ts {
		if th.InteractionID != id {
			continue
		}
		for i := len(th.Messages) - 1; i >= 0; i-- {
			if th.Messages[i].From == "them" && th.Messages[i].Kind == "text" {
				return th.Messages[i]
			}
		}
	}
	t.Fatalf("no reply from provider on %s", id)
	return ThreadMsg{}
}

// TestAutoReplyVisionRoundTrip drives the built-in auto-reply loop end to end: a delegation WITH an image
// is answered via the OpenAI-compatible backend (image inlined as a base64 data URL, conversation mapped
// to chat messages), a follow-up keeps context, and the peer's end proposal is auto-accepted so the
// receipt is issued.
func TestAutoReplyVisionRoundTrip(t *testing.T) {
	api := &fakeOpenAI{reply: "a yellow circle on blue"}
	f := newAutoReplyFixture(t, AutoReplyConfig{
		Model: "test-vl", SystemPrompt: "you are a vision service",
	}, api)

	img := filepath.Join(t.TempDir(), "pic.png")
	imgBytes := []byte("\x89PNG\r\n\x1a\n-not-really-a-png")
	if err := os.WriteFile(img, imgBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "describe this image", []string{img})
	if err != nil {
		t.Fatal(err)
	}

	f.tick(t)
	if got := f.lastPeerMsg(t, id); got.Body != "a yellow circle on blue" {
		t.Fatalf("reply = %q, want the backend answer", got.Body)
	}
	body, _ := api.lastBody.Load().(string)
	for _, want := range []string{"you are a vision service", "describe this image", "data:image/png;base64,", "image_url"} {
		if !strings.Contains(body, want) {
			t.Fatalf("backend request missing %q:\n%s", want, body)
		}
	}

	// A second tick must NOT reply again (the last word is ours — statelessly derived).
	f.tick(t)
	if got := api.calls.Load(); got != 1 {
		t.Fatalf("backend called %d times, want 1 (no double reply)", got)
	}

	// Follow-up question (no new image) is answered with the prior context, image included.
	if err := f.req.SendMessage(f.ctx, id, "what text is in it?", nil); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if got := api.calls.Load(); got != 2 {
		t.Fatalf("backend called %d times after follow-up, want 2", got)
	}
	body, _ = api.lastBody.Load().(string)
	for _, want := range []string{"what text is in it?", "a yellow circle on blue", "data:image/png;base64,"} {
		if !strings.Contains(body, want) {
			t.Fatalf("follow-up request missing %q", want)
		}
	}

	// The requester proposes ending; the loop accepts → provider issues the receipt → results are done.
	if err := f.req.RequestEnd(f.ctx, id); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	results, err := f.req.Results(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].InteractionID != id {
		t.Fatalf("results = %+v, want ended %s", results, id)
	}
}


// TestAutoReplyOutbound verifies auto-reply is symmetric: when WE delegated a task (an outbound thread)
// and the peer replies, our autopilot answers too — not only when others delegate to us.
func TestAutoReplyOutbound(t *testing.T) {
	api := &fakeOpenAI{reply: "sure, here is more detail"}
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api)

	// The peer must accept the delegation we're about to send it (the fixture's requester allows no one).
	allowPeers(t, f.req, f.prov.AID())
	// The autopilot provider INITIATES by delegating out to the requester.
	id, err := f.prov.Delegate(f.ctx, f.req.AID(), "can you look into X?", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The peer receives it and replies manually (acting as a human/other agent).
	if err := f.req.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.req.SendMessage(f.ctx, id, "yes — what exactly do you need?", nil); err != nil {
		t.Fatal(err)
	}
	// The provider's OUTBOUND thread now ends with the peer's message → autopilot owes a reply.
	f.tick(t)
	if got := api.calls.Load(); got != 1 {
		t.Fatalf("backend called %d times on outbound thread, want 1", got)
	}
	if got := f.lastPeerMsg(t, id); got.Body != "sure, here is more detail" {
		t.Fatalf("outbound auto-reply = %q, want the backend answer", got.Body)
	}
}

// TestAutoReplyCapProposesEnd verifies the runaway guard: once we have auto-sent the most messages one interaction allows
// in one interaction, the provider completes the task instead of replying again — so two autopilots
// cannot loop forever. (Before wire 2 it proposed an end the requester had to accept.)
func TestAutoReplyCapProposesEnd(t *testing.T) {
	prev := autoReplyMaxReplies
	autoReplyMaxReplies = 1
	defer func() { autoReplyMaxReplies = prev }()
	api := &fakeOpenAI{reply: "reply"}
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api)

	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "q1", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t) // our 1st (and only allowed) reply
	if got := api.calls.Load(); got != 1 {
		t.Fatalf("want 1 backend call, got %d", got)
	}
	// Peer sends again; we are at the cap → propose end, no new backend call.
	// It hears from its hub first that the delegation arrived: a follow-up
	// goes only once it did (engine ENG-O2).
	askHubNow(t, f.req)
	if err := f.req.SendMessage(f.ctx, id, "q2", nil); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if got := api.calls.Load(); got != 1 {
		t.Fatalf("cap breached: %d backend calls, want 1", got)
	}
	if err := f.req.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	ts, err := f.req.Threads()
	if err != nil {
		t.Fatal(err)
	}
	ended := false
	for _, th := range ts {
		if th.InteractionID == id && th.State == string(interactions.StateCompleted) {
			ended = true
		}
	}
	if !ended {
		t.Fatal("expected the provider to complete the task at the auto-reply cap")
	}
}

// TestAutoReplyDoneSentinelProposesEnd verifies completion-awareness: when the backend appends the
// task-done marker, the loop strips it, still delivers the surrounding text, and the provider completes
// the task — so two autopilots converge on a finished deliverable instead of chatting until the runaway
// cap.
func TestAutoReplyDoneSentinelProposesEnd(t *testing.T) {
	api := &fakeOpenAI{reply: "here is your function: def f(): pass\n" + autoReplyDoneSentinel}
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api)

	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "write f()", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)

	// The delivered message must NOT contain the raw marker.
	got := f.lastPeerMsg(t, id)
	if strings.Contains(got.Body, autoReplyDoneSentinel) {
		t.Fatalf("delivered reply still contains the marker: %q", got.Body)
	}
	if !strings.Contains(got.Body, "here is your function") {
		t.Fatalf("stripped away the actual reply text: %q", got.Body)
	}
	// The provider must have proposed ending the task.
	if err := f.req.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	ts, err := f.req.Threads()
	if err != nil {
		t.Fatal(err)
	}
	ended := false
	for _, th := range ts {
		if th.InteractionID == id && th.State == string(interactions.StateCompleted) {
			ended = true
		}
	}
	if !ended {
		t.Fatal("expected the provider to complete the task after the task-done marker")
	}
}


// outboxReplier is a test backend that drops a file in rc.Outbox (like the exec agent delivering an
// artifact) and returns its reply text — exercising the daemon's outbox → attachment path.
type outboxReplier struct {
	text string
	file string
	data []byte
}

func (r *outboxReplier) Reply(_ context.Context, rc replyContext, _ []chatTurn) (string, error) {
	if rc.Outbox != "" && r.file != "" {
		_ = os.WriteFile(filepath.Join(rc.Outbox, r.file), r.data, 0o600)
	}
	return r.text, nil
}

// TestAutoReplyOutboxAttachment verifies a backend delivers files by writing them to $ANET_OUTBOX: the
// daemon sends them WITH the reply text as one message (no self-call, no duplicate).
func TestAutoReplyOutboxAttachment(t *testing.T) {
	api := &fakeOpenAI{reply: "unused"}
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api)
	f.replier = &outboxReplier{text: "here is your file", file: "game.html", data: []byte("<html>hi</html>")}

	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "make a file", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)

	got := f.lastPeerMsg(t, id)
	if got.Body != "here is your file" {
		t.Fatalf("body = %q, want the reply text", got.Body)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Name != "game.html" {
		t.Fatalf("want exactly 1 attachment game.html, got %+v", got.Attachments)
	}
}

// TestNewAutoReplierValidation ensures a misconfigured block fails loudly instead of silently not replying.
func TestNewAutoReplierValidation(t *testing.T) {
	layout := NewLayout(t.TempDir())
	if _, err := newAutoReplier(AutoReplyConfig{}, testEnv(layout)); err == nil {
		t.Fatal("want error for missing api_base/model")
	}
	if _, err := newAutoReplier(AutoReplyConfig{Backend: "quantum"}, testEnv(layout)); err == nil {
		t.Fatal("want error for unknown backend")
	}
	if _, err := newAutoReplier(AutoReplyConfig{Backend: "exec"}, testEnv(layout)); err == nil {
		t.Fatal("want error for exec without agent")
	}
}

// execStub installs a stub agent that answers with a fixed line and counts
// its runs in a file.
func execStub(t *testing.T) (runs func() int) {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "runs")
	stub := filepath.Join(dir, "stub.sh")
	script := "#!/bin/sh\necho run >> " + count + "\necho EXEC-OK\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setExecCommand(t, stub)
	return func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "run")
	}
}



// sleepyReplier answers after a while, as an exec agent doing real work
// does.
type sleepyReplier struct{ after time.Duration }

func (r sleepyReplier) Reply(ctx context.Context, _ replyContext, _ []chatTurn) (string, error) {
	select {
	case <-time.After(r.after):
		return "done, after a while", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// A first turn the auto-reply backend is slow to answer is announced to the
// requester as working before the reply (announceSlowTurn), so that a turn
// longer than the requester's no_response_after is not failed there as no
// answer — its reply, a text message, would not be kept on a failed task.
// A quick turn costs no extra message.
func TestASlowAutoReplyTurnIsAnnouncedAsWorking(t *testing.T) {
	old := slowTurnAfter
	slowTurnAfter = 50 * time.Millisecond
	t.Cleanup(func() { slowTurnAfter = old })
	fromProvider := func(d *Daemon, id, prov string) []string {
		t.Helper()
		msgs, err := d.ix.Messages(id)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for _, m := range msgs {
			if m.SenderAID == prov {
				kinds = append(kinds, m.Kind)
			}
		}
		return kinds
	}

	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, &fakeOpenAI{reply: "quick"})
	f.replier = sleepyReplier{after: 400 * time.Millisecond}
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "a long piece of work", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if got := f.lastPeerMsg(t, id); got.Body != "done, after a while" {
		t.Fatalf("reply %q", got.Body)
	}
	if kinds := fromProvider(f.req, id, f.prov.AID()); len(kinds) != 2 || kinds[0] != interactions.MsgStatus || kinds[1] != interactions.MsgText {
		t.Fatalf("the requester got %v from the provider, want a status, then the reply", kinds)
	}

	quick := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, &fakeOpenAI{reply: "quick"})
	id2, err := quick.req.Delegate(quick.ctx, quick.prov.AID(), "a small question", nil)
	if err != nil {
		t.Fatal(err)
	}
	quick.tick(t)
	time.Sleep(150 * time.Millisecond)
	if got := quick.lastPeerMsg(t, id2); got.Body != "quick" {
		t.Fatalf("reply %q", got.Body)
	}
	if kinds := fromProvider(quick.req, id2, quick.prov.AID()); len(kinds) != 1 || kinds[0] != interactions.MsgText {
		t.Fatalf("a quick turn: the requester got %v from the provider, want the reply alone", kinds)
	}
}

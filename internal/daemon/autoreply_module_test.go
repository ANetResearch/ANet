package daemon

// autoreply_module_test.go: the kernel's tests that need the auto-reply module running on a real node. The
// module is the one registered by the import in autoreply_alias_test.go; it is configured through the
// control route (SetAutoReply), runs its own loop, and is woken by the inbound message as in a deployment.
// What the module does with a turn (the budget, the gate, the sandbox, the streams, the refusals) is
// asserted by module/autoreply on its own stand-in node; these tests assert what the node does with the
// module: which tasks it is given, what reaches the peer, how the configuration is kept, and what a node
// without the module does.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/anethome"
	"github.com/ANetResearch/ANet/internal/execwork"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/seal"
)

// fakeOpenAI is a minimal OpenAI-compatible /chat/completions endpoint that records the last request
// body and returns a fixed reply. calls counts invocations.
type fakeOpenAI struct {
	calls    atomic.Int64
	lastBody atomic.Value // string
	reply    string
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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": f.reply}}},
		})
	})
}

// arFixture is a hub, a requester and a provider whose auto-reply module answers through a fake model
// service. The relay loops are stopped: the test pulls the mailboxes itself, and the module's loop is woken
// by the inbound message, as in a deployment.
type arFixture struct {
	ctx  context.Context
	req  *Daemon
	prov *Daemon
	api  *fakeOpenAI
	cfg  AutoReplyConfig
}

// newARFixtureIdle builds the nodes and the model service and leaves the module unconfigured: nothing
// answers until start.
func newARFixtureIdle(t *testing.T, cfg AutoReplyConfig, api *fakeOpenAI) *arFixture {
	t.Helper()
	hub := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, hub.URL, false)
	prov := newTestDaemon(t, hub.URL, true)
	if err := req.RegisterWithHub(ctx, hub.URL, "Alice", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, hub.URL, "Vision Bot", []string{"vision"}); err != nil {
		t.Fatal(err)
	}
	// Re-stop the relay loops the registration restarted; the test polls explicitly.
	for _, d := range []*Daemon{req, prov} {
		d.mu.Lock()
		if d.relayStop != nil {
			d.relayStop()
			d.relayStop = nil
		}
		d.mu.Unlock()
	}
	if api != nil {
		apiSrv := httptest.NewServer(api.handler())
		t.Cleanup(apiSrv.Close)
		cfg.APIBase = apiSrv.URL + "/v1"
	}
	return &arFixture{ctx: ctx, req: req, prov: prov, api: api, cfg: cfg}
}

// newARFixture is newARFixtureIdle with the module answering.
func newARFixture(t *testing.T, cfg AutoReplyConfig, api *fakeOpenAI) *arFixture {
	t.Helper()
	f := newARFixtureIdle(t, cfg, api)
	f.start(t)
	return f
}

// start puts the configuration in force: the module's loop starts.
func (f *arFixture) start(t *testing.T) {
	t.Helper()
	cfg := f.cfg
	if err := f.prov.SetAutoReply(&cfg); err != nil {
		t.Fatal(err)
	}
}

// deliver pulls the provider's mailbox; the module is woken by what lands.
func (f *arFixture) deliver(t *testing.T) {
	t.Helper()
	if err := f.prov.pollOnce(f.ctx); err != nil {
		t.Fatalf("provider poll: %v", err)
	}
	f.prov.kickTurns()
}

// settle gives the module's loop time to look at every owed turn, for an assertion that nothing was done.
func (f *arFixture) settle() {
	f.prov.kickTurns()
	time.Sleep(300 * time.Millisecond)
}

// peerTexts pulls the requester's mailbox and returns the text messages the provider sent in id, oldest first.
func (f *arFixture) peerTexts(t *testing.T, id string) []ThreadMsg {
	t.Helper()
	if err := f.req.pollOnce(f.ctx); err != nil {
		t.Fatalf("requester poll: %v", err)
	}
	ts, err := f.req.Threads()
	if err != nil {
		t.Fatal(err)
	}
	var out []ThreadMsg
	for _, th := range ts {
		if th.InteractionID != id {
			continue
		}
		for _, m := range th.Messages {
			if m.From == "them" && m.Kind == "text" {
				out = append(out, m)
			}
		}
	}
	return out
}

// waitPeerText waits until the provider has sent n text messages in id and returns the last.
func (f *arFixture) waitPeerText(t *testing.T, id string, n int) ThreadMsg {
	t.Helper()
	var got []ThreadMsg
	waitUntil(t, "the provider's reply", func() bool {
		got = f.peerTexts(t, id)
		return len(got) >= n
	})
	return got[len(got)-1]
}

// A delegation with an image is answered through the OpenAI-compatible backend (the image inlined as a
// base64 data URL, the conversation mapped to chat messages), a second scan does not answer again, a
// follow-up keeps the context, and the peer's end proposal is accepted so that the receipt is issued.
func TestAutoReplyVisionRoundTrip(t *testing.T) {
	api := &fakeOpenAI{reply: "a yellow circle on blue"}
	f := newARFixture(t, AutoReplyConfig{Model: "test-vl", SystemPrompt: "you are a vision service"}, api)

	img := filepath.Join(t.TempDir(), "pic.png")
	if err := os.WriteFile(img, []byte("\x89PNG\r\n\x1a\n-not-really-a-png"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "describe this image", []string{img})
	if err != nil {
		t.Fatal(err)
	}
	f.deliver(t)
	if got := f.waitPeerText(t, id, 1); got.Body != "a yellow circle on blue" {
		t.Fatalf("reply = %q, want the backend answer", got.Body)
	}
	body, _ := api.lastBody.Load().(string)
	for _, want := range []string{"you are a vision service", "describe this image", "data:image/png;base64,", "image_url"} {
		if !strings.Contains(body, want) {
			t.Fatalf("backend request missing %q:\n%s", want, body)
		}
	}
	f.settle() // the last word is ours: a second scan does not reply again
	if got := api.calls.Load(); got != 1 {
		t.Fatalf("backend called %d times, want 1 (no double reply)", got)
	}

	if err := f.req.SendMessage(f.ctx, id, "what text is in it?", nil); err != nil {
		t.Fatal(err)
	}
	f.deliver(t)
	f.waitPeerText(t, id, 2)
	if got := api.calls.Load(); got != 2 {
		t.Fatalf("backend called %d times after the follow-up, want 2", got)
	}
	body, _ = api.lastBody.Load().(string)
	for _, want := range []string{"what text is in it?", "a yellow circle on blue", "data:image/png;base64,"} {
		if !strings.Contains(body, want) {
			t.Fatalf("follow-up request missing %q", want)
		}
	}

	if err := f.req.RequestEnd(f.ctx, id); err != nil {
		t.Fatal(err)
	}
	f.deliver(t)
	waitUntil(t, "the receipt", func() bool {
		if err := f.req.pollOnce(f.ctx); err != nil {
			t.Fatal(err)
		}
		results, err := f.req.Results(f.ctx)
		return err == nil && len(results) == 1 && results[0].InteractionID == id
	})
}

// The module answers the tasks this node delegated too: when the peer replies, the node answers.
func TestAutoReplyAnswersAnOutboundTask(t *testing.T) {
	api := &fakeOpenAI{reply: "sure, here is more detail"}
	f := newARFixture(t, AutoReplyConfig{Model: "test"}, api)
	// The peer must accept the delegation the provider is about to send it (the requester allows no one).
	allowPeers(t, f.req, f.prov.AID())
	id, err := f.prov.Delegate(f.ctx, f.req.AID(), "can you look into X?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.req.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.req.SendMessage(f.ctx, id, "yes, what exactly do you need?", nil); err != nil {
		t.Fatal(err)
	}
	f.deliver(t)
	// The provider's own delegation is the first text it sent; the reply is the second.
	if got := f.waitPeerText(t, id, 2); got.Body != "sure, here is more detail" {
		t.Fatalf("outbound auto-reply = %q, want the backend answer", got.Body)
	}
	if got := api.calls.Load(); got != 1 {
		t.Fatalf("backend called %d times on an outbound thread, want 1", got)
	}
}

// An interaction that has ended gets no auto-reply turn, even though the peer had the last word:
// ActiveThreads leaves ended interactions out and the scan skips them again. Either filter alone keeps the
// backend from being asked, so this fails only when both are gone (supp c35-5). The module is switched on
// after the cancel arrived, and a task that has not ended, sent the same way, is answered: the test asks for
// the absence of a turn where there would otherwise be one.
func TestAutoReplyLeavesEndedTasksAlone(t *testing.T) {
	api := &fakeOpenAI{reply: "an answer"}
	f := newARFixtureIdle(t, AutoReplyConfig{Model: "m"}, api)

	ended, err := f.req.Delegate(f.ctx, f.prov.AID(), "a task to cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.prov.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.req.CancelTask(f.ctx, ended); err != nil {
		t.Fatal(err)
	}
	if err := f.prov.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, f.prov, ended); st != interactions.StateCanceled {
		t.Fatalf("provider state after the cancel arrived = %s", st)
	}
	f.start(t)
	f.settle()
	if n := api.calls.Load(); n != 0 {
		t.Fatalf("the backend was asked %d times about a task that had ended", n)
	}

	live, err := f.req.Delegate(f.ctx, f.prov.AID(), "a task to answer", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.deliver(t)
	f.waitPeerText(t, live, 1)
	f.settle()
	if n := api.calls.Load(); n != 1 {
		t.Fatalf("the backend was asked %d times in all, want 1: a task that had ended got a turn", n)
	}
}

// An auto-reply that judges the task done completes it with its reply in the result (one write with the
// completion); a reply that does not complete the task is an ordinary message.
func TestAnAutoReplyThatCompletesFoldsItsReplyIntoTheResult(t *testing.T) {
	api := &fakeOpenAI{reply: "here is your function: def f(): pass\n" + "<<ANET_TASK_DONE>>"}
	f := newARFixture(t, AutoReplyConfig{Model: "test"}, api)
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "write f()", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.deliver(t)
	waitUntil(t, "the task to complete", func() bool { return mustIX(t, f.prov, id).State == interactions.StateCompleted })
	rows, err := f.prov.ix.OutboxRows(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Type == seal.TypeMessage {
			t.Fatalf("the done reply went as a message of its own: %+v", r)
		}
	}
	waitUntil(t, "the requester to see the completion", func() bool {
		if err := f.req.pollOnce(f.ctx); err != nil {
			t.Fatal(err)
		}
		return mustIX(t, f.req, id).State == interactions.StateCompleted
	})
	task, err := f.req.taskView(mustIX(t, f.req, id), viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != a2ashape.TaskStateCompleted || replyArtifact(task) != "here is your function: def f(): pass" {
		t.Fatalf("state %s, reply %q", task.Status.State, replyArtifact(task))
	}
	noHistoryState(t, task)

	api2 := &fakeOpenAI{reply: "which language?"}
	g := newARFixture(t, AutoReplyConfig{Model: "test"}, api2)
	id2, err := g.req.Delegate(g.ctx, g.prov.AID(), "write f()", nil)
	if err != nil {
		t.Fatal(err)
	}
	g.deliver(t)
	waitUntil(t, "the question", func() bool { return msgWithBody(t, g.prov, id2, "which language?") != nil })
	if ix := mustIX(t, g.prov, id2); ix.IsTerminal() {
		t.Fatalf("a question completed the task: %s", ix.State)
	}
	if m := msgWithBody(t, g.prov, id2, "which language?"); m == nil || m.Metadata != "" {
		t.Fatalf("the question: %+v", m)
	}
}

// A2A-DESIGN section 11.6 and an internal note item, on the daemon's side: only a trusted peer's text task
// reaches a module's backend. A peer on the allow list alone, and a stranger under policy open, do not. A
// task the backend has is not also given to the auto-reply agent, while the others still are. The backend
// sees one context per (peer, context), so two peers naming the same context never share a conversation.
// A task comes again with the requester's next message, in the same context.
func TestInboundTasksReachTheBackendOnlyFromTrustedPeers(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	peerA := registered(t, srv.URL, "a")
	peerB := registered(t, srv.URL, "b")
	allowed := registered(t, srv.URL, "allowed")
	stranger := registered(t, srv.URL, "stranger")
	trustPeers(t, prov, peerA.AID(), peerB.AID())
	allowPeers(t, prov, allowed.AID())
	if err := prov.SetInboundPolicy(PolicyOpen); err != nil {
		t.Fatal(err)
	}
	api := &fakeOpenAI{reply: "an auto-reply"}
	apiSrv := httptest.NewServer(api.handler())
	defer apiSrv.Close()
	arCfg := AutoReplyConfig{Model: "m", APIBase: apiSrv.URL + "/v1"}
	if err := prov.SetAutoReply(&arCfg); err != nil {
		t.Fatal(err)
	}

	fctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	host := a2aHost(prov)
	tasks, err := host.InboundTasks(fctx)
	if err != nil {
		t.Fatal(err)
	}

	delegate := func(from *Daemon, goal, contextID string) string {
		t.Helper()
		id, err := from.DelegateIn(ctx, prov.AID(), goal, nil, contextID)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a1 := delegate(peerA, "a first", "shared")
	a2 := delegate(peerA, "ask a second", "shared")
	b1 := delegate(peerB, "b first", "shared")
	al := delegate(allowed, "from an allowed peer", "shared")
	st := delegate(stranger, "from a stranger", "shared")
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a1, a2, b1, al, st} {
		if _, err := prov.ix.Get(id); err != nil {
			t.Fatalf("%s was not accepted: %v", id, err)
		}
	}

	// The auto-reply agent answers the two tasks no backend is given, and leaves the three a backend is,
	// although nothing has answered them yet.
	waitUntil(t, "the two tasks no backend has to be answered", func() bool {
		return lastFromMe(t, prov, al) == "an auto-reply" && lastFromMe(t, prov, st) == "an auto-reply"
	})
	prov.kickTurns()
	time.Sleep(300 * time.Millisecond)
	if n := api.calls.Load(); n != 2 {
		t.Fatalf("auto-reply called %d times, want 2 (allowed peer, stranger)", n)
	}
	for _, id := range []string{a1, a2, b1} {
		if got := lastFromMe(t, prov, id); got != "" {
			t.Fatalf("auto-reply answered %s, a task the backend has: %q", id, got)
		}
	}

	backend := newFakeA2ABackend(t)
	errs := forwardInbound(t, fctx, host, tasks, backend)
	waitUntil(t, "the three trusted tasks to be answered", func() bool {
		return stateOf(t, prov, a1) == interactions.StateCompleted && stateOf(t, prov, b1) == interactions.StateCompleted &&
			stateOf(t, prov, a2) == interactions.StateInputRequired
	})
	if len(*errs) != 0 {
		t.Fatalf("forwarding: %v", *errs)
	}
	if got := lastFromMe(t, prov, a1); got != "backend: a first" {
		t.Fatalf("a1 answered %q", got)
	}
	calls := backend.snapshot()
	if len(calls) != 3 {
		t.Fatalf("the backend received %d tasks: %+v", len(calls), calls)
	}
	for _, c := range calls {
		if strings.Contains(c.text, "allowed") || strings.Contains(c.text, "stranger") {
			t.Fatalf("an untrusted peer's task reached the backend: %q", c.text)
		}
		if c.meta[a2ashape.KeyTrusted] != true {
			t.Fatalf("%q: anet.trusted = %v", c.text, c.meta[a2ashape.KeyTrusted])
		}
	}
	ca1, ca2, cb1 := backend.byText(t, "a first"), backend.byText(t, "ask a second"), backend.byText(t, "b first")
	if ca1.meta[a2ashape.KeyPeerAID] != peerA.AID() || cb1.meta[a2ashape.KeyPeerAID] != peerB.AID() {
		t.Fatalf("anet.peer_aid: %v, %v", ca1.meta[a2ashape.KeyPeerAID], cb1.meta[a2ashape.KeyPeerAID])
	}
	if ca1.contextID != ca2.contextID || ca1.contextID == cb1.contextID || ca1.contextID == "shared" || cb1.contextID == "shared" {
		t.Fatalf("backend contexts: a %s, %s; b %s — want one per peer, not the requester's", ca1.contextID, ca2.contextID, cb1.contextID)
	}

	// The requester reads the question and answers it: the task comes again, in the same context, and the
	// auto-reply agent still leaves it alone.
	if err := peerA.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := peerA.SendMessage(ctx, a2, "ask no more: the red one", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the follow-up to reach the backend", func() bool { return len(backend.snapshot()) == 4 })
	if n := api.calls.Load(); n != 2 {
		t.Fatalf("auto-reply called for a follow-up the backend has (%d calls)", n)
	}
	if c := backend.byText(t, "ask no more: the red one"); c.contextID != ca1.contextID {
		t.Fatalf("the follow-up came in context %s, not %s", c.contextID, ca1.contextID)
	}
	waitUntil(t, "the follow-up to be answered", func() bool { return lastFromMe(t, prov, a2) == "backend: ask no more: the red one" })

	// Only the tasks a module was given can be answered through the seam.
	answer := a2ashape.Message{Role: a2ashape.RoleAgent, Parts: []a2ashape.Part{a2ashape.TextPart("no")}}
	for _, id := range []string{al, st, "ix_nope"} {
		if _, err := host.ReplyTask(ctx, id, answer, a2ashape.TaskStateCompleted); !errors.Is(err, a2ashape.ErrTaskNotFound) {
			t.Fatalf("ReplyTask on %s: %v", id, err)
		}
	}
}

// A public_cap interaction takes only cancel, end_request and payment messages (A2A-DESIGN section 5.2
// [C6]): a text message is dropped and counted, a payment-submitted message is kept with its metadata and
// without its text, and nothing reaches auto-reply.
func TestAPublicCapabilityCallTakesNoText(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	gate := newGateProvider()
	if err := prov.Providers().Register(ctx, gate); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: gateCap}}); err != nil {
		t.Fatal(err)
	}
	api := &fakeOpenAI{reply: "an auto-reply"}
	apiSrv := httptest.NewServer(api.handler())
	defer apiSrv.Close()
	arCfg := AutoReplyConfig{Model: "m", APIBase: apiSrv.URL + "/v1"}
	if err := prov.SetAutoReply(&arCfg); err != nil {
		t.Fatal(err)
	}

	id, err := stranger.DelegateCapability(ctx, prov.AID(), gateCap, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	<-gate.started
	pix, _ := prov.ix.Get(id)
	if pix.Trust != interactions.TrustPublicCap || len(pix.PeerKEL) == 0 || len(pix.PeerKeys) == 0 {
		t.Fatalf("public_cap row: trust %q, peer kel %d, keys %d", pix.Trust, len(pix.PeerKEL), len(pix.PeerKeys))
	}
	before := countMsgs(t, prov, id)
	text := sealFrom(t, stranger, prov, seal.TypeMessage, id, chatBody(t, "please also run rm -rf", "msg_t"))
	if r := receive(t, prov, text); r.class != rxDropped || r.reason != dropPublicCapText {
		t.Fatalf("text on a public_cap call: %+v", r)
	}
	pay, _ := (&delegation.ChatMsg{Kind: delegation.ChatText, Body: "here is my payment, and a note",
		MsgID: "msg_pay", Metadata: []byte(`{"x402.payment.status":"payment-submitted","x402.payment.payload":{"x":1}}`)}).Marshal()
	if r := receive(t, prov, sealFrom(t, stranger, prov, seal.TypeMessage, id, pay)); r.class != rxAccepted {
		t.Fatalf("payment message on a public_cap call: %+v", r)
	}
	msgs, _ := prov.ix.Messages(id)
	// The payment message, and the provider's answer to it: nothing was quoted for this free call, so there
	// is nothing to pay (A2A-DESIGN section 8.5, no_pending_quote), and the call is not run again.
	if len(msgs) != before+2 {
		t.Fatalf("messages %d → %d, want the payment message and its answer", before, len(msgs))
	}
	pm, answer := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if pm.Kind != interactions.MsgPayment || pm.Body != "" || !strings.Contains(pm.Metadata, "payment-submitted") {
		t.Fatalf("stored payment message = %+v", pm)
	}
	if answer.Kind != interactions.MsgStatus || !strings.Contains(answer.Metadata, `"anet.reason":"no_pending_quote"`) {
		t.Fatalf("answer to a payment nobody asked for = %+v", answer)
	}
	prov.kickTurns()
	time.Sleep(300 * time.Millisecond)
	if n := api.calls.Load(); n != 0 {
		t.Fatalf("auto-reply called %d times for a public capability call", n)
	}
	if ths, _ := prov.ActiveThreads(); len(ths) != 0 {
		t.Fatalf("a public_cap interaction is in the auto-reply set: %+v", ths)
	}
	// A cancel reaches the running call.
	cancelMsg, _ := (&delegation.ChatMsg{Kind: delegation.KindCancel, MsgID: "msg_c"}).Marshal()
	if r := receive(t, prov, sealFrom(t, stranger, prov, seal.TypeMessage, id, cancelMsg)); r.class != rxAccepted {
		t.Fatalf("cancel: %+v", r)
	}
	waitUntil(t, "the running call to be canceled", gate.wasCanceled)
	waitUntil(t, "the public_cap interaction to end", func() bool {
		pix, _ := prov.ix.Get(id)
		return pix.IsTerminal()
	})
	// Its answers to the caller are queued: the caller's keys are kept, as they may have to be sealed again
	// (engine ENG-S7).
	if pix, _ := prov.ix.Get(id); len(outboxRows(t, prov, id)) == 0 || len(pix.PeerKEL) == 0 || len(pix.PeerKeys) == 0 {
		t.Fatalf("ended with its answers queued, the caller's keys: kel %d, keys %d", len(pix.PeerKEL), len(pix.PeerKeys))
	}
	// The caller takes them, and the provider hears so: the keys go with the last row.
	waitUntil(t, "the caller's answers to be confirmed and its keys dropped", func() bool {
		settleDelivery(t, prov, stranger)
		pix, _ := prov.ix.Get(id)
		return len(outboxRows(t, prov, id)) == 0 && len(pix.PeerKEL) == 0 && len(pix.PeerKeys) == 0
	})
	if _, err := prov.ix.PeerIdentity(stranger.AID()); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("a public_cap caller was recorded in peer_identity")
	}
}

// Section 17 C6/C29: both sides run the exec auto-reply backend and trust each other; a quoted, paid,
// completed capability call never reaches it. A plain task on the same nodes does (the control).
func TestExecAutoReplyOnBothSidesStaysOutOfAPaidCall(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "agent"), []byte("#!/bin/sh\necho called >> "+calls+"\necho a reply\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work := &meteredWork{price: 30}
	_, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	trustPeers(t, prov, req.AID())
	trustPeers(t, req, prov.AID())
	cfg := AutoReplyConfig{Backend: "exec", Agent: "cursor"}
	for _, d := range []*Daemon{req, prov} {
		c := cfg
		if err := d.SetAutoReply(&c); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the paid call to complete", func() bool {
		poll(t, prov, req)
		return getIX(t, req, id).State == interactions.StateCompleted
	})
	req.kickTurns()
	prov.kickTurns()
	time.Sleep(300 * time.Millisecond)
	if work.invoked.Load() != 1 {
		t.Fatalf("the paid call ran %d times", work.invoked.Load())
	}
	if b, err := os.ReadFile(calls); err == nil && len(b) > 0 {
		t.Fatalf("the exec backend ran %d times during a paid call", strings.Count(string(b), "\n"))
	}
	// The control: the stub is the agent on these nodes, and a plain task from a trusted peer reaches it.
	plain, err := req.Delegate(ctx, prov.AID(), "a plain task", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the plain task to be answered", func() bool {
		poll(t, prov, req)
		return lastFromMe(t, prov, plain) == "a reply"
	})
	if b, err := os.ReadFile(calls); err != nil || len(b) == 0 {
		t.Fatalf("the exec backend did not run for a plain task: %v", err)
	}
}

// MOD-3 (2), TestConfigRewritePreservesModuleBlocks: a write the kernel makes of config.json (a policy
// write) carries the auto_reply block and `without` byte for byte, and so does the module's own write the
// other keys.
func TestConfigRewritePreservesModuleBlocks(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "without": []string{"x402"},
		"auto_reply": map[string]any{"backend": "openai", "api_base": "http://127.0.0.1:1/v1", "model": "m", "api_key": "k"}})
	d, err := New(NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	read := func() map[string]json.RawMessage {
		b, err := os.ReadFile(filepath.Join(root, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	before := read()
	if err := d.SetInboundPolicy(PolicyApprove); err != nil {
		t.Fatal(err)
	}
	after := read()
	for _, k := range []string{"auto_reply", "without"} {
		if !bytes.Equal(before[k], after[k]) {
			t.Errorf("%s changed under a policy write:\n%s\n%s", k, before[k], after[k])
		}
	}
	// The module's write replaces only auto_reply.
	if code, out, err := d.autoReplyRoute("POST /autoreply", []byte(`{"model":"m2"}`)); err != nil || code != http.StatusOK {
		t.Fatalf("the module's write: %d %v %v", code, out, err)
	}
	final := read()
	if !bytes.Equal(after["without"], final["without"]) || !bytes.Equal(after["inbound"], final["inbound"]) {
		t.Errorf("the module's write changed another key")
	}
	if !strings.Contains(string(final["auto_reply"]), `"m2"`) {
		t.Errorf("auto_reply = %s", final["auto_reply"])
	}
}

// MOD section 7.1, TestOnlyAutoReplyAssertsTurnHost: no file of the kernel asserts a Host to
// module.TurnHost; the module does, and the grants are what hands the interface to it alone.
func TestOnlyAutoReplyAssertsTurnHost(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ta, ok := n.(*ast.TypeAssertExpr)
			if !ok || ta.Type == nil {
				return true
			}
			if se, ok := ta.Type.(*ast.SelectorExpr); ok && se.Sel.Name == "TurnHost" {
				t.Errorf("%s asserts module.TurnHost; only the auto-reply module may", name)
			}
			return true
		})
	}
}

// MOD-3 (3), TestRetentionPrunesExecDirsInEveryBuild: with auto-reply turned off the work directories of
// ended tasks and the session bindings are still removed by the maintenance period.
func TestRetentionPrunesExecDirsInEveryBuild(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	d := startWith(t, map[string]any{"without": []string{"autoreply"}}, nil)
	if arModule(d) != nil {
		t.Fatal("the auto-reply module runs although it is in without")
	}
	aid := d.AID()
	direct := filepath.Join(execwork.WorkRoot(), execwork.AidShort(aid), execwork.IxDirName("ix-gone"))
	sandboxed := filepath.Join(execwork.SandboxWorkRoot(), execwork.AidShort(aid), execwork.IxDirName("ix-gone"))
	for _, p := range []string{direct, sandboxed} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sess := execwork.Sessions(anethome.ModuleDir(d.layout.Root, "autoreply"))
	sess.Set("ix-gone", "s")
	d.pruneEndedWorkDirs()
	for _, p := range []string{direct, sandboxed} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s was not removed (%v)", p, err)
		}
	}
	if sess.Get("ix-gone") != "" {
		t.Error("the session binding of an ended task stayed")
	}
	if _, err := os.Stat(filepath.Join(d.layout.Root, "exec_sessions.json")); err == nil {
		t.Error("the old session file exists")
	}
}

// MOD-3 (4): without the module nothing answers: its routes say module_absent, /status has no auto_reply
// member, no loop runs, and the task stays in the inbox under each inbound policy.
func TestWithoutAutoReplyIsAbsent(t *testing.T) {
	d := startWith(t, map[string]any{"without": []string{"autoreply"}}, nil)
	if arModule(d) != nil {
		t.Fatal("module started")
	}
	for _, route := range []string{"POST /autoreply", "POST /autoreply-test"} {
		rec := httptest.NewRecorder()
		h := d.moduleRouteGuard(route, d.moduleRoute("autoreply", route))
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader(`{"off":true}`)))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 409 || body["reason"] != "module_absent" && !strings.Contains(rec.Body.String(), "module_absent") {
			t.Errorf("%s: %d %s", route, rec.Code, rec.Body)
		}
	}
	st := statusJSON(t, d)
	if _, ok := st["auto_reply"]; ok {
		t.Error("/status has an auto_reply member")
	}
	if got, _ := st["build"].(map[string]any)["without"].([]any); len(got) != 1 || got[0] != "autoreply" {
		t.Errorf("/status.build.without = %v", got)
	}
	// A block with the module turned off is the module_absent.configured refusal at start.
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "without": []string{"autoreply"},
		"auto_reply": map[string]any{"backend": "openai", "api_base": "http://127.0.0.1:1/v1", "model": "m"}})
	_, err := New(NewLayout(root))
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) || ref.Key != "module_absent.configured" || ref.Args["key"] != "auto_reply" {
		t.Errorf("New = %v, want module_absent.configured for auto_reply", err)
	}
}

// MOD-3 (4), TestNoAutoReplyLeavesTasksInInbox: with the module turned off a delegated task is kept and
// nothing is sent, whatever the inbound policy, and no local program is started (a canary on PATH).
func TestNoAutoReplyLeavesTasksInInbox(t *testing.T) {
	canary := filepath.Join(t.TempDir(), "claude")
	mark := filepath.Join(t.TempDir(), "ran")
	if err := os.WriteFile(canary, []byte("#!/bin/sh\ntouch "+mark+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(canary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, policy := range []string{PolicyOpen, PolicyApprove, PolicyClosed} {
		srv := newFakeHub(t)
		ctx := context.Background()
		prov := registered(t, srv.URL, "prov")
		req := registered(t, srv.URL, "req")
		allowPeers(t, prov, req.AID())
		trustPeers(t, prov, req.AID())
		if err := prov.SetInboundPolicy(policy); err != nil {
			t.Fatal(err)
		}
		prov = restartWithout(t, prov, "autoreply")
		if arModule(prov) != nil {
			t.Fatal("the module started although it is in without")
		}
		id, err := req.Delegate(ctx, prov.AID(), "please run", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = prov.pollOnce(ctx)
		prov.kickTurns()
		time.Sleep(100 * time.Millisecond)
		if got := lastFromMe(t, prov, id); got != "" {
			t.Errorf("policy %s: the node answered %q", policy, got)
		}
		if st := stateOf(t, prov, id); policy != PolicyClosed && st != interactions.StateSubmitted {
			t.Errorf("policy %s: state %s", policy, st)
		}
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("a local program was started")
	}
}

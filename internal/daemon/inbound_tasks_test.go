package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// fakeA2ABackend is an A2A server built from a2a-go's own server stack. It
// answers "backend: <text>" and records what each call carried.
type fakeA2ABackend struct {
	srv   *httptest.Server
	card  *a2a.AgentCard
	mu    sync.Mutex
	calls []backendCall
}

type backendCall struct {
	text, contextID string
	meta            map[string]any
}

func newFakeA2ABackend(t *testing.T) *fakeA2ABackend {
	t.Helper()
	b := &fakeA2ABackend{}
	exec := a2asrv.AgentExecutorFunc(func(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			text := ""
			if len(ec.Message.Parts) > 0 {
				text = ec.Message.Parts[0].Text()
			}
			b.mu.Lock()
			b.calls = append(b.calls, backendCall{text: text, contextID: string(ec.ContextID), meta: ec.Message.Metadata})
			b.mu.Unlock()
			yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("backend: "+text)), nil)
		}
	})
	mux := http.NewServeMux()
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	b.card = &a2a.AgentCard{Name: "backend", Description: "d", Version: "1",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(b.srv.URL+"/rpc", a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{{ID: "chat", Name: "chat", Description: "d", Tags: []string{"chat"}}}}
	mux.Handle("/rpc", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(exec)))
	return b
}

func (b *fakeA2ABackend) snapshot() []backendCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]backendCall(nil), b.calls...)
}

func (b *fakeA2ABackend) byText(t *testing.T, text string) backendCall {
	t.Helper()
	for _, c := range b.snapshot() {
		if c.text == text {
			return c
		}
	}
	t.Fatalf("the backend never received %q", text)
	return backendCall{}
}

// forwardInbound stands in for module/a2a's forwarder, which this package's
// tests cannot link (its init would start the local A2A interface in every
// test daemon): each delivered task's latest message goes to the backend in
// the task's context, and the backend's answer comes back through
// ReplyTask — a question for a message that starts with "ask", a final
// answer otherwise. Errors are collected for the test to read.
func forwardInbound(t testing.TB, ctx context.Context, h module.InboundTaskHost, tasks <-chan module.Task, b *fakeA2ABackend) *[]error {
	// The forwarder stops, and is waited for, before the daemons it
	// replies through close: a reply still on its way would use a store
	// being closed.
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
	})
	var mu sync.Mutex
	errs := &[]error{}
	fail := func(err error) {
		mu.Lock()
		*errs = append(*errs, err)
		mu.Unlock()
	}
	cl, err := a2aclient.NewFromCard(ctx, b.card, a2aclient.WithJSONRPCTransport(http.DefaultClient))
	if err != nil {
		fail(err)
		close(done)
		return errs
	}
	go func() {
		defer close(done)
		for {
			var task module.Task
			select {
			case <-ctx.Done():
				return
			case tk, ok := <-tasks:
				if !ok {
					return
				}
				task = tk
			}
			last := task.History[len(task.History)-1]
			text := last.Parts[0].Text
			msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
			msg.ContextID = task.ContextID
			msg.Metadata = map[string]any{a2ashape.KeyPeerAID: task.Metadata[a2ashape.KeyPeerAID],
				a2ashape.KeyTrusted: task.Metadata[a2ashape.KeyTrusted]}
			res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg})
			if err != nil {
				fail(err)
				continue
			}
			rm, ok := res.(*a2a.Message)
			if !ok || len(rm.Parts) == 0 {
				fail(errors.New("the backend answered with no message"))
				continue
			}
			state := a2ashape.TaskStateCompleted
			if strings.HasPrefix(text, "ask") {
				state = a2ashape.TaskStateInputRequired
			}
			answer := a2ashape.Message{Role: a2ashape.RoleAgent, Parts: []a2ashape.Part{a2ashape.TextPart(rm.Parts[0].Text())}}
			if _, err := h.ReplyTask(ctx, task.ID, answer, state); err != nil && ctx.Err() == nil {
				fail(err)
			}
		}
	}()
	return errs
}

// lastFromMe is the body of the provider's latest text message on id.
func lastFromMe(t *testing.T, d *Daemon, id string) string {
	t.Helper()
	msgs, err := d.ix.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].SenderAID == d.AID() && msgs[i].Kind == interactions.MsgText {
			return msgs[i].Body
		}
	}
	return ""
}

// A backend that accepts untrusted peers (declared through
// DeclareUntrustedBackend): an allowed peer's task is delivered, marked
// untrusted, and can be answered, while a stranger's task accepted under an
// earlier policy open is not; policy open is refused in either order —
// written after the declaration, or configured when the daemon starts with a
// module that declares one.
func TestInboundTasksWithAnUntrustedBackend(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	allowed := registered(t, srv.URL, "allowed")
	stranger := registered(t, srv.URL, "stranger")
	allowPeers(t, prov, allowed.AID())
	// A stranger's task from a time the node ran policy open.
	if err := prov.SetInboundPolicy(PolicyOpen); err != nil {
		t.Fatal(err)
	}
	old, err := stranger.Delegate(ctx, prov.AID(), "from the open days", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetInboundPolicy(PolicyApprove); err != nil {
		t.Fatal(err)
	}
	host := a2aHost(prov)
	host.DeclareUntrustedBackend()
	if err := prov.SetInboundPolicy(PolicyOpen); !errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("policy open with an untrusted backend declared: %v", err)
	}
	fctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	tasks, err := host.InboundTasks(fctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := allowed.Delegate(ctx, prov.AID(), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var task module.Task
	select {
	case task = <-tasks:
	case <-time.After(5 * time.Second):
		t.Fatal("the allowed peer's task was not delivered")
	}
	if task.ID != id || task.Metadata[a2ashape.KeyTrusted] != false || task.Metadata[a2ashape.KeyPeerAID] != allowed.AID() {
		t.Fatalf("delivered %s with %v", task.ID, task.Metadata)
	}
	answer := a2ashape.Message{Role: a2ashape.RoleAgent, Parts: []a2ashape.Part{a2ashape.TextPart("hi")}}
	if _, err := host.ReplyTask(ctx, old, answer, a2ashape.TaskStateCompleted); !errors.Is(err, a2ashape.ErrTaskNotFound) {
		t.Fatalf("ReplyTask on a stranger's task from policy open: %v", err)
	}
	if _, err := host.ReplyTask(ctx, id, answer, a2ashape.TaskStateCompleted); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, prov, id); st != interactions.StateCompleted {
		t.Fatalf("state after the answer: %s", st)
	}
	select {
	case x := <-tasks:
		t.Fatalf("delivered %s (%v) as well", x.ID, x.Metadata[a2ashape.KeyPeerAID])
	case <-time.After(200 * time.Millisecond):
	}

	// At start.
	root := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"control_addr": "127.0.0.1:0", "inbound": map[string]any{"policy": "open"},
		"modules": map[string]any{declaringModuleName: map[string]any{}}})
	if err := os.WriteFile(filepath.Join(root, "config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := New(NewLayout(root)); !errors.Is(err, ErrPolicyConflict) {
		if d != nil {
			d.Close()
		}
		t.Fatalf("start with policy open and a module declaring an untrusted backend: %v", err)
	}
}

// backendTakes is the one decision: a trusted peer's text task; an
// untrusted peer's only under the declaration and only when someone named
// the peer (allow list, approval) — never a stranger accepted under policy
// open or a row with no admission recorded; never a capability call, a
// public capability call or a denied peer.
func TestBackendTakes(t *testing.T) {
	ps := peerSets{allow: map[string]bool{"al": true}, trust: map[string]bool{"tr": true, "dn": true},
		deny: map[string]bool{"dn": true}}
	cases := []struct {
		name, peer, trust             string
		capability, declare, admitted bool
		trusted, ok                   bool
	}{
		{"trusted", "tr", interactions.TrustPeer, false, false, false, true, true},
		{"trusted now, accepted under open", "tr", interactions.TrustPublic, false, false, false, true, true},
		{"allowed", "al", interactions.TrustPeer, false, false, false, false, false},
		{"allowed, declared", "al", interactions.TrustPeer, false, true, false, false, true},
		{"approved, declared", "ap", interactions.TrustApproved, false, true, false, false, true},
		{"stranger under open, declared", "st", interactions.TrustPublic, false, true, false, false, false},
		{"no admission recorded, declared", "st", "", false, true, false, false, false},
		{"denied although trusted", "dn", interactions.TrustPeer, false, true, false, false, false},
		{"capability call", "tr", interactions.TrustPeer, true, true, false, false, false},
		{"public capability", "tr", interactions.TrustPublicCap, false, true, false, false, false},
		{"no peer", "", interactions.TrustPeer, false, true, false, false, false},
		// inbound.admit (design v0.3.9 §A): an admitted peer counts as named.
		{"admitted, declared", "st", interactions.TrustPublic, false, true, true, false, true},
		{"admitted, not declared", "st", interactions.TrustPeer, false, false, true, false, false},
		{"admitted but denied", "dn", interactions.TrustPeer, false, true, true, false, false},
	}
	for _, c := range cases {
		trusted, ok := backendTakes(c.peer, c.trust, c.capability, ps, c.declare, c.admitted)
		if trusted != c.trusted || ok != c.ok {
			t.Errorf("%s: trusted %v ok %v, want %v %v", c.name, trusted, ok, c.trusted, c.ok)
		}
	}
	// A deny list that cannot be read refuses everyone.
	if _, ok := backendTakes("tr", interactions.TrustPeer, false, peerSets{trust: map[string]bool{"tr": true}}, true, true); ok {
		t.Error("delivered with the deny list unreadable")
	}
}

// The feed delivers nothing before the daemon has started; a timed scan
// leaves a task that just changed to the wake-up that follows its files
// being stored; and a delivery carries the latest message's files inline,
// the earlier ones as references.
func TestInboundFeedStartSettleAndFiles(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	peer := registered(t, srv.URL, "peer")
	trustPeers(t, prov, peer.AID())
	file := func(name, body string) []delegation.Attachment {
		a, err := attachmentFromBytes(name, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return []delegation.Attachment{a}
	}
	id, err := peer.DelegateIn(ctx, prov.AID(), "with a file", file("one.txt", "the first file"), "")
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, peer, prov) // the follow-up goes once it is confirmed (engine ENG-O2)
	if err := peer.SendMessageAtts(ctx, id, "and another", file("two.txt", "the second file")); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	out := make(chan module.Task, 4)
	seen := map[string]feedMark{}
	prov.feedInbound(ctx, out, seen, time.Now().Add(-time.Hour))
	if len(out) != 0 {
		t.Fatal("a timed scan delivered a task that changed within the settle time")
	}
	prov.feedInbound(ctx, out, seen, time.Time{})
	if len(out) != 1 {
		t.Fatalf("a wake-up delivered %d tasks, want 1", len(out))
	}
	task := <-out
	var files []a2ashape.Part
	for _, m := range task.History {
		for _, p := range m.Parts {
			if p.Kind == a2ashape.PartRaw || p.Kind == a2ashape.PartURL {
				files = append(files, p)
			}
		}
	}
	if len(files) != 2 || files[0].Kind != a2ashape.PartURL || files[1].Kind != a2ashape.PartRaw ||
		string(files[1].Raw) != "the second file" {
		t.Fatalf("files in the delivery: %+v", files)
	}
	prov.feedInbound(ctx, out, seen, time.Time{})
	if len(out) != 0 {
		t.Fatal("delivered again with nothing new")
	}

	// A daemon that has not finished starting delivers nothing.
	prov.inFeed.mu.Lock()
	prov.inFeed.ready, prov.inFeed.started = make(chan struct{}), false
	prov.inFeed.mu.Unlock()
	fctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	tasks, err := a2aHost(prov).InboundTasks(fctx)
	if err != nil {
		t.Fatal(err)
	}
	prov.inFeed.wake()
	select {
	case x := <-tasks:
		t.Fatalf("delivered %s before the daemon started", x.ID)
	case <-time.After(300 * time.Millisecond):
	}
	prov.inFeed.open()
	select {
	case x := <-tasks:
		if x.ID != id {
			t.Fatalf("delivered %s", x.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing delivered once the daemon started")
	}
}

// declaringModule declares an untrusted backend from Start, as module/a2a
// does for a backend with accept_untrusted. Built only when configured, so
// the other tests' daemons do not have it.
const declaringModuleName = "testdeclaresuntrusted"

type declaringModule struct{}

func (declaringModule) Name() string { return declaringModuleName }
func (declaringModule) Start(_ context.Context, h module.Host) error {
	h.DeclareUntrustedBackend()
	return nil
}
func (declaringModule) Stop(context.Context) error { return nil }

func init() {
	module.Register(declaringModuleName, func(raw []byte) (module.Module, error) {
		if len(raw) == 0 {
			return nil, nil
		}
		return declaringModule{}, nil
	})
}

// InboundTask, which a module asks before each retry of a failed forward
// (A2A-DESIGN §11.6), decides as the feed does and with the lists as they
// are now: a trusted peer's task waiting on the provider comes back as the
// feed delivers it (the backend's context, anet.peer_aid, anet.trusted);
// once the task is answered, or its peer is denied, or for an id that is no
// such task, it does not.
func TestInboundTaskIsDecidedAfresh(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	peer := registered(t, srv.URL, "peer")
	allowed := registered(t, srv.URL, "allowed")
	trustPeers(t, prov, peer.AID())
	allowPeers(t, prov, allowed.AID())
	host := a2aHost(prov)
	answered, err := peer.DelegateIn(ctx, prov.AID(), "answer me", nil, "c1")
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := peer.DelegateIn(ctx, prov.AID(), "still waiting", nil, "c1")
	if err != nil {
		t.Fatal(err)
	}
	notTrusted, err := allowed.DelegateIn(ctx, prov.AID(), "from an allowed peer", nil, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	task, ok, err := host.InboundTask(ctx, waiting)
	if err != nil || !ok {
		t.Fatalf("a trusted peer's waiting task: ok %v, %v", ok, err)
	}
	if task.ID != waiting || task.Metadata[a2ashape.KeyPeerAID] != peer.AID() || task.Metadata[a2ashape.KeyTrusted] != true ||
		task.ContextID != backendContextID(peer.AID(), "c1") || len(task.History) == 0 ||
		task.History[len(task.History)-1].Parts[0].Text != "still waiting" {
		t.Fatalf("task %+v", task)
	}
	answer := a2ashape.Message{Role: a2ashape.RoleAgent, Parts: []a2ashape.Part{a2ashape.TextPart("which one?")}}
	if _, err := host.ReplyTask(ctx, answered, answer, a2ashape.TaskStateInputRequired); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{answered, notTrusted, "ix_nope"} {
		if _, ok, err := host.InboundTask(ctx, id); ok || err != nil {
			t.Fatalf("%s: ok %v, %v; want no", id, ok, err)
		}
	}
	denyPeers(t, prov, peer.AID())
	if _, ok, _ := host.InboundTask(ctx, waiting); ok {
		t.Fatal("a denied peer's task is still to be forwarded")
	}
}

// A module whose backend is slow to answer a first turn says so with
// ReplyTask(working, no message) (module/a2a announceWhenSlow): the
// requester is sent status working and its task leaves submitted, so its
// no-response deadline (§4.2) does not fail a turn that is only long; the
// task stays the backend's (the requester's message is still the latest),
// and the answer that follows completes it.
func TestAModuleTellsTheRequesterItsBackendIsWorking(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	peer := registered(t, srv.URL, "peer")
	trustPeers(t, prov, peer.AID())
	host := a2aHost(prov)
	id, err := peer.DelegateIn(ctx, prov.AID(), "a long one", nil, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := host.ReplyTask(ctx, id, a2ashape.Message{}, a2ashape.TaskStateWorking); err != nil {
		t.Fatalf("ReplyTask working: %v", err)
	}
	if st := stateOf(t, prov, id); st != interactions.StateWorking {
		t.Fatalf("provider state %s, want working", st)
	}
	if _, ok, err := host.InboundTask(ctx, id); !ok || err != nil {
		t.Fatalf("after working the task is no longer the backend's: ok %v, %v", ok, err)
	}
	if err := peer.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, peer, id); st != interactions.StateWorking {
		t.Fatalf("requester state %s, want working", st)
	}
	if got := peer.failUnanswered(time.Now().Add(2 * DefaultNoResponseAfter).UnixMilli()); len(got) != 0 {
		t.Fatalf("the requester failed %v as no_response", got)
	}
	answer := a2ashape.Message{Role: a2ashape.RoleAgent, Parts: []a2ashape.Part{a2ashape.TextPart("done")}}
	if _, err := host.ReplyTask(ctx, id, answer, a2ashape.TaskStateCompleted); err != nil {
		t.Fatal(err)
	}
	if err := peer.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, peer, id); st != interactions.StateCompleted {
		t.Fatalf("requester state %s after the answer, want completed", st)
	}
}

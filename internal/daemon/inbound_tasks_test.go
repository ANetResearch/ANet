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
func forwardInbound(ctx context.Context, h moduleHost, tasks <-chan module.Task, b *fakeA2ABackend) *[]error {
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
		return errs
	}
	go func() {
		for task := range tasks {
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

// A2A-DESIGN §11.6 and 0017 Q23, on the daemon's side: only a trusted
// peer's text task reaches a module's backend — a peer on the allow list
// alone, and a stranger under policy open, do not; a task the backend has
// is not also given to the auto-reply agent, while the others still are;
// the backend sees one context per (peer, context), so two peers naming the
// same context never share a conversation; and a task comes again with the
// requester's next message, in the same context.
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
	replier, err := newAutoReplier(arCfg, prov.layout)
	if err != nil {
		t.Fatal(err)
	}

	fctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	host := moduleHost{prov}
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

	// The auto-reply agent answers the two tasks no backend is given, and
	// leaves the three a backend is, although nothing has answered them yet.
	prov.autoReplyOnce(ctx, arCfg, replier)
	if n := api.calls.Load(); n != 2 {
		t.Fatalf("auto-reply called %d times, want 2 (allowed peer, stranger)", n)
	}
	for _, id := range []string{al, st} {
		if got := lastFromMe(t, prov, id); got != "an auto-reply" {
			t.Fatalf("%s: last answer %q", id, got)
		}
	}
	for _, id := range []string{a1, a2, b1} {
		if got := lastFromMe(t, prov, id); got != "" {
			t.Fatalf("auto-reply answered %s, a task the backend has: %q", id, got)
		}
	}

	backend := newFakeA2ABackend(t)
	errs := forwardInbound(fctx, host, tasks, backend)
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

	// The requester answers the question: the task comes again, in the same
	// context, and the auto-reply agent still leaves it alone.
	if err := peerA.SendMessage(ctx, a2, "ask no more: the red one", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	prov.autoReplyOnce(ctx, arCfg, replier)
	if n := api.calls.Load(); n != 2 {
		t.Fatalf("auto-reply called for a follow-up the backend has (%d calls)", n)
	}
	waitUntil(t, "the follow-up to reach the backend", func() bool { return len(backend.snapshot()) == 4 })
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
	host := moduleHost{prov}
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
		name, peer, trust   string
		capability, declare bool
		trusted, ok         bool
	}{
		{"trusted", "tr", interactions.TrustPeer, false, false, true, true},
		{"trusted now, accepted under open", "tr", interactions.TrustPublic, false, false, true, true},
		{"allowed", "al", interactions.TrustPeer, false, false, false, false},
		{"allowed, declared", "al", interactions.TrustPeer, false, true, false, true},
		{"approved, declared", "ap", interactions.TrustApproved, false, true, false, true},
		{"stranger under open, declared", "st", interactions.TrustPublic, false, true, false, false},
		{"no admission recorded, declared", "st", "", false, true, false, false},
		{"denied although trusted", "dn", interactions.TrustPeer, false, true, false, false},
		{"capability call", "tr", interactions.TrustPeer, true, true, false, false},
		{"public capability", "tr", interactions.TrustPublicCap, false, true, false, false},
		{"no peer", "", interactions.TrustPeer, false, true, false, false},
	}
	for _, c := range cases {
		trusted, ok := backendTakes(c.peer, c.trust, c.capability, ps, c.declare)
		if trusted != c.trusted || ok != c.ok {
			t.Errorf("%s: trusted %v ok %v, want %v %v", c.name, trusted, ok, c.trusted, c.ok)
		}
	}
	// A deny list that cannot be read refuses everyone.
	if _, ok := backendTakes("tr", interactions.TrustPeer, false, peerSets{trust: map[string]bool{"tr": true}}, true); ok {
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
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
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
	tasks, err := moduleHost{prov}.InboundTasks(fctx)
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
	host := moduleHost{prov}
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

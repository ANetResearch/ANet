//go:build !no_a2a

package a2a

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// inboundHost is a Host that hands inbound tasks to the module and records
// its replies and evidence.
type inboundHost struct {
	testHost
	tasks chan module.Task

	mu       sync.Mutex
	replies  []sentReply
	evidence []map[string]any
	replied  chan sentReply
}

type sentReply struct {
	taskID string
	msg    a2ashape.Message
	state  a2ashape.TaskState
}

func newInboundHost(t *testing.T) *inboundHost {
	return &inboundHost{testHost: testHost{dir: t.TempDir()}, tasks: make(chan module.Task, 8), replied: make(chan sentReply, 8)}
}

func (h *inboundHost) InboundTasks(context.Context) (<-chan module.Task, error) { return h.tasks, nil }

func (h *inboundHost) ReplyTask(_ context.Context, id string, msg a2ashape.Message, st a2ashape.TaskState) (module.Task, error) {
	r := sentReply{id, msg, st}
	h.mu.Lock()
	h.replies = append(h.replies, r)
	h.mu.Unlock()
	h.replied <- r
	return module.Task{ID: id}, nil
}

func (h *inboundHost) RecordEvidence(kind string, payload any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := payload.(map[string]any)
	p["kind"] = kind
	h.evidence = append(h.evidence, p)
	return nil
}

func (h *inboundHost) wait(t *testing.T) sentReply {
	t.Helper()
	select {
	case r := <-h.replied:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	return sentReply{}
}

// testBackend is an A2A server built from a2a-go's own server stack: it
// answers "backend: <text>", asks back when told "ask", and completes a
// task it asked about with an artifact.
type testBackend struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []*a2asrv.ExecutorContext
	exts  [][]string
}

const backendToken = "backend-token"

func newTestBackend(t *testing.T) *testBackend {
	return newTestBackendAt(t, func(base string) string { return base })
}

// newTestBackendAt is a test backend whose card names its interface at
// iface(its own base URL).
func newTestBackendAt(t *testing.T, iface func(base string) string) *testBackend {
	b := &testBackend{}
	exec := a2asrv.AgentExecutorFunc(func(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			b.mu.Lock()
			b.calls = append(b.calls, ec)
			exts, _ := ec.ServiceParams.Get(a2a.SvcParamExtensions)
			b.exts = append(b.exts, exts)
			b.mu.Unlock()
			text := ""
			if len(ec.Message.Parts) > 0 {
				text = ec.Message.Parts[0].Text()
			}
			switch {
			case ec.StoredTask != nil:
				if !yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart("final: "+text)), nil) {
					return
				}
				yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
			case text == "ask":
				if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
					return
				}
				q := a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("which one?"))
				yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, q), nil)
			default:
				yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("backend: "+text)), nil)
			}
		}
	})
	mux := http.NewServeMux()
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+backendToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(b.srv.Close)
	card := &a2a.AgentCard{Name: "backend", Description: "d", Version: "1",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(iface(b.srv.URL)+"/rpc", a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{{ID: "chat", Name: "chat", Description: "d", Tags: []string{"chat"}}}}
	mux.Handle("/.well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("/rpc", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(exec)))
	return b
}

func (b *testBackend) n() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

func inboundTask(id, peer string, trusted bool, msgs ...string) module.Task {
	t := module.Task{ID: id, ContextID: "ctx-" + id, Status: a2ashape.TaskStatus{State: a2ashape.TaskStateSubmitted},
		Metadata: map[string]any{a2ashape.KeyPeerAID: peer, a2ashape.KeyTrusted: trusted}}
	for i, s := range msgs {
		role := a2ashape.RoleUser
		if i%2 == 1 {
			role = a2ashape.RoleAgent
		}
		t.History = append(t.History, a2ashape.Message{ID: "m" + id + s, Role: role, TaskID: id, ContextID: t.ContextID,
			Parts: []a2ashape.Part{a2ashape.TextPart(s)},
			Metadata: map[string]any{a2ashape.KeyServiceParameters: map[string]any{
				"A2A-Extensions": []any{"https://ext.example/one"}, "A2A-Version": "1.0"}}})
	}
	return t
}

func startBackendModule(t *testing.T, h *inboundHost, cfg string) {
	t.Helper()
	isolateHome(t)
	m, err := New([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := m.Start(ctx, h); err != nil {
		t.Fatal(err)
	}
}

func tokenFile(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(p, []byte(backendToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBackendForwardsTrustedTextTasks(t *testing.T) {
	b := newTestBackend(t)
	h := newInboundHost(t)
	startBackendModule(t, h, `{"backends":[{"match":"*","url":"`+b.srv.URL+`","token_file":"`+tokenFile(t)+`"}]}`)

	// A trusted peer's task: forwarded, with who sent it, and answered.
	h.tasks <- inboundTask("ix1", "bafypeer", true, "hello")
	r := h.wait(t)
	if r.taskID != "ix1" || r.state != a2ashape.TaskStateCompleted || len(r.msg.Parts) != 1 || r.msg.Parts[0].Text != "backend: hello" ||
		r.msg.Role != a2ashape.RoleAgent {
		t.Fatalf("reply %+v", r)
	}
	b.mu.Lock()
	ec := b.calls[0]
	exts := b.exts[0]
	b.mu.Unlock()
	if ec.Message.Metadata[a2ashape.KeyPeerAID] != "bafypeer" || ec.Message.Metadata[a2ashape.KeyTrusted] != true ||
		ec.Message.Metadata[a2ashape.KeyServiceParameters] != nil || ec.ContextID != "ctx-ix1" {
		t.Fatalf("the backend received %+v (context %s)", ec.Message.Metadata, ec.ContextID)
	}
	if !slices.Contains(exts, "https://ext.example/one") {
		t.Fatalf("service parameters not restored as headers: %v", exts)
	}
	h.mu.Lock()
	ev := h.evidence
	h.mu.Unlock()
	if len(ev) != 1 || ev[0]["kind"] != "anet.backend.forwarded" || ev[0]["interaction_id"] != "ix1" ||
		ev[0]["peer_aid"] != "bafypeer" || ev[0]["trusted"] != true || ev[0]["backend"] != b.srv.URL {
		t.Fatalf("evidence %+v", ev)
	}

	// A peer that is not trusted: left in the inbox.
	h.tasks <- inboundTask("ix2", "bafystranger", false, "hello")
	time.Sleep(200 * time.Millisecond)
	if b.n() != 1 {
		t.Fatal("an untrusted peer's task reached a trusted-only backend")
	}

	// A question and its answer continue one task on the backend.
	h.tasks <- inboundTask("ix3", "bafypeer", true, "ask")
	r = h.wait(t)
	if r.state != a2ashape.TaskStateInputRequired || r.msg.Parts[0].Text != "which one?" {
		t.Fatalf("question %+v", r)
	}
	h.tasks <- inboundTask("ix3", "bafypeer", true, "ask", "which one?", "the red one")
	r = h.wait(t)
	if r.state != a2ashape.TaskStateCompleted || r.msg.Parts[0].Text != "final: the red one" {
		t.Fatalf("answer %+v", r)
	}
	b.mu.Lock()
	first, second := b.calls[1], b.calls[2]
	b.mu.Unlock()
	if second.StoredTask == nil || second.TaskID != first.TaskID {
		t.Fatalf("the follow-up started a new backend task (%s, then %s)", first.TaskID, second.TaskID)
	}
}

func TestBackendAcceptUntrusted(t *testing.T) {
	b := newTestBackend(t)
	h := newInboundHost(t)
	startBackendModule(t, h, `{"backends":[{"match":"*","url":"`+b.srv.URL+`","token_file":"`+tokenFile(t)+`","accept_untrusted":true,"toolless":true}]}`)
	if !h.untrusted {
		t.Fatal("not declared to the kernel")
	}
	h.tasks <- inboundTask("ix1", "bafystranger", false, "hi")
	if r := h.wait(t); r.msg.Parts[0].Text != "backend: hi" {
		t.Fatalf("reply %+v", r)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.calls[0].Message.Metadata[a2ashape.KeyTrusted] != false {
		t.Fatal("the backend was not told the peer is untrusted")
	}
}

func TestBackendFailureLeavesTheTask(t *testing.T) {
	b := newTestBackend(t)
	h := newInboundHost(t)
	// Wrong token: the backend refuses, nothing is answered or recorded.
	p := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(p, []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	startBackendModule(t, h, `{"backends":[{"match":"*","url":"`+b.srv.URL+`","token_file":"`+p+`"}]}`)
	h.tasks <- inboundTask("ix1", "bafypeer", true, "hi")
	time.Sleep(300 * time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.replies) != 0 || len(h.evidence) != 0 || b.n() != 0 {
		t.Fatalf("a refused forward was answered: %+v %+v", h.replies, h.evidence)
	}
}

func TestBackendTokenFileMustBeReadable(t *testing.T) {
	isolateHome(t)
	m, err := New([]byte(`{"backends":[{"match":"*","url":"http://127.0.0.1:9","token_file":"/nonexistent/tok"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), newInboundHost(t)); err == nil {
		t.Fatal("started with an unreadable token file")
	}
}

// The calls go where the backend's card points, and the card is held to the
// url rule too: an interface in plain http off this machine receives
// neither the task nor the token. (0.0.0.0 reaches this machine's listener
// on Linux, so without the rule the forward would succeed.)
func TestBackendCardInterfacesFollowTheURLRule(t *testing.T) {
	b := newTestBackendAt(t, func(base string) string { return strings.Replace(base, "127.0.0.1", "0.0.0.0", 1) })
	h := newInboundHost(t)
	startBackendModule(t, h, `{"backends":[{"match":" * ","url":"`+b.srv.URL+`","token_file":"`+tokenFile(t)+`"}]}`)
	h.tasks <- inboundTask("ix1", "bafypeer", true, "hello")
	time.Sleep(300 * time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if b.n() != 0 || len(h.replies) != 0 || len(h.evidence) != 0 {
		t.Fatalf("forwarded through a card interface in the clear: calls %d, replies %+v", b.n(), h.replies)
	}
}

// A match is held trimmed: " * " is the catch-all, not a skill.
func TestBackendMatchIsTrimmed(t *testing.T) {
	b := newTestBackend(t)
	h := newInboundHost(t)
	startBackendModule(t, h, `{"backends":[{"match":" * ","url":"`+b.srv.URL+`","token_file":"`+tokenFile(t)+`"}]}`)
	h.tasks <- inboundTask("ix1", "bafypeer", true, "hi")
	if r := h.wait(t); r.msg.Parts[0].Text != "backend: hi" {
		t.Fatalf("reply %+v", r)
	}
}

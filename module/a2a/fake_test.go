//go:build !no_a2a

package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// fakeSeam is a TaskSeam over memory: tasks scoped by peer the way the
// kernel scopes them, a remote agent that answers "echo: <text>", and a
// Watch that delivers what changes afterwards and closes at a terminal
// state.
type fakeSeam struct {
	mu       sync.Mutex
	n        int
	tasks    map[string]*fakeTask
	watchers map[string][]chan module.TaskEvent
	cards    map[string]module.RemoteAgent
	agents   []module.RemoteAgent

	sends []module.TaskSend
	// pays are the payment decisions made: through Pay, or through Send
	// with a payment message, as the kernel routes one (§8.7).
	pays []module.PayDecision
	// quote makes new tasks stop at input-required with a payment quote,
	// waiting as the kernel says a quote above the automatic tier waits
	// (anet.reason needs_operator_approval).
	quote map[string]any
	// hold answers payment-submitted as the kernel does above the agent
	// tier: the task, still waiting, told the operator decides (§8.3).
	hold bool
}

type fakeTask struct {
	peer string
	t    module.Task
	seq  int64
	// watched is closed when the first watcher arrives, so the scripted
	// answer of a task sent with returnImmediately cannot overtake it.
	watched chan struct{}
}

func newFakeSeam() *fakeSeam {
	return &fakeSeam{tasks: map[string]*fakeTask{}, watchers: map[string][]chan module.TaskEvent{},
		cards: map[string]module.RemoteAgent{}}
}

func notFound(id string) error { return a2ashape.Errorf(a2ashape.ErrTaskNotFound, "%s", id) }

// find is the kernel's scoped lookup: another peer's task is not found.
func (f *fakeSeam) find(peer, id string) (*fakeTask, error) {
	ft, ok := f.tasks[id]
	if !ok || ft.peer != peer {
		return nil, notFound(id)
	}
	return ft, nil
}

func (f *fakeSeam) setState(ft *fakeTask, st a2ashape.TaskState, msg *a2ashape.Message) {
	ft.seq++
	now := time.Now().UTC()
	ft.t.Status = a2ashape.TaskStatus{State: st, Message: msg, Timestamp: &now}
	if ft.t.Metadata == nil {
		ft.t.Metadata = map[string]any{}
	}
	ft.t.Metadata[a2ashape.KeyStateSeq] = ft.seq
	ft.t.Metadata[a2ashape.KeyPeerAID] = ft.peer
}

// publish sends an event to the task's watchers; a terminal status closes
// them. Called with f.mu held.
func (f *fakeSeam) publish(id string, ev module.TaskEvent) {
	terminal := ev.StatusUpdate != nil && ev.StatusUpdate.Status.State.Terminal()
	for _, ch := range f.watchers[id] {
		ch <- ev
		if terminal {
			close(ch)
		}
	}
	if terminal {
		delete(f.watchers, id)
	}
}

func (f *fakeSeam) status(ft *fakeTask) {
	t := copyTask(ft.t)
	su := a2ashape.StatusUpdate(t)
	f.publish(t.ID, module.TaskEvent{StatusUpdate: &su})
}

func textOf(m a2ashape.Message) string {
	var b []string
	for _, p := range m.Parts {
		if p.Kind == a2ashape.PartText {
			b = append(b, p.Text)
		}
	}
	return strings.Join(b, " ")
}

// answer is the remote agent: working, then the reply artifact, then
// completed. With withArtifactEvent false the kernel's artifact event is
// left out, which the interface must make up for.
func (f *fakeSeam) answer(id, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ft := f.tasks[id]
	if ft == nil || ft.t.Status.State.Terminal() {
		return
	}
	f.setState(ft, a2ashape.TaskStateWorking, nil)
	f.status(ft)
	reply := a2ashape.Artifact{ID: a2ashape.ArtifactReply, Name: a2ashape.ArtifactReply,
		Parts: []a2ashape.Part{a2ashape.TextPart("echo: " + text)}}
	ft.t.Artifacts = []a2ashape.Artifact{reply}
	au := a2ashape.ArtifactUpdates(ft.t)[0]
	f.publish(id, module.TaskEvent{ArtifactUpdate: &au})
	f.setState(ft, a2ashape.TaskStateCompleted, nil)
	ft.t.Metadata[a2ashape.KeyReceiptVerified] = a2ashape.ReceiptVerified
	f.status(ft)
}

func (f *fakeSeam) Send(ctx context.Context, peer string, req module.TaskSend) (module.Task, error) {
	f.mu.Lock()
	f.sends = append(f.sends, req)
	msg := req.Message
	var ft *fakeTask
	if msg.TaskID != "" && hasX402(msg.Metadata) {
		f.mu.Unlock()
		return f.payMessage(ctx, peer, req)
	}
	if msg.TaskID != "" {
		var err error
		if ft, err = f.find(peer, msg.TaskID); err != nil {
			f.mu.Unlock()
			return module.Task{}, err
		}
		if ft.t.Status.State.Terminal() {
			f.mu.Unlock()
			return module.Task{}, a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s has ended", ft.t.ID)
		}
		ft.t.History = append(ft.t.History, msg)
		f.setState(ft, a2ashape.TaskStateWorking, nil)
		f.status(ft)
	} else {
		f.n++
		id := fmt.Sprintf("task%d", f.n)
		ctxID := msg.ContextID
		if ctxID == "" {
			ctxID = fmt.Sprintf("ctx%d", f.n)
		}
		msg.TaskID, msg.ContextID = id, ctxID
		ft = &fakeTask{peer: peer, watched: make(chan struct{}),
			t: module.Task{ID: id, ContextID: ctxID, History: []a2ashape.Message{msg}}}
		f.tasks[id] = ft
		f.setState(ft, a2ashape.TaskStateSubmitted, nil)
		if f.quote != nil {
			q := a2ashape.Message{ID: "quote-" + id, Role: a2ashape.RoleAgent, TaskID: id, ContextID: ctxID,
				Parts:    []a2ashape.Part{a2ashape.TextPart("payment required")},
				Metadata: map[string]any{a2ashape.KeyX402Status: a2ashape.PaymentRequired, a2ashape.KeyX402Required: f.quote}}
			f.setState(ft, a2ashape.TaskStateInputRequired, &q)
			ft.t.Metadata[a2ashape.KeyX402Status] = a2ashape.PaymentRequired
			ft.t.Metadata[a2ashape.KeyX402Required] = f.quote
			ft.t.Metadata[a2ashape.KeyReason] = x402a2a.ReasonNeedsOperatorApproval
			t := copyTask(ft.t)
			f.mu.Unlock()
			return t, nil
		}
	}
	id, text := ft.t.ID, textOf(msg)
	watched := ft.watched
	f.mu.Unlock()
	if req.ReturnImmediately {
		go func() {
			select {
			case <-watched:
			case <-time.After(200 * time.Millisecond):
			}
			f.answer(id, text)
		}()
		return f.Get(ctx, peer, id, req.HistoryLength)
	}
	f.answer(id, text)
	return f.Get(ctx, peer, id, req.HistoryLength)
}

func copyTask(t module.Task) module.Task {
	out := t
	out.History = append([]a2ashape.Message(nil), t.History...)
	out.Artifacts = append([]a2ashape.Artifact(nil), t.Artifacts...)
	out.Metadata = map[string]any{}
	for k, v := range t.Metadata {
		out.Metadata[k] = v
	}
	return out
}

func trimHistory(t module.Task, n *int) module.Task {
	if n != nil && len(t.History) > *n {
		t.History = t.History[len(t.History)-*n:]
	}
	return t
}

func (f *fakeSeam) Get(_ context.Context, peer, id string, historyLen *int) (module.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ft, err := f.find(peer, id)
	if err != nil {
		return module.Task{}, err
	}
	return trimHistory(copyTask(ft.t), historyLen), nil
}

func (f *fakeSeam) List(_ context.Context, peer string, flt module.TaskFilter) (module.TaskPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	size, err := a2ashape.PageSize(flt.PageSize)
	if err != nil {
		return module.TaskPage{}, err
	}
	page := module.TaskPage{PageSize: size}
	for i := f.n; i >= 1; i-- {
		ft := f.tasks[fmt.Sprintf("task%d", i)]
		if ft == nil || ft.peer != peer || (flt.ContextID != "" && ft.t.ContextID != flt.ContextID) ||
			(flt.State != "" && string(ft.t.Status.State) != flt.State) {
			continue
		}
		page.TotalSize++
		if len(page.Tasks) < size {
			t := trimHistory(copyTask(ft.t), flt.HistoryLen)
			if !flt.IncludeArtifacts {
				t.Artifacts = nil
			}
			page.Tasks = append(page.Tasks, t)
		}
	}
	return page, nil
}

func (f *fakeSeam) Cancel(_ context.Context, peer, id string) (module.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ft, err := f.find(peer, id)
	if err != nil {
		return module.Task{}, err
	}
	if ft.t.Status.State.Terminal() {
		return module.Task{}, a2ashape.Errorf(a2ashape.ErrTaskNotCancelable, "%s", id)
	}
	f.setState(ft, a2ashape.TaskStateCanceled, nil)
	f.status(ft)
	return copyTask(ft.t), nil
}

func (f *fakeSeam) Watch(ctx context.Context, peer, id string) (module.Task, <-chan module.TaskEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ft, err := f.find(peer, id)
	if err != nil {
		return module.Task{}, nil, err
	}
	ch := make(chan module.TaskEvent, 64)
	if ft.t.Status.State.Terminal() {
		close(ch)
	} else {
		f.watchers[id] = append(f.watchers[id], ch)
	}
	select {
	case <-ft.watched:
	default:
		close(ft.watched)
	}
	return copyTask(ft.t), ch, nil
}

func (f *fakeSeam) Agents(context.Context, module.AgentQuery) ([]module.RemoteAgent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]module.RemoteAgent(nil), f.agents...), nil
}

func (f *fakeSeam) Card(_ context.Context, aid string) (module.RemoteAgent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ra, ok := f.cards[aid]; ok {
		return ra, nil
	}
	return module.RemoteAgent{AID: aid, Verification: unverified}, nil
}

func (f *fakeSeam) Pay(_ context.Context, peer, id string, d module.PayDecision) (module.Task, error) {
	f.mu.Lock()
	ft, err := f.find(peer, id)
	if err != nil {
		f.mu.Unlock()
		return module.Task{}, err
	}
	f.pays = append(f.pays, d)
	// Decided: the task no longer waits for a decision.
	delete(ft.t.Metadata, a2ashape.KeyReason)
	if d.Decision == module.PayReject {
		f.setState(ft, a2ashape.TaskStateCanceled, nil)
		f.status(ft)
		t := copyTask(ft.t)
		f.mu.Unlock()
		return t, nil
	}
	f.setState(ft, a2ashape.TaskStateWorking, nil)
	ft.t.Metadata[a2ashape.KeyX402Status] = a2ashape.PaymentSubmitted
	f.status(ft)
	t := copyTask(ft.t)
	f.mu.Unlock()
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.answer(id, "paid")
	}()
	return t, nil
}

// payMessage is the kernel's handling of a payment message on Send
// (internal/daemon taskPaymentMessage): a payload of the client's own or an
// option the quote does not offer is refused as a task with a
// payment-failed status message and nothing else happens; submitted and
// rejected are decisions, as Pay makes them; any other status is
// InvalidParams.
func (f *fakeSeam) payMessage(ctx context.Context, peer string, req module.TaskSend) (module.Task, error) {
	m := req.Message
	refuse := func(reason string) (module.Task, error) {
		t, err := f.Get(ctx, peer, m.TaskID, req.HistoryLength)
		if err != nil {
			return module.Task{}, err
		}
		return a2ashape.PaymentRefusal(t, "refusal-"+m.TaskID, "", reason, a2ashape.PaymentRefusalDetail(reason)), nil
	}
	if _, ok := m.Metadata[a2ashape.KeyX402Payload]; ok {
		return refuse(x402a2a.ReasonClientPayloadUnsupported)
	}
	var d module.PayDecision
	switch m.Metadata[a2ashape.KeyX402Status] {
	case a2ashape.PaymentSubmitted:
		d.Decision = module.PaySubmit
		if v, ok := m.Metadata[a2ashape.KeyPaymentAccept]; ok && v != nil {
			b, _ := json.Marshal(v)
			if !f.offered(b) {
				return refuse(x402a2a.ReasonOptionNotOffered)
			}
			d.Accept = b
		}
		f.mu.Lock()
		hold := f.hold
		f.mu.Unlock()
		if hold {
			t, err := f.Get(ctx, peer, m.TaskID, req.HistoryLength)
			if err != nil {
				return module.Task{}, err
			}
			return a2ashape.PaymentHold(t, "hold-"+m.TaskID, x402a2a.ReasonNeedsOperatorApproval,
				"Payment not submitted: above agent_max. Run `anet pay "+m.TaskID+"` in a terminal."), nil
		}
	case a2ashape.PaymentRejected:
		d.Decision = module.PayReject
	default:
		return module.Task{}, a2ashape.Errorf(a2ashape.ErrInvalidParams, "a payment message is payment-submitted or payment-rejected")
	}
	t, err := f.Pay(ctx, peer, m.TaskID, d)
	if err != nil || req.ReturnImmediately || d.Decision == module.PayReject {
		return t, err
	}
	// The kernel waits like any send; the scripted answer comes shortly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if t, err = f.Get(ctx, peer, m.TaskID, req.HistoryLength); err != nil || t.Status.State.Terminal() {
			return t, err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return t, nil
}

// offered compares a chosen option with the quote's accepts, as canonical
// JSON (the kernel compares with the requirements it stored).
func (f *fakeSeam) offered(accept []byte) bool {
	f.mu.Lock()
	quote := f.quote
	f.mu.Unlock()
	accepts, _ := quote["accepts"].([]any)
	want := canonicalOption(accept)
	for _, a := range accepts {
		b, _ := json.Marshal(a)
		if canonicalOption(b) == want {
			return true
		}
	}
	return false
}

func canonicalOption(b []byte) string {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func hasX402(meta map[string]any) bool {
	for k := range meta {
		if strings.HasPrefix(k, "x402.") {
			return true
		}
	}
	return false
}

var _ module.TaskSeam = (*fakeSeam)(nil)

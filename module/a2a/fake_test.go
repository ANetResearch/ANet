//go:build !no_a2a

package a2a

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
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
	pays  []module.PayDecision
	// quote makes new tasks stop at input-required with a payment quote.
	quote map[string]any
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

var _ module.TaskSeam = (*fakeSeam)(nil)

package autoreply

// turnfx_test.go is the module's own stand-in for the node the daemon tests ran a turn on before the module
// moved out of internal/daemon: a Host that owes turns, takes the module's sends and refusals and keeps the
// module's evidence, and the module on top of it. A test delivers a peer's task with deliver, runs one scan
// with tick, and reads what the requester would have been told with ix (MOD-4 notes; T-1 EXE F-2). The kernel
// decides which turns are owed and what a send does; the stand-in keeps only what these tests rest on: a turn
// is owed while the last text message is the peer's and the task is not rejected or completed.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/moduletest"
)

// fxSent is one message the module sent as this node.
type fxSent struct {
	Text  string
	Atts  []module.Attachment
	Final bool
}

// fxIx is one interaction of the stand-in node.
type fxIx struct {
	turn       module.Turn
	state      string
	sent       []fxSent
	reason     string        // the last RefuseTurn reason
	retryAfter time.Duration // with it
	refusals   int
	ended      int
	working    int
	denied     bool              // the peer is on the deny list: nothing is owed and nothing goes out
	files      map[string][]byte // attachment bytes by cid, as TurnAttachment reads them
	previewed  bool              // the round asked for previews and the node grants them
	offers     []string          // what the module offered to the preview sink, oldest first
}

type fakeTurnHost struct {
	moduletest.NopHost
	n *testNode

	mu    sync.Mutex
	ixs   map[string]*fxIx
	order []string
	evs   []module.Evidence
	wake  chan struct{}
	seq   int
}

func (h *fakeTurnHost) AID() string      { return h.n.mod.env.AID }
func (h *fakeTurnHost) StateDir() string { return h.n.mod.env.StateDir }

func (h *fakeTurnHost) RecordEvidence(typ string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var p map[string]any
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.evs = append(h.evs, module.Evidence{Type: typ, At: time.Now(), Payload: p})
	return nil
}

func (h *fakeTurnHost) ReadOwnEvidence(typ string, since time.Time) ([]module.Evidence, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []module.Evidence
	for _, e := range h.evs {
		if e.Type == typ && !e.At.Before(since) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (h *fakeTurnHost) SensitivePaths() []string { return testSensitive{h.n}.SensitivePaths() }

func (h *fakeTurnHost) UpdateModuleConfig(fn func(old json.RawMessage) (json.RawMessage, error)) error {
	_, err := fn(nil)
	return err
}

func (h *fakeTurnHost) owed(x *fxIx) bool {
	if x.state == "rejected" || x.state == "completed" || x.denied {
		return false
	}
	ms := x.turn.Messages
	return len(ms) > 0 && ms[len(ms)-1].From == "them"
}

func (h *fakeTurnHost) OwedTurns(context.Context) ([]module.Turn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []module.Turn
	for _, id := range h.order {
		if x := h.ixs[id]; h.owed(x) {
			t := x.turn
			t.State = x.state
			t.Messages = append([]module.TurnMessage(nil), x.turn.Messages...)
			out = append(out, t)
		}
	}
	return out, nil
}

func (h *fakeTurnHost) Wake() <-chan struct{} { return h.wake }

func (h *fakeTurnHost) get(ix string) (*fxIx, error) {
	x := h.ixs[ix]
	if x == nil || !h.owed(x) {
		return nil, module.ErrTurnGone
	}
	return x, nil
}

func (h *fakeTurnHost) SendTurn(_ context.Context, ix, text string, atts []module.Attachment, final bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	x, err := h.get(ix)
	if err != nil {
		return err
	}
	x.sent = append(x.sent, fxSent{Text: text, Atts: atts, Final: final})
	x.turn.Messages = append(x.turn.Messages, module.TurnMessage{From: "me", Body: text})
	x.turn.MsgCount++
	if final {
		x.state = "completed"
	} else {
		x.state = "working"
	}
	return nil
}

func (h *fakeTurnHost) RefuseTurn(_ context.Context, ix, reason string, retryAfter time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	x, err := h.get(ix)
	if err != nil {
		return err
	}
	switch reason {
	case module.ReasonSandboxUnavailable, module.ReasonBudget:
		x.state = "rejected"
	case module.ReasonBackendFailed:
		x.state = "input-required"
	default:
		return module.ErrRefuseReason
	}
	x.reason, x.retryAfter = reason, retryAfter
	x.refusals++
	return nil
}

func (h *fakeTurnHost) WorkingTurn(_ context.Context, ix string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if x := h.ixs[ix]; x != nil {
		x.working++
	}
	return nil
}

func (h *fakeTurnHost) EndTurn(_ context.Context, ix string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if x := h.ixs[ix]; x != nil {
		x.ended++
		x.turn.EndReqBy = "me"
	}
	return nil
}

func (h *fakeTurnHost) TurnAttachment(ix, cid string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if x := h.ixs[ix]; x != nil {
		if b, ok := x.files[cid]; ok {
			return b, nil
		}
	}
	return nil, fmt.Errorf("no attachment")
}

// fxSink is the preview sink of one interaction of the stand-in node.
type fxSink struct {
	h  *fakeTurnHost
	ix string
}

func (s fxSink) Offer(text, _ string) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.h.ixs[s.ix].offers = append(s.h.ixs[s.ix].offers, text)
}

func (h *fakeTurnHost) PreviewTurn(ix string) (module.PreviewSink, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if x := h.ixs[ix]; x != nil && x.previewed {
		return fxSink{h, ix}, true
	}
	return nil, false
}

var _ module.TurnHost = (*fakeTurnHost)(nil)
var _ module.OwnEvidenceHost = (*fakeTurnHost)(nil)
var _ module.SensitivePathsHost = (*fakeTurnHost)(nil)
var _ module.ModuleConfigHost = (*fakeTurnHost)(nil)

// turnFx is the module on the stand-in node, configured with cfg.
type turnFx struct {
	t       *testing.T
	n       *testNode
	h       *fakeTurnHost
	m       *autoReplyModule
	cfg     AutoReplyConfig
	replier autoReplier
	ctx     context.Context
}

// newTurnFx starts the module on a stand-in node. api, when not nil, is the model service the openai backend
// (and a chat agent) talks to; a sandboxed claude speaks only the Anthropic API and is given no api_base.
func newTurnFx(t *testing.T, cfg AutoReplyConfig, api *fakeOpenAI) *turnFx {
	t.Helper()
	quietLog(t)
	n := newBareDaemon(t)
	h := &fakeTurnHost{n: n, ixs: map[string]*fxIx{}, wake: make(chan struct{}, 1)}
	fx := &turnFx{t: t, n: n, h: h, m: n.mod, ctx: context.Background()}
	fx.attach(h)
	if api != nil {
		srv := httptest.NewServer(api.handler())
		t.Cleanup(srv.Close)
		if cfg.Backend != "exec" || cfg.Agent != agentClaude {
			cfg.APIBase = srv.URL + "/v1"
		}
	}
	fx.setConfig(cfg)
	return fx
}

func (fx *turnFx) attach(h *fakeTurnHost) {
	m := fx.m
	m.host, m.turns, m.ev, m.sens, m.cfgw, m.ctx = h, h, h, h, h, fx.ctx
}

// setConfig puts cfg in force without starting the module's own loop; tick runs the scan.
func (fx *turnFx) setConfig(cfg AutoReplyConfig) {
	fx.t.Helper()
	replier, err := newAutoReplier(cfg, fx.m.env)
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.cfg, fx.replier = cfg, replier
	fx.m.mu.Lock()
	c := cfg
	fx.m.cfg = &c
	fx.m.mu.Unlock()
}

// restart is a new process of the same node: a new module on the same data directory and the same
// evidence, with the memory of the old one gone.
func (fx *turnFx) restart() {
	fx.t.Helper()
	old := fx.m
	fx.m = &autoReplyModule{env: old.env}
	fx.m.sens = old.sens
	fx.attach(fx.h)
	fx.setConfig(fx.cfg)
}

// deliver is a task a peer delegated to this node: an inbound turn whose last message is the peer's.
func (fx *turnFx) deliver(peer string, trusted bool, text string) string {
	fx.h.mu.Lock()
	defer fx.h.mu.Unlock()
	fx.h.seq++
	ix := fmt.Sprintf("ix-%04d", fx.h.seq)
	fx.h.ixs[ix] = &fxIx{state: "submitted", turn: module.Turn{Ix: ix, Role: "inbound", Peer: peer, Trusted: trusted,
		Goal: text, MsgCount: 1, PeerMsgCount: 1, Messages: []module.TurnMessage{{From: "them", Body: text}}}}
	fx.h.order = append(fx.h.order, ix)
	return ix
}

// say is another message of the peer in ix.
func (fx *turnFx) say(ix, text string) {
	fx.h.mu.Lock()
	defer fx.h.mu.Unlock()
	x := fx.h.ixs[ix]
	x.turn.Messages = append(x.turn.Messages, module.TurnMessage{From: "them", Body: text})
	x.turn.MsgCount++
	x.turn.PeerMsgCount++
	if x.state != "rejected" && x.state != "completed" {
		x.state = "working"
	}
}

// tick is one scan of the module over the owed turns.
func (fx *turnFx) tick() {
	fx.t.Helper()
	fx.m.autoReplyOnce(fx.ctx, fx.cfg, fx.replier)
}

// ix is a copy of what the stand-in node holds of ix.
func (fx *turnFx) ix(ix string) fxIx {
	fx.h.mu.Lock()
	defer fx.h.mu.Unlock()
	return *fx.h.ixs[ix]
}

// lastSent is the text of the last message the module sent in ix, and whether it sent any.
func (fx *turnFx) lastSent(ix string) (string, bool) {
	x := fx.ix(ix)
	if len(x.sent) == 0 {
		return "", false
	}
	return x.sent[len(x.sent)-1].Text, true
}

// invoked is the module's anet.autoreply.invoked records, oldest first, each checked against the registered
// payload (G-EVID).
func (fx *turnFx) invoked() []map[string]any {
	fx.t.Helper()
	evs, _ := fx.h.ReadOwnEvidence(EvAutoReplyInvoked, time.Time{})
	var out []map[string]any
	for _, e := range evs {
		assertPayloadShape(fx.t, EvAutoReplyInvoked, e.Payload)
		out = append(out, e.Payload)
	}
	return out
}

// deliverOutbound is a task this node delegated to peer and peer has answered: an outbound turn.
func (fx *turnFx) deliverOutbound(peer string, trusted bool, goal, answer string) string {
	fx.h.mu.Lock()
	defer fx.h.mu.Unlock()
	fx.h.seq++
	ix := fmt.Sprintf("ix-%04d", fx.h.seq)
	fx.h.ixs[ix] = &fxIx{state: "working", turn: module.Turn{Ix: ix, Role: "outbound", Peer: peer, Trusted: trusted,
		Goal: goal, MsgCount: 2, PeerMsgCount: 1, Messages: []module.TurnMessage{{From: "me", Body: goal}, {From: "them", Body: answer}}}}
	fx.h.order = append(fx.h.order, ix)
	return ix
}

// trust puts the peer of ix on the trust list, or takes it off.
func (fx *turnFx) trust(ix string, trusted bool) {
	fx.h.mu.Lock()
	defer fx.h.mu.Unlock()
	fx.h.ixs[ix].turn.Trusted = trusted
}

// deny puts the peer of ix on the deny list, or takes it off.
func (fx *turnFx) deny(ix string, denied bool) {
	fx.h.mu.Lock()
	defer fx.h.mu.Unlock()
	fx.h.ixs[ix].denied = denied
}

// previewed makes the round of ix one that asked for previews, or not.
func (fx *turnFx) previewed(ix string, on bool) {
	fx.h.mu.Lock()
	defer fx.h.mu.Unlock()
	fx.h.ixs[ix].previewed = on
}

// wakeLoop signals the host's Wake channel, as an inbound message does.
func (fx *turnFx) wakeLoop() {
	select {
	case fx.h.wake <- struct{}{}:
	default:
	}
}

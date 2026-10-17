package autoreply

// autoreply.go is the daemon's built-in provider harness: a background loop that watches inbound
// conversations and answers them by calling the operator's OWN service. It generalizes "who can join the
// network" beyond LLM coding agents — anyone who deploys a small model behind an OpenAI-compatible REST
// API can become a provider with a config block, no external harness process required.
//
// anet's core promise is unchanged: anet runs no model. The loop only moves bytes between the local
// interactions store and the operator-configured endpoint:
//
//	any thread owes a reply  →  conversation → backend → reply text  →  SendMessage
//
// Design notes:
//   - Symmetric / bidirectional: a conversation owes a reply whenever the LAST message is the peer's,
//     regardless of who started it — both tasks others delegated to us (inbound) AND tasks we delegated
//     to others where the peer has since replied (outbound). Autopilot keeps its side of the conversation
//     going in either direction.
//   - Stateless: "owes a reply" is derived from the conversation itself — the last text message being from
//     the peer means we owe one; after we reply, the last message is ours. Restarts never double-reply and
//     no cursor/state file is needed.
//   - Runaway guard: max_auto_replies caps how many messages we auto-send per interaction; at the cap we
//     propose `end` instead of replying, so two autopilot agents can't ping-pong (and burn tokens) forever.
//   - Backend failures are reported to the requester as a status, input-required with the reason
//     backend_failed and no text (UX-R3); the requester renders the sentence itself. The turn is not
//     tried again until the peer writes, so there is no retry storm.
//   - End negotiation: when the peer proposes ending, the loop accepts — the daemon then issues the
//     signed receipt and the requester can review us.
//   - The autoReplier seam is deliberately tiny so future backends (e.g. "exec": spawn a local coding
//     agent like cursor/claude to compose the reply) plug in without touching the loop.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/execwork"
	"github.com/ANetResearch/ANet/module"
)

const (
	// autoReplyInterval is how often the loop scans for owed turns. A constant: there is nothing to tune.
	autoReplyInterval          = 5 * time.Second
	autoReplyDefaultMaxHistory = 20
	autoReplyDefaultAPITimeout = 180 * time.Second

	autoReplyDefaultSystemPrompt = "You are a helpful assistant serving requests delegated over AgentNetwork. Reply in the language of the request."

	// autoReplyDoneSentinel is the marker a completion-aware backend appends to its reply when the task is
	// finished and no further exchange is needed. The loop strips it, sends whatever text remains, then
	// proposes `end` — so two autopilots converge on a delivered result instead of chatting pleasantries
	// until the runaway cap. Both roles may emit it; the peer's autopilot auto-accepts the end proposal.
	autoReplyDoneSentinel = "<<ANET_TASK_DONE>>"
)

// autoReplyMaxReplies is how many messages WE auto-send in one interaction before proposing `end`
// instead of replying again: the runaway guard, a constant in effect. A variable only so a test can
// lower it.
//
// This is the runaway guard: two autopilot agents talking to each other (I delegate to you, you reply,
// I reply, …) would otherwise ping-pong forever and burn tokens.
var autoReplyMaxReplies = 30

// replyContext tells a completion-aware backend WHERE it stands in the interaction so it can decide
// whether the task is finished: our role (are we the requester who set the goal, or the provider serving
// it), the original goal, and the interaction id.
type replyContext struct {
	Role          string // "inbound" (someone delegated to us — we are the provider) | "outbound" (we delegated — we are the requester)
	Goal          string
	InteractionID string
	// Outbox is a daemon-created scratch dir handed to the backend. The backend drops any files it wants
	// to deliver here (instead of calling `anet message` itself); the daemon sends them WITH the reply
	// text as one message and owns end negotiation. Empty if the daemon could not create it.
	Outbox string
	// Preview, when set, is where a backend that writes its answer piece by piece gives what it has
	// written so far — the whole text and the whole reasoning, not a delta (CUX-P5, previewOffer). It is
	// set only when CUX-P4 holds for the turn; a backend that cannot stream ignores it.
	Preview func(text, reasoning string)
}

// stripDoneSentinel reports whether reply carries the completion marker and returns the reply with the
// marker (and any now-dangling whitespace) removed. Detection is lenient: the marker may sit on its own
// line or inline, anywhere in the text.
func stripDoneSentinel(reply string) (clean string, done bool) {
	if !strings.Contains(reply, autoReplyDoneSentinel) {
		return reply, false
	}
	clean = strings.ReplaceAll(reply, autoReplyDoneSentinel, "")
	return strings.TrimSpace(clean), true
}

// chatTurn is one conversation turn handed to a reply backend, backend-agnostically: who spoke
// (user = the requester, assistant = us), the text, and any image bytes attached to a user turn.
type chatTurn struct {
	Role   string // "user" | "assistant"
	Text   string
	Images []chatImage
}

// chatImage is one decoded image attachment riding on a user turn.
type chatImage struct {
	Mime string
	Data []byte
}

// autoReplier produces a reply to a conversation. Implementations are the pluggable part of the
// auto-reply loop: v0.1 ships "openai" (any OpenAI-compatible /chat/completions endpoint); a future
// "exec" backend can spawn a local agent (cursor/claude/…) against the same seam.
type autoReplier interface {
	Reply(ctx context.Context, rc replyContext, turns []chatTurn) (string, error)
}

// newAutoReplier builds the configured backend. Unknown backends are an error so a typo'd config fails
// loudly at startup instead of silently never replying. env says where the node keeps its files.
func newAutoReplier(cfg AutoReplyConfig, env arEnv) (autoReplier, error) {
	switch cfg.Backend {
	case "", "openai":
		if cfg.APIBase == "" || cfg.Model == "" {
			return nil, fmt.Errorf("auto_reply: backend %q requires api_base and model", "openai")
		}
		return &openAIReplier{cfg: cfg}, nil
	case "exec":
		if cfg.Agent == "" {
			return nil, fmt.Errorf("auto_reply: backend %q requires agent (supported: %s)", "exec", strings.Join(SupportedExecAgents(), ", "))
		}
		if _, err := lookupAgent(cfg.Agent); err != nil {
			return nil, err
		}
		return &execReplier{cfg: cfg, dataDir: env.DataDir, aid: env.AID, sessions: execwork.Sessions(env.StateDir)}, nil
	default:
		return nil, fmt.Errorf("auto_reply: unknown backend %q (supported: openai, exec)", cfg.Backend)
	}
}

// arEnv is where the module keeps and finds things: the node's data directory (work directories must
// lie outside it), the module's own state directory and the node's AID (which names the work directory).
type arEnv struct {
	DataDir, StateDir, AID string
}

// --- the module ---

// autoReplyModule is the auto-reply module: a module.Module that answers the turns the kernel says
// are owed, through the seams of its Host and nothing else (MOD §3.1). It holds what used to be six
// fields of the daemon: the refused-turn memory, the run budget, the leftover sweep, the write lock of
// the configuration, the running loop and the loop's wake.
type autoReplyModule struct {
	raw json.RawMessage // the auto_reply block at construction

	host  module.Host
	turns module.TurnHost
	ev    module.OwnEvidenceHost
	sens  module.SensitivePathsHost
	cfgw  module.ModuleConfigHost
	env   arEnv

	ctx context.Context
	wg  sync.WaitGroup

	mu      sync.Mutex
	cfg     *AutoReplyConfig // the configuration in force; nil: auto-reply is off
	stop    context.CancelFunc
	stopped bool

	write     sync.Mutex // POST /autoreply's read, merge and save are one step
	sbRefused sandboxRefused
	budget    runBudget
	swept     sync.Once
}

// newAutoReplyModule is the module's factory; the daemon passes the raw auto_reply block.
func newAutoReplyModule(raw []byte) (module.Module, error) {
	return &autoReplyModule{raw: raw}, nil
}

func (m *autoReplyModule) Name() string { return "autoreply" }

var (
	_ module.Module            = (*autoReplyModule)(nil)
	_ module.ControlHandler    = (*autoReplyModule)(nil)
	_ module.StatusContributor = (*autoReplyModule)(nil)
)

// Start takes the seams out of the Host (a Host that lacks one is a node the name table did not grant
// them to: the module does not start) and starts the loop when auto-reply is configured.
func (m *autoReplyModule) Start(ctx context.Context, h module.Host) error {
	var ok bool
	if m.turns, ok = h.(module.TurnHost); !ok {
		return fmt.Errorf("autoreply: the Host grants no TurnHost")
	}
	if m.ev, ok = h.(module.OwnEvidenceHost); !ok {
		return fmt.Errorf("autoreply: the Host grants no OwnEvidenceHost")
	}
	if m.sens, ok = h.(module.SensitivePathsHost); !ok {
		return fmt.Errorf("autoreply: the Host grants no SensitivePathsHost")
	}
	if m.cfgw, ok = h.(module.ModuleConfigHost); !ok {
		return fmt.Errorf("autoreply: the Host grants no ModuleConfigHost")
	}
	m.host, m.ctx = h, ctx
	state := h.StateDir()
	if state == "" {
		return fmt.Errorf("autoreply: the Host grants no state directory")
	}
	// <data>/modules/autoreply: the data directory is two levels up.
	m.env = arEnv{DataDir: filepath.Dir(filepath.Dir(state)), StateDir: state, AID: h.AID()}
	cfg, err := parseAutoReply(m.raw)
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	// A block the backend cannot run with stops the node, in the module's own sentence (LEC §3.2),
	// instead of a log line and a node that never replies.
	if ref := checkAutoReply(*cfg); ref != nil {
		return ref
	}
	return m.startLoop(*cfg)
}

// Stop ends the loop and waits for the turns under way: after it returns, nothing the module started is
// running and nothing more starts. The daemon calls it before the evidence ledger closes, so a turn that
// ends on the way out still records.
func (m *autoReplyModule) Stop(context.Context) error {
	m.mu.Lock()
	m.stopped = true
	if m.stop != nil {
		m.stop()
		m.stop = nil
	}
	m.mu.Unlock()
	m.wg.Wait()
	return nil
}

// --- loop lifecycle ---

// startLoop validates the config and starts the background loop under the module's context, stopping
// the one that ran before. A config change takes effect at once (POST /autoreply).
func (m *autoReplyModule) startLoop(cfg AutoReplyConfig) error {
	replier, err := newAutoReplier(cfg, m.env)
	if err != nil {
		return err
	}
	// Before the loop's first sandboxed run: what an earlier process's runs left (LEC SB-9).
	if cfg.Backend == "exec" {
		m.sweepSandboxLeftovers()
	}
	interval := autoReplyInterval
	backend := cfg.Backend
	if backend == "" {
		backend = "openai"
	}
	switch backend {
	case "exec":
		log.Printf("anet: auto-reply on — backend=exec agent=%s work_dir=%s (every %s)", cfg.Agent, cfg.WorkDir, interval)
	default:
		log.Printf("anet: auto-reply on — backend=%s model=%s api=%s (every %s)", backend, cfg.Model, cfg.APIBase, interval)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop != nil {
		m.stop()
		m.stop = nil
	}
	m.cfg = &cfg
	if m.stopped || m.ctx.Err() != nil {
		return nil // closing: nothing starts
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.stop = cancel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.autoReplyLoop(ctx, cfg, replier, interval)
	}()
	return nil
}

// stopLoop ends the running loop (auto-reply off).
func (m *autoReplyModule) stopLoop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop != nil {
		m.stop()
		m.stop = nil
	}
	m.cfg = nil
}

// current is the configuration in force, nil when auto-reply is off.
func (m *autoReplyModule) current() *AutoReplyConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg == nil {
		return nil
	}
	c := *m.cfg
	return &c
}

// apply changes the auto-reply configuration live (no restart): produce gets the block as it is on the
// disk and returns the one to write (nil: off). The write is the kernel's (UpdateModuleConfig, which
// checks it, saves it atomically and audits it); then the loop is stopped and the new one started. A
// write that failed to save leaves the running loop and the configuration as they were. This is what
// `anet autoreply set|off` reaches.
func (m *autoReplyModule) apply(produce func(cur *AutoReplyConfig) (*AutoReplyConfig, error)) (*AutoReplyConfig, error) {
	m.write.Lock()
	defer m.write.Unlock()
	var next *AutoReplyConfig
	err := m.cfgw.UpdateModuleConfig(func(old json.RawMessage) (json.RawMessage, error) {
		cur, err := parseAutoReply(old)
		if err != nil {
			cur = nil // a block that does not decode is replaced, not merged into
		}
		cfg, err := produce(cur)
		if err != nil {
			return nil, err
		}
		if cfg != nil {
			if _, err := newAutoReplier(*cfg, m.env); err != nil {
				return nil, err // fail loudly on a bad config instead of silently persisting a loop that never runs
			}
			if ref := checkAutoReplyConfig(cfg, m.env.DataDir); ref != nil {
				return nil, ref
			}
			if cfg.Sandbox.empty() {
				c := *cfg
				c.Sandbox = nil
				cfg = &c
			}
		}
		next = cfg
		return marshalAutoReply(cfg), nil
	})
	if err != nil {
		return nil, err
	}
	if next == nil {
		m.stopLoop()
		log.Printf("anet: auto-reply OFF")
		return nil, nil
	}
	if err := m.startLoop(*next); err != nil {
		return next, err
	}
	return next, nil
}

// autoReplyDefaultTestPrompt is the synthetic self-check used by `anet autoreply test` when the operator
// gives no prompt. It exercises the whole backend path (API reachable / agent spawns + returns text).
const autoReplyDefaultTestPrompt = "Self-check: in one sentence, say who you are, what powers you and what you can help me with."

// TestAutoReply runs the CURRENTLY configured auto-reply backend once against a synthetic single-turn
// conversation and returns the reply — WITHOUT touching the Hub or creating any identity. This is the
// pollution-free self-test behind `anet autoreply test`: previously operators had to spin up a throwaway
// identity and delegate to themselves, which left a dead node registered on the Hub.
func (m *autoReplyModule) TestAutoReply(ctx context.Context, prompt string) (string, error) {
	arp := m.current()
	if arp == nil {
		return "", fmt.Errorf("auto-reply is not configured — run `anet autoreply set …` first")
	}
	replier, err := newAutoReplier(*arp, m.env)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = autoReplyDefaultTestPrompt
	}
	timeout := autoReplyDefaultAPITimeout
	if arp.APITimeoutSeconds > 0 {
		timeout = time.Duration(arp.APITimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return replier.Reply(ctx, replyContext{Role: "inbound", Goal: prompt}, []chatTurn{{Role: "user", Text: prompt}})
}

func (m *autoReplyModule) autoReplyLoop(ctx context.Context, cfg AutoReplyConfig, replier autoReplier, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	wake := m.turns.Wake()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.autoReplyOnce(ctx, cfg, replier)
		case <-wake: // an inbound message just landed — service it now, don't wait for the tick
			m.autoReplyOnce(ctx, cfg, replier)
		}
	}
}

// autoReplyOnce asks the kernel which turns owe a reply and services every one — in BOTH directions:
// tasks others delegated to us (inbound) AND tasks we delegated to others where the peer has since
// replied (outbound). "Owing a reply" is symmetric: whenever the last message is the peer's (not ours),
// we answer. Which turns those are is the kernel's decision (TurnHost.OwedTurns: admitted, not ended,
// not a capability call, not taken by an A2A backend, the peer not denied).
//
// A trusted peer's turn runs in the scan, one after another. An untrusted peer's turn runs beside the
// scan, at most two at once and one per peer (LEC BU-5): a turn that finds no free slot waits for the
// next scan. The scan returns once every turn it started has ended, so no thread is ever answered twice
// at once and the next scan sees what this one sent.
func (m *autoReplyModule) autoReplyOnce(ctx context.Context, cfg AutoReplyConfig, replier autoReplier) {
	turns, err := m.turns.OwedTurns(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("anet: auto-reply: list threads: %v", err)
		}
		return
	}
	owed := make(map[string]bool, len(turns))
	for _, t := range turns {
		owed[t.Ix] = true
	}
	m.sbRefused.prune(owed, time.Now())
	var runs sync.WaitGroup
	defer runs.Wait()
	for _, t := range turns {
		if ctx.Err() != nil {
			return
		}
		if err := m.autoReplyThread(ctx, cfg, replier, t, &runs); err != nil && !errors.Is(err, module.ErrTurnGone) {
			log.Printf("anet: auto-reply %s: %v", t.Ix, err)
		}
	}
}

// slowTurnAfter is how long the backend may have the first turn of a task
// sent to this node before its requester is told the task is being worked
// on (announceSlowTurn). A variable so tests can shorten it.
var slowTurnAfter = time.Minute

// announceSlowTurn tells the requester of th, a task sent to this node that
// nobody has answered yet (submitted), that it is being worked on — status
// working — once the backend has had the turn for slowTurnAfter without
// answering. Until then the requester has heard nothing, and a requester
// that hears nothing for its no_response_after (15m by default) fails the
// task as no_response (A2A-DESIGN §4.2): an exec agent's long turn is work,
// not silence, and its answer would come to a task already failed there,
// where a text message is not kept. A turn answered within slowTurnAfter
// costs no extra message. The returned function ends the wait and returns
// once an announcement under way is done, so that it never follows the
// reply.
func (m *autoReplyModule) announceSlowTurn(th module.Turn) func() {
	if th.Role != roleInbound || th.State != stateSubmitted || slowTurnAfter <= 0 {
		return func() {}
	}
	stop, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		tm := time.NewTimer(slowTurnAfter)
		defer tm.Stop()
		select {
		case <-tm.C:
		case <-stop:
			return
		case <-m.ctx.Done():
			return
		}
		if err := m.turns.WorkingTurn(m.ctx, th.Ix); err != nil && m.ctx.Err() == nil && !errors.Is(err, module.ErrTurnGone) {
			log.Printf("anet: auto-reply %s: tell the requester the backend is working on it: %v", th.Ix, err)
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-finished
	}
}

// The values of a turn's role and state that this file tests (the kernel's words, spelled out here so
// that the file touches the kernel through package module only).
const (
	roleInbound    = "inbound"
	stateSubmitted = "submitted"
)

// autoReplyThread services ONE conversation (inbound or outbound): produce the reply we owe (usage
// hint / backend answer / error report), or end the task at the runaway cap or when the backend says
// it is done. No-op when nothing is owed. A requester's end request needs no reply: the provider
// daemon completes on receiving it (A2A-DESIGN §4.2). An untrusted peer's turn is started on runs and
// left running (autoReplyOnce).
func (m *autoReplyModule) autoReplyThread(ctx context.Context, cfg AutoReplyConfig, replier autoReplier, th module.Turn, runs *sync.WaitGroup) error {
	// How this turn runs is decided on every turn from the lists as they are now (LEC GT-1): a peer
	// taken off the trust list stops reaching the agent unconfined at the next turn. th.Trusted was read
	// when the kernel handed the turn over.
	gate := gateFor(cfg, th.Trusted)
	if gate.mode == "" {
		return nil // untrusted=off: left in the inbox for the operator
	}
	if m.sbRefused.before(th) {
		return nil // this turn already failed closed; wait for a new message
	}

	turns, err := m.conversationTurns(th, cfg.MaxHistory)
	if err != nil {
		return err
	}
	if len(turns) == 0 || turns[len(turns)-1].Role != "user" {
		return nil // nothing owed: the last word is ours (or there is no content yet)
	}

	// Runaway guard: cap how many times we auto-send in one interaction. Without this, two autopilot
	// agents (each auto-replying in both directions) would ping-pong forever and burn tokens. At the cap
	// we propose `end` once instead of replying, so the interaction concludes gracefully (a peer autopilot
	// accepts the end; a human can review the transcript). See autoReplyMaxReplies.
	maxReplies := autoReplyMaxReplies
	ourReplies := 0
	for _, msg := range th.Messages {
		if msg.From == "me" && (msg.Body != "" || len(msg.Attachments) > 0) {
			ourReplies++
		}
	}
	if ourReplies >= maxReplies {
		if th.EndReqBy != "me" {
			if err := m.turns.EndTurn(ctx, th.Ix); err != nil {
				return fmt.Errorf("propose end at auto-reply cap: %w", err)
			}
			execwork.Sessions(m.env.StateDir).Del(th.Ix)
			log.Printf("anet: auto-reply %s: reached auto-reply cap (%d) — proposed end", th.Ix, maxReplies)
		}
		return nil
	}

	if !gate.counted {
		return m.replyTurn(ctx, cfg, replier, gate, th, turns, nil)
	}
	// An untrusted peer's run takes its place in the budget before anything
	// starts (LEC BU-2 to BU-5).
	ticket, verdict, wait := m.reserveRun(cfg, th.Peer)
	switch verdict {
	case budgetOff:
		return nil // a budget of 0 is untrusted=off: left in the inbox for the operator
	case budgetBusy:
		return nil // no free slot: the next scan tries again; nothing is refused
	case budgetSpent:
		return m.refuseTurn(ctx, th, autoReplyRun{Agent: gate.agent(cfg), InteractionID: th.Ix,
			Peer: th.Peer, Trusted: gate.trusted, Mode: gate.mode, Exit: exitBudget}, wait)
	}
	runs.Add(1)
	go func() {
		defer runs.Done()
		if err := m.replyTurn(ctx, cfg, replier, gate, th, turns, ticket); err != nil && !errors.Is(err, module.ErrTurnGone) {
			log.Printf("anet: auto-reply %s: %v", th.Ix, err)
		}
	}()
	return nil
}

// replyTurn runs the backend for one owed turn and sends what it produced. ticket is the turn's place
// in the run budget, nil when it does not count; it is released when the turn ends, and its count is
// given back when nothing started (the sandbox was unavailable).
func (m *autoReplyModule) replyTurn(ctx context.Context, cfg AutoReplyConfig, replier autoReplier, gate execGate, th module.Turn,
	turns []chatTurn, ticket *budgetTicket) error {
	counted := true
	defer func() { ticket.release(counted) }()
	timeout := autoReplyDefaultAPITimeout
	if cfg.APITimeoutSeconds > 0 {
		timeout = time.Duration(cfg.APITimeoutSeconds) * time.Second
	}
	// Attachment outbox: the backend drops any files it wants to deliver here so it never has to call
	// `anet message` itself (doing so caused duplicate sends + a leaked done-marker). The daemon sends the
	// reply text + these files as ONE message and owns end negotiation. It lives in the interaction's
	// exec work dir, outside the data dir (autoreply_exec.go). Creation failure is non-fatal — we just
	// fall back to text-only replies.
	outbox, cleanupOutbox, obErr := m.newExecOutbox(th.Ix, gate.mode == modeSandbox)
	if obErr != nil {
		log.Printf("anet: auto-reply %s: no outbox: %v", th.Ix, obErr)
		outbox = ""
	} else {
		defer cleanupOutbox()
	}

	rctx, cancel := context.WithTimeout(ctx, timeout)
	rc := replyContext{Role: th.Role, Goal: th.Goal, InteractionID: th.Ix, Outbox: outbox}
	if th.Role == roleInbound && gate.mode != modeSandbox {
		// CUX-P5: the openai backend streams when this round is previewed (CUX-P4), and only then.
		if sink, ok := m.turns.PreviewTurn(th.Ix); ok {
			rc.Preview = sink.Offer
		}
	}
	announced := m.announceSlowTurn(th)
	reply, run, err := m.invokeReplier(rctx, cfg, replier, gate, th, rc, turns)
	cancel()
	announced()
	switch run.Exit {
	case exitSandboxUnavailable:
		// Nothing started: the run does not count, and its record is the
		// refusal's (once per interaction).
		counted = false
	default:
		m.recordAutoReplyInvoked(run)
	}
	if errors.Is(err, errSandboxUnavailable) {
		return m.refuseTurn(ctx, th, run, 0)
	}
	if err != nil {
		return m.backendFailed(ctx, th, err)
	}

	// Completion-aware backends append autoReplyDoneSentinel when the task is finished. Send whatever text
	// remains (plus any files the backend dropped in the outbox) as one message, then propose ending so the
	// interaction actually closes instead of drifting into pleasantries until the runaway cap.
	reply, done := stripDoneSentinel(reply)
	atts, oerr := collectOutbox(outbox)
	if oerr != nil {
		return m.backendFailed(ctx, th, fmt.Errorf("outbox: %w", oerr))
	}
	sessions := execwork.Sessions(m.env.StateDir)
	// A provider's reply that completes the task goes in the result, one
	// write with the completion (completeWithReply, CUX-R4, R5): TurnHost.SendTurn with final set.
	if done && th.EndReqBy != "me" && th.Role == roleInbound {
		if err := m.turns.SendTurn(ctx, th.Ix, reply, modAtts(atts), true); err != nil {
			if errors.Is(err, module.ErrTurnGone) {
				return err // denied while the backend worked, or ended: nothing more goes to the peer
			}
			return fmt.Errorf("complete after task-done: %w", err)
		}
		sessions.Del(th.Ix)
		log.Printf("anet: auto-reply %s: backend replied (%d chars, %d attachment(s)) — task done, completed",
			th.Ix, len(reply), len(atts))
		return nil
	}
	if reply != "" || len(atts) > 0 {
		if err := m.turns.SendTurn(ctx, th.Ix, reply, modAtts(atts), false); err != nil {
			return err
		}
		doneNote := ""
		if done {
			doneNote = " — task done, proposing end"
		}
		log.Printf("anet: auto-reply %s: backend replied (%d chars, %d attachment(s))%s", th.Ix, len(reply), len(atts), doneNote)
	}
	if done && th.EndReqBy != "me" {
		if err := m.turns.EndTurn(ctx, th.Ix); err != nil {
			return fmt.Errorf("propose end after task-done: %w", err)
		}
		sessions.Del(th.Ix)
		if reply == "" {
			log.Printf("anet: auto-reply %s: backend signaled task done (no text) — proposed end", th.Ix)
		}
	}
	return nil
}

// modAtts is the outbox files as the seam takes them.
func modAtts(atts []delegation.Attachment) []module.Attachment {
	out := make([]module.Attachment, 0, len(atts))
	for _, a := range atts {
		out = append(out, module.Attachment{Name: a.Name, Mime: a.Mime, Data: a.Data})
	}
	return out
}

// backendFailed is how a turn whose backend failed ends (UX-R3): an inbound task is answered
// status{input-required} with anet.reason backend_failed and no text, which the requester renders in its
// own words, and the task stays open for another try; an outbound one (this node is the requester) has
// nobody to tell. The failure's detail — agent stderr, paths, the model API's answer — goes to the local
// log only; the peer never gets it. The turn is not tried again until the peer writes, so a backend that
// keeps failing costs one call per message, not one per scan.
func (m *autoReplyModule) backendFailed(ctx context.Context, th module.Turn, err error) error {
	log.Printf("anet: auto-reply %s: backend failed: %v", th.Ix, err)
	m.sbRefused.holdFailed(th)
	if th.Role != roleInbound {
		return nil
	}
	return m.turns.RefuseTurn(ctx, th.Ix, module.ReasonBackendFailed, 0)
}

// conversationTurns renders a turn's text messages as backend-agnostic chat turns (peer → user, us →
// assistant), loading image attachment bytes through the seam onto their user turns. maxHistory caps how
// many trailing turns are kept (0 ⇒ default).
func (m *autoReplyModule) conversationTurns(th module.Turn, maxHistory int) (turns []chatTurn, err error) {
	if maxHistory <= 0 {
		maxHistory = autoReplyDefaultMaxHistory
	}
	for _, msg := range th.Messages {
		if msg.Body == "" && len(msg.Attachments) == 0 {
			continue // empty rows are not conversation (control messages were left out by the kernel)
		}
		turn := chatTurn{Role: "assistant", Text: msg.Body}
		if msg.From == "them" {
			turn.Role = "user"
			for _, a := range msg.Attachments {
				if !strings.HasPrefix(a.Mime, "image/") {
					continue
				}
				data, aerr := m.turns.TurnAttachment(th.Ix, a.CID)
				if aerr != nil {
					return nil, fmt.Errorf("load attachment %s: %w", a.CID, aerr)
				}
				turn.Images = append(turn.Images, chatImage{Mime: a.Mime, Data: data})
			}
		}
		if turn.Text == "" && len(turn.Images) == 0 {
			continue
		}
		turns = append(turns, turn)
	}
	if len(turns) > maxHistory {
		turns = turns[len(turns)-maxHistory:]
	}
	return turns, nil
}

// --- "openai" backend ---

// openAIReplier answers a conversation by POSTing it to any OpenAI-compatible /chat/completions endpoint
// (ollama / vLLM / llama.cpp server / SGLang / a cloud API). Images ride inline as base64 data URLs — the
// de-facto standard all of the above accept.
type openAIReplier struct {
	cfg AutoReplyConfig
}

func (r *openAIReplier) Reply(ctx context.Context, rc replyContext, turns []chatTurn) (string, error) {
	_, turns = boundConversation("", turns) // LEC AV-9, as for the exec backend
	system := r.cfg.SystemPrompt
	if system == "" {
		system = autoReplyDefaultSystemPrompt
	}
	msgs := []map[string]any{{"role": "system", "content": system}}
	for _, t := range turns {
		if len(t.Images) == 0 {
			msgs = append(msgs, map[string]any{"role": t.Role, "content": t.Text})
			continue
		}
		parts := []map[string]any{}
		if t.Text != "" {
			parts = append(parts, map[string]any{"type": "text", "text": t.Text})
		}
		for _, img := range t.Images {
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:" + img.Mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)},
			})
		}
		msgs = append(msgs, map[string]any{"role": t.Role, "content": parts})
	}
	req0 := map[string]any{"model": r.cfg.Model, "messages": msgs}
	if rc.Preview != nil {
		req0["stream"] = true // CUX-P5: asked only when a preview is wanted; otherwise the call is as ever
	}
	body, err := json.Marshal(req0)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(r.cfg.APIBase, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if rc.Preview != nil && resp.StatusCode/100 == 2 && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		// One call, never repeated without stream: a failure here is a failed call (CUX-P5).
		return readChatStream(resp.Body, rc.Preview)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("model API %s: %s", resp.Status, truncate(string(raw), 300))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("model API returned non-JSON: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("model API returned no content")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// EvAutoReplyInvoked records one auto-reply turn that ran, or was turned away before it started
// (LEC §2.5): which agent, for which interaction and peer, whether the peer was trusted, how and where
// it ran, how it ended and how long it took, and, for a sandboxed run, what it did through its egress
// endpoint.
const EvAutoReplyInvoked = "anet.autoreply.invoked"

// How a turn runs (LEC GT-1, GT-4): the mode member of anet.autoreply.invoked.
const (
	modeDirect  = "direct"  // the exec backend as the operator's user
	modeSandbox = "sandbox" // the exec backend in the sandbox
	modeOpenAI  = "openai"  // the openai backend: no local process
)

// How a turn ended: the exit member of anet.autoreply.invoked.
const (
	exitOK                 = "ok"
	exitError              = "error"
	exitSandboxUnavailable = "sandbox_unavailable" // turned away before the agent started (GT-2)
	exitBudget             = "budget"              // turned away by the run budget (BU-4)
	exitResource           = "resource"            // killed for its disk use (BU-8)
)

// execGate is the auto-reply gating decision for one turn.
type execGate struct {
	exec    bool   // the backend runs a local program
	trusted bool   // the peer is on the trust list
	mode    string // modeDirect, modeSandbox or modeOpenAI; "" when nothing runs (untrusted=off)
	counted bool   // the turn counts against the run budget for untrusted peers (BU-1)
}

// gateFor decides how a turn runs (LEC GT-1, GT-4, BU-1). The exec backend runs a trusted peer's turn
// as auto_reply.trusted says (direct by default) and an untrusted peer's in the sandbox when
// auto_reply.untrusted=sandbox, else not at all; a turn decided for the sandbox never falls back to a
// direct run (GT-2). The openai backend starts no local process and answers every peer the lists let
// through, but an untrusted peer's turn counts against the budget like a sandboxed run.
func gateFor(cfg AutoReplyConfig, trusted bool) execGate {
	if cfg.Backend != "exec" {
		return execGate{trusted: trusted, mode: modeOpenAI, counted: !trusted}
	}
	g := execGate{exec: true, trusted: trusted}
	switch {
	case trusted && cfg.TrustedMode() == TrustedSandbox:
		g.mode = modeSandbox
	case trusted:
		g.mode = modeDirect
	case cfg.UntrustedMode() == UntrustedSandbox:
		g.mode, g.counted = modeSandbox, true
	}
	return g
}

// agent is the agent member of the turn's record: the exec backend's agent, none for openai.
func (g execGate) agent(cfg AutoReplyConfig) string {
	if g.exec {
		return cfg.Agent
	}
	return ""
}

// invokeReplier runs the backend for one turn under the gating decision: a direct run and the openai
// backend go to the configured replier; a sandboxed one to runSandboxed, which never falls back to an
// unconfined run. It returns the turn's record for the caller to write.
func (m *autoReplyModule) invokeReplier(ctx context.Context, cfg AutoReplyConfig, replier autoReplier, gate execGate,
	th module.Turn, rc replyContext, turns []chatTurn) (string, autoReplyRun, error) {
	run := autoReplyRun{Agent: gate.agent(cfg), InteractionID: th.Ix, Peer: th.Peer,
		Trusted: gate.trusted, Mode: gate.mode}
	start := time.Now()
	var reply string
	var err error
	if gate.mode == modeSandbox {
		var res sandboxResult
		res, err = m.runSandboxed(ctx, sandboxRequest{Cfg: cfg, RC: rc, Turns: turns})
		reply, run.Sandboxed, run.Egress = res.Reply, res.Sandboxed, res.Egress
		if res.Sandboxed {
			run.Kind = sandboxKind
		}
	} else {
		reply, err = replier.Reply(ctx, rc, turns)
	}
	run.Duration = time.Since(start)
	run.Exit, run.Unavailable = exitOf(err)
	return reply, run, err
}

// exitOf is how a turn that returned err ended, and for one turned away before its agent started the
// local reason (ux §1.4 H) when there is one.
func exitOf(err error) (exit, unavailable string) {
	var pe *sandboxProbeError
	switch {
	case err == nil:
		return exitOK, ""
	case errors.Is(err, errSandboxResource):
		return exitResource, ""
	case errors.Is(err, errSandboxUnavailable):
		if errors.As(err, &pe) {
			return exitSandboxUnavailable, pe.Code
		}
		return exitSandboxUnavailable, ""
	}
	return exitError, ""
}

// autoReplyRun is one anet.autoreply.invoked record (LEC §2.5).
type autoReplyRun struct {
	Agent, InteractionID, Peer string
	Trusted, Sandboxed         bool
	Mode                       string // modeDirect, modeSandbox, modeOpenAI
	Kind                       string // the sandbox it ran in: "bwrap", "seatbelt", or ""
	Exit                       string // exitOK … exitResource
	Unavailable                string // with exitSandboxUnavailable: the local reason, if known
	Duration                   time.Duration
	Egress                     *egressSummary // a sandboxed run's egress counts (EG-7)
}

// payload is the record's payload: exactly the members of LEC §2.5 — never a path, a request body, a
// prompt, the model's output or a key.
func (r autoReplyRun) payload() map[string]any {
	p := map[string]any{
		"agent": r.Agent, "interaction_id": r.InteractionID, "peer_aid": r.Peer,
		"trusted": r.Trusted, "sandboxed": r.Sandboxed, "duration_ms": r.Duration.Milliseconds(),
		"mode": r.Mode, "sandbox": r.Kind, "exit": r.Exit,
	}
	if r.Unavailable != "" {
		p["unavailable"] = r.Unavailable
	}
	if e := r.Egress; e != nil {
		counts := func(m map[string]int) map[string]any {
			out := make(map[string]any, len(m))
			for k, v := range m {
				out[k] = v
			}
			return out
		}
		p["egress"] = map[string]any{
			"model_requests": e.ModelRequests, "bytes_up": e.BytesUp, "bytes_down": e.BytesDown,
			"policy_refused": counts(e.PolicyRefused),
			"connect": map[string]any{
				"allowed": counts(e.Connect.Allowed), "refused": counts(e.Connect.Refused),
				"bytes_up": e.Connect.BytesUp, "bytes_down": e.Connect.BytesDown,
			},
		}
	}
	return p
}

func (m *autoReplyModule) recordAutoReplyInvoked(run autoReplyRun) {
	if err := m.host.RecordEvidence(EvAutoReplyInvoked, run.payload()); err != nil {
		log.Printf("anet: auto-reply evidence: %v", err)
	}
}

// sandboxRefused remembers the turns that were turned away: an outbound turn is not tried again every
// few seconds (a new message from the peer is a new turn), and a turn turned away again for the same
// reason is not logged or recorded again (LEC GT-2).
type sandboxRefused struct {
	mu    sync.Mutex
	m     map[string]int             // interaction id -> message count at an outbound refusal
	noted map[string]map[string]bool // interaction id -> the exits already logged and recorded
	// failed: interaction id -> the peer's message count when its backend failed on the turn (UX-R3).
	failed map[string]int
	// seen: interaction id -> when it was last among the owed turns; what is not seen for a week is
	// forgotten (prune), which bounds the memory without the active set the module no longer has.
	seen map[string]time.Time
}

// refusedForget is how long an interaction may be out of the owed turns before the memory of it goes.
const refusedForget = 7 * 24 * time.Hour

// before reports whether this turn of th was already turned away, or its backend already failed on it.
func (s *sandboxRefused) before(th module.Turn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.m[th.Ix]; ok && n == th.MsgCount {
		return true
	}
	n, ok := s.failed[th.Ix]
	return ok && n == th.PeerMsgCount
}

// holdFailed keeps the turn th, whose backend failed, from being tried again until the peer writes.
func (s *sandboxRefused) holdFailed(th module.Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed == nil {
		s.failed = map[string]int{}
	}
	s.failed[th.Ix] = th.PeerMsgCount
}

// hold keeps the outbound turn th from being tried again until the peer writes.
func (s *sandboxRefused) hold(th module.Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]int{}
	}
	s.m[th.Ix] = th.MsgCount
}

// first reports whether ix is turned away for exit for the first time, and notes it.
func (s *sandboxRefused) first(ix, exit string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.noted[ix][exit] {
		return false
	}
	if s.noted == nil {
		s.noted = map[string]map[string]bool{}
	}
	if s.noted[ix] == nil {
		s.noted[ix] = map[string]bool{}
	}
	s.noted[ix][exit] = true
	return true
}

// prune notes the interactions that are owed now and forgets the ones not owed for a week.
func (s *sandboxRefused) prune(owed map[string]bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]time.Time{}
	}
	for ix := range owed {
		s.seen[ix] = now
	}
	forget := func(ix string) bool {
		if owed[ix] {
			return false
		}
		t, ok := s.seen[ix]
		if !ok {
			s.seen[ix] = now // first sight of an entry made without a scan: start its clock
			return false
		}
		return now.Sub(t) > refusedForget
	}
	for ix := range s.m {
		if forget(ix) {
			delete(s.m, ix)
		}
	}
	for ix := range s.noted {
		if forget(ix) {
			delete(s.noted, ix)
		}
	}
	for ix := range s.failed {
		if forget(ix) {
			delete(s.failed, ix)
		}
	}
	for ix, t := range s.seen {
		if !owed[ix] && now.Sub(t) > refusedForget {
			delete(s.seen, ix)
		}
	}
}

// refuseTurn turns a turn away by direction (LEC GT-2, BU-4, BU-8): an inbound task is answered
// status{rejected} through the outbound queue, with anet.reason (budget, or sandbox_unavailable for
// every local reason the sandbox could not run it) and for budget the wait until the next local
// midnight, and no text — the requester renders the reason itself (ux UX-R1); an outbound one (this
// node is the requester) sends nothing and stays in the inbox. A refusal before anything started is
// logged and recorded once per interaction and reason; a run killed for its disk use was recorded by
// its caller, since it counts.
func (m *autoReplyModule) refuseTurn(ctx context.Context, th module.Turn, run autoReplyRun, retryAfter time.Duration) error {
	reason := module.ReasonSandboxUnavailable
	if run.Exit == exitBudget {
		reason = module.ReasonBudget
	}
	if m.sbRefused.first(th.Ix, run.Exit) {
		if run.Exit != exitResource {
			m.recordAutoReplyInvoked(run)
		}
		why := run.Exit
		if run.Unavailable != "" {
			why += " (" + run.Unavailable + ")"
		}
		dir := "nothing sent, the task stays in the inbox"
		if th.Role == roleInbound {
			dir = "the task is refused"
		}
		log.Printf("anet: auto-reply %s: turned away: %s; %s", th.Ix, why, dir)
	}
	if th.Role != roleInbound {
		m.sbRefused.hold(th)
		return nil
	}
	return m.turns.RefuseTurn(ctx, th.Ix, reason, retryAfter)
}

// --- streaming (CUX-P5) ---

const (
	// maxChatStream bounds what one streamed answer may hold, text and reasoning together, like the
	// non-streamed answer's 8 MiB.
	maxChatStream = 8 << 20
	// maxSSELine bounds one line of the stream.
	maxSSELine = 1 << 20
	// previewOfferEvery is the least time between two offers to the coalescer while a stream is read; the
	// last change is always offered when the stream ends or stops.
	previewOfferEvery = 250 * time.Millisecond
)

// readChatStream reads an OpenAI-compatible SSE answer (data: lines, [DONE]; delta.content, and
// delta.reasoning_content or delta.reasoning for the reasoning), gives what has accumulated to offer as
// it grows, and returns the answer as the non-streamed call would have: the text, trimmed. A line that
// does not parse is ignored; a stream that breaks or ends before [DONE] or a finish_reason, or that holds
// no text, is an error.
func readChatStream(body io.Reader, offer func(text, reasoning string)) (string, error) {
	var text, reasoning strings.Builder
	sc := bufio.NewScanner(io.LimitReader(body, maxChatStream+maxSSELine))
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
	var last time.Time
	pending, finished := false, false
	flush := func() {
		offer(text.String(), reasoning.String())
		last, pending = time.Now(), false
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			finished = true
			break
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil || len(ev.Choices) == 0 {
			continue
		}
		if fr := ev.Choices[0].FinishReason; fr != nil && *fr != "" {
			finished = true
		}
		d := ev.Choices[0].Delta
		r := d.ReasoningContent
		if r == "" {
			r = d.Reasoning
		}
		if d.Content == "" && r == "" {
			continue
		}
		if text.Len()+reasoning.Len()+len(d.Content)+len(r) > maxChatStream {
			return "", fmt.Errorf("model API stream over the %d MiB limit", maxChatStream>>20)
		}
		text.WriteString(d.Content)
		reasoning.WriteString(r)
		pending = true
		if time.Since(last) >= previewOfferEvery {
			flush()
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("model API stream: %w", err)
	}
	if pending {
		flush()
	}
	if !finished {
		return "", fmt.Errorf("model API stream ended before it finished")
	}
	out := strings.TrimSpace(text.String())
	if out == "" {
		return "", fmt.Errorf("model API returned no content")
	}
	return out, nil
}

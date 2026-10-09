package daemon

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
//   - Input contract: if the input does not fit (e.g. a vision-only service got no image and
//     require_image is set), the loop replies with the configured usage hint INSTEAD of calling the
//     backend — the requester immediately learns how to use this service.
//   - Backend failures are reported to the requester as a normal reply (error_reply + detail) and the
//     loop moves on; since that reply is ours, there is no retry storm.
//   - End negotiation: when the peer proposes ending, the loop accepts — the daemon then issues the
//     signed receipt and the requester can review us.
//   - The autoReplier seam is deliberately tiny so future backends (e.g. "exec": spawn a local coding
//     agent like cursor/claude to compose the reply) plug in without touching the loop.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

const (
	autoReplyDefaultInterval   = 5 * time.Second
	autoReplyDefaultMaxHistory = 20
	autoReplyDefaultAPITimeout = 180 * time.Second
	// autoReplyDefaultMaxReplies caps how many messages WE auto-send in one interaction before proposing
	// `end` instead of replying again. This is the runaway guard: two autopilot agents talking to each
	// other (I delegate to you, you reply, I reply, …) would otherwise ping-pong forever and burn tokens.
	autoReplyDefaultMaxReplies = 30

	autoReplyDefaultSystemPrompt = "You are a helpful assistant serving requests delegated over AgentNetwork. Reply in the language of the request."
	autoReplyDefaultUsageHint    = "你好！这个 agent 是一个自动化服务，我目前无法处理这条输入。请查看我在 Hub 上的自述（readme）了解正确的使用方式。"
	autoReplyDefaultErrorReply   = "抱歉，我这边的服务暂时出错了，请稍后重试。"

	// autoReplyDoneSentinel is the marker a completion-aware backend appends to its reply when the task is
	// finished and no further exchange is needed. The loop strips it, sends whatever text remains, then
	// proposes `end` — so two autopilots converge on a delivered result instead of chatting pleasantries
	// until the runaway cap. Both roles may emit it; the peer's autopilot auto-accepts the end proposal.
	autoReplyDoneSentinel = "<<ANET_TASK_DONE>>"
)

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
// loudly at startup instead of silently never replying.
func newAutoReplier(cfg AutoReplyConfig, layout Layout) (autoReplier, error) {
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
		return &execReplier{cfg: cfg, dataDir: layout.Root, layout: layout, sessions: sharedExecSessionStore(layout.Root)}, nil
	default:
		return nil, fmt.Errorf("auto_reply: unknown backend %q (supported: openai, exec)", cfg.Backend)
	}
}

// --- loop lifecycle ---

// startAutoReply validates the config and starts the background loop under the daemon ctx. Called from
// New when config.json carries an auto_reply block; a config change requires a daemon restart.
func (d *Daemon) startAutoReply(cfg AutoReplyConfig) {
	replier, err := newAutoReplier(cfg, d.layout)
	if err != nil {
		log.Printf("anet: auto-reply DISABLED: %v", err)
		return
	}
	// Before the loop's first sandboxed run: what an earlier process's runs left (LEC SB-9).
	if cfg.Backend == "exec" {
		d.sweepSandboxLeftovers()
	}
	interval := autoReplyDefaultInterval
	if cfg.PollIntervalSeconds > 0 {
		interval = time.Duration(cfg.PollIntervalSeconds) * time.Second
	}
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
	// Run under a child context stored on the daemon so SetAutoReply can stop/restart the loop live.
	ctx, cancel := context.WithCancel(d.ctx)
	d.mu.Lock()
	if d.autoReplyStop != nil {
		d.autoReplyStop()
	}
	d.autoReplyStop = cancel
	d.mu.Unlock()
	// A background goroutine like the others, so that Close waits for the
	// loop, and the untrusted turns a scan waits for, before it closes the
	// store they read and write. When the daemon is already closing nothing
	// starts.
	if !d.goBackground(func() { d.autoReplyLoop(ctx, cfg, replier, interval) }) {
		cancel()
	}
}

// SetAutoReply reconfigures the built-in auto-reply loop live (no daemon restart): it validates cfg,
// persists it to config.json, stops any running loop, and starts the new one. Passing nil turns
// auto-reply OFF (stops the loop and clears the config block). This is the CLI-accessible switch behind
// `anet autoreply set|off` — an agent (or human) never has to hand-edit config.json.
func (d *Daemon) SetAutoReply(cfg *AutoReplyConfig) error {
	if cfg != nil {
		if _, err := newAutoReplier(*cfg, d.layout); err != nil {
			return err // fail loudly on a bad config instead of silently persisting a loop that never runs
		}
		if ref := checkAutoReplyConfig(cfg, d.layout.Root); ref != nil {
			return ref
		}
		if cfg.Sandbox.empty() {
			c := *cfg
			c.Sandbox = nil
			cfg = &c
		}
	}
	// Saved first and in force after (SetInboundPolicy): a write that
	// failed to save leaves the running loop and the config as they were.
	d.cfgWrite.Lock()
	defer d.cfgWrite.Unlock()
	next := d.config()
	next.AutoReply = cfg
	// The same check as at start and on inbound policy writes (A2A-DESIGN
	// §5.1).
	if err := validatePolicy(next, d.untrustedBackend.Load()); err != nil {
		return err
	}
	if err := SaveConfig(d.layout, next); err != nil {
		return err
	}
	d.mu.Lock()
	from := d.cfg.AutoReply
	if d.autoReplyStop != nil {
		d.autoReplyStop()
		d.autoReplyStop = nil
	}
	d.cfg.AutoReply = cfg
	d.mu.Unlock()
	d.recordPolicyChange("auto_reply", autoReplySummary(from), autoReplySummary(cfg), nil)
	recordEgressAllowChange(d, from, cfg)
	recordTrustedChange(d, from, cfg)
	recordBudgetChange(d, from, cfg)
	if cfg != nil {
		d.startAutoReply(*cfg)
	} else {
		log.Printf("anet: auto-reply OFF")
	}
	return nil
}

// autoReplyDefaultTestPrompt is the synthetic self-check used by `anet autoreply test` when the operator
// gives no prompt. It exercises the whole backend path (API reachable / agent spawns + returns text).
const autoReplyDefaultTestPrompt = "自检：请用一句话说明你是谁、由什么驱动、能帮我做什么。"

// TestAutoReply runs the CURRENTLY configured auto-reply backend once against a synthetic single-turn
// conversation and returns the reply — WITHOUT touching the Hub or creating any identity. This is the
// pollution-free self-test behind `anet autoreply test`: previously operators had to spin up a throwaway
// identity and delegate to themselves, which left a dead node registered on the Hub.
func (d *Daemon) TestAutoReply(ctx context.Context, prompt string) (string, error) {
	d.mu.Lock()
	arp := d.cfg.AutoReply
	d.mu.Unlock()
	if arp == nil {
		return "", fmt.Errorf("auto-reply is not configured — run `anet autoreply set …` first")
	}
	replier, err := newAutoReplier(*arp, d.layout)
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

func (d *Daemon) autoReplyLoop(ctx context.Context, cfg AutoReplyConfig, replier autoReplier, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.autoReplyOnce(ctx, cfg, replier)
		case <-d.autoReplyKick: // an inbound message just landed — service it now, don't wait for the tick
			d.autoReplyOnce(ctx, cfg, replier)
		}
	}
}

// autoReplyOnce scans the local store (the relay loop keeps it fresh) and services every conversation
// that owes an action — in BOTH directions: tasks others delegated to us (inbound) AND tasks we delegated
// to others where the peer has since replied (outbound). "Owing a reply" is symmetric: whenever the last
// message is the peer's (not ours), we answer.
//
// A trusted peer's turn runs in the scan, one after another. An untrusted peer's turn runs beside the
// scan, at most two at once and one per peer (LEC BU-5): a turn that finds no free slot waits for the
// next scan. The scan returns once every turn it started has ended, so no thread is ever answered twice
// at once and the next scan sees what this one sent.
func (d *Daemon) autoReplyOnce(ctx context.Context, cfg AutoReplyConfig, replier autoReplier) {
	// ActiveThreads skips finished (done/failed) interactions before loading their message history, so this
	// every-few-seconds loop stays O(active) rather than O(all-history) as completed tasks accumulate.
	threads, err := d.ActiveThreads()
	if err != nil {
		log.Printf("anet: auto-reply: list threads: %v", err)
		return
	}
	active := make(map[string]bool, len(threads))
	for _, th := range threads {
		active[th.InteractionID] = true
	}
	// Keep the exec session map bounded: any binding whose interaction is no longer active (ended/failed/
	// gone) can never be resumed again (ids are unique), so drop it. This also mops up bindings orphaned
	// by an abrupt requester exit.
	if cfg.Backend == "exec" {
		sharedExecSessionStore(d.layout.Root).pruneExcept(active)
	}
	d.sbRefused.prune(active)
	var runs sync.WaitGroup
	defer runs.Wait()
	for _, th := range threads {
		if ctx.Err() != nil {
			return
		}
		if interactions.State(th.State).IsTerminal() || th.IsCapability || th.Trust == interactions.TrustPublicCap {
			// Ended interactions get no further replies, and capability calls
			// never reach auto-reply (A2A-DESIGN §6): ActiveThreads already
			// leaves them out; this keeps the rule where it is applied.
			continue
		}
		if err := d.autoReplyThread(ctx, cfg, replier, th, &runs); err != nil {
			log.Printf("anet: auto-reply %s: %v", th.InteractionID, err)
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
func (d *Daemon) announceSlowTurn(th Thread) func() {
	if th.Role != string(interactions.RoleInbound) || th.State != string(interactions.StateSubmitted) ||
		th.IsCapability || slowTurnAfter <= 0 {
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
		case <-d.ctx.Done():
			return
		}
		ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
		defer cancel()
		if err := d.SendStatus(ctx, th.InteractionID, interactions.StateWorking, "", nil); err != nil && d.ctx.Err() == nil {
			log.Printf("anet: auto-reply %s: tell the requester the backend is working on it: %v", th.InteractionID, err)
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-finished
	}
}

// autoReplyThread services ONE conversation (inbound or outbound): produce the reply we owe (usage
// hint / backend answer / error report), or end the task at the runaway cap or when the backend says
// it is done. No-op when nothing is owed. A requester's end request needs no reply: the provider
// daemon completes on receiving it (A2A-DESIGN §4.2). An untrusted peer's turn is started on runs and
// left running (autoReplyOnce).
func (d *Daemon) autoReplyThread(ctx context.Context, cfg AutoReplyConfig, replier autoReplier, th Thread, runs *sync.WaitGroup) error {
	// A task a module's A2A backend is given is answered there, not here
	// as well (A2A-DESIGN §11.6, inbound_tasks.go).
	if d.backendAnswers(th) {
		return nil
	}
	// The lists are read on every turn, whatever the backend: a peer put
	// on the deny list stops reaching the model or the agent at the next
	// turn, not at the next revocation sweep (A2A-DESIGN §5.1).
	ps := d.readPeers()
	if ps.denied(th.Peer) {
		return nil
	}
	// How this turn runs is decided on every turn from the lists as they
	// are now (LEC GT-1): a peer taken off the trust list stops reaching
	// the agent unconfined at the next turn.
	gate := gateFor(cfg, ps.trusted(th.Peer))
	if gate.mode == "" {
		return nil // untrusted=off: left in the inbox for the operator
	}
	if d.sbRefused.before(th) {
		return nil // this turn already failed closed; wait for a new message
	}

	turns, hasImage, err := d.conversationTurns(th, cfg.MaxHistory)
	if err != nil {
		return err
	}
	if len(turns) == 0 || turns[len(turns)-1].Role != "user" {
		return nil // nothing owed: the last word is ours (or there is no content yet)
	}

	// Runaway guard: cap how many times we auto-send in one interaction. Without this, two autopilot
	// agents (each auto-replying in both directions) would ping-pong forever and burn tokens. At the cap
	// we propose `end` once instead of replying, so the interaction concludes gracefully (a peer autopilot
	// accepts the end; a human can review the transcript). See autoReplyDefaultMaxReplies.
	maxReplies := cfg.MaxAutoReplies
	if maxReplies <= 0 {
		maxReplies = autoReplyDefaultMaxReplies
	}
	ourReplies := 0
	for _, m := range th.Messages {
		if m.Kind == interactions.MsgText && m.From == "me" && (m.Body != "" || len(m.Attachments) > 0) &&
			!hasControlMeta(string(m.Metadata)) {
			ourReplies++
		}
	}
	if ourReplies >= maxReplies {
		if th.EndReqBy != "me" {
			hctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
			defer cancel()
			if err := d.RequestEnd(hctx, th.InteractionID); err != nil {
				return fmt.Errorf("propose end at auto-reply cap: %w", err)
			}
			sharedExecSessionStore(d.layout.Root).del(th.InteractionID)
			log.Printf("anet: auto-reply %s: reached auto-reply cap (%d) — proposed end", th.InteractionID, maxReplies)
		}
		return nil
	}

	// Input contract: a vision-only service without any image replies with usage instructions instead of
	// burning a backend call it cannot serve.
	if cfg.RequireImage && !hasImage {
		hint := cfg.UsageHint
		if hint == "" {
			hint = autoReplyDefaultUsageHint
		}
		log.Printf("anet: auto-reply %s: input misses the contract (no image) — sending usage hint", th.InteractionID)
		return d.sendAutoReply(ctx, th.InteractionID, hint)
	}

	if !gate.counted {
		return d.replyTurn(ctx, cfg, replier, gate, th, turns, nil)
	}
	// An untrusted peer's run takes its place in the budget before anything
	// starts (LEC BU-2 to BU-5).
	ticket, verdict, wait := d.reserveRun(cfg, th.Peer)
	switch verdict {
	case budgetBusy:
		return nil // no free slot: the next scan tries again; nothing is refused
	case budgetSpent:
		return d.refuseTurn(ctx, th, autoReplyRun{Agent: gate.agent(cfg), InteractionID: th.InteractionID,
			Peer: th.Peer, Trusted: gate.trusted, Mode: gate.mode, Exit: exitBudget}, wait)
	}
	runs.Add(1)
	go func() {
		defer runs.Done()
		if err := d.replyTurn(ctx, cfg, replier, gate, th, turns, ticket); err != nil {
			log.Printf("anet: auto-reply %s: %v", th.InteractionID, err)
		}
	}()
	return nil
}

// replyTurn runs the backend for one owed turn and sends what it produced. ticket is the turn's place
// in the run budget, nil when it does not count; it is released when the turn ends, and its count is
// given back when nothing started (the sandbox was unavailable).
func (d *Daemon) replyTurn(ctx context.Context, cfg AutoReplyConfig, replier autoReplier, gate execGate, th Thread,
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
	outbox, cleanupOutbox, obErr := d.newExecOutbox(th.InteractionID, gate.mode == modeSandbox)
	if obErr != nil {
		log.Printf("anet: auto-reply %s: no outbox: %v", th.InteractionID, obErr)
		outbox = ""
	} else {
		defer cleanupOutbox()
	}

	rctx, cancel := context.WithTimeout(ctx, timeout)
	rc := replyContext{Role: th.Role, Goal: th.Goal, InteractionID: th.InteractionID, Outbox: outbox}
	announced := d.announceSlowTurn(th)
	reply, run, err := d.invokeReplier(rctx, cfg, replier, gate, th, rc, turns)
	cancel()
	announced()
	switch run.Exit {
	case exitSandboxUnavailable:
		// Nothing started: the run does not count, and its record is the
		// refusal's (once per interaction).
		counted = false
	default:
		d.recordAutoReplyInvoked(run)
	}
	if d.readPeers().denied(th.Peer) {
		// Denied while the backend worked: nothing more goes to the peer.
		return nil
	}
	if errors.Is(err, errSandboxUnavailable) {
		return d.refuseTurn(ctx, th, run, 0)
	}
	if err != nil {
		// Report the failure to the requester as a normal message. That reply is ours, so the loop will
		// not retry this turn — no error storm; the requester can simply ask again. The peer gets the
		// configured error reply and a reference id; the detail goes to the local log only.
		return d.sendAutoReply(ctx, th.InteractionID, autoReplyFailureReply(cfg, th.InteractionID, err))
	}

	// Completion-aware backends append autoReplyDoneSentinel when the task is finished. Send whatever text
	// remains (plus any files the backend dropped in the outbox) as one message, then propose ending so the
	// interaction actually closes instead of drifting into pleasantries until the runaway cap.
	reply, done := stripDoneSentinel(reply)
	atts, oerr := collectOutbox(outbox)
	if oerr != nil {
		return d.sendAutoReply(ctx, th.InteractionID, autoReplyFailureReply(cfg, th.InteractionID, fmt.Errorf("outbox: %w", oerr)))
	}
	// A provider's reply that completes the task is its final reply (0017
	// Q30): marked so the requester keeps waiting for the result that
	// follows instead of stopping at an input-required between the two.
	var meta map[string]any
	if done && th.EndReqBy != "me" && th.Role == string(interactions.RoleInbound) {
		meta = a2ashape.FinalReplyMetadata()
	}
	if reply != "" || len(atts) > 0 {
		if err := d.sendAutoReplyAttachments(ctx, th.InteractionID, reply, atts, meta); err != nil {
			return err
		}
		doneNote := ""
		if done {
			doneNote = " — task done, proposing end"
		}
		log.Printf("anet: auto-reply %s: backend replied (%d chars, %d attachment(s))%s", th.InteractionID, len(reply), len(atts), doneNote)
	}
	if done && th.EndReqBy != "me" {
		hctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
		defer cancel()
		if err := d.RequestEnd(hctx, th.InteractionID); err != nil {
			return fmt.Errorf("propose end after task-done: %w", err)
		}
		sharedExecSessionStore(d.layout.Root).del(th.InteractionID)
		if reply == "" {
			log.Printf("anet: auto-reply %s: backend signaled task done (no text) — proposed end", th.InteractionID)
		}
	}
	return nil
}

func (d *Daemon) sendAutoReply(ctx context.Context, interactionID, body string) error {
	return d.sendAutoReplyAtts(ctx, interactionID, body, nil)
}

// sendAutoReplyAtts sends the auto-reply text plus any local files the backend produced (outbox) as a
// single relayed message.
func (d *Daemon) sendAutoReplyAtts(ctx context.Context, interactionID, body string, attachPaths []string) error {
	sctx, cancel := context.WithTimeout(ctx, relayCallTimeout)
	defer cancel()
	return d.SendMessage(sctx, interactionID, body, attachPaths)
}

// conversationTurns renders a thread's text messages as backend-agnostic chat turns (peer → user, us →
// assistant), loading image attachment bytes from the local store onto their user turns. hasImage reports
// whether ANY peer turn carried an image (drives the require_image contract). maxHistory caps how many
// trailing turns are kept (0 ⇒ default).
func (d *Daemon) conversationTurns(th Thread, maxHistory int) (turns []chatTurn, hasImage bool, err error) {
	if maxHistory <= 0 {
		maxHistory = autoReplyDefaultMaxHistory
	}
	for _, m := range th.Messages {
		if m.Kind != interactions.MsgText || (m.Body == "" && len(m.Attachments) == 0) ||
			hasControlMeta(string(m.Metadata)) {
			continue // end requests, cancels, status and payment messages, and empty rows are not conversation
		}
		turn := chatTurn{Role: "assistant", Text: m.Body}
		if m.From == "them" {
			turn.Role = "user"
			for _, a := range m.Attachments {
				if !strings.HasPrefix(a.Mime, "image/") {
					continue
				}
				_, mime, data, aerr := d.AttachmentBytes(th.InteractionID, a.CID)
				if aerr != nil {
					return nil, false, fmt.Errorf("load attachment %s: %w", a.CID, aerr)
				}
				turn.Images = append(turn.Images, chatImage{Mime: mime, Data: data})
				hasImage = true
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
	return turns, hasImage, nil
}

// --- "openai" backend ---

// openAIReplier answers a conversation by POSTing it to any OpenAI-compatible /chat/completions endpoint
// (ollama / vLLM / llama.cpp server / SGLang / a cloud API). Images ride inline as base64 data URLs — the
// de-facto standard all of the above accept.
type openAIReplier struct {
	cfg AutoReplyConfig
}

func (r *openAIReplier) Reply(ctx context.Context, _ replyContext, turns []chatTurn) (string, error) {
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
	body, err := json.Marshal(map[string]any{"model": r.cfg.Model, "messages": msgs})
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
func (d *Daemon) invokeReplier(ctx context.Context, cfg AutoReplyConfig, replier autoReplier, gate execGate,
	th Thread, rc replyContext, turns []chatTurn) (string, autoReplyRun, error) {
	run := autoReplyRun{Agent: gate.agent(cfg), InteractionID: th.InteractionID, Peer: th.Peer,
		Trusted: gate.trusted, Mode: gate.mode}
	start := time.Now()
	var reply string
	var err error
	if gate.mode == modeSandbox {
		var res sandboxResult
		res, err = d.runSandboxed(ctx, sandboxRequest{Cfg: cfg, RC: rc, Turns: turns})
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

func (d *Daemon) recordAutoReplyInvoked(run autoReplyRun) {
	if d.ledger == nil {
		return
	}
	if _, err := d.ledger.Append(EvAutoReplyInvoked, run.payload()); err != nil {
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
}

// before reports whether this turn of th was already turned away.
func (s *sandboxRefused) before(th Thread) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.m[th.InteractionID]
	return ok && n == len(th.Messages)
}

// hold keeps the outbound turn th from being tried again until the peer writes.
func (s *sandboxRefused) hold(th Thread) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]int{}
	}
	s.m[th.InteractionID] = len(th.Messages)
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

// prune forgets the interactions that are no longer active.
func (s *sandboxRefused) prune(active map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ix := range s.m {
		if !active[ix] {
			delete(s.m, ix)
		}
	}
	for ix := range s.noted {
		if !active[ix] {
			delete(s.noted, ix)
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
func (d *Daemon) refuseTurn(ctx context.Context, th Thread, run autoReplyRun, retryAfter time.Duration) error {
	reason := reasonSandboxUnavailble
	if run.Exit == exitBudget {
		reason = "budget"
	}
	if d.sbRefused.first(th.InteractionID, run.Exit) {
		if run.Exit != exitResource {
			d.recordAutoReplyInvoked(run)
		}
		why := run.Exit
		if run.Unavailable != "" {
			why += " (" + run.Unavailable + ")"
		}
		dir := "nothing sent, the task stays in the inbox"
		if th.Role == string(interactions.RoleInbound) {
			dir = "the task is refused"
		}
		log.Printf("anet: auto-reply %s: turned away: %s; %s", th.InteractionID, why, dir)
	}
	if th.Role != string(interactions.RoleInbound) {
		d.sbRefused.hold(th)
		return nil
	}
	meta := map[string]any{a2ashape.KeyReason: reason}
	if run.Exit == exitBudget {
		meta[a2ashape.KeyRetryAfterMS] = max(retryAfter.Milliseconds(), 1)
	}
	sctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	return d.SendStatus(sctx, th.InteractionID, interactions.StateRejected, "", meta)
}

// autoReplySummary is the part of an auto-reply configuration a policy
// change event records: the backend, the agent, how untrusted and trusted
// peers' turns run, the run budgets and whether an API key is set (LEC
// §2.5: never the key). Prompts and paths are left out.
func autoReplySummary(c *AutoReplyConfig) map[string]any {
	if c == nil {
		return nil
	}
	backend := c.Backend
	if backend == "" {
		backend = "openai"
	}
	daily, perPeer := c.runBudgets()
	return map[string]any{"backend": backend, "agent": c.Agent, "untrusted": c.UntrustedMode(),
		"trusted": c.TrustedMode(), "runs_per_day": daily, "runs_per_peer_per_day": perPeer,
		"api_key_set": c.APIKey != ""}
}

// recordTrustedChange writes anet.policy.changed when a configuration write changes how trusted peers'
// turns run (LEC §2.5). Turning auto-reply on or off is the auto_reply record's.
func recordTrustedChange(d *Daemon, from, to *AutoReplyConfig) {
	if from == nil || to == nil {
		return
	}
	if a, b := from.TrustedMode(), to.TrustedMode(); a != b {
		d.recordPolicyChange("auto_reply.trusted", a, b, nil)
	}
}

// writeConfigRefusal answers a configuration write the checks refused (budget_invalid,
// work_dir_invalid, config_invalid; LEC §2.3.4) with 400 and the error body with its reason (UX-R9),
// and reports whether err was one.
func writeConfigRefusal(w http.ResponseWriter, err error) bool {
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) {
		return false
	}
	writeRefusal(w, http.StatusBadRequest, ref)
	return true
}

// checkTrustedMode is config_invalid for an auto_reply.trusted that is neither direct nor sandbox (LEC
// §3.2).
func checkTrustedMode(c *AutoReplyConfig) *a2ashape.Refusal {
	if c == nil {
		return nil
	}
	switch c.Trusted {
	case "", TrustedDirect, TrustedSandbox:
		return nil
	}
	return a2ashape.Refuse(reasonConfigInvalid, a2ashape.Args{"key": "auto_reply.trusted",
		"rule": ruleText("只能是 direct 或 sandbox", "it must be direct or sandbox")})
}

// checkAutoReplyConfig is the refusal for an auto-reply configuration this area does not take (LEC
// §3.2): auto_reply.trusted neither direct nor sandbox, a run budget out of range, or a work_dir in the
// data dir or the sandbox work root. dataDir empty skips the data dir.
func checkAutoReplyConfig(c *AutoReplyConfig, dataDir string) *a2ashape.Refusal {
	if r := checkTrustedMode(c); r != nil {
		return r
	}
	if r := checkRunBudgets(c); r != nil {
		return r
	}
	return checkWorkDir(c, dataDir)
}

// The auto-reply configuration is checked when the daemon starts (LEC §3.2); the data dir is checked
// by New, which knows it (Daemon.validate).
func init() {
	configChecks = append(configChecks, func(c Config, _ map[string]any) *a2ashape.Refusal {
		if r := checkTrustedMode(c.AutoReply); r != nil {
			return r
		}
		return checkWorkDir(c.AutoReply, "")
	})
}

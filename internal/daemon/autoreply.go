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
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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

// collectOutboxFiles returns the plain files a backend dropped in its outbox (top-level only), sorted for
// stable ordering. These are attached to the auto-reply the daemon sends.
func collectOutboxFiles(dir string) []string {
	if dir == "" {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Strings(files)
	return files
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
	go d.autoReplyLoop(ctx, cfg, replier, interval)
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
	}
	d.mu.Lock()
	if d.autoReplyStop != nil {
		d.autoReplyStop()
		d.autoReplyStop = nil
	}
	d.cfg.AutoReply = cfg
	saved := d.cfg
	d.mu.Unlock()
	if err := SaveConfig(d.layout, saved); err != nil {
		return err
	}
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
// message is the peer's (not ours), we answer. Threads are handled sequentially: replies are model calls
// and the loop must never answer the same thread twice concurrently.
func (d *Daemon) autoReplyOnce(ctx context.Context, cfg AutoReplyConfig, replier autoReplier) {
	// ActiveThreads skips finished (done/failed) interactions before loading their message history, so this
	// every-few-seconds loop stays O(active) rather than O(all-history) as completed tasks accumulate.
	threads, err := d.ActiveThreads()
	if err != nil {
		log.Printf("anet: auto-reply: list threads: %v", err)
		return
	}
	// Keep the exec session map bounded: any binding whose interaction is no longer active (ended/failed/
	// gone) can never be resumed again (ids are unique), so drop it. This also mops up bindings orphaned
	// by an abrupt requester exit.
	if cfg.Backend == "exec" {
		active := make(map[string]bool, len(threads))
		for _, th := range threads {
			active[th.InteractionID] = true
		}
		sharedExecSessionStore(d.layout.Root).pruneExcept(active)
	}
	for _, th := range threads {
		if ctx.Err() != nil {
			return
		}
		if th.Status == "done" || th.Status == "failed" {
			continue // finished interactions get no further replies (receipt already issued)
		}
		if err := d.autoReplyThread(ctx, cfg, replier, th); err != nil {
			log.Printf("anet: auto-reply %s: %v", th.InteractionID, err)
		}
	}
}

// autoReplyThread services ONE conversation (inbound or outbound): accept a pending end proposal, or
// produce the reply we owe (usage hint / backend answer / error report). No-op when nothing is owed.
func (d *Daemon) autoReplyThread(ctx context.Context, cfg AutoReplyConfig, replier autoReplier, th Thread) error {
	// The peer proposed ending and we have not answered → accept, so the receipt is issued.
	if th.EndReqBy == "them" && th.EndAccBy == "" {
		hctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
		defer cancel()
		if err := d.AcceptEnd(hctx, th.InteractionID); err != nil {
			return fmt.Errorf("accept end: %w", err)
		}
		sharedExecSessionStore(d.layout.Root).del(th.InteractionID) // free the agent session bound to this interaction
		log.Printf("anet: auto-reply %s: peer proposed ending — accepted", th.InteractionID)
		return nil
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
		if m.Kind == "text" && m.From == "me" && (m.Body != "" || len(m.Attachments) > 0) {
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

	timeout := autoReplyDefaultAPITimeout
	if cfg.APITimeoutSeconds > 0 {
		timeout = time.Duration(cfg.APITimeoutSeconds) * time.Second
	}
	// Attachment outbox: the backend drops any files it wants to deliver here so it never has to call
	// `anet message` itself (doing so caused duplicate sends + a leaked done-marker). The daemon sends the
	// reply text + these files as ONE message and owns end negotiation. Creation failure is non-fatal —
	// we just fall back to text-only replies.
	outbox, obErr := os.MkdirTemp(d.layout.Root, "exec-outbox-")
	if obErr != nil {
		outbox = ""
	} else {
		defer os.RemoveAll(outbox)
	}

	rctx, cancel := context.WithTimeout(ctx, timeout)
	reply, err := replier.Reply(rctx, replyContext{Role: th.Role, Goal: th.Goal, InteractionID: th.InteractionID, Outbox: outbox}, turns)
	cancel()
	if err != nil {
		// Report the failure to the requester as a normal message. That reply is ours, so the loop will
		// not retry this turn — no error storm; the requester can simply ask again.
		log.Printf("anet: auto-reply %s: backend failed: %v", th.InteractionID, err)
		errReply := cfg.ErrorReply
		if errReply == "" {
			errReply = autoReplyDefaultErrorReply
		}
		return d.sendAutoReply(ctx, th.InteractionID, errReply+"\n\n(technical detail: "+err.Error()+")")
	}

	// Completion-aware backends append autoReplyDoneSentinel when the task is finished. Send whatever text
	// remains (plus any files the backend dropped in the outbox) as one message, then propose ending so the
	// interaction actually closes instead of drifting into pleasantries until the runaway cap.
	reply, done := stripDoneSentinel(reply)
	atts := collectOutboxFiles(outbox)
	if reply != "" || len(atts) > 0 {
		if err := d.sendAutoReplyAtts(ctx, th.InteractionID, reply, atts); err != nil {
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
		if m.Kind != "text" || (m.Body == "" && len(m.Attachments) == 0) {
			continue // end-negotiation handshakes and empty rows are not conversation
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

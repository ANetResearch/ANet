package daemon

// autoreply_exec.go — auto_reply backend "exec": spawn a local coding agent (cursor / claude / codex /
// openclaw / hermes) headlessly to compose the reply. anet still sends the message; the agent only
// returns text (and may use tools, including the anet CLI, while running).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const autoReplyDefaultExecPrompt = `You are an AgentNetwork autopilot handling ONE task over anet. Read the conversation below, do any real
work needed (you may run shell commands), then produce the single next message to send to the other party.
Output ONLY that message text — no preamble, no markdown code fences wrapping the whole reply. If image
attachment paths are listed, you may open or inspect those files.

DELIVERY — let anet do the sending; do NOT drive anet yourself:
- Do NOT run "anet message" / "anet end" (or otherwise act on this interaction via the anet CLI). Just
  RETURN your reply as your final output; anet sends it and handles ending. Sending it yourself causes
  duplicate messages and leaks control markers into the chat.
- To attach files (code, images, archives, …), WRITE them as plain files into the directory named by the
  $ANET_OUTBOX environment variable. anet delivers everything in $ANET_OUTBOX together with your reply text.

DRIVE THE TASK TO A REAL RESULT — then end; do not stall OR quit early:
- Judge against the GOAL stated below what "actually done" means.
- If you are the REQUESTER: you act on the requester's behalf to actually GET THE GOAL DONE. If the provider
  needs an input to do its job (e.g. a vision service asks for an image, a tool asks for a file or specific
  details), PROVIDE it and continue — obtain or create a suitable file yourself and drop it in $ANET_OUTBOX,
  or supply the missing detail. Do NOT end just because the provider replied, restated how to use it, or
  asked you for something: that means the task is NOT done yet. Only once you actually hold a result that
  satisfies the goal do you confirm briefly and finish. Never trade pleasantries or invent extra work.
- If you are the PROVIDER: propose ending ONLY after you have delivered a concrete requested deliverable and
  nothing is pending. A greeting, an identity question ("你是哪位"/"who are you"), a capability question
  ("你能干什么"), or any opener where the requester has NOT yet stated a concrete task is NOT a task to finish:
  answer it helpfully and WAIT for their real request — do NOT propose ending. When in doubt about whether the
  requester is done, do NOT end; let THEM propose ending (anet auto-accepts).
- ONLY when the task is genuinely complete and no further exchange is needed, append a final line containing
  EXACTLY:
  <<ANET_TASK_DONE>>
  anet will send your message and then propose ending the task (the other side accepts, a signed receipt is
  issued). Do NOT append the marker when: (a) the other side is still waiting on you to act (e.g. they asked
  for an image you have not sent), OR (b) you are now waiting on THEM — you just sent an input, file, or
  question that needs their substantive reply (e.g. you attached the image and asked them to describe it), OR
  (c) the incoming message is only a greeting / identity / capability question and no concrete task has been
  stated yet: in all these cases send WITHOUT the marker and wait. Only append the marker once a concrete
  requested deliverable is in hand and nothing further is needed.`

// execRoleContext renders the "## Task" block injected between the system prompt and the conversation:
// which side we are (requester who set the goal, or provider serving it) plus the goal itself, so the
// agent can judge completion against a concrete target.
func execRoleContext(rc replyContext) string {
	role := "the PROVIDER serving a task another agent delegated to you"
	if rc.Role == "outbound" {
		role = "the REQUESTER — you delegated this goal and the other party is working it"
	}
	var b strings.Builder
	b.WriteString("\n\n## Task\n\nYour role: You are ")
	b.WriteString(role)
	b.WriteString(".")
	if g := strings.TrimSpace(rc.Goal); g != "" {
		b.WriteString("\nGoal: ")
		b.WriteString(g)
	}
	return b.String()
}

// execReplier invokes a configured local coding agent once per owed reply.
type execReplier struct {
	cfg      AutoReplyConfig
	dataDir  string
	layout   Layout
	sessions *execSessionStore // interaction_id -> agent-native chat id (cursor session mode)
}

func (r *execReplier) sessionStore() *execSessionStore {
	if r.sessions == nil {
		r.sessions = sharedExecSessionStore(r.layout.Root)
	}
	return r.sessions
}

func (r *execReplier) Reply(ctx context.Context, rc replyContext, turns []chatTurn) (string, error) {
	base := r.baseOpts(rc)

	// Cursor uses NATIVE SESSIONS: bind this interaction to its own cursor chat and send only the new
	// message each turn (the agent keeps the conversation itself). This replaces transcript replay for
	// cursor; anet still stores the full transcript for observation. Other backends keep the replay
	// path below. If a session cannot be established (e.g. `create-chat` unavailable), we fall through.
	if r.cfg.Agent == agentCursor && rc.InteractionID != "" {
		if reply, handled, err := r.replyCursorSession(ctx, rc, turns, base); handled {
			return reply, err
		}
	}

	prompt, cleanup, err := r.buildPrompt(turns)
	if err != nil {
		return "", err
	}
	defer cleanup()
	system := r.cfg.SystemPrompt
	if system == "" {
		system = autoReplyDefaultExecPrompt
	}
	base.Prompt = system + execRoleContext(rc) + "\n\n## Conversation\n\n" + prompt
	return InvokeAgent(ctx, base)
}

// replyCursorSession runs one turn against the persistent cursor chat bound to rc.InteractionID.
// Returns (reply, handled, err); handled=false means session mode was unavailable — caller falls back
// to the legacy full-transcript one-shot.
func (r *execReplier) replyCursorSession(ctx context.Context, rc replyContext, turns []chatTurn, base execInvokeOpts) (string, bool, error) {
	store := r.sessionStore()
	sid := store.get(rc.InteractionID)
	firstTurn := sid == ""

	// Only the turns since our last reply are new to the agent's session.
	delta := turnsSinceLastAssistant(turns)
	if len(delta) == 0 {
		return "", false, nil
	}
	body, cleanup, err := r.buildPrompt(delta)
	if err != nil {
		return "", true, err
	}
	defer cleanup()

	if firstTurn {
		newID, err := cursorCreateChat(ctx, base)
		if err != nil || !looksLikeSessionID(newID) {
			return "", false, nil // degrade to replay path
		}
		sid = newID
		store.set(rc.InteractionID, sid)
		// Seed the fresh chat with the system prompt + role/goal framing (once).
		system := r.cfg.SystemPrompt
		if system == "" {
			system = autoReplyDefaultExecPrompt
		}
		body = system + execRoleContext(rc) + "\n\n## Conversation\n\n" + body
	}

	base.SessionID = sid
	base.Prompt = body
	reply, err := InvokeAgent(ctx, base)
	return reply, true, err
}

// baseOpts assembles the invocation options shared by both the session and replay paths (Prompt and
// SessionID are filled in by the caller).
func (r *execReplier) baseOpts(rc replyContext) execInvokeOpts {
	workDir := r.cfg.WorkDir
	if workDir == "" {
		workDir = r.dataDir
	}
	if abs, err := filepath.Abs(workDir); err == nil {
		workDir = abs
	}
	env := append([]string(nil), agentExecEnv(r.cfg)...)
	env = append(env, "ANET_DATA_DIR="+r.dataDir)
	if rc.Outbox != "" {
		env = append(env, "ANET_OUTBOX="+rc.Outbox)
	}
	timeout := autoReplyDefaultAPITimeout
	if r.cfg.APITimeoutSeconds > 0 {
		timeout = time.Duration(r.cfg.APITimeoutSeconds) * time.Second
	}
	return execInvokeOpts{
		AgentID:       r.cfg.Agent,
		WorkDir:       workDir,
		Model:         r.cfg.Model,
		OpenClawAgent: r.cfg.OpenClawAgent,
		Command:       r.cfg.Command,
		ExtraArgs:     r.cfg.ExtraArgs,
		Env:           env,
		Timeout:       timeout,
	}
}

func (r *execReplier) buildPrompt(turns []chatTurn) (prompt string, cleanup func(), err error) {
	cleanup = func() {}
	var b strings.Builder
	tmpParent := filepath.Join(r.layout.Root, "exec-tmp")
	_ = os.MkdirAll(tmpParent, 0o700)
	tmpDir, err := os.MkdirTemp(tmpParent, "run-")
	if err != nil {
		return "", cleanup, err
	}
	cleanup = func() { _ = os.RemoveAll(tmpDir) }

	for i, t := range turns {
		role := "Requester"
		if t.Role == "assistant" {
			role = "You (previous reply)"
		}
		if t.Text != "" {
			fmt.Fprintf(&b, "%s: %s\n\n", role, t.Text)
		}
		for j, img := range t.Images {
			ext := ".bin"
			switch {
			case strings.Contains(img.Mime, "jpeg"), strings.Contains(img.Mime, "jpg"):
				ext = ".jpg"
			case strings.Contains(img.Mime, "png"):
				ext = ".png"
			case strings.Contains(img.Mime, "webp"):
				ext = ".webp"
			case strings.Contains(img.Mime, "gif"):
				ext = ".gif"
			}
			name := fmt.Sprintf("turn%d-img%d%s", i, j, ext)
			path := filepath.Join(tmpDir, name)
			if err := os.WriteFile(path, img.Data, 0o600); err != nil {
				return "", cleanup, err
			}
			fmt.Fprintf(&b, "%s attachment: %s (%s)\n\n", role, path, img.Mime)
		}
	}
	return strings.TrimSpace(b.String()), cleanup, nil
}

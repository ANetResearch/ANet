package autoreply

// agents.go — registry of the external coding agents the exec auto-reply backend can run headless
// (`auto_reply.backend = "exec"`). Supported: cursor, claude, codex, opencode, openclaw, hermes.
// Registering anet's MCP server with these tools is internal/agentwire (`anet agents wire`).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ANetResearch/ANet/internal/fsperm"
	"github.com/ANetResearch/ANet/internal/pathsafe"
	"github.com/ANetResearch/ANet/internal/procgroup"
)

const (
	agentCursor   = "cursor"
	agentClaude   = "claude"
	agentCodex    = "codex"
	agentOpenCode = "opencode"
	agentOpenClaw = "openclaw"
	agentHermes   = "hermes"
)

// execInvokeOpts carries one headless agent invocation (built by the exec auto-reply backend).
//
// What the other party wrote never reaches a command line (LEC AV-1, AV-2): /proc/<pid>/cmdline is
// readable by every local user, and a single argument is capped at 128 KiB. Instructions is fixed text
// (the operator's system prompt or the default, the role, the notices) and may go on argv, through the
// agent's system prompt flag where it has one. Conversation is the goal and the turns: it goes to the
// agent on stdin, or, for an agent that reads no prompt from stdin, into a 0600 file in the work dir
// whose path is the only thing argv says about it (agentSpec.input).
type execInvokeOpts struct {
	AgentID string
	// Instructions is the fixed part of the prompt; "" on a turn that continues an agent-native session.
	Instructions string
	// Conversation is the peer-authored part of the prompt.
	Conversation string
	// InputDir is the turn's private input directory in the work dir (.anet-in-<rand>, removed after the
	// turn), where a file-input agent's conversation.md is written; "" makes one for the call.
	InputDir      string
	WorkDir       string
	Model         string
	OpenClawAgent string
	Command       string // optional binary override (config, or execCommandForTest in tests)
	ExtraArgs     []string
	// Env is the agent's complete environment (see execEnv); nothing else from the daemon's
	// environment is inherited.
	Env       []string
	Timeout   time.Duration
	SessionID string // resume this agent-native chat/session instead of a fresh one (cursor: --resume)
	// Sandbox, when set, runs the agent inside the bubblewrap sandbox it describes.
	Sandbox *sandboxPlan
}

// agentInput is how an agent receives the conversation (LEC AV-2, AV-4).
type agentInput int

const (
	// inputStdin: the conversation is the agent's stdin (claude -p, codex exec -).
	inputStdin agentInput = iota
	// inputFile: the conversation is written to conversation.md in the turn's input directory, and the
	// agent's argv carries only the fixed instructions and one fixed sentence naming that file.
	inputFile
)

type agentSpec struct {
	id, displayName string
	// input is the agent's conversation channel.
	input agentInput
	// sandbox is how the agent runs in the sandbox (LEC AV-4); nil for an agent v0.3.0 does not run there,
	// whose sandboxed turns fail closed (sandbox_unavailable, local detail agent_unsupported, with
	// sandboxNote as the detail).
	sandbox     *agentSandbox
	sandboxNote string
	// defaultModel is the CHEAPEST model to use when the operator did not pass auto_reply.model — an
	// unattended provider should default to the cheapest tier, not a premium one. Empty means "let the
	// agent CLI use its own configured default" (used where we can't name a safe cheap model / the CLI
	// takes no model flag). See InvokeAgent.
	defaultModel string
	detectBin    func() (string, error)
	invoke       func(ctx context.Context, o execInvokeOpts) (string, error)
}

// agentRegistry is every agent the exec backend runs. The channels (input, and where each invoke puts the
// fixed instructions) and the sandbox configurations are the ones the per-agent trials established
// (LEC AV-4, EXE-6; the versions tried are in the comments of agents_sandbox.go).
func agentRegistry() []agentSpec {
	return []agentSpec{
		{id: agentCursor, displayName: "Cursor Agent", input: inputFile, defaultModel: "auto", detectBin: detectCursorBin, invoke: invokeCursor,
			sandboxNote: "cursor has no model service address to point at this node"},
		{id: agentClaude, displayName: "Claude Code", input: inputStdin, defaultModel: "haiku", detectBin: detectClaudeBin, invoke: invokeClaude,
			sandbox: claudeSandbox},
		{id: agentCodex, displayName: "Codex CLI", input: inputStdin, detectBin: detectCodexBin, invoke: invokeCodex,
			sandbox: codexSandbox},
		{id: agentOpenCode, displayName: "opencode", input: inputStdin, detectBin: detectOpenCodeBin, invoke: invokeOpenCode,
			sandbox: openCodeSandbox},
		{id: agentOpenClaw, displayName: "OpenClaw", input: inputFile, detectBin: detectOpenClawBin, invoke: invokeOpenClaw,
			sandbox: openClawSandbox},
		{id: agentHermes, displayName: "hermes-agent", input: inputFile, detectBin: detectHermesBin, invoke: invokeHermes,
			sandbox: hermesSandbox},
	}
}

// SupportedExecAgents lists agent ids valid for auto_reply.backend=exec.
func SupportedExecAgents() []string {
	r := agentRegistry()
	ids := make([]string, 0, len(r))
	for _, a := range r {
		ids = append(ids, a.id)
	}
	return ids
}

func lookupAgent(id string) (agentSpec, error) {
	for _, a := range agentRegistry() {
		if a.id == id {
			return a, nil
		}
	}
	return agentSpec{}, fmt.Errorf("unknown agent %q (supported: %s)", id, strings.Join(SupportedExecAgents(), ", "))
}

// InvokeAgent runs one headless agent turn and returns its final reply text.
func InvokeAgent(ctx context.Context, o execInvokeOpts) (string, error) {
	if o.AgentID == "" {
		return "", fmt.Errorf("exec agent: agent id required")
	}
	spec, err := lookupAgent(o.AgentID)
	if err != nil {
		return "", err
	}
	if o.Model == "" {
		o.Model = spec.defaultModel // cheapest tier by default for an unattended provider
	}
	if o.Command == "" {
		o.Command = execCommandForTest
	}
	if o.Command == "" {
		bin, err := spec.detectBin()
		if err != nil {
			return "", fmt.Errorf("exec agent %s: %w", o.AgentID, err)
		}
		o.Command = bin
	}
	if o.Timeout <= 0 {
		o.Timeout = autoReplyDefaultAPITimeout
	}
	if o.Env == nil {
		// A caller that assembled no environment gets the allowlisted one, never the daemon's own.
		o.Env = execEnv(AutoReplyConfig{Agent: o.AgentID}, o.Sandbox, "")
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	return spec.invoke(ctx, o)
}

func lookPathFirst(names ...string) (string, error) {
	for _, n := range names {
		if p, err := lookAgentPath(n); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found on PATH", strings.Join(names, ", "))
}

// lookAgentPath is exec.LookPath for an agent binary, except that a program found only relative to the
// daemon's working directory counts as not found (exec.ErrDot, or a relative result where a GODEBUG
// setting turned that error off): an agent never runs from the daemon's working directory.
func lookAgentPath(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		return "", &exec.Error{Name: name, Err: exec.ErrDot}
	}
	return p, nil
}

// runCmd runs one agent CLI to completion, or kills it and everything it started when ctx expires.
// It runs in o.WorkDir with exactly o.Env as its environment, and inside the sandbox when o.Sandbox is
// set. stdin is what the agent reads on its standard input (nil: nothing, /dev/null); in the sandbox it
// travels on fd 4 to init, which hands it to the agent (LEC SB-5, §2.6.5). It is how the conversation
// reaches the agent without being on any command line (AV-1).
//
// The process group and the WaitDelay are both load-bearing. A coding agent CLI is a launcher: it
// spawns model calls, language servers, sometimes a shell of its own. CommandContext's own watchdog
// signals only the process it started, so on a timeout the launcher died and its children kept the
// stdout pipe open — and cmd.Run blocks until that pipe reaches EOF. The configured timeout was
// therefore not a bound on anything: a hung agent held the auto-reply loop for as long as its
// children lived, and a node with one stuck task stopped answering everything else.
//
// Measured before the fix: a 2s timeout against an agent that slept 60s returned after 60s.
//
// cmd.Cancel replaces that watchdog rather than racing it — two killers waiting on the same Wait
// means which one wins decides whether the group survives. WaitDelay then bounds the wait for any
// pipe still held by something that escaped the group entirely (a child that called setsid). A
// sandboxed agent runs under bwrap --die-with-parent in its own PID namespace, so killing bwrap ends
// every process inside.
func runCmd(ctx context.Context, o execInvokeOpts, stdin []byte, bin string, args ...string) (stdout, stderr []byte, err error) {
	// On Windows a batch file runs through cmd.exe, which does not receive the arguments intact.
	if err := refuseBatchAgent(bin); err != nil {
		return nil, nil, err
	}
	// Always set, even when empty: a nil Env would make the child inherit the daemon's whole
	// environment.
	env := append([]string{}, o.Env...)
	var extra []*os.File
	var in io.Reader
	if stdin != nil {
		in = bytes.NewReader(stdin)
	}
	if o.Sandbox != nil {
		// The sandbox runs bwrap around anet's own init; the agent's command and environment travel in
		// the spec on fd 3, its stdin on fd 4, none of it on bwrap's command line (sandbox_linux.go).
		l, err := o.Sandbox.command(o.WorkDir, append([]string{bin}, args...), o.Env, stdin)
		if err != nil {
			return nil, nil, err
		}
		defer l.Close()
		bin, args, env, extra, in = l.Path, l.Args, l.Env, l.Files, nil
	}
	// The agent's output is the peer's to cause (a prompt injection is enough to make an agent print in a
	// loop), so it is kept up to a bound and the run is killed when it passes it: pipes are covered by no
	// RLIMIT_FSIZE and no disk watch, and the daemon's heap is outside the sandbox's cgroup.
	parent := ctx
	ctx, stop := context.WithCancel(parent)
	defer stop()
	over := &outputOverflow{stop: stop}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = o.WorkDir
	cmd.Env = env
	cmd.Stdin = in
	cmd.ExtraFiles = extra
	// procgroup puts the command in a process group of its own (Setpgid) and makes cmd.Cancel kill
	// the whole group; on Windows the group is a Job object that is terminated as a whole.
	cmd.WaitDelay = 3 * time.Second
	outBuf, errBuf := &cappedBuffer{max: agentStdoutMax, over: over}, &cappedBuffer{max: agentStderrMax, over: over}
	cmd.Stdout, cmd.Stderr = outBuf, errBuf
	err = procgroup.Run(cmd)
	if over.hit.Load() {
		return outBuf.Bytes(), errBuf.Bytes(), fmt.Errorf("%w (more than %d bytes on stdout or %d on stderr)", errAgentOutputTooLarge, agentStdoutMax, agentStderrMax)
	}
	// A killed group reports the kill, not the timeout that caused it. Say which it was, because
	// "signal: killed" sends an operator looking for an OOM that did not happen.
	if ctxErr := parent.Err(); err != nil && ctxErr != nil {
		err = fmt.Errorf("%w (agent did not finish: %v)", ctxErr, err)
	}
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// How much of an agent's output the daemon keeps. A reply is text and a few KiB; 4 MiB of stdout covers a
// verbose JSON event stream, and stderr is only for the log (400 bytes of it are used).
const (
	agentStdoutMax = 4 << 20
	agentStderrMax = 64 << 10
)

// errAgentOutputTooLarge ends a run that printed more than agentStdoutMax or agentStderrMax: the agent and
// everything it started are killed. In a sandbox the turn ends as resource (errSandboxResource).
var errAgentOutputTooLarge = errors.New("agent output over the limit")

// outputOverflow is shared by an agent's two pipes: the first one to pass its bound kills the run.
type outputOverflow struct {
	hit  atomic.Bool
	stop context.CancelFunc
}

// cappedBuffer keeps the first max bytes written to it. A write past the bound is accepted (so the writer
// is not blocked while the group is being killed) but not kept, and raises the overflow.
type cappedBuffer struct {
	mu   sync.Mutex
	buf  []byte
	max  int
	over *outputOverflow
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	room := c.max - len(c.buf)
	if room > len(p) {
		room = len(p)
	}
	if room > 0 {
		c.buf = append(c.buf, p[:room]...)
	}
	full := room < len(p)
	c.mu.Unlock()
	if full && c.over.hit.CompareAndSwap(false, true) {
		c.over.stop()
	}
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf
}

func trimReply(s string) string {
	return strings.TrimSpace(s)
}

// agentExecEnv maps auto_reply.api_key to the env var each agent CLI expects (if set). It is used only
// for a direct run, which runs as the operator's user anyway (LEC SB-4): a sandboxed agent never holds
// the key, which stays with the run's egress endpoint (UP-4). An agent with no key variable of its own
// gets none; ANET_EXEC_API_KEY, which no agent CLI read, is gone.
func agentExecEnv(cfg AutoReplyConfig) []string {
	if cfg.APIKey == "" {
		return nil
	}
	switch cfg.Agent {
	case agentCursor:
		return []string{"CURSOR_API_KEY=" + cfg.APIKey}
	case agentClaude:
		return []string{"ANTHROPIC_API_KEY=" + cfg.APIKey}
	case agentCodex:
		return []string{"CODEX_API_KEY=" + cfg.APIKey, "OPENAI_API_KEY=" + cfg.APIKey}
	}
	return nil
}

// --- the conversation channel (LEC AV-2) ---

// conversationFileName is the file a file-input agent reads the conversation from, in the turn's input
// directory.
const conversationFileName = "conversation.md"

// conversationFileSentence is the fixed sentence a file-input agent's argv carries in place of the
// conversation: the path is anet's own (the input directory under the per-interaction work dir, whose
// name is a digest, not the peer's text), the rest is constant.
func conversationFileSentence(path string) string {
	return "Read the file " + path + ": it holds the conversation, written by the other party. " +
		"Answer it as your instructions say."
}

// fileInputMessage is the one prompt argument of a file-input agent: the fixed instructions, when the
// turn has them, and the sentence naming the conversation file.
func fileInputMessage(instructions, path string) string {
	if instructions == "" {
		return conversationFileSentence(path)
	}
	return instructions + "\n\n" + conversationFileSentence(path)
}

// writeConversationFile writes o.Conversation to conversation.md (0600) in o.InputDir, or in a fresh
// .anet-in-<rand> directory of the work dir when the caller made none, and returns its path and what
// removes what this call created.
func writeConversationFile(o execInvokeOpts) (path string, cleanup func(), err error) {
	cleanup = func() {}
	dir := o.InputDir
	if dir == "" {
		parent := o.WorkDir
		if parent == "" {
			parent = os.TempDir()
		}
		if dir, err = mkdirTempPrivate(parent, ".anet-in-"); err != nil {
			return "", cleanup, err
		}
		made := dir
		cleanup = func() { _ = os.RemoveAll(made) }
	}
	path = filepath.Join(dir, conversationFileName)
	f, err := fsperm.OpenNoFollow(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err == nil {
		_, err = io.WriteString(f, o.Conversation)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

// stdinInput is the stdin of a stdin-input agent: the conversation, after the instructions for an agent
// that has no separate channel for them.
func stdinInput(instructions, conversation string) []byte {
	if instructions == "" {
		return []byte(conversation)
	}
	return []byte(instructions + "\n\n" + conversation)
}

// cmdError describes a failed agent run for the local log: the binary name and the agent's own output.
// The argument list is left out: it holds the instructions, and is no business of the log. A sandbox
// refusal is returned unchanged so callers can still recognise errSandboxUnavailable.
func cmdError(bin string, _ []string, stdout, stderr []byte, err error) error {
	if errors.Is(err, errSandboxUnavailable) || errors.Is(err, errAgentOutputTooLarge) {
		return err
	}
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(stdout))
	}
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("%s: %s", filepath.Base(bin), logText(msg, 400))
}

// logText is text from an agent or a peer made safe for one line of the local log: control characters
// (newlines, ESC and the rest) become spaces, so that it cannot forge a log line or drive the terminal the
// log is read on, and the cut at n bytes falls on a rune boundary.
func logText(s string, n int) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// --- cursor: agent -p --output-format text [--model] [--workspace] MESSAGE (conversation in a file) ---

func detectCursorBin() (string, error) {
	if p, err := lookPathFirst("agent", "cursor-agent"); err == nil {
		return p, nil
	}
	if p, err := lookAgentPath("cursor"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("cursor agent not found — install Cursor Agent CLI (`agent`) or Cursor IDE")
}

func invokeCursor(ctx context.Context, o execInvokeOpts) (string, error) {
	args := []string{"-p", "--output-format", "text", "--force", "--trust"}
	if o.SessionID != "" {
		args = append(args, "--resume", o.SessionID) // continue this interaction's own chat session
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.WorkDir != "" {
		args = append(args, "--workspace", o.WorkDir)
	}
	args = append(args, o.ExtraArgs...)
	conv, cleanup, err := writeConversationFile(o)
	if err != nil {
		return "", err
	}
	defer cleanup()
	args = append(args, fileInputMessage(o.Instructions, conv))
	bin := o.Command
	if agentBinIs(bin, "cursor") {
		args = append([]string{"agent"}, args...)
	}
	stdout, stderr, err := runCmd(ctx, o, nil, bin, args...)
	if err != nil {
		return "", cmdError(bin, args, stdout, stderr, err)
	}
	reply := trimReply(string(stdout))
	if reply == "" || strings.Contains(reply, "Authentication required") {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = reply
		}
		if strings.Contains(msg, "Authentication required") || strings.Contains(msg, "agent login") {
			return "", fmt.Errorf("cursor agent not authenticated — run `agent login` once (or set auto_reply.api_key / CURSOR_API_KEY)")
		}
		if reply == "" {
			return "", fmt.Errorf("cursor agent returned empty output")
		}
	}
	return reply, nil
}

// cursorCreateChat opens a fresh Cursor chat and returns its id (`cursor-agent create-chat`), so that
// each anet interaction gets its own persistent session and later turns resume it with only the new
// message. Resolves the binary the same way InvokeAgent does (config override → execCommandForTest →
// autodetect) so the test stub is honored.
func cursorCreateChat(ctx context.Context, o execInvokeOpts) (string, error) {
	bin := o.Command
	if bin == "" {
		bin = execCommandForTest
	}
	if bin == "" {
		b, err := detectCursorBin()
		if err != nil {
			return "", err
		}
		bin = b
	}
	args := []string{"create-chat"}
	if agentBinIs(bin, "cursor") {
		args = append([]string{"agent"}, args...)
	}
	stdout, stderr, err := runCmd(ctx, o, nil, bin, args...)
	if err != nil {
		return "", cmdError(bin, args, stdout, stderr, err)
	}
	id := ""
	for _, ln := range strings.Split(string(stdout), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			id = s // the id is the last non-empty line
		}
	}
	return id, nil
}

// --- claude: claude -p --output-format text --permission-mode dontAsk|bypassPermissions --bare [--model]
//     [--append-system-prompt INSTRUCTIONS] < CONVERSATION ---

func detectClaudeBin() (string, error) {
	return lookPathFirst("claude")
}

func invokeClaude(ctx context.Context, o execInvokeOpts) (string, error) {
	args := claudeArgs(o)
	stdout, stderr, err := runCmd(ctx, o, []byte(o.Conversation), o.Command, args...)
	if err != nil {
		return "", cmdError(o.Command, args, stdout, stderr, err)
	}
	reply := trimReply(string(stdout))
	if reply == "" {
		return "", fmt.Errorf("claude returned empty output")
	}
	return reply, nil
}

// claudeArgs is claude's command line for o (the conversation is its stdin).
func claudeArgs(o execInvokeOpts) []string {
	// dontAsk denies every tool that would ask (Bash, Edit, Write), which keeps a direct run to what the
	// operator's own permission rules allow. In the sandbox there are no such rules (its home is empty)
	// and the sandbox is the boundary, so the agent may use its tools there (LEC AV-4, EXE-6).
	mode := "dontAsk"
	if o.Sandbox != nil {
		mode = "bypassPermissions"
	}
	args := []string{"-p", "--output-format", "text", "--permission-mode", mode, "--bare"}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	args = append(args, o.ExtraArgs...)
	// The fixed instructions go through claude's own system prompt channel; with -p and no prompt
	// argument, claude reads the prompt — the conversation — from stdin (LEC AV-4).
	if o.Instructions != "" {
		args = append(args, "--append-system-prompt", o.Instructions)
	}
	return args
}

// --- codex: codex exec --skip-git-repo-check --ephemeral -s MODE -o outfile [-C workdir] [-m MODEL]
//     [-c developer_instructions=INSTRUCTIONS] - < CONVERSATION ---

func detectCodexBin() (string, error) {
	return lookPathFirst("codex")
}

func invokeCodex(ctx context.Context, o execInvokeOpts) (string, error) {
	// The reply file lives in the work dir: a sandboxed codex has its own empty /tmp, and the work dir
	// is the one location both sides see. It is private to this interaction either way.
	dir := o.WorkDir
	if dir == "" {
		dir = os.TempDir()
	}
	outFile := filepath.Join(dir, fmt.Sprintf(".anet-codex-%d.txt", time.Now().UnixNano()))
	defer os.Remove(outFile)
	args := codexArgs(o, outFile)
	stdout, stderr, err := runCmd(ctx, o, []byte(o.Conversation), o.Command, args...)
	if err != nil {
		return "", cmdError(o.Command, args, stdout, stderr, err)
	}
	// Read through the work dir, never through a link put in the reply file's place (readWorkFile).
	if b, err := readWorkFile(dir, filepath.Base(outFile), pathsafe.MaxAttachmentBytes); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return trimReply(string(b)), nil
	}
	reply := trimReply(string(stdout))
	if reply == "" {
		return "", fmt.Errorf("codex exec returned empty output")
	}
	return reply, nil
}

// codexArgs is codex's command line for o, writing its reply to outFile (the conversation is its stdin).
func codexArgs(o execInvokeOpts, outFile string) []string {
	// The work dir is not a git repository, which codex otherwise refuses. --full-auto is gone from codex
	// (0.162 rejects it); its sandbox mode is set directly: workspace-write for a direct run, as
	// --full-auto did; in anet's sandbox codex cannot build its own (it needs a user namespace, which the
	// sandbox refuses), so its commands run in anet's sandbox as they are.
	mode := "workspace-write"
	if o.Sandbox != nil {
		mode = "danger-full-access"
	}
	args := []string{"exec", "--skip-git-repo-check", "--ephemeral", "-s", mode, "-o", outFile}
	if o.WorkDir != "" {
		args = append(args, "-C", o.WorkDir)
	}
	if o.Model != "" {
		args = append(args, "-m", o.Model)
	}
	// The fixed instructions go through codex's developer instructions setting; the conversation is the
	// prompt, read from stdin ("-") (LEC AV-2, AV-4).
	if o.Instructions != "" {
		args = append(args, "-c", "developer_instructions="+tomlString(o.Instructions))
	}
	args = append(args, o.ExtraArgs...)
	return append(args, "-")
}

// --- opencode: opencode run [--model MODEL] -- INSTRUCTIONS < CONVERSATION ---

func detectOpenCodeBin() (string, error) {
	return lookPathFirst("opencode")
}

// invokeOpenCode runs one non-interactive opencode turn.
//
// `opencode run` prints the assistant's reply on stdout and exits, which is exactly the shape this
// backend needs. Its message is its arguments followed by what it reads on stdin: the fixed instructions
// are the argument and the conversation comes on stdin (opencode 1.18, EXE-6). No model is passed unless
// the operator named one: opencode resolves its own default from the provider the user has configured,
// and there is no model id that is both cheap and valid across the providers it supports — naming one
// here would break the node for anybody on a different provider, which is worse than a default that
// costs more than necessary. In the sandbox the model is the pinned one, on the provider the run's
// configuration names (openCodeSandbox).
func invokeOpenCode(ctx context.Context, o execInvokeOpts) (string, error) {
	args := openCodeArgs(o)
	stdout, stderr, err := runCmd(ctx, o, []byte(o.Conversation), o.Command, args...)
	if err != nil {
		return "", cmdError(o.Command, args, stdout, stderr, err)
	}
	reply := trimReply(string(stdout))
	if reply == "" {
		return "", fmt.Errorf("opencode run returned empty output")
	}
	return reply, nil
}

// openCodeArgs is opencode's command line for o (the conversation is its stdin). The instructions follow
// "--": the operator's system_prompt may start with "-", and opencode would read it as an option
// otherwise. opencode takes what follows "--" as its message, as it takes positional words (opencode
// run joins both).
func openCodeArgs(o execInvokeOpts) []string {
	args := []string{"run"}
	if model := o.Model; model != "" {
		if o.Sandbox != nil {
			model = sandboxProviderModel(model)
		}
		args = append(args, "--model", model)
	}
	args = append(args, o.ExtraArgs...)
	if o.Instructions != "" {
		args = append(args, "--", o.Instructions)
	}
	return args
}

// --- openclaw: openclaw agent --local --json --agent ID --message-file FILE, or in the sandbox
//     openclaw agent exec --json --message-file FILE --cwd WORKDIR (instructions and conversation in the
//     file) ---

func detectOpenClawBin() (string, error) {
	return lookPathFirst("openclaw")
}

func invokeOpenClaw(ctx context.Context, o execInvokeOpts) (string, error) {
	agentName := o.OpenClawAgent
	if agentName == "" {
		agentName = "main"
	}
	// openclaw reads its message from a file (--message-file), so the message is the whole prompt — the
	// fixed instructions, then the conversation — in the turn's 0600 conversation file, and argv names
	// only the file. The agent needs no tool call to see the conversation (openclaw 2026.9, EXE-6).
	whole := o
	whole.Conversation = string(stdinInput(o.Instructions, o.Conversation))
	conv, cleanup, err := writeConversationFile(whole)
	if err != nil {
		return "", err
	}
	defer cleanup()
	args := openClawArgs(o, agentName, conv)
	stdout, stderr, err := runCmd(ctx, o, nil, o.Command, args...)
	if err != nil {
		return "", cmdError(o.Command, args, stdout, stderr, err)
	}
	raw := strings.TrimSpace(string(stdout))
	if raw == "" {
		return "", fmt.Errorf("openclaw agent returned empty output")
	}
	return openClawReply(raw), nil
}

// openClawArgs is openclaw's command line for o, its message in the file conv. A direct run is a turn of
// the operator's agent agentName. A sandboxed run is `agent exec`, openclaw's isolated headless turn: its
// tools work in the interaction's work dir (--cwd), not in the agent workspace under $HOME, which the
// sandbox discards with the run, and it bootstraps no workspace files (openclaw 2026.9, EXE-6).
func openClawArgs(o execInvokeOpts, agentName, conv string) []string {
	args := []string{"agent", "--local", "--json", "--agent", agentName, "--message-file", conv}
	if o.Sandbox != nil {
		args = []string{"agent", "exec", "--json", "--message-file", conv}
		if o.WorkDir != "" {
			args = append(args, "--cwd", o.WorkDir)
		}
	}
	if o.Model != "" {
		model := o.Model
		if o.Sandbox != nil {
			model = sandboxProviderModel(model)
		}
		args = append(args, "--model", model)
	}
	return append(args, o.ExtraArgs...)
}

// openClawReply is the reply text of `openclaw agent --json`: the text of its payloads (openclaw
// 2026.9: {"payloads":[{"text":…}],"meta":…}), or one of the single text members older versions
// printed, or, when the output is not such an object, the output itself.
func openClawReply(raw string) string {
	var parsed struct {
		Payloads []struct {
			Text string `json:"text"`
		} `json:"payloads"`
		Reply   string `json:"reply"`
		Message string `json:"message"`
		Text    string `json:"text"`
		Result  string `json:"result"`
		Output  string `json:"output"`
	}
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return trimReply(raw)
	}
	var texts []string
	for _, p := range parsed.Payloads {
		if t := strings.TrimSpace(p.Text); t != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) > 0 {
		return strings.Join(texts, "\n\n")
	}
	for _, s := range []string{parsed.Reply, parsed.Message, parsed.Text, parsed.Result, parsed.Output} {
		if strings.TrimSpace(s) != "" {
			return trimReply(s)
		}
	}
	return trimReply(raw)
}

// --- hermes: hermes --oneshot=MESSAGE [-m MODEL] (conversation in a file) ---

func detectHermesBin() (string, error) {
	return lookPathFirst("hermes")
}

func invokeHermes(ctx context.Context, o execInvokeOpts) (string, error) {
	conv, cleanup, err := writeConversationFile(o)
	if err != nil {
		return "", err
	}
	defer cleanup()
	// --oneshot (-z) is hermes's one-shot mode: it prints only the final reply and approves its own tool
	// calls, and takes -m for the model (hermes 0.19; `chat -q` printed a banner and session lines around
	// the reply). hermes takes its prompt only as an argument, so the conversation stays in the file it
	// names.
	args := hermesArgs(o, conv)
	stdout, stderr, err := runCmd(ctx, o, nil, o.Command, args...)
	if err != nil {
		return "", cmdError(o.Command, args, stdout, stderr, err)
	}
	reply := trimReply(string(stdout))
	if reply == "" {
		return "", fmt.Errorf("hermes returned empty output")
	}
	return reply, nil
}

// hermesArgs is hermes's command line for o, the conversation in the file conv. The message is attached to
// its option (--oneshot=MESSAGE): the operator's system_prompt may start with "-", and hermes would read a
// separate argument starting with "-" as an option of its own rather than as the value of -z.
func hermesArgs(o execInvokeOpts, conv string) []string {
	args := []string{hermesOneShot + fileInputMessage(o.Instructions, conv)}
	if o.Model != "" {
		args = append(args, "-m", o.Model)
	}
	return append(args, o.ExtraArgs...)
}

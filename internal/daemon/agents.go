package daemon

// agents.go — shared registry for wiring anet into external coding agents (`anet install --agent`)
// and for headless one-shot invocation (`auto_reply.backend = "exec"`). Supported: cursor, claude,
// codex, opencode, openclaw, hermes.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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
type execInvokeOpts struct {
	AgentID       string
	Prompt        string
	WorkDir       string
	Model         string
	OpenClawAgent string
	Command       string // optional binary override (config or ANET_EXEC_COMMAND)
	ExtraArgs     []string
	Env           []string
	Timeout       time.Duration
	SessionID     string // resume this agent-native chat/session instead of a fresh one (cursor: --resume)
}

type agentSpec struct {
	id, displayName string
	// defaultModel is the CHEAPEST model to use when the operator did not pass auto_reply.model — an
	// unattended provider should default to the cheapest tier, not a premium one. Empty means "let the
	// agent CLI use its own configured default" (used where we can't name a safe cheap model / the CLI
	// takes no model flag). See InvokeAgent.
	defaultModel string
	detectBin    func() (string, error)
	install      func() ([]string, error)
	invoke       func(ctx context.Context, o execInvokeOpts) (string, error)
}

func agentRegistry() []agentSpec {
	return []agentSpec{
		{id: agentCursor, displayName: "Cursor Agent", defaultModel: "auto", detectBin: detectCursorBin, install: installCursor, invoke: invokeCursor},
		{id: agentClaude, displayName: "Claude Code", defaultModel: "haiku", detectBin: detectClaudeBin, install: installClaude, invoke: invokeClaude},
		{id: agentCodex, displayName: "Codex CLI", detectBin: detectCodexBin, install: installCodex, invoke: invokeCodex},
		{id: agentOpenCode, displayName: "opencode", detectBin: detectOpenCodeBin, install: installOpenCode, invoke: invokeOpenCode},
		{id: agentOpenClaw, displayName: "OpenClaw", detectBin: detectOpenClawBin, install: installOpenClaw, invoke: invokeOpenClaw},
		{id: agentHermes, displayName: "hermes-agent", detectBin: detectHermesBin, install: installHermesCLI, invoke: invokeHermes},
	}
}

// SupportedExecAgents lists agent ids valid for auto_reply.backend=exec and `anet install --agent`.
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
		o.Command = os.Getenv("ANET_EXEC_COMMAND") // test hook: path to a stub binary
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
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	return spec.invoke(ctx, o)
}

func lookPathFirst(names ...string) (string, error) {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found on PATH", strings.Join(names, ", "))
}

// runCmd runs one agent CLI to completion, or kills it and everything it started when ctx expires.
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
// pipe still held by something that escaped the group entirely (a child that called setsid).
func runCmd(ctx context.Context, dir string, env []string, bin string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // negative pid: the whole group
	}
	cmd.WaitDelay = 3 * time.Second
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	// A killed group reports the kill, not the timeout that caused it. Say which it was, because
	// "signal: killed" sends an operator looking for an OOM that did not happen.
	if ctxErr := ctx.Err(); err != nil && ctxErr != nil {
		err = fmt.Errorf("%w (agent did not finish: %v)", ctxErr, err)
	}
	return outBuf.Bytes(), errBuf.Bytes(), err
}

func trimReply(s string) string {
	return strings.TrimSpace(s)
}

// agentExecEnv maps auto_reply.api_key to the env var each agent CLI expects (if set).
func agentExecEnv(cfg AutoReplyConfig) []string {
	if cfg.APIKey == "" {
		return nil
	}
	var out []string
	switch cfg.Agent {
	case agentCursor:
		out = append(out, "CURSOR_API_KEY="+cfg.APIKey)
	case agentClaude:
		out = append(out, "ANTHROPIC_API_KEY="+cfg.APIKey)
	case agentCodex:
		out = append(out, "CODEX_API_KEY="+cfg.APIKey, "OPENAI_API_KEY="+cfg.APIKey)
	default:
		out = append(out, "ANET_EXEC_API_KEY="+cfg.APIKey)
	}
	return out
}

func cmdError(bin string, args []string, stdout, stderr []byte, err error) error {
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(stdout))
	}
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("%s %s: %s", filepath.Base(bin), strings.Join(args, " "), truncate(msg, 400))
}

// --- cursor: agent -p --output-format text [--model] [--workspace] PROMPT ---

func detectCursorBin() (string, error) {
	if p, err := lookPathFirst("agent", "cursor-agent"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("cursor"); err == nil {
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
	args = append(args, o.Prompt)
	bin := o.Command
	if filepath.Base(bin) == "cursor" {
		args = append([]string{"agent"}, args...)
	}
	stdout, stderr, err := runCmd(ctx, o.WorkDir, o.Env, bin, args...)
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
// message. Resolves the binary the same way InvokeAgent does (config override → ANET_EXEC_COMMAND →
// autodetect) so the test stub is honored.
func cursorCreateChat(ctx context.Context, o execInvokeOpts) (string, error) {
	bin := o.Command
	if bin == "" {
		bin = os.Getenv("ANET_EXEC_COMMAND")
	}
	if bin == "" {
		b, err := detectCursorBin()
		if err != nil {
			return "", err
		}
		bin = b
	}
	args := []string{"create-chat"}
	if filepath.Base(bin) == "cursor" {
		args = append([]string{"agent"}, args...)
	}
	stdout, stderr, err := runCmd(ctx, o.WorkDir, o.Env, bin, args...)
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

// --- claude: claude -p --output-format text [--model] [--permission-mode dontAsk] PROMPT ---

func detectClaudeBin() (string, error) {
	return lookPathFirst("claude")
}

func invokeClaude(ctx context.Context, o execInvokeOpts) (string, error) {
	args := []string{"-p", "--output-format", "text", "--permission-mode", "dontAsk", "--bare"}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	args = append(args, o.ExtraArgs...)
	args = append(args, o.Prompt)
	stdout, stderr, err := runCmd(ctx, o.WorkDir, o.Env, o.Command, args...)
	if err != nil {
		return "", cmdError(o.Command, args, stdout, stderr, err)
	}
	reply := trimReply(string(stdout))
	if reply == "" {
		return "", fmt.Errorf("claude returned empty output")
	}
	return reply, nil
}

// --- codex: codex exec [--full-auto] [-C workdir] [-o outfile] PROMPT ---

func detectCodexBin() (string, error) {
	return lookPathFirst("codex")
}

func invokeCodex(ctx context.Context, o execInvokeOpts) (string, error) {
	outFile := filepath.Join(os.TempDir(), fmt.Sprintf("anet-codex-%d.txt", time.Now().UnixNano()))
	defer os.Remove(outFile)
	args := []string{"exec", "--full-auto", "--ephemeral", "-o", outFile}
	if o.WorkDir != "" {
		args = append(args, "-C", o.WorkDir)
	}
	args = append(args, o.ExtraArgs...)
	args = append(args, o.Prompt)
	stdout, stderr, err := runCmd(ctx, o.WorkDir, o.Env, o.Command, args...)
	if err != nil {
		return "", cmdError(o.Command, args, stdout, stderr, err)
	}
	if b, err := os.ReadFile(outFile); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return trimReply(string(b)), nil
	}
	reply := trimReply(string(stdout))
	if reply == "" {
		return "", fmt.Errorf("codex exec returned empty output")
	}
	return reply, nil
}

// --- opencode: opencode run --print-logs=false [-m MODEL] PROMPT ---

func detectOpenCodeBin() (string, error) {
	return lookPathFirst("opencode")
}

// invokeOpenCode runs one non-interactive opencode turn.
//
// `opencode run` prints the assistant's reply on stdout and exits, which is exactly the shape this
// backend needs. No model is passed unless the operator named one: opencode resolves its own default
// from the provider the user has configured, and there is no model id that is both cheap and valid
// across the providers it supports — naming one here would break the node for anybody on a different
// provider, which is worse than a default that costs more than necessary.
func invokeOpenCode(ctx context.Context, o execInvokeOpts) (string, error) {
	args := []string{"run"}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	args = append(args, o.ExtraArgs...)
	args = append(args, o.Prompt)
	stdout, stderr, err := runCmd(ctx, o.WorkDir, o.Env, o.Command, args...)
	if err != nil {
		return "", cmdError(o.Command, args, stdout, stderr, err)
	}
	reply := trimReply(string(stdout))
	if reply == "" {
		return "", fmt.Errorf("opencode run returned empty output")
	}
	return reply, nil
}

// --- openclaw: openclaw agent --local --json --agent ID --message PROMPT ---

func detectOpenClawBin() (string, error) {
	return lookPathFirst("openclaw")
}

func invokeOpenClaw(ctx context.Context, o execInvokeOpts) (string, error) {
	agentName := o.OpenClawAgent
	if agentName == "" {
		agentName = "main"
	}
	args := []string{"agent", "--local", "--json", "--agent", agentName, "--message", o.Prompt}
	args = append(args, o.ExtraArgs...)
	stdout, stderr, err := runCmd(ctx, o.WorkDir, o.Env, o.Command, args...)
	if err != nil {
		return "", cmdError(o.Command, args, stdout, stderr, err)
	}
	raw := strings.TrimSpace(string(stdout))
	if raw == "" {
		return "", fmt.Errorf("openclaw agent returned empty output")
	}
	// Best-effort JSON parse; fall back to raw stdout.
	var parsed struct {
		Reply   string `json:"reply"`
		Message string `json:"message"`
		Text    string `json:"text"`
		Result  string `json:"result"`
		Output  string `json:"output"`
	}
	if json.Unmarshal([]byte(raw), &parsed) == nil {
		for _, s := range []string{parsed.Reply, parsed.Message, parsed.Text, parsed.Result, parsed.Output} {
			if strings.TrimSpace(s) != "" {
				return trimReply(s), nil
			}
		}
	}
	return trimReply(raw), nil
}

// --- hermes: hermes -z PROMPT ---

func detectHermesBin() (string, error) {
	return lookPathFirst("hermes")
}

func invokeHermes(ctx context.Context, o execInvokeOpts) (string, error) {
	var args []string
	if o.Model != "" {
		args = []string{"chat", "-q", o.Prompt, "-m", o.Model}
	} else {
		args = []string{"-z", o.Prompt}
	}
	args = append(args, o.ExtraArgs...)
	stdout, stderr, err := runCmd(ctx, o.WorkDir, o.Env, o.Command, args...)
	if err != nil {
		return "", cmdError(o.Command, args, stdout, stderr, err)
	}
	reply := trimReply(string(stdout))
	if reply == "" {
		return "", fmt.Errorf("hermes returned empty output")
	}
	return reply, nil
}

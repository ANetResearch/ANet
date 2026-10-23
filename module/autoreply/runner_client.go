package autoreply

// runner_client.go — the node's side of auto_reply.runner (design v0.3.9 §C): a direct run becomes a job
// for `anet runner serve`, which runs the agent as its own account (runner_serve.go).
//
// The node builds the prompt as it would for a direct run (the instructions, the conversation bounded by
// AV-9, the untrusted content notice), sends the conversation's images as files of the job, and names them
// in the conversation by file name only: where the runner puts them is the runner's to say. The socket is
// checked before every connection as a service backend's is (internal/backendconn: the path, and that the
// account listening is expected_user); a refusal sends nothing. The reply's files are written into the
// turn's outbox, where the module collects them with the outbox's checks (collectOutbox), so a runner's
// files and an agent's are held to the same caps.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/execwork"
	"github.com/ANetResearch/ANet/internal/pathsafe"
	"github.com/ANetResearch/ANet/module"
)

// errRunnerRefused is a job the runner turned away (exit refused); the code is in the message.
var errRunnerRefused = errors.New("the runner refused the job")

// runnerReply runs one direct turn on the runner and returns the reply; the job's files are in rc.Outbox.
func (m *autoReplyModule) runnerReply(ctx context.Context, cfg AutoReplyConfig, th module.Turn, rc replyContext, turns []chatTurn) (string, error) {
	rc.Goal, turns = boundConversation(rc.Goal, turns) // AV-9
	er := &execReplier{cfg: cfg}
	prompt, files := runnerPrompt(turns)
	timeout := autoReplyDefaultAPITimeout
	if cfg.APITimeoutSeconds > 0 {
		timeout = time.Duration(cfg.APITimeoutSeconds) * time.Second
	}
	env := map[string]string{}
	for _, kv := range rc.RunEnv {
		if k, v, ok := strings.Cut(kv, "="); ok && runnerEnvName(k) {
			env[k] = v
		}
	}
	job := runnerJob{Agent: cfg.Agent, Model: cfg.Model, Instructions: er.instructions(rc),
		Conversation: execConversation(rc.Goal, prompt), Files: files, Env: env,
		TimeoutS: int(timeout / time.Second), Peer: runnerPeerOf(th.Peer)}
	res, err := callRunner(ctx, cfg.Runner, job)
	if err != nil {
		return "", err
	}
	switch res.Exit {
	case runnerExitOK:
	case runnerExitTooLarge:
		return "", errAgentOutputTooLarge
	case runnerExitTimeout:
		return "", fmt.Errorf("%w (the runner ended the agent: %s)", context.DeadlineExceeded, res.Detail)
	case runnerExitRefused:
		return "", fmt.Errorf("%w: %s (%s)", errRunnerRefused, a2ashape.Sentence(res.Code, a2ashape.English,
			a2ashape.Args{"agent": cfg.Agent}), logText(res.Detail, 200))
	default:
		return "", fmt.Errorf("runner: %s", logText(res.Detail, 400))
	}
	if err := writeRunnerFiles(rc.Outbox, res.Files); err != nil {
		return "", fmt.Errorf("runner files: %w", err)
	}
	return trimReply(res.Reply), nil
}

// runnerPeerOf names the per-peer work directory of a peer on the runner: the tail of its AID, as the
// node's own work directories are named (execwork.AidShort).
func runnerPeerOf(aid string) string {
	p := execwork.AidShort(aid)
	if !runnerPeerName(p) {
		return "unknown"
	}
	return p
}

// runnerPrompt renders the turns as execReplier.buildPrompt does, naming each image by its file name in
// the job, and returns the images as the job's files. Images past the job's limits are left out and the
// conversation says so.
func runnerPrompt(turns []chatTurn) (string, []runnerFile) {
	var b strings.Builder
	var files []runnerFile
	total := 0
	for i, t := range turns {
		role := "Requester"
		if t.Role == "assistant" {
			role = "You (previous reply)"
		}
		if t.Text != "" {
			fmt.Fprintf(&b, "%s: %s\n\n", role, t.Text)
		}
		for j, img := range t.Images {
			name := fmt.Sprintf("turn%d-img%d%s", i, j, imageExt(img.Mime))
			if len(files) >= runnerMaxFiles || total+len(img.Data) > runnerMaxFileBytes {
				fmt.Fprintf(&b, "%s attachment: %s (%s), left out: over the size limit of this node\n\n", role, name, img.Mime)
				continue
			}
			total += len(img.Data)
			files = append(files, runnerFile{Name: name, Mime: img.Mime, Data: img.Data})
			fmt.Fprintf(&b, "%s attachment: %s (%s)\n\n", role, name, img.Mime)
		}
	}
	return strings.TrimSpace(b.String()), files
}

// imageExt is the file extension of an image's media type (as buildPrompt chooses it).
func imageExt(mime string) string {
	switch {
	case strings.Contains(mime, "jpeg"), strings.Contains(mime, "jpg"):
		return ".jpg"
	case strings.Contains(mime, "png"):
		return ".png"
	case strings.Contains(mime, "webp"):
		return ".webp"
	case strings.Contains(mime, "gif"):
		return ".gif"
	}
	return ".bin"
}

// callRunner sends one job and returns the runner's final line. Ending ctx closes the connection, which
// cancels the job on the runner.
func callRunner(ctx context.Context, rc *AutoReplyRunnerConfig, job runnerJob) (runnerLine, error) {
	if rc == nil {
		return runnerLine{}, errors.New("no runner is configured")
	}
	rules, err := runnerPolicy(rc).Resolve()
	if err != nil {
		return runnerLine{}, err
	}
	sock := strings.TrimSpace(rc.Socket)
	c, err := rules.DialUnix(ctx, nil, sock)
	if err != nil {
		return runnerLine{}, fmt.Errorf("runner: %w", err)
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if err := writeRunnerLine(c, job); err != nil {
		return runnerLine{}, runnerConnErr(ctx, err)
	}
	br := bufio.NewReaderSize(c, 64<<10)
	for {
		var l runnerLine
		if err := readRunnerLine(br, runnerLineMax, &l); err != nil {
			return runnerLine{}, runnerConnErr(ctx, err)
		}
		if l.Exit != "" {
			return l, nil
		}
	}
}

// runnerConnErr is a broken connection to the runner: ctx's own end when that was the cause.
func runnerConnErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w (the job on the runner was cancelled)", ctx.Err())
	}
	return fmt.Errorf("runner: the connection ended before the job did: %w", err)
}

// writeRunnerFiles writes the files of a runner's reply into the turn's outbox, each as a new regular
// file under a safe name. The outbox's caps are checked here too, and again when it is collected.
func writeRunnerFiles(outbox string, files []runnerFile) error {
	if len(files) == 0 {
		return nil
	}
	if outbox == "" {
		log.Printf("anet: auto-reply: the runner returned %d file(s) and this turn has no outbox; they are not sent", len(files))
		return nil
	}
	if len(files) > maxOutboxFiles {
		return fmt.Errorf("%d files, over the limit of %d", len(files), maxOutboxFiles)
	}
	total := 0
	seen := map[string]bool{}
	for _, f := range files {
		total += len(f.Data)
		if total > maxOutboxBytes {
			return fmt.Errorf("files total over %d MiB", maxOutboxBytes>>20)
		}
		name := pathsafe.SafeName(f.Name)
		if seen[name] {
			return fmt.Errorf("two files are named %q", name)
		}
		seen[name] = true
		if err := writeNewPrivate(filepath.Join(outbox, name), f.Data); err != nil {
			return err
		}
	}
	return nil
}

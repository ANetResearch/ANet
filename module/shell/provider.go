//go:build shell

package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/provider"
)

// EvCommandRun records one execution on this node's own chain.
//
// Written BEFORE the output is returned and regardless of how the command
// exited: the record is of what this machine was asked to do and did, and
// a failed command is exactly as much a thing that happened as a
// successful one. An execution nobody can point back at is one nobody can
// dispute — which matters most for the calls somebody later wishes had
// not been made.
const EvCommandRun = "anet.shell.command"

// EvCommandRefused records a call this node would not run.
//
// Kept because a refusal is the more interesting record: it is where an
// AID that is not on the allowlist shows up, and an operator reviewing
// who has been knocking has nothing to review if refusals leave no trace.
const EvCommandRefused = "anet.shell.refused"

type shellProvider struct{ m *Module }

func (p *shellProvider) ID() string { return name }

func (p *shellProvider) Capabilities(context.Context) ([]string, error) {
	out := []string{CapList}
	for n := range p.m.cfg.Commands {
		out = append(out, runPrefix+n)
	}
	if p.m.cfg.AllowArbitrary {
		out = append(out, CapExec)
	}
	// Sorted so a card's capability list is stable across restarts: a
	// list that reorders itself makes every card look changed.
	sort.Strings(out)
	return out, nil
}

func (p *shellProvider) Describe(context.Context) (string, error) { return "", nil }

func (p *shellProvider) Health(context.Context) error {
	if p.m.host == nil {
		return fmt.Errorf("shell: not started")
	}
	// An unreadable allowlist is a health problem, not a per-call one: it
	// means this node cannot tell who may call it, and answering calls
	// while unable to answer that question is the wrong way to fail.
	if _, err := p.m.allowed(); err != nil {
		return fmt.Errorf("shell: allowlist unreadable: %w", err)
	}
	return nil
}

func (p *shellProvider) Invoke(ctx context.Context, call provider.Call) (effect.Effect, error) {
	if p.m.host == nil {
		return effect.Effect{}, fmt.Errorf("shell: not started")
	}
	// Who is asking, before what they asked for.
	//
	// An empty CallerAID is not "the local operator" — it is "this call
	// arrived with no verified identity", and the two are only the same
	// thing as long as every call site remembers to fill the field in.
	// Admitting it by default would make a forgotten assignment anywhere
	// in the daemon a silent bypass of everything below, so it takes its
	// own switch and that switch defaults to off.
	if call.CallerAID == "" {
		if !p.m.cfg.AllowLocal {
			p.record(EvCommandRefused, map[string]any{
				"caller": "", "capability": call.Capability,
				"reason": "the call carried no caller identity and allow_local is off",
			})
			return effect.Effect{Status: effect.Unavailable,
				Message: "this node does not accept commands that carry no caller identity"}, nil
		}
	} else {
		who, err := p.m.allowed()
		if err != nil {
			return effect.Effect{Status: effect.Unavailable,
				Message: "this node cannot read its own allowlist"}, nil
		}
		if !who[call.CallerAID] {
			p.record(EvCommandRefused, map[string]any{
				"caller": call.CallerAID, "capability": call.Capability,
				"reason": "not on the allowlist",
			})
			// Named plainly. A caller who is refused should learn that
			// they are not allowed, not be left guessing whether the
			// capability exists — the hub already told them it does.
			return effect.Effect{Status: effect.Unavailable,
				Message: "this node does not accept commands from " + call.CallerAID}, nil
		}
	}

	switch {
	case call.Capability == CapList:
		return p.list(call)
	case call.Capability == CapExec:
		return p.arbitrary(ctx, call)
	case strings.HasPrefix(call.Capability, runPrefix):
		return p.named(ctx, call, strings.TrimPrefix(call.Capability, runPrefix))
	}
	return effect.Effect{}, fmt.Errorf("shell: unknown capability %q", call.Capability)
}

// list reports what this node offers, and to an admitted local caller
// also who may ask for it.
//
// The roster is withheld from remote callers on purpose. A caller needs
// to know what it may run; it does not need the names of the other AIDs
// this node trusts, and handing them over turns one accepted caller into
// a directory of every other machine's operator. Local calls get the
// full list, because that is the operator reading their own config.
func (p *shellProvider) list(call provider.Call) (effect.Effect, error) {
	who, err := p.m.allowed()
	if err != nil {
		return effect.Effect{}, err
	}
	cmds := map[string]string{}
	for n, c := range p.m.cfg.Commands {
		d := c.Description
		if d == "" {
			d = c.Run
		}
		cmds[n] = d
	}
	body := map[string]any{
		"commands":        cmds,
		"allow_arbitrary": p.m.cfg.AllowArbitrary,
		"allowed_callers": len(who),
		"timeout_seconds": int(p.m.timeout(Command{}).Seconds()),
	}
	if call.CallerAID == "" {
		names := make([]string, 0, len(who))
		for a := range who {
			names = append(names, a)
		}
		sort.Strings(names)
		body["allowed_caller_aids"] = names
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return effect.Effect{}, err
	}
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{
			"commands": float64(len(cmds)), "allowed_callers": float64(len(who)),
		}},
		Evidence: &effect.Evidence{Protocol: name, Requested: CapList,
			ObservedState: string(raw), VerifyTrust: 2},
	}, nil
}

// named runs one command the operator approved.
func (p *shellProvider) named(ctx context.Context, call provider.Call, cmdName string) (effect.Effect, error) {
	c, ok := p.m.cfg.Commands[cmdName]
	if !ok {
		return effect.Effect{}, fmt.Errorf("shell: no command %q", cmdName)
	}
	line := c.Run
	if c.Args {
		extra, err := quoteArgv(call.Args["argv"])
		if err != nil {
			return effect.Effect{}, fmt.Errorf("shell: %s: %w", cmdName, err)
		}
		if extra != "" {
			line += " " + extra
		}
	} else if call.Args["argv"] != nil {
		// Silently dropping them would run something other than what the
		// caller asked for and report success.
		return effect.Effect{}, fmt.Errorf(
			"shell: command %q does not take arguments (set \"args\": true to allow them)", cmdName)
	}
	return p.run(ctx, call, line, c.Dir, p.m.timeout(c), cmdName)
}

// arbitrary runs whatever the caller sent.
func (p *shellProvider) arbitrary(ctx context.Context, call provider.Call) (effect.Effect, error) {
	if !p.m.cfg.AllowArbitrary {
		return effect.Effect{}, fmt.Errorf("shell: arbitrary commands are not enabled on this node")
	}
	line, _ := call.Args["command"].(string)
	if strings.TrimSpace(line) == "" {
		return effect.Effect{}, fmt.Errorf("shell: exec needs args.command")
	}
	dir, _ := call.Args["dir"].(string)
	return p.run(ctx, call, line, dir, p.m.timeout(Command{}), "")
}

// run executes one command line and reports what happened.
//
// The effect is honest about the difference between "it ran and worked",
// "it ran and failed" and "it did not finish": an exit code of zero is
// OK, a non-zero exit is FAILED with the code and the output, and a
// timeout is FAILED saying it was killed — never OK with empty output,
// which is what a caller would otherwise act on.
func (p *shellProvider) run(ctx context.Context, call provider.Call,
	line, dir string, timeout time.Duration, named string) (effect.Effect, error) {

	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(rctx, p.m.shell(), "-c", line)
	cmd.Dir = dir
	// Its own process group, and the whole group is killed on timeout.
	//
	// Killing only the shell leaves whatever it started still running — a
	// build, a flash, a loop — with nothing holding a handle to it. On a
	// board that is how a machine ends up doing work nobody can find.
	//
	// The kill hangs on cmd.Cancel rather than on a select of our own
	// against cmd.Wait(): CommandContext installs its own watchdog that
	// kills the shell alone, and two killers racing for the same Wait
	// means the group sometimes outlives the timeout depending on which
	// got there first. Replacing the watchdog is the only way to be sure
	// which kill happens.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // negative: the group
	}
	// Bounds the wait for output pipes held open by anything that escaped
	// the group (a child of its own session). Without it a single
	// setsid-ing command hangs this call forever.
	cmd.WaitDelay = 3 * time.Second

	var buf bytes.Buffer
	lim := &limitedWriter{w: &buf, n: p.m.maxOutput()}
	cmd.Stdout, cmd.Stderr = lim, lim

	started := time.Now()
	if err := cmd.Start(); err != nil {
		// Distinct from a command that ran and failed: nothing executed,
		// so there is no exit status to report and FAILED would claim
		// more than this node knows.
		return effect.Effect{}, fmt.Errorf("shell: start: %w", err)
	}
	waitErr := cmd.Wait()
	elapsed := time.Since(started)
	killed := rctx.Err() != nil

	code := 0
	var ee *exec.ExitError
	switch {
	case errors.As(waitErr, &ee):
		code = ee.ExitCode()
	case waitErr != nil && !killed && !errors.Is(waitErr, exec.ErrWaitDelay):
		code = -1
	}
	out := buf.String()
	if lim.truncated {
		out += fmt.Sprintf("\n[output truncated at %d bytes]", p.m.maxOutput())
	}

	what := named
	if what == "" {
		what = line
	}
	// On the chain before the caller is answered. The output is not
	// recorded — it can be large and can carry whatever the command
	// printed — but what ran, who asked and how it ended are.
	p.record(EvCommandRun, map[string]any{
		"caller": call.CallerAID, "capability": call.Capability,
		"command": what, "exit_code": code, "killed": killed,
		"duration_ms": elapsed.Milliseconds(), "output_bytes": len(out),
	})

	ev := &effect.Evidence{
		Protocol: name, Requested: call.Capability, NativeAck: true,
		ObservedState: out, LatencyMS: elapsed.Milliseconds(),
		// V2: the exit status and the output were read back from the
		// process itself. Not V4 — this node observed what the command
		// printed, not that the world changed the way the command claims.
		VerifyTrust: 2,
	}
	rec := &tsir.EffectRecord{Metrics: map[string]float64{
		"exit_code": float64(code), "duration_ms": float64(elapsed.Milliseconds()),
		"output_bytes": float64(len(out)),
	}}
	switch {
	case killed:
		return effect.Effect{Status: effect.Failed, Record: rec, Evidence: ev,
			Message: fmt.Sprintf("killed after %s", timeout)}, nil
	case code != 0:
		return effect.Effect{Status: effect.Failed, Record: rec, Evidence: ev,
			Message: fmt.Sprintf("exit %d", code)}, nil
	}
	return effect.Effect{Status: effect.OK, Record: rec, Evidence: ev}, nil
}

func (p *shellProvider) record(kind string, payload map[string]any) {
	if p.m.host == nil {
		return
	}
	_ = p.m.host.RecordEvidence(kind, payload)
}

// quoteArgv renders caller-supplied arguments for a shell command line.
//
// Single-quoted with embedded quotes escaped, which makes every argument
// one word to the shell whatever it contains. The command line itself is
// the operator's; the arguments are the network's, and the two must not
// be able to become the same thing.
func quoteArgv(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	raw, ok := v.([]any)
	if !ok {
		return "", fmt.Errorf("argv must be a list of strings")
	}
	var b strings.Builder
	for i, x := range raw {
		s, ok := x.(string)
		if !ok {
			return "", fmt.Errorf("argv[%d] is not a string", i)
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(s, `'`, `'\''`))
		b.WriteByte('\'')
	}
	return b.String(), nil
}

// limitedWriter stops after n bytes and remembers that it did.
type limitedWriter struct {
	w         *bytes.Buffer
	n         int
	truncated bool
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		l.truncated = true
		return len(p), nil // absorbed, so the command is not killed by EPIPE
	}
	if len(p) > l.n {
		l.w.Write(p[:l.n])
		l.n = 0
		l.truncated = true
		return len(p), nil
	}
	l.w.Write(p)
	l.n -= len(p)
	return len(p), nil
}

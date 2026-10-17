//go:build !no_mcp

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/agentwire"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// runAgents is `anet agents [status] | wire|unwire [--all|<tool>…]
// [--refresh] [--a2a <aid>…]` (A2A-DESIGN §13.1): register this node's MCP
// server and its operating guide with the coding agents on this machine,
// or take them out again. It works on files only and needs no daemon — the
// installer runs it before the first `anet up`.
func runAgents(layout daemon.Layout, rest []string) error {
	verb := "status"
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "--") {
		verb, rest = rest[0], rest[1:]
	}
	args, err := parseAgentsArgs(rest)
	if err != nil {
		return err
	}
	opts, err := wireOptions(layout)
	if err != nil {
		return err
	}
	opts.A2A, opts.Refresh = args.a2a, args.refresh
	lang := a2ashape.Lang()
	opts.Lang = lang
	switch verb {
	case "status", "list", "ls":
		if len(args.tools) > 0 || args.all || len(args.a2a) > 0 || args.refresh {
			return usageError{"anet agents  (status takes no arguments)"}
		}
		return agentsStatus(outW(), lang, opts)
	case "wire":
		results, err := agentwire.Wire(opts, args.tools, args.all)
		if err != nil {
			return wireRefusal(errW(), lang, err)
		}
		return printWireResults(outW(), errW(), lang, "wire", results, opts)
	case "unwire":
		if args.refresh {
			return usageError{"anet agents wire [--all|<tool>…] [--refresh]  (--refresh is for wire only)"}
		}
		results, err := agentwire.Unwire(opts, args.tools, args.all)
		if err != nil {
			return wireRefusal(errW(), lang, err)
		}
		return printWireResults(outW(), errW(), lang, "unwire", results, opts)
	}
	return usageError{fmt.Sprintf("anet agents [wire|unwire …]  (%q is not one of wire, unwire, status)", printable(verb, 64))}
}

type agentsArgs struct {
	tools   []string
	all     bool
	refresh bool
	a2a     []string
}

// parseAgentsArgs reads tool names and flags. --a2a takes one or more
// AIDs: comma-separated, repeated, or as the words that follow it up to the
// next flag or tool name.
func parseAgentsArgs(rest []string) (agentsArgs, error) {
	var a agentsArgs
	isTool := map[string]bool{"claude-code": true}
	for _, t := range agentwire.Tools() {
		isTool[t] = true
	}
	addAIDs := func(v string) {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				a.a2a = append(a.a2a, s)
			}
		}
	}
	inA2A, sawA2A := false, false
	for _, arg := range rest {
		switch {
		case arg == "--all":
			a.all, inA2A = true, false
		case arg == "--refresh":
			a.refresh, inA2A = true, false
		case arg == "--a2a":
			inA2A, sawA2A = true, true
		case strings.HasPrefix(arg, "--a2a="):
			addAIDs(strings.TrimPrefix(arg, "--a2a="))
			inA2A, sawA2A = true, true
		case strings.HasPrefix(arg, "--"):
			return a, usageError{fmt.Sprintf("anet agents wire|unwire [--all|<tool>…] [--refresh] [--a2a <aid>…]  (%q is not one of them)", printable(arg, 64))}
		case inA2A && !isTool[strings.ToLower(arg)]:
			addAIDs(arg)
		default:
			inA2A = false
			a.tools = append(a.tools, arg)
		}
	}
	if a.all && len(a.tools) > 0 {
		return a, usageError{"anet agents wire|unwire --all|<tool>…  (--all or tool names, not both)"}
	}
	// `--a2a` with nothing after it would otherwise be a plain wire, and a
	// plain unwire removes the MCP entries as well as every token.
	if sawA2A && len(a.a2a) == 0 {
		return a, usageError{"anet agents wire|unwire hermes --a2a <aid>…  (--a2a needs at least one AID)"}
	}
	return a, nil
}

// wireOptions pins the entries to this binary and this identity.
func wireOptions(layout daemon.Layout) (agentwire.Options, error) {
	bin, err := anetBinary()
	if err != nil {
		return agentwire.Options{}, err
	}
	dataDir, err := filepath.Abs(layout.Root)
	if err != nil {
		return agentwire.Options{}, err
	}
	return agentwire.Options{Bin: bin, DataDir: dataDir}, nil
}

// anetBinary is the absolute path the MCP entries run.
//
// The path the operator invoked is preferred to the resolved executable
// when both are the same file: on Linux os.Executable follows symlinks, and
// an entry pinned to /opt/anet/anet-0.2.0 behind a /usr/local/bin/anet link
// would stop working at the next upgrade.
func anetBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the anet binary: %w", err)
	}
	var cand string
	if strings.ContainsRune(os.Args[0], os.PathSeparator) {
		cand, _ = filepath.Abs(os.Args[0])
	} else if p, err := exec.LookPath(os.Args[0]); err == nil {
		cand, _ = filepath.Abs(p)
	}
	if cand != "" {
		a, aerr := os.Stat(cand)
		b, berr := os.Stat(exe)
		if aerr == nil && berr == nil && os.SameFile(a, b) {
			exe = cand
		}
	}
	// `go run` builds into a temporary directory that is gone when it
	// exits; an MCP entry pointing there fails on the client's next start.
	if strings.Contains(exe, string(os.PathSeparator)+"go-build") {
		return "", fmt.Errorf("this anet is a temporary `go run` build (%s); run wire from an installed binary", exe)
	}
	return exe, nil
}

// wireText is the one-line sentence of a message in a language: the
// sentence, then its reason code in brackets when it has one.
func wireText(lang a2ashape.Language, m agentwire.Msg) string {
	t := m.Text(lang)
	if c := m.Code(); c != "" {
		t += " [" + c + "]"
	}
	return t
}

// wireRefusal prints an error of the wire code on w, the sentence with its
// code and the next step after nextPrefix (ux UX-R2), and ends the command
// with exit status 1. An error that is not one of the package's own is
// returned as it is.
func wireRefusal(w io.Writer, lang a2ashape.Language, err error) error {
	m, ok := agentwire.AsMsg(err)
	if !ok {
		return err
	}
	say(w, wireText(lang, m), m.Next(lang))
	return errExit1
}

// wireIndent is the prefix of the lines under a tool's name.
const wireIndent = "            "

func printWireResults(out, errw io.Writer, lang a2ashape.Language, verb string, results []agentwire.Result, opts agentwire.Options) error {
	head := a2ashape.Sentence("wire.head."+verb, lang, a2ashape.Args{"bin": opts.Bin, "path": opts.DataDir})
	fmt.Fprintln(out, head)
	failed, changed := 0, false
	for _, r := range results {
		fmt.Fprintf(out, "  %-9s %s\n", r.Tool, a2ashape.Sentence(r.Status.Key(), lang, nil))
		for _, c := range r.Changes {
			fmt.Fprintf(out, "%s- %s\n", wireIndent, c.Text(lang))
		}
		for _, b := range r.Backups {
			fmt.Fprintf(out, "%s%s %s\n", wireIndent, a2ashape.Label("wire.backup", lang), b)
		}
		for _, n := range r.Notes {
			fmt.Fprintf(out, "%s%s%s%s\n", wireIndent, a2ashape.Label("wire.note", lang), noteColon(lang), n.Text(lang))
		}
		if r.Err != nil {
			printResultError(out, lang, r.Err)
		}
		if !r.OK() {
			failed++
		}
		if r.Status == agentwire.Written || r.Status == agentwire.Removed {
			changed = true
		}
	}
	if changed && verb == "wire" {
		fmt.Fprintln(out, a2ashape.Sentence("wire.footer.restart", lang, nil))
		fmt.Fprintln(out, a2ashape.Sentence("wire.footer.undo", lang, nil))
	}
	if failed > 0 {
		// A conflict is an answer the operator has to act on, so the exit
		// status says so — an installer running this must not report success.
		say(errw, wireText(lang, agentwire.Msg{Key: "wire_tools_failed", Args: a2ashape.Args{"n": failed}}), a2ashape.Next("wire_tools_failed", lang, nil))
		return errExit1
	}
	return nil
}

// noteColon follows the label of a note: a half-width colon and a space in
// English, a full-width colon in Chinese.
func noteColon(lang a2ashape.Language) string {
	if lang == a2ashape.Chinese {
		return "："
	}
	return ": "
}

// printResultError prints why a tool failed or conflicted under its name:
// the sentence with its code, the next step, and any lines of detail.
func printResultError(w io.Writer, lang a2ashape.Language, err error) {
	m, ok := agentwire.AsMsg(err)
	if !ok {
		fmt.Fprintf(w, "%s%s\n", wireIndent, strings.ReplaceAll(err.Error(), "\n", "\n"+wireIndent))
		return
	}
	fmt.Fprintf(w, "%s%s\n", wireIndent, wireText(lang, m))
	if next := m.Next(lang); next != "" {
		fmt.Fprintf(w, "%s%s\n", wireIndent, strings.TrimLeft(nextPrefix, " ")+next)
	}
	if m.Detail != "" {
		fmt.Fprintf(w, "%s%s\n", wireIndent, strings.ReplaceAll(m.Detail, "\n", "\n"+wireIndent))
	}
}

func agentsStatus(w io.Writer, lang a2ashape.Language, opts agentwire.Options) error {
	states, err := agentwire.Inspect(opts)
	if err != nil {
		return wireRefusal(errW(), lang, err)
	}
	fmt.Fprintln(w, a2ashape.Sentence("wire.head.status", lang, a2ashape.Args{"bin": opts.Bin, "path": opts.DataDir}))
	sentence := func(key string, args a2ashape.Args) string { return a2ashape.Sentence(key, lang, args) }
	for _, s := range states {
		fix := "anet agents wire " + s.Tool
		for _, a := range s.A2A {
			if a.Stale() {
				fix += " --refresh"
				break
			}
		}
		var line string
		switch {
		case s.Conflict != "":
			line = sentence("wire.state.conflict", a2ashape.Args{"detail": s.Conflict})
		case s.Err != "":
			line = sentence("wire.state.unchecked", a2ashape.Args{"detail": s.Err})
		case s.Wired && s.Current:
			line = sentence("wire.state.current", nil)
		case s.Wired:
			line = sentence("wire.state.stale", a2ashape.Args{"command": fix})
		case !s.Detected:
			line = sentence("wire.state.not_installed", nil)
		default:
			line = sentence("wire.state.not_wired", a2ashape.Args{"command": "anet agents wire " + s.Tool})
		}
		fmt.Fprintf(w, "  %-9s %s\n", s.Tool, line)
		if s.Tool == agentwire.ToolHermes && s.Mode != 0 && len(s.A2A) > 0 && s.Mode&0o077 != 0 {
			fmt.Fprintf(w, "%s%s\n", wireIndent, sentence("wire.state.token_mode", a2ashape.Args{
				"path": s.Config, "value": fmt.Sprintf("%o", s.Mode), "command": "anet agents wire hermes"}))
		}
		for _, a := range s.A2A {
			var l string
			switch {
			case a.Unknown != "":
				l = sentence("wire.state.a2a_unknown", a2ashape.Args{"aid": a.AID, "detail": a.Unknown})
			case a.PortOK && a.TokenOK:
				l = sentence("wire.state.a2a_ok", a2ashape.Args{"aid": a.AID})
			default:
				l = sentence("wire.state.a2a_changed", a2ashape.Args{"aid": a.AID, "command": "anet agents wire hermes --refresh"})
			}
			fmt.Fprintf(w, "%s%s\n", wireIndent, l)
		}
	}
	return nil
}

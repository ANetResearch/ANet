//go:build !no_mcp

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
	switch verb {
	case "status", "list", "ls":
		if len(args.tools) > 0 || args.all || len(args.a2a) > 0 || args.refresh {
			return usageError{"anet agents  (status takes no arguments)"}
		}
		return agentsStatus(opts)
	case "wire":
		results, err := agentwire.Wire(opts, args.tools, args.all)
		if err != nil {
			return err
		}
		return printWireResults("wire", results, opts)
	case "unwire":
		if args.refresh {
			return usageError{"anet agents wire [--all|<tool>…] [--refresh]  (--refresh is for wire only)"}
		}
		results, err := agentwire.Unwire(opts, args.tools, args.all)
		if err != nil {
			return err
		}
		return printWireResults("unwire", results, opts)
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

func printWireResults(verb string, results []agentwire.Result, opts agentwire.Options) error {
	fmt.Printf("anet agents %s(anet %s,数据目录 %s)\n", verb, opts.Bin, opts.DataDir)
	failed, changed := 0, false
	for _, r := range results {
		fmt.Printf("  %-9s %s\n", r.Tool, r.Status.Label())
		for _, c := range r.Changes {
			fmt.Printf("            - %s\n", c)
		}
		for _, b := range r.Backups {
			fmt.Printf("            备份 %s\n", b)
		}
		for _, n := range r.Notes {
			fmt.Printf("            注意:%s\n", n)
		}
		if r.Err != nil {
			fmt.Printf("            %s\n", strings.ReplaceAll(r.Err.Error(), "\n", "\n            "))
		}
		if !r.OK() {
			failed++
		}
		if r.Status == agentwire.Written || r.Status == agentwire.Removed {
			changed = true
		}
	}
	if changed && verb == "wire" {
		fmt.Println("重启这些工具(或新开会话)后生效;它们调用 `anet mcp`,需要本机 daemon 在运行(anet up)。")
		fmt.Println("撤销:anet agents unwire <工具>(或 --all)")
	}
	if failed > 0 {
		// A conflict is an answer the operator has to act on, so the exit
		// status says so — an installer running this must not report success.
		return fmt.Errorf("%d of the tools were not done; see the lines above", failed)
	}
	return nil
}

func agentsStatus(opts agentwire.Options) error {
	states, err := agentwire.Inspect(opts)
	if err != nil {
		return err
	}
	fmt.Printf("anet agents(anet %s,数据目录 %s)\n", opts.Bin, opts.DataDir)
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
			line = "冲突:" + s.Conflict
		case s.Err != "":
			line = "无法检查:" + s.Err
		case s.Wired && s.Current:
			line = "已接入,最新"
		case s.Wired:
			line = "已接入,需要更新(" + fix + ")"
		case !s.Detected:
			line = "未安装"
		default:
			line = "未接入(anet agents wire " + s.Tool + ")"
		}
		fmt.Printf("  %-9s %s\n", s.Tool, line)
		if s.Tool == agentwire.ToolHermes && s.Mode != 0 && len(s.A2A) > 0 && s.Mode&0o077 != 0 {
			fmt.Printf("            %s 含令牌但权限是 %o(应为 0600;anet agents wire hermes 会收紧)\n", s.Config, s.Mode)
		}
		for _, a := range s.A2A {
			switch {
			case a.Unknown != "":
				fmt.Printf("            a2a %s:无法核对(%s)\n", a.AID, a.Unknown)
			case a.PortOK && a.TokenOK:
				fmt.Printf("            a2a %s:正常\n", a.AID)
			default:
				fmt.Printf("            a2a %s:端口或令牌已变(anet agents wire hermes --refresh)\n", a.AID)
			}
		}
	}
	return nil
}

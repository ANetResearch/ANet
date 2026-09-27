//go:build !no_mcp

// Package agentwire registers this node's MCP server, and the short
// operating guide that goes with it, in the coding agents installed on this
// machine: Claude Code, Codex, Cursor, opencode and Hermes
// (A2A-DESIGN §13.1, `anet agents wire|unwire`).
//
// Everything it writes points at `anet mcp`, so it carries the same build
// tag as internal/mcpserv: a -tags no_mcp binary has nothing to register and
// links none of this.
//
// It edits other programs' configuration files, so the rules are strict and
// the same for every tool:
//
//   - A file is copied to <name>.anet-bak-<time> before anet changes it.
//   - The entry names the anet binary by absolute path and pins the identity
//     with ANET_DATA_DIR. An MCP client does not load the operator's shell
//     profile, and `anet mcp` resolves strictly once an identity is pinned
//     (§7.8), so the entry reaches this identity or nothing.
//   - Writes are atomic (sibling temp file + rename) and keep the mode of
//     the file they replace.
//   - Wiring twice changes nothing the second time.
//   - What anet did not write it does not overwrite: a same-named entry that
//     is not anet's is reported as a conflict and the tool is left untouched.
//
// TOML (Codex) and YAML (Hermes) are edited as text inside marked blocks
// rather than parsed: the standard library reads neither, and a parser
// dependency would be linked into every build for one command
// (docs/notes/0008 §7.3).
package agentwire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Tool names, as typed on the command line.
const (
	ToolClaude   = "claude"
	ToolCodex    = "codex"
	ToolCursor   = "cursor"
	ToolOpenCode = "opencode"
	ToolHermes   = "hermes"
)

// A2AAddrFile and A2ATokenFile are the local A2A interface's state files in
// the data dir, written by module/a2a (A2A-DESIGN §11.1, X5). Only Hermes'
// a2a_agents entries read them.
const (
	A2AAddrFile  = "a2a_addr.txt"
	A2ATokenFile = "a2a_token.txt"
)

// Options is what wiring needs to know about this machine and this node.
type Options struct {
	// Bin is the absolute path of the anet binary the MCP entries run.
	Bin string
	// DataDir is the identity's data dir, written as ANET_DATA_DIR.
	DataDir string
	// Home is the user's home directory ("" → os.UserHomeDir).
	Home string
	// Getenv reads CLAUDE_CONFIG_DIR, CODEX_HOME, XDG_CONFIG_HOME and
	// HERMES_HOME (nil → os.Getenv).
	Getenv func(string) string
	// LookPath finds a tool's executable (nil → exec.LookPath).
	LookPath func(string) (string, error)
	// Run executes a tool's own CLI (nil → exec.CommandContext). Only
	// Claude Code's `claude mcp add|remove` goes through it.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Now stamps backup names (nil → time.Now).
	Now func() time.Time

	// A2A lists remote agents (AIDs) to add to Hermes' a2a_agents on wire,
	// or to remove from it on unwire.
	A2A []string
	// Refresh rewrites the existing Hermes a2a_agents entries from the
	// current a2a_addr.txt and a2a_token.txt, after the port or the token
	// changed.
	Refresh bool
}

func (o *Options) normalize() error {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Run == nil {
		o.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return fmt.Errorf("找不到家目录: %v", err)
		}
		o.Home = h
	}
	if !filepath.IsAbs(o.Bin) {
		return fmt.Errorf("anet 可执行文件路径必须是绝对路径,得到 %q", o.Bin)
	}
	if !filepath.IsAbs(o.DataDir) {
		return fmt.Errorf("数据目录必须是绝对路径,得到 %q", o.DataDir)
	}
	for _, aid := range o.A2A {
		if !validAID(aid) {
			return fmt.Errorf("%q 不是 AID(小写字母与数字,8–128 个字符)", aid)
		}
	}
	return nil
}

// Status is the outcome for one tool.
type Status string

const (
	Written  Status = "written"       // wire changed at least one file
	UpToDate Status = "up-to-date"    // wire found everything already in place
	Skipped  Status = "not-installed" // --all and the tool is not on this machine
	Conflict Status = "conflict"      // a same-named entry anet does not manage; nothing changed
	Removed  Status = "removed"       // unwire removed at least one thing
	NotWired Status = "not-wired"     // unwire found nothing of anet's
	Failed   Status = "error"
)

// Label is the status as the CLI prints it.
func (s Status) Label() string {
	switch s {
	case Written:
		return "已写"
	case UpToDate:
		return "已是最新"
	case Skipped:
		return "未安装,跳过"
	case Conflict:
		return "冲突"
	case Removed:
		return "已移除"
	case NotWired:
		return "未接入"
	case Failed:
		return "失败"
	}
	return string(s)
}

// Result is what happened to one tool.
type Result struct {
	Tool    string
	Status  Status
	Changes []string // one line per file changed, or per CLI call made
	Backups []string // backup copies made before changing a file
	Notes   []string // anything else the operator should know
	Err     error    // Conflict and Failed: why
}

// OK reports whether the result needs no action from the operator.
func (r Result) OK() bool { return r.Status != Conflict && r.Status != Failed }

// Tools lists the supported tools in the order results are reported.
func Tools() []string {
	return []string{ToolClaude, ToolCodex, ToolCursor, ToolOpenCode, ToolHermes}
}

// tool is one supported coding agent.
type tool interface {
	name() string
	// detected reports whether the tool is on this machine: its executable
	// is on PATH or its configuration directory exists. A directory alone
	// counts because an IDE (Cursor) need not put a CLI on PATH.
	detected(o *Options) bool
	// planWire and planUnwire compute the changes without making any. A
	// *ConflictError means the tool must be left alone.
	planWire(o *Options) ([]change, error)
	planUnwire(o *Options) ([]change, []string, error)
	// configPath is the file that holds the tool's MCP entry.
	configPath(o *Options) string
}

func toolByName(name string) (tool, bool) {
	switch name {
	case ToolClaude:
		return claudeTool{}, true
	case ToolCodex:
		return codexTool{}, true
	case ToolCursor:
		return cursorTool{}, true
	case ToolOpenCode:
		return opencodeTool{}, true
	case ToolHermes:
		return hermesTool{}, true
	}
	return nil, false
}

// selectTools resolves the command line's tool list. all=true (or no names)
// means every supported tool; explicit names are checked and de-duplicated.
func selectTools(names []string, all bool) ([]tool, bool, error) {
	if all || len(names) == 0 {
		var ts []tool
		for _, n := range Tools() {
			t, _ := toolByName(n)
			ts = append(ts, t)
		}
		return ts, true, nil
	}
	seen := map[string]bool{}
	var ts []tool
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "claude-code" {
			n = ToolClaude
		}
		t, ok := toolByName(n)
		if !ok {
			return nil, false, fmt.Errorf("不认识的工具 %q(支持:%s)", n, strings.Join(Tools(), ", "))
		}
		if !seen[n] {
			seen[n] = true
			ts = append(ts, t)
		}
	}
	// Report in the fixed order, whatever order they were typed in.
	order := map[string]int{}
	for i, n := range Tools() {
		order[n] = i
	}
	sort.SliceStable(ts, func(i, j int) bool { return order[ts[i].name()] < order[ts[j].name()] })
	return ts, false, nil
}

// Wire registers anet in the named tools, or with all=true (or no names) in
// every supported tool that is installed. Explicitly named tools are wired
// even when not detected: the operator asked for that one. With all, the
// persona block an old `anet install --agent openclaw` left behind is taken
// out as well (see LegacyOpenClaw).
func Wire(o Options, names []string, all bool) ([]Result, error) {
	if err := o.normalize(); err != nil {
		return nil, err
	}
	tools, implicit, err := selectTools(names, all)
	if err != nil {
		return nil, err
	}
	if len(o.A2A) > 0 && !includes(tools, ToolHermes) {
		return nil, fmt.Errorf("--a2a 写的是 Hermes 的 a2a_agents;请同时指定 hermes(或 --all)")
	}
	var out []Result
	for _, t := range tools {
		r := Result{Tool: t.name()}
		if implicit && !t.detected(&o) {
			r.Status = Skipped
			if t.name() == ToolHermes && len(o.A2A) > 0 {
				r.Notes = append(r.Notes, "--a2a 未写入:没有找到 Hermes(设置 HERMES_HOME,或明确写 hermes)")
			}
			out = append(out, r)
			continue
		}
		if !implicit && !t.detected(&o) {
			r.Notes = append(r.Notes, "未检测到该工具,按要求照写")
		}
		changes, err := t.planWire(&o)
		out = append(out, finish(&o, r, changes, nil, err, Written, UpToDate))
	}
	if implicit {
		if r, found := legacyOpenClaw(&o); found {
			out = append(out, r)
		}
	}
	return out, nil
}

// LegacyOpenClaw is the name results about ~/.openclaw/AGENTS.md carry.
// OpenClaw is not a tool wire supports — how it registers an MCP server is
// unverified — but the old `anet install --agent openclaw` appended its
// persona block there, and that block describes a network that no longer
// exists and tells the agent to list itself and take work from anyone.
const LegacyOpenClaw = "openclaw"

func openclawAgentsMD(o *Options) string { return filepath.Join(o.Home, ".openclaw", "AGENTS.md") }

// legacyOpenClaw removes the old persona block from OpenClaw's AGENTS.md.
// found is false when there is none, so --all reports OpenClaw only when
// there was something of anet's to take out.
func legacyOpenClaw(o *Options) (Result, bool) {
	r := Result{Tool: LegacyOpenClaw}
	c, err := planDropTextBlock(o, openclawAgentsMD(o), "旧版 anet 指引块(OpenClaw 不在支持的工具内)")
	if err == nil && c == nil {
		return r, false
	}
	var changes []change
	if c != nil {
		changes = append(changes, *c)
	}
	return finish(o, r, changes, nil, err, Removed, NotWired), true
}

// RemoveLegacyOpenClaw is legacyOpenClaw for `anet install --agent
// openclaw`, the command that wrote the block.
func RemoveLegacyOpenClaw(o Options) (Result, error) {
	if err := o.normalize(); err != nil {
		return Result{}, err
	}
	r, found := legacyOpenClaw(&o)
	if !found {
		r.Status = NotWired
	}
	return r, nil
}

// Unwire removes what Wire added from the named tools, or from every
// supported tool with all=true (or no names), the old OpenClaw persona
// block included. With Options.A2A set, only those Hermes a2a_agents
// entries are removed.
func Unwire(o Options, names []string, all bool) ([]Result, error) {
	if err := o.normalize(); err != nil {
		return nil, err
	}
	tools, _, err := selectTools(names, all)
	if err != nil {
		return nil, err
	}
	if len(o.A2A) > 0 && !includes(tools, ToolHermes) {
		return nil, fmt.Errorf("--a2a 删除的是 Hermes 的 a2a_agents 条目;请同时指定 hermes(或 --all)")
	}
	var out []Result
	for _, t := range tools {
		r := Result{Tool: t.name()}
		if len(o.A2A) > 0 && t.name() != ToolHermes {
			continue // `unwire --all --a2a X` is about Hermes' entries only
		}
		changes, notes, err := t.planUnwire(&o)
		out = append(out, finish(&o, r, changes, notes, err, Removed, NotWired))
	}
	if (all || len(names) == 0) && len(o.A2A) == 0 {
		if r, found := legacyOpenClaw(&o); found {
			out = append(out, r)
		}
	}
	return out, nil
}

func includes(ts []tool, name string) bool {
	for _, t := range ts {
		if t.name() == name {
			return true
		}
	}
	return false
}

// finish applies a plan and turns it into a Result.
func finish(o *Options, r Result, changes []change, notes []string, err error, changed, unchanged Status) Result {
	r.Notes = append(r.Notes, notes...)
	var ce *ConflictError
	switch {
	case errors.As(err, &ce):
		r.Status, r.Err = Conflict, err
		return r
	case err != nil:
		r.Status, r.Err = Failed, err
		return r
	}
	if len(changes) == 0 {
		r.Status = unchanged
		return r
	}
	for _, c := range changes {
		bak, err := c.apply(o)
		if bak != "" {
			r.Backups = append(r.Backups, short(o, bak))
		}
		if err != nil {
			r.Status, r.Err = Failed, err
			return r
		}
		r.Changes = append(r.Changes, c.note)
	}
	r.Status = changed
	return r
}

// ConflictError is a same-named entry anet did not write. The tool is left
// untouched; the operator removes or renames the entry and runs wire again.
type ConflictError struct {
	Path string
	Line int // 1-based; 0 when the file has no line structure worth naming
	What string
}

func (e *ConflictError) Error() string {
	loc := e.Path
	if e.Line > 0 {
		loc = fmt.Sprintf("%s:%d", e.Path, e.Line)
	}
	return fmt.Sprintf("%s: %s", loc, e.What)
}

// validAID accepts the AID alphabet (a CID in lowercase base32) and nothing
// else. The AID goes into a URL path and a YAML key; a narrow alphabet
// keeps both free of anything that needs escaping.
func validAID(aid string) bool {
	if len(aid) < 8 || len(aid) > 128 {
		return false
	}
	for i := 0; i < len(aid); i++ {
		c := aid[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// dirExists reports whether p is an existing directory.
func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// onPath reports whether any of the executables is on PATH.
func onPath(o *Options, names ...string) bool {
	for _, n := range names {
		if _, err := o.LookPath(n); err == nil {
			return true
		}
	}
	return false
}

// envOr returns the environment variable when set, else def.
func envOr(o *Options, key, def string) string {
	if v := strings.TrimSpace(o.Getenv(key)); v != "" {
		return v
	}
	return def
}

// mcpEnv is the environment every MCP entry carries.
func mcpEnv(o *Options) map[string]string {
	return map[string]string{"ANET_DATA_DIR": o.DataDir}
}

// isAnetCommand reports whether command names the anet binary, however it
// was spelled: an absolute path from an earlier wire, or the bare `anet`
// the tutorials told people to write by hand.
func isAnetCommand(command string) bool {
	base := filepath.Base(strings.TrimSpace(command))
	base = strings.TrimSuffix(base, ".exe")
	return base == "anet"
}

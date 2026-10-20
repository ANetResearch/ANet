//go:build !no_mcp

// Package agentwire registers this node's MCP server, and the short
// operating guide that goes with it, in the coding agents installed on this
// machine: Claude Code, Codex, Cursor, opencode, Hermes and WorkBuddy
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
// (an internal note §7.3).
package agentwire

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/anethome"
)

// Tool names, as typed on the command line.
const (
	ToolClaude   = "claude"
	ToolCodex    = "codex"
	ToolCursor   = "cursor"
	ToolOpenCode = "opencode"
	ToolHermes   = "hermes"
	// ToolWorkBuddy is Tencent's WorkBuddy desktop app.
	ToolWorkBuddy = "workbuddy"
)

// A2AAddrFile and A2ATokenFile are the local A2A interface's state files,
// written by module/a2a into its state directory in the data dir
// (anethome.A2ADir: <data dir>/modules/a2a; A2A-DESIGN §11.1, X5). Only
// Hermes' a2a_agents entries read them, through a2aStatePath.
const (
	A2AAddrFile  = anethome.A2AAddrFile
	A2ATokenFile = anethome.A2ATokenFile
	// A2AConflictFile is there while the interface is down because another
	// process held its recorded port (module/a2a); readA2A refuses to wire
	// until it is gone.
	A2AConflictFile = anethome.A2AConflictFile
)

// a2aStatePath is where module/a2a keeps file for the data dir dataDir.
func a2aStatePath(dataDir, file string) string {
	return filepath.Join(anethome.A2ADir(dataDir), file)
}

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
	// Lang is the language of the text State carries (Inspect); results and
	// errors hold keys and are rendered by their caller. The zero value is
	// English.
	Lang a2ashape.Language
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
			return fail("wire_home_not_found")
		}
		o.Home = h
	}
	if !filepath.IsAbs(o.Bin) {
		return fail("wire_path_not_absolute.bin", "path", o.Bin)
	}
	if !filepath.IsAbs(o.DataDir) {
		return fail("wire_path_not_absolute.data", "path", o.DataDir)
	}
	for _, aid := range o.A2A {
		if !validAID(aid) {
			return fail("wire_bad_aid", "value", aid)
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

// Key is the status word's key in the reason table; the CLI prints the
// sentence in its language.
func (s Status) Key() string {
	switch s {
	case Written:
		return "wire.status.written"
	case UpToDate:
		return "wire.status.up_to_date"
	case Skipped:
		return "wire.status.skipped"
	case Conflict:
		return "wire.status.conflict"
	case Removed:
		return "wire.status.removed"
	case NotWired:
		return "wire.status.not_wired"
	}
	return "wire.status.failed"
}

// Result is what happened to one tool.
type Result struct {
	Tool    string
	Status  Status
	Changes []Msg    // one line per file changed, or per CLI call made
	Backups []string // backup copies made before changing a file
	Notes   []Msg    // anything else the operator should know
	Err     error    // Conflict and Failed: why (an *Error or *ConflictError)
}

// OK reports whether the result needs no action from the operator.
func (r Result) OK() bool { return r.Status != Conflict && r.Status != Failed }

// Tools lists the supported tools in the order results are reported.
func Tools() []string {
	return []string{ToolClaude, ToolCodex, ToolCursor, ToolOpenCode, ToolHermes, ToolWorkBuddy}
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
	planUnwire(o *Options) ([]change, []Msg, error)
	// configPath is the file that holds the tool's MCP entry.
	configPath(o *Options) string
}

// noter is implemented by a tool that has something to tell the operator
// after wire that the plan itself cannot express: a file the tool reads
// first that hides anet's, or a step the tool needs that anet cannot take.
type noter interface {
	wireNotes(o *Options, st Status) []Msg
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
	case ToolWorkBuddy:
		return workbuddyTool{}, true
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
			return nil, false, fail("wire_unknown_tool", "name", n, "value", strings.Join(Tools(), ", "))
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
		return nil, fail("wire_a2a_needs_hermes.wire")
	}
	var out []Result
	for _, t := range tools {
		r := Result{Tool: t.name()}
		if implicit && !t.detected(&o) {
			r.Status = Skipped
			if t.name() == ToolHermes && len(o.A2A) > 0 {
				r.Notes = append(r.Notes, msg("wire.note.a2a_skipped"))
			}
			out = append(out, r)
			continue
		}
		if !implicit && !t.detected(&o) {
			r.Notes = append(r.Notes, msg("wire.note.not_detected"))
		}
		changes, err := t.planWire(&o)
		r = finish(&o, r, changes, nil, err, Written, UpToDate)
		if n, ok := t.(noter); ok && (r.Status == Written || r.Status == UpToDate) {
			r.Notes = append(r.Notes, n.wireNotes(&o, r.Status)...)
		}
		out = append(out, r)
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
	c, err := planDropTextBlock(o, openclawAgentsMD(o), "wire.change.openclaw_drop")
	if err == nil && c == nil {
		return r, false
	}
	var changes []change
	if c != nil {
		changes = append(changes, *c)
	}
	return finish(o, r, changes, nil, err, Removed, NotWired), true
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
		return nil, fail("wire_a2a_needs_hermes.unwire")
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
func finish(o *Options, r Result, changes []change, notes []Msg, err error, changed, unchanged Status) Result {
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
// the tutorials told people to write by hand. The name is compared by
// sameBinaryName (os_unix.go, os_windows.go).
func isAnetCommand(command string) bool {
	return sameBinaryName(filepath.Base(strings.TrimSpace(command)), "anet")
}

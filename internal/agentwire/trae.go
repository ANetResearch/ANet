//go:build !no_mcp

package agentwire

import (
	"bytes"
	"path/filepath"
	"runtime"
	"strings"
)

// Trae and Trae CN (ByteDance), checked against Trae 2.3 for Linux and
// Windows (its workbench and main process code; nothing was run):
//
//   - The user-level MCP file is mcp.json next to the user settings, in the
//     user data directory of the product: <base>/<product>/User/mcp.json,
//     where base is %APPDATA% on Windows, ~/Library/Application Support on
//     macOS and $XDG_CONFIG_HOME (else ~/.config) on Linux, and product is
//     "Trae" or "Trae CN". Its servers are in the member mcpServers, in the
//     shape Cursor uses (command, args, env).
//   - User rules are the Markdown files of <home>/<data folder>/user_rules/
//     (data folder .trae, or .trae-cn for Trae CN); a file without front
//     matter, or with alwaysApply: true, applies to every session. wire
//     writes the short guide as user_rules/anet.md there.
//
// Trae documents that the command of an MCP server must not contain spaces;
// wire writes the absolute path of anet as the command all the same (it has
// no other) and says so when the path has a space.
//
// Both products are wired when both are present. The bundle of the anet
// plugin for Trae (deploy/plugins, target trae) registers its own entry
// "anet" through /bin/sh or anet-mcp.cmd; wire reports that entry as a
// conflict instead of replacing it.
type traeTool struct{}

// traeProduct is one of the two products.
type traeProduct struct {
	product    string // the directory name of the user data directory
	dataFolder string // the directory under home that holds rules and skills
	cli        string // the shell command the product installs, "" when unknown
}

var traeProducts = []traeProduct{
	{product: "Trae", dataFolder: ".trae", cli: "trae"},
	{product: "Trae CN", dataFolder: ".trae-cn"},
}

// traeGOOS is the operating system whose paths are used; tests set it.
var traeGOOS = runtime.GOOS

func (traeTool) name() string { return ToolTrae }

// traeUserDataBase is the directory that holds the user data directory of a
// product, as Trae computes it (VS Code's userDataPath).
func traeUserDataBase(o *Options) string {
	switch traeGOOS {
	case "windows":
		if v := strings.TrimSpace(o.Getenv("APPDATA")); v != "" && traeAbs(v) {
			return v
		}
		return filepath.Join(o.Home, "AppData", "Roaming")
	case "darwin":
		return filepath.Join(o.Home, "Library", "Application Support")
	}
	if v := strings.TrimSpace(o.Getenv("XDG_CONFIG_HOME")); v != "" && traeAbs(v) {
		return v
	}
	return filepath.Join(o.Home, ".config")
}

// traeAbs accepts an absolute path of traeGOOS: a drive or UNC path on
// Windows (checked as text, so that the tests can run anywhere).
func traeAbs(p string) bool {
	if traeGOOS == "windows" {
		return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') || strings.HasPrefix(p, `\\`)
	}
	return filepath.IsAbs(p)
}

func traeMCP(o *Options, p traeProduct) string {
	return filepath.Join(traeUserDataBase(o), p.product, "User", "mcp.json")
}

func traeRulePath(o *Options, p traeProduct) string {
	return filepath.Join(o.Home, p.dataFolder, "user_rules", "anet.md")
}

func (traeTool) productDetected(o *Options, p traeProduct) bool {
	return dirExists(filepath.Join(o.Home, p.dataFolder)) ||
		dirExists(filepath.Join(traeUserDataBase(o), p.product)) ||
		(p.cli != "" && onPath(o, p.cli))
}

// products are the products to wire: the ones on this machine, or Trae
// when neither is.
func (t traeTool) products(o *Options) []traeProduct {
	var ps []traeProduct
	for _, p := range traeProducts {
		if t.productDetected(o, p) {
			ps = append(ps, p)
		}
	}
	if len(ps) == 0 {
		ps = traeProducts[:1]
	}
	return ps
}

func (t traeTool) detected(o *Options) bool {
	for _, p := range traeProducts {
		if t.productDetected(o, p) {
			return true
		}
	}
	return false
}

func (t traeTool) configPath(o *Options) string { return traeMCP(o, t.products(o)[0]) }

// traeRule is the rule file wire writes: the short guide, applied to every
// session.
const traeRule = "---\nalwaysApply: true\n---\n\n" + personaMarkdown + "\n"

// traeRuleMark is the text that tells a rule file written by anet.
const traeRuleMark = "## AgentNetwork (anet)"

func (t traeTool) planWire(o *Options) ([]change, error) {
	var changes []change
	for _, p := range t.products(o) {
		c, err := cursorTool{}.serverAt(o, traeMCP(o, p)).planWire(o)
		if err != nil {
			return nil, err
		}
		if c != nil {
			changes = append(changes, *c)
		}
		path := traeRulePath(o, p)
		cur, err := readMaybe(realPath(path))
		if err != nil {
			return nil, err
		}
		if bytes.Equal(cur, []byte(traeRule)) {
			continue
		}
		if cur != nil && !strings.Contains(string(cur), traeRuleMark) {
			return nil, conflict("wire_conflict.rule_file", "path", short(o, path))
		}
		changes = append(changes, change{path: path, before: cur, after: []byte(traeRule), mode: 0o644,
			note: msg("wire.change.rule_write", "path", short(o, path))})
	}
	return changes, nil
}

func (t traeTool) planUnwire(o *Options) ([]change, []Msg, error) {
	var changes []change
	var notes []Msg
	// Every product, present or not: unwire takes out whatever anet wrote.
	for _, p := range traeProducts {
		c, n, err := cursorTool{}.serverAt(o, traeMCP(o, p)).planUnwire(o)
		if err != nil {
			return nil, nil, err
		}
		notes = append(notes, n...)
		if c != nil {
			changes = append(changes, *c)
		}
		path := traeRulePath(o, p)
		cur, err := readMaybe(realPath(path))
		if err != nil {
			return nil, nil, err
		}
		if cur == nil || !strings.Contains(string(cur), traeRuleMark) {
			continue
		}
		changes = append(changes, change{path: path, before: cur, del: true, generated: []byte(traeRule),
			rmdir: filepath.Dir(path), note: msg("wire.change.rule_drop", "path", short(o, path))})
	}
	return changes, notes, nil
}

func (traeTool) wireNotes(o *Options, st Status) []Msg {
	if strings.ContainsAny(o.Bin, " \t") {
		return []Msg{msg("wire.note.trae_space", "path", o.Bin)}
	}
	return nil
}

//go:build !no_mcp

package agentwire

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// jsonServer describes one MCP entry in a JSON settings file: Cursor's
// mcpServers and opencode's mcp, which differ in shape but are edited the
// same way.
type jsonServer struct {
	path      string
	container string // the member holding the servers
	want      any    // the entry wire writes
	matches   func(raw json.RawMessage) bool
	ours      func(raw json.RawMessage) bool
	// fresh are the members a file created by wire starts with.
	fresh []jsonMember
	mode  fs.FileMode
	// manual is what the operator adds by hand when the file cannot be
	// edited safely (a JSONC file with comments).
	manual string
}

func (s jsonServer) load(o *Options) ([]byte, *jsonObject, *jsonObject, error) {
	cur, err := readMaybe(realPath(s.path))
	if err != nil {
		return nil, nil, nil, err
	}
	root, err := parseObject(cur)
	if err != nil {
		// opencode accepts JSONC; anet's editor does not keep comments, so
		// it stops rather than silently dropping them.
		return nil, nil, nil, fmt.Errorf("%s 不是纯 JSON(%v;可能含注释),anet 不改写它。请手动加入:\n%s",
			short(o, s.path), err, s.manual)
	}
	if cur == nil {
		root = &jsonObject{members: append([]jsonMember{}, s.fresh...)}
	}
	servers, _, err := root.subObject(s.container)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %v", short(o, s.path), err)
	}
	return cur, root, servers, nil
}

func (s jsonServer) planWire(o *Options) (*change, error) {
	cur, root, servers, err := s.load(o)
	if err != nil {
		return nil, err
	}
	raw, has := servers.get("anet")
	if has && s.matches(raw) {
		return nil, nil
	}
	if has && !s.ours(raw) {
		return nil, &ConflictError{Path: s.path, What: "已有一个不是 anet 写的 " + s.container +
			".anet(命令不是 `anet mcp`);删除或改名后再运行 wire"}
	}
	want, err := marshalNoEscape(s.want)
	if err != nil {
		return nil, err
	}
	servers.set("anet", want)
	root.set(s.container, servers.raw())
	after, err := root.render(cur)
	if err != nil {
		return nil, err
	}
	return &change{path: s.path, before: cur, after: after, mode: s.mode,
		note: s.container + ".anet → " + short(o, s.path)}, nil
}

func (s jsonServer) planUnwire(o *Options) (*change, []string, error) {
	cur, root, servers, err := s.load(o)
	if err != nil || cur == nil {
		return nil, nil, err
	}
	raw, has := servers.get("anet")
	if !has {
		return nil, nil, nil
	}
	if !s.ours(raw) {
		return nil, []string{short(o, s.path) + " 的 " + s.container + ".anet 不是 anet 写的,未改动"}, nil
	}
	servers.del("anet")
	if servers.empty() {
		root.del(s.container)
	} else {
		root.set(s.container, servers.raw())
	}
	after, err := root.render(cur)
	if err != nil {
		return nil, nil, err
	}
	// A file left holding only what a new file starts with was created by
	// wire; removing it gives the original state back.
	del := root.empty() || sameMembers(root.members, s.fresh)
	return &change{path: s.path, before: cur, after: after, del: del,
		note: "删除 " + s.container + ".anet:" + short(o, s.path)}, nil, nil
}

func sameMembers(a, b []jsonMember) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].key != b[i].key || strings.TrimSpace(string(a[i].val)) != strings.TrimSpace(string(b[i].val)) {
			return false
		}
	}
	return true
}

// Cursor: ~/.cursor/mcp.json, read by the IDE and by cursor-agent. Cursor
// has no user-level rules file (user rules live in its settings UI), so
// there is no guide file: the MCP server's own instructions carry it. The
// rule file the old install wrote under ~/.cursor/rules — which Cursor does
// not read from the home directory, and whose text is out of date — is
// removed.
type cursorTool struct{}

func (cursorTool) name() string { return ToolCursor }

func cursorDir(o *Options) string               { return filepath.Join(o.Home, ".cursor") }
func (cursorTool) configPath(o *Options) string { return filepath.Join(cursorDir(o), "mcp.json") }
func cursorLegacy(o *Options) string {
	return filepath.Join(cursorDir(o), "rules", "agentnetwork-anet.mdc")
}

func (cursorTool) detected(o *Options) bool {
	return onPath(o, "cursor-agent", "cursor") || dirExists(cursorDir(o))
}

type stdioEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

func (t cursorTool) server(o *Options) jsonServer {
	want := stdioEntry{Command: o.Bin, Args: []string{"mcp"}, Env: mcpEnv(o)}
	snippet, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{"anet": want}}, "", "  ")
	return jsonServer{
		path: t.configPath(o), container: "mcpServers", want: want, mode: 0o644,
		matches: func(raw json.RawMessage) bool {
			var e stdioEntry
			return decodeLoose(raw, &e) && e.Command == want.Command &&
				sameStrings(e.Args, want.Args) && sameEnv(e.Env, want.Env)
		},
		ours: func(raw json.RawMessage) bool {
			var e stdioEntry
			return decodeLoose(raw, &e) && isAnetCommand(e.Command) && sameStrings(e.Args, []string{"mcp"})
		},
		manual: string(snippet),
	}
}

func (t cursorTool) planWire(o *Options) ([]change, error) {
	var changes []change
	c, err := t.server(o).planWire(o)
	if err != nil {
		return nil, err
	}
	if c != nil {
		changes = append(changes, *c)
	}
	if l, err := planLegacyFile(o, cursorLegacy(o)); err != nil {
		return nil, err
	} else if l != nil {
		changes = append(changes, *l)
	}
	return changes, nil
}

func (t cursorTool) planUnwire(o *Options) ([]change, []string, error) {
	var changes []change
	c, notes, err := t.server(o).planUnwire(o)
	if err != nil {
		return nil, nil, err
	}
	if c != nil {
		changes = append(changes, *c)
	}
	if l, err := planLegacyFile(o, cursorLegacy(o)); err != nil {
		return nil, nil, err
	} else if l != nil {
		changes = append(changes, *l)
	}
	return changes, notes, nil
}

// planLegacyFile removes a whole file the old install wrote, recognised by
// its content so an unrelated file of the same name is never deleted.
func planLegacyFile(o *Options, path string) (*change, error) {
	cur, err := readMaybe(realPath(path))
	if err != nil || cur == nil {
		return nil, err
	}
	if !strings.Contains(string(cur), "AgentNetwork (anet)") {
		return nil, nil
	}
	return &change{path: path, before: cur, del: true, note: "删除旧版规则文件:" + short(o, path)}, nil
}

// opencode: the mcp member of $XDG_CONFIG_HOME/opencode/opencode.json. Its
// shape is not Claude's — type "local", the command and its arguments in
// one array, an explicit enabled flag, and "environment" rather than "env"
// — so an entry copied from another tool's file would be ignored.
type opencodeTool struct{}

func (opencodeTool) name() string { return ToolOpenCode }

func opencodeDir(o *Options) string {
	return filepath.Join(envOr(o, "XDG_CONFIG_HOME", filepath.Join(o.Home, ".config")), "opencode")
}

// configPath is opencode.json, or opencode.jsonc when that is the only one
// there: opencode reads either, and anet edits the one in use.
func (opencodeTool) configPath(o *Options) string {
	j := filepath.Join(opencodeDir(o), "opencode.json")
	if _, err := os.Stat(j); err != nil {
		jc := filepath.Join(opencodeDir(o), "opencode.jsonc")
		if _, err := os.Stat(jc); err == nil {
			return jc
		}
	}
	return j
}

func opencodeAgentsMD(o *Options) string { return filepath.Join(opencodeDir(o), "AGENTS.md") }

func (opencodeTool) detected(o *Options) bool {
	return onPath(o, "opencode") || dirExists(opencodeDir(o))
}

type opencodeEntry struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command"`
	Enabled     *bool             `json:"enabled,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
}

func (t opencodeTool) server(o *Options) jsonServer {
	on := true
	want := opencodeEntry{Type: "local", Command: []string{o.Bin, "mcp"}, Enabled: &on, Environment: mcpEnv(o)}
	snippet, _ := json.MarshalIndent(map[string]any{"mcp": map[string]any{"anet": want}}, "", "  ")
	schema, _ := marshalNoEscape("https://opencode.ai/config.json")
	return jsonServer{
		path: t.configPath(o), container: "mcp", want: want, mode: 0o644,
		fresh: []jsonMember{{key: "$schema", val: schema}},
		matches: func(raw json.RawMessage) bool {
			var e opencodeEntry
			return decodeLoose(raw, &e) && e.Type == "local" && sameStrings(e.Command, want.Command) &&
				e.Enabled != nil && *e.Enabled && sameEnv(e.Environment, want.Environment)
		},
		ours: func(raw json.RawMessage) bool {
			var e opencodeEntry
			return decodeLoose(raw, &e) && len(e.Command) == 2 && isAnetCommand(e.Command[0]) && e.Command[1] == "mcp"
		},
		manual: string(snippet),
	}
}

func (t opencodeTool) planWire(o *Options) ([]change, error) {
	var changes []change
	c, err := t.server(o).planWire(o)
	if err != nil {
		return nil, err
	}
	if c != nil {
		changes = append(changes, *c)
	}
	p, err := planTextBlock(o, opencodeAgentsMD(o), personaMarkdown, "anet 指引块")
	if err != nil {
		return nil, err
	}
	if p != nil {
		changes = append(changes, *p)
	}
	return changes, nil
}

func (t opencodeTool) planUnwire(o *Options) ([]change, []string, error) {
	var changes []change
	c, notes, err := t.server(o).planUnwire(o)
	if err != nil {
		return nil, nil, err
	}
	if c != nil {
		changes = append(changes, *c)
	}
	p, err := planDropTextBlock(o, opencodeAgentsMD(o), "anet 指引块")
	if err != nil {
		return nil, nil, err
	}
	if p != nil {
		changes = append(changes, *p)
	}
	return changes, notes, nil
}

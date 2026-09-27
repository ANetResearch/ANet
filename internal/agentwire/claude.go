//go:build !no_mcp

package agentwire

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Claude Code.
//
// The MCP server goes into user scope — every project on this machine —
// through Claude Code's own CLI when it is on PATH (`claude mcp add -s user`,
// which knows its file's locking and format better than anet does), and by
// editing ~/.claude.json directly when it is not: the one-line installer may
// run before the operator's shell has claude on PATH. The guide goes in as a
// skill, loaded when relevant, instead of the ~3 KB block the old install
// appended to ~/.claude/CLAUDE.md and every session paid for; wire removes
// that block.
type claudeTool struct{}

func (claudeTool) name() string { return ToolClaude }

// claudeDir is Claude Code's configuration directory. CLAUDE_CONFIG_DIR
// moves it, and with it the user-scope settings file.
func claudeDir(o *Options) string {
	return envOr(o, "CLAUDE_CONFIG_DIR", filepath.Join(o.Home, ".claude"))
}

func claudeJSON(o *Options) string {
	if d := envOr(o, "CLAUDE_CONFIG_DIR", ""); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	return filepath.Join(o.Home, ".claude.json")
}

func claudeSkill(o *Options) string { return filepath.Join(claudeDir(o), "skills", "anet", "SKILL.md") }

// claudeLegacy is where the old `anet install --agent claude` appended its
// persona block.
func claudeLegacy(o *Options) string { return filepath.Join(claudeDir(o), "CLAUDE.md") }

func (claudeTool) configPath(o *Options) string { return claudeJSON(o) }

func (claudeTool) detected(o *Options) bool {
	if onPath(o, "claude") || dirExists(claudeDir(o)) {
		return true
	}
	_, err := os.Stat(claudeJSON(o))
	return err == nil
}

// claudeEntry is a stdio server in ~/.claude.json, in the member order
// `claude mcp add` writes.
type claudeEntry struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

func claudeWant(o *Options) claudeEntry {
	return claudeEntry{Type: "stdio", Command: o.Bin, Args: []string{"mcp"}, Env: mcpEnv(o)}
}

// matches reports whether raw is exactly the entry wire would write now.
func (claudeTool) matches(o *Options, raw json.RawMessage) bool {
	var e claudeEntry
	if !decodeLoose(raw, &e) {
		return false
	}
	w := claudeWant(o)
	return (e.Type == "" || e.Type == "stdio") && e.Command == w.Command &&
		sameStrings(e.Args, w.Args) && sameEnv(e.Env, w.Env)
}

// ours reports whether raw is an anet entry, current or not.
func (claudeTool) ours(raw json.RawMessage) bool {
	var e claudeEntry
	return decodeLoose(raw, &e) && (e.Type == "" || e.Type == "stdio") &&
		isAnetCommand(e.Command) && sameStrings(e.Args, []string{"mcp"})
}

// servers reads ~/.claude.json and its user-scope mcpServers.
func (claudeTool) servers(o *Options) (cur []byte, root, servers *jsonObject, err error) {
	path := claudeJSON(o)
	if cur, err = readMaybe(realPath(path)); err != nil {
		return nil, nil, nil, err
	}
	if root, err = parseObject(cur); err != nil {
		return nil, nil, nil, fmt.Errorf("%s 不是合法的 JSON(%v),anet 不改它", path, err)
	}
	if servers, _, err = root.subObject("mcpServers"); err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %v", path, err)
	}
	return cur, root, servers, nil
}

func (t claudeTool) planWire(o *Options) ([]change, error) {
	path := claudeJSON(o)
	cur, root, servers, err := t.servers(o)
	if err != nil {
		return nil, err
	}
	var changes []change
	raw, has := servers.get("anet")
	if !has || !t.matches(o, raw) {
		if has && !t.ours(raw) {
			return nil, &ConflictError{Path: path, What: "已有一个不是 anet 写的 mcpServers.anet(命令不是 `anet mcp`);" +
				"删除或改名(`claude mcp remove -s user anet`)后再运行 wire"}
		}
		if claude, err := o.LookPath("claude"); err == nil {
			w := claudeWant(o)
			var run [][]string
			if has {
				run = append(run, []string{claude, "mcp", "remove", "-s", "user", "anet"})
			}
			run = append(run, []string{claude, "mcp", "add", "-s", "user", "anet",
				"-e", "ANET_DATA_DIR=" + o.DataDir, "--", w.Command, "mcp"})
			changes = append(changes, change{
				path: path, before: cur, run: run,
				verify: func(after []byte) error {
					_, _, s, err := t.serversOf(after)
					if err != nil {
						return err
					}
					if raw, ok := s.get("anet"); !ok || !t.matches(o, raw) {
						return fmt.Errorf("之后 %s 里没有对应的 mcpServers.anet", path)
					}
					return nil
				},
				note: "claude mcp add -s user anet(" + short(o, path) + ")",
			})
		} else {
			want, _ := marshalNoEscape(claudeWant(o))
			servers.set("anet", want)
			root.set("mcpServers", servers.raw())
			after, err := root.render(cur)
			if err != nil {
				return nil, err
			}
			changes = append(changes, change{path: path, before: cur, after: after, mode: 0o600,
				note: "MCP 服务 anet(user 作用域)→ " + short(o, path) + "(claude 不在 PATH,直接改文件)"})
		}
	}
	skill, err := readMaybe(realPath(claudeSkill(o)))
	if err != nil {
		return nil, err
	}
	if string(skill) != skillMarkdown {
		changes = append(changes, change{path: claudeSkill(o), before: skill, after: []byte(skillMarkdown),
			mode: 0o644, note: "技能 → " + short(o, claudeSkill(o))})
	}
	legacy, err := planDropTextBlock(o, claudeLegacy(o), "旧版 anet 指引块")
	if err != nil {
		return nil, err
	}
	if legacy != nil {
		changes = append(changes, *legacy)
	}
	return changes, nil
}

// serversOf parses a ~/.claude.json already read.
func (claudeTool) serversOf(b []byte) ([]byte, *jsonObject, *jsonObject, error) {
	root, err := parseObject(b)
	if err != nil {
		return nil, nil, nil, err
	}
	s, _, err := root.subObject("mcpServers")
	return b, root, s, err
}

func (t claudeTool) planUnwire(o *Options) ([]change, []string, error) {
	path := claudeJSON(o)
	cur, root, servers, err := t.servers(o)
	if err != nil {
		return nil, nil, err
	}
	var changes []change
	var notes []string
	if raw, has := servers.get("anet"); has {
		if !t.ours(raw) {
			notes = append(notes, short(o, path)+" 的 mcpServers.anet 不是 anet 写的,未改动")
		} else if claude, err := o.LookPath("claude"); err == nil {
			changes = append(changes, change{
				path: path, before: cur,
				run: [][]string{{claude, "mcp", "remove", "-s", "user", "anet"}},
				verify: func(after []byte) error {
					_, _, s, err := t.serversOf(after)
					if err != nil {
						return err
					}
					if _, ok := s.get("anet"); ok {
						return fmt.Errorf("之后 %s 里仍有 mcpServers.anet", path)
					}
					return nil
				},
				note: "claude mcp remove -s user anet(" + short(o, path) + ")",
			})
		} else {
			servers.del("anet")
			if servers.empty() {
				root.del("mcpServers")
			} else {
				root.set("mcpServers", servers.raw())
			}
			after, err := root.render(cur)
			if err != nil {
				return nil, nil, err
			}
			// A file that held nothing but anet's entry was created by wire;
			// removing it is what gives the original state back.
			changes = append(changes, change{path: path, before: cur, after: after, del: root.empty(),
				note: "删除 MCP 服务 anet:" + short(o, path)})
		}
	}
	skillPath := claudeSkill(o)
	if skill, err := readMaybe(realPath(skillPath)); err != nil {
		return nil, nil, err
	} else if skill != nil {
		changes = append(changes, change{path: skillPath, before: skill, del: true, generated: []byte(skillMarkdown),
			rmdir: filepath.Dir(skillPath), note: "删除技能:" + short(o, skillPath)})
	}
	legacy, err := planDropTextBlock(o, claudeLegacy(o), "旧版 anet 指引块")
	if err != nil {
		return nil, nil, err
	}
	if legacy != nil {
		changes = append(changes, *legacy)
	}
	return changes, notes, nil
}

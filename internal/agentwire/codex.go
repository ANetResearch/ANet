//go:build !no_mcp

package agentwire

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Codex CLI: an [mcp_servers.anet] table in $CODEX_HOME/config.toml
// (default ~/.codex), inside a marked block, plus the short guide in
// AGENTS.md next to it.
//
// TOML forbids defining a table twice, so a second [mcp_servers.anet] would
// not be a duplicate Codex ignores — it would stop Codex loading its
// configuration at all. An existing one that anet did not write is
// therefore a conflict: reported with its line, and nothing is written.
type codexTool struct{}

func (codexTool) name() string { return ToolCodex }

func codexHome(o *Options) string {
	return envOr(o, "CODEX_HOME", filepath.Join(o.Home, ".codex"))
}

func codexConfig(o *Options) string   { return filepath.Join(codexHome(o), "config.toml") }
func codexAgentsMD(o *Options) string { return filepath.Join(codexHome(o), "AGENTS.md") }

func (codexTool) configPath(o *Options) string { return codexConfig(o) }

func (codexTool) detected(o *Options) bool {
	return onPath(o, "codex") || dirExists(codexHome(o))
}

var tomlMarkers = markers{begin: "# >>> anet", end: "# <<< anet <<<"}

const tomlBegin = "# >>> anet (managed by `anet agents wire`; edits inside this block are overwritten) >>>"

// codexBlock is the managed block. tool_timeout_sec is raised from Codex's
// 60-second default because wait_task blocks until a task moves, and a
// remote agent's task is measured in minutes; it matches the 15 minutes
// `anet mcp` itself allows one control-plane call.
func codexBlock(o *Options) []string {
	return []string{
		tomlBegin,
		"[mcp_servers.anet]",
		"command = " + tomlString(o.Bin),
		`args = ["mcp"]`,
		"env = { ANET_DATA_DIR = " + tomlString(o.DataDir) + " }",
		"startup_timeout_sec = 30",
		"tool_timeout_sec = 900",
		tomlMarkers.end,
	}
}

func (codexTool) planWire(o *Options) ([]change, error) {
	path := codexConfig(o)
	cur, err := readMaybe(realPath(path))
	if err != nil {
		return nil, err
	}
	lines := splitLines(string(cur))
	b, e, err := tomlMarkers.locate(lines, path)
	if err != nil {
		return nil, err
	}
	if line, what, named := codexConflict(lines, b, e); line > 0 {
		if named {
			what += ";anet 不改动它。删除或改名这张表后再运行 wire"
		}
		return nil, &ConflictError{Path: path, Line: line, What: what}
	}
	var out []string
	if b >= 0 {
		out = replaceBlock(lines, b, e, codexBlock(o))
	} else {
		out = appendBlock(lines, codexBlock(o))
	}
	var changes []change
	if after := joinLines(out); after != string(cur) {
		changes = append(changes, change{path: path, before: cur, after: []byte(after), mode: 0o600,
			note: "[mcp_servers.anet] → " + short(o, path)})
	}
	c, err := planTextBlock(o, codexAgentsMD(o), personaMarkdown, "anet 指引块")
	if err != nil {
		return nil, err
	}
	if c != nil {
		changes = append(changes, *c)
	}
	return changes, nil
}

func (codexTool) planUnwire(o *Options) ([]change, []string, error) {
	path := codexConfig(o)
	cur, err := readMaybe(realPath(path))
	if err != nil {
		return nil, nil, err
	}
	var changes []change
	var notes []string
	lines := splitLines(string(cur))
	b, e, err := tomlMarkers.locate(lines, path)
	if err != nil {
		return nil, nil, err
	}
	if b >= 0 {
		out := removeBlock(lines, b, e)
		changes = append(changes, change{path: path, before: cur, after: []byte(joinLines(out)),
			del: onlyBlank(out), note: "删除 [mcp_servers.anet]:" + short(o, path)})
	} else if line, _, named := codexConflict(lines, -1, -1); line > 0 && named {
		notes = append(notes, fmt.Sprintf("%s:%d 的 mcp_servers.anet 不是 anet 写的,未改动", short(o, path), line))
	}
	c, err := planDropTextBlock(o, codexAgentsMD(o), "anet 指引块")
	if err != nil {
		return nil, nil, err
	}
	if c != nil {
		changes = append(changes, *c)
	}
	return changes, notes, nil
}

var (
	tomlHeader = regexp.MustCompile(`^\s*\[\[?\s*([^\[\]]+?)\s*\]\]?\s*(#.*)?$`)
	tomlKey    = regexp.MustCompile(`^\s*((?:[A-Za-z0-9_-]+|"(?:[^"\\]|\\.)*"|'[^']*')(?:\s*\.\s*(?:[A-Za-z0-9_-]+|"(?:[^"\\]|\\.)*"|'[^']*'))*)\s*=(.*)$`)
	tomlAnetIn = regexp.MustCompile(`(^|[{,\s])("anet"|'anet'|anet)\s*[=.]`)
)

// codexConflict finds an mcp_servers.anet definition outside anet's block,
// in any of the forms TOML allows: a [mcp_servers.anet] (or
// [mcp_servers.anet.env]) header, a dotted key at the root or under
// [mcp_servers], or an inline table. An inline `mcp_servers = { … }`
// without anet in it is reported too: anet's block cannot be added to it.
// It returns the 1-based line and what it found, or 0; named is true when
// what it found is an anet entry rather than a form anet cannot extend.
//
// It reads lines, not TOML. Multi-line strings are skipped so a prompt that
// happens to quote a table header is not mistaken for one.
func codexConflict(lines []string, b, e int) (line int, what string, named bool) {
	var table []string
	inMulti := ""
	for i, l := range lines {
		if inBlock(i, b, e) {
			continue
		}
		if inMulti != "" {
			inMulti = tomlScan(l, inMulti)
			continue
		}
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if m := tomlHeader.FindStringSubmatch(t); m != nil {
			table = tomlPath(m[1])
			if len(table) >= 2 && table[0] == "mcp_servers" && table[1] == "anet" {
				return i + 1, "已有一张不是 anet 写的 [" + m[1] + "] 表", true
			}
			continue
		}
		if m := tomlKey.FindStringSubmatch(t); m != nil {
			full := append(append([]string{}, table...), tomlPath(m[1])...)
			if len(full) >= 2 && full[0] == "mcp_servers" && full[1] == "anet" {
				return i + 1, "已有不是 anet 写的 mcp_servers.anet 定义", true
			}
			if len(full) == 1 && full[0] == "mcp_servers" {
				if tomlAnetIn.MatchString(m[2]) {
					return i + 1, "行内表 mcp_servers 里已有不是 anet 写的 anet 条目", true
				}
				// An inline table is closed: a [mcp_servers.anet] header
				// after it redefines the table, and Codex stops loading its
				// configuration altogether.
				return i + 1, "mcp_servers 是行内表(mcp_servers = { … }),TOML 不允许再用 [mcp_servers.anet] 向它追加;" +
					"请把它改成 [mcp_servers.<名字>] 表的写法", false
			}
		}
		inMulti = tomlScan(l, "")
	}
	return 0, "", false
}

// tomlScan follows one line through TOML's strings and comments. in is the
// delimiter of the multi-line string the line starts inside (three double
// or three single quotes, or "" outside one); it returns the one the line
// ends inside. Three quotes inside a one-line string or a comment open
// nothing, and an escaped quote neither ends a basic string nor closes a
// multi-line one. Counting delimiters per line took the first for an
// opening, and every line after it — a real [mcp_servers.anet] among them
// — for string content (docs/notes/0033).
func tomlScan(l, in string) string {
	for i := 0; i < len(l); {
		if in != "" {
			n := closeMulti(l[i:], in)
			if n < 0 {
				return in
			}
			i, in = i+n, ""
			continue
		}
		switch {
		case l[i] == '#':
			return ""
		case strings.HasPrefix(l[i:], `"""`), strings.HasPrefix(l[i:], `'''`):
			in, i = l[i:i+3], i+3
		case l[i] == '"':
			i = skipBasic(l, i+1)
		case l[i] == '\'':
			j := strings.IndexByte(l[i+1:], '\'')
			if j < 0 {
				return ""
			}
			i += j + 2
		default:
			i++
		}
	}
	return in
}

// closeMulti is the length of s up to and including the delimiter that
// closes a multi-line string of delim, or -1. A basic one skips escaped
// characters; up to two quotes before the closing three belong to the
// string (`""""` ends a string with a quote in it).
func closeMulti(s, delim string) int {
	for i := 0; i < len(s); i++ {
		if delim == `"""` && s[i] == '\\' {
			i++
			continue
		}
		if strings.HasPrefix(s[i:], delim) {
			n := 3
			for n < 5 && i+n < len(s) && s[i+n] == delim[0] {
				n++
			}
			return i + n
		}
	}
	return -1
}

// skipBasic is the index after the quote that ends the basic string whose
// content starts at i, or len(l).
func skipBasic(l string, i int) int {
	for ; i < len(l); i++ {
		switch l[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(l)
}

// tomlPath splits a dotted TOML key into its parts, unquoting each. A
// basic-string part means what its escapes say ("an\u0065t" is anet), as
// Codex's TOML reader takes it (docs/notes/0033).
func tomlPath(key string) []string {
	var parts []string
	for _, p := range splitDotted(key) {
		p = strings.TrimSpace(p)
		switch {
		case len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"':
			p = tomlUnescape(p[1 : len(p)-1])
		case len(p) >= 2 && p[0] == '\'' && p[len(p)-1] == '\'':
			p = p[1 : len(p)-1]
		}
		parts = append(parts, p)
	}
	return parts
}

// tomlEscapes are TOML's one-character escapes (\e is TOML 1.1's).
var tomlEscapes = map[byte]byte{'b': '\b', 't': '\t', 'n': '\n', 'f': '\f', 'r': '\r', 'e': 0x1b, '"': '"', '\\': '\\'}

// tomlUnescape reads the escapes of a TOML basic string; one it does not
// know is kept as written.
func tomlUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		c := s[i+1]
		if r, ok := tomlEscapes[c]; ok {
			b.WriteByte(r)
			i++
			continue
		}
		n := map[byte]int{'u': 4, 'U': 8}[c]
		if n > 0 && i+2+n <= len(s) {
			if r, err := strconv.ParseUint(s[i+2:i+2+n], 16, 32); err == nil {
				b.WriteRune(rune(r))
				i += 1 + n
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// splitDotted splits on dots outside quotes.
func splitDotted(s string) []string {
	var parts []string
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '"' && c == '\\':
			i++ // an escaped character, a quote among them
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '.':
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// tomlString renders s as a TOML basic string. Only the escapes TOML 1.0
// defines are used (Go's %q would produce \x.., which TOML 1.0 rejects).
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\u00`)
			b.WriteByte("0123456789ABCDEF"[r>>4])
			b.WriteByte("0123456789ABCDEF"[r&0xF])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

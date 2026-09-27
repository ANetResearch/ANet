//go:build !no_mcp

package agentwire

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANet/internal/loopguard"
)

// Hermes (hermes-agent): mcp_servers.anet in $HERMES_HOME/config.yaml
// (default ~/.hermes), the short guide in SOUL.md, and — only when asked —
// a2a_agents entries that point Hermes' own A2A client at this node's local
// A2A interface (A2A-DESIGN §13.1, §11.1).
//
// The MCP entry runs the binary directly. Hermes quarantines an MCP entry
// that starts a shell interpreter with network or persistence side effects
// (its mcp_security.py), so `sh -c "anet mcp"` would be written and then
// disabled on Hermes' next start (docs/notes/0008 §7.2).
//
// An a2a_agents entry carries the local A2A bearer token (a2a_token.txt,
// X5). The file is therefore kept 0600 — Hermes itself narrows it to that
// on most systems — and unwire takes the tokens out with the entries.
type hermesTool struct{}

func (hermesTool) name() string { return ToolHermes }

func hermesHome(o *Options) string {
	return envOr(o, "HERMES_HOME", filepath.Join(o.Home, ".hermes"))
}

func hermesConfig(o *Options) string { return filepath.Join(hermesHome(o), "config.yaml") }
func hermesSoul(o *Options) string   { return filepath.Join(hermesHome(o), "SOUL.md") }

func (hermesTool) configPath(o *Options) string { return hermesConfig(o) }

// detected does not look at /opt/data, which the old install counted as a
// Hermes home: that directory exists on plenty of servers that have never
// seen Hermes, and the install wrote a SOUL.md into it.
func (hermesTool) detected(o *Options) bool {
	return onPath(o, "hermes") || dirExists(hermesHome(o))
}

var (
	hermesMCP = yamlMap{key: "mcp_servers",
		markers: markers{begin: "# >>> anet mcp", end: "# <<< anet mcp <<<"},
		begin:   "# >>> anet mcp (managed by `anet agents wire`; edits inside this block are overwritten) >>>"}
	hermesA2A = yamlMap{key: "a2a_agents",
		markers: markers{begin: "# >>> anet a2a", end: "# <<< anet a2a <<<"},
		begin:   "# >>> anet a2a (managed by `anet agents wire`; holds the local A2A token — keep this file 0600) >>>"}
)

// hermesMCPBody is the mcp_servers.anet entry. timeout is Hermes' per-call
// limit (default 120 s), raised for wait_task as for Codex.
func hermesMCPBody(o *Options) []string {
	return []string{
		"anet:",
		"  command: " + yamlString(o.Bin),
		`  args: ["mcp"]`,
		"  env:",
		"    ANET_DATA_DIR: " + yamlString(o.DataDir),
		"  timeout: 900",
		"  connect_timeout: 30",
	}
}

// a2aEndpoint is what one a2a_agents entry needs from this node.
type a2aEndpoint struct {
	addr  string // host:port of the local A2A interface
	token string
}

// readA2A reads the local A2A interface's address and token. Both are
// written by module/a2a when the daemon first serves it; before that, or in
// a build without the module, there is nothing to point Hermes at.
func readA2A(o *Options) (a2aEndpoint, error) {
	addrPath := a2aStatePath(o.DataDir, A2AAddrFile)
	tokPath := a2aStatePath(o.DataDir, A2ATokenFile)
	ab, err := os.ReadFile(addrPath)
	if errors.Is(err, fs.ErrNotExist) {
		return a2aEndpoint{}, fmt.Errorf("找不到 %s:本机 A2A 接口还没有启动过。它由 daemon 的 a2a 模块在第一次启动时写出"+
			"(-tags no_a2a 的构建没有这个模块);先 `anet up`,再运行本命令", addrPath)
	}
	if err != nil {
		return a2aEndpoint{}, err
	}
	tb, err := os.ReadFile(tokPath)
	if errors.Is(err, fs.ErrNotExist) {
		return a2aEndpoint{}, fmt.Errorf("找不到 %s:本机 A2A 令牌还没有生成;先 `anet up`,再运行本命令", tokPath)
	}
	if err != nil {
		return a2aEndpoint{}, err
	}
	addr, err := parseA2AAddr(string(ab))
	if err != nil {
		return a2aEndpoint{}, fmt.Errorf("%s: %v", addrPath, err)
	}
	tok := strings.TrimSpace(string(tb))
	if tok == "" || strings.ContainsAny(tok, " \t\r\n\"'\\") {
		return a2aEndpoint{}, fmt.Errorf("%s 的内容不是一个令牌", tokPath)
	}
	return a2aEndpoint{addr: addr, token: tok}, nil
}

// parseA2AAddr accepts host:port (or an http:// URL of one) and requires a
// loopback host: the interface never listens anywhere else (§11.1), and a
// token must not be written into a URL that leaves the machine.
//
// "Loopback" is the interface's own rule (internal/loopguard): 127.0.0.1,
// localhost or [::1]. Any other 127/8 or IPv4-mapped address reaches the
// machine too, but the interface answers such a Host 421, so wiring it
// would give Hermes an entry that can never work.
func parseA2AAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimRight(s, "/")
	host, port, err := net.SplitHostPort(s)
	if n, perr := strconv.Atoi(port); err != nil || perr != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%q 不是 host:port", s)
	}
	if loopguard.CheckLoopbackAddr(s) != nil {
		return "", fmt.Errorf("%q 不是回环地址(本机 A2A 接口只在 127.0.0.1、localhost 或 [::1] 上监听)", s)
	}
	return net.JoinHostPort(host, port), nil
}

// a2aURL is the per-agent base URL. Hermes fetches
// <url>/.well-known/agent-card.json with the bearer token and posts to the
// JSON-RPC interface the proxy card names (§11.2, §11.3).
func a2aURL(addr, aid string) string {
	return "http://" + addr + "/a2a/v1/agents/" + aid
}

func hermesA2ABody(ep a2aEndpoint, aids []string) []string {
	var body []string
	for _, aid := range aids {
		body = append(body,
			`"`+aid+`":`,
			"  url: "+yamlString(a2aURL(ep.addr, aid)),
			"  auth:",
			"    type: bearer",
			"    token: "+yamlString(ep.token),
			"  timeout: 3600",
		)
	}
	return body
}

// a2aEntry is one entry read back from anet's a2a block.
type a2aEntry struct {
	aid, url, token string
}

var a2aKeyLine = regexp.MustCompile(`^"([a-z0-9]+)":\s*$`)

// hermesA2AEntries reads the entries anet wrote, in file order.
func hermesA2AEntries(src, path string) ([]a2aEntry, error) {
	body, err := hermesA2A.blockBody(src, path)
	if err != nil {
		return nil, err
	}
	var out []a2aEntry
	for _, l := range body {
		if m := a2aKeyLine.FindStringSubmatch(l); m != nil {
			out = append(out, a2aEntry{aid: m[1]})
			continue
		}
		if len(out) == 0 {
			continue
		}
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "url:"):
			out[len(out)-1].url = yamlUnquote(strings.TrimPrefix(t, "url:"))
		case strings.HasPrefix(t, "token:"):
			out[len(out)-1].token = yamlUnquote(strings.TrimPrefix(t, "token:"))
		}
	}
	return out, nil
}

func (t hermesTool) planWire(o *Options) ([]change, error) {
	path := hermesConfig(o)
	cur, err := readMaybe(realPath(path))
	if err != nil {
		return nil, err
	}
	out, err := hermesMCP.set(string(cur), path, hermesMCPBody(o), []string{"anet"})
	if err != nil {
		return nil, err
	}
	var notes []string
	if out != string(cur) {
		notes = append(notes, "mcp_servers.anet")
	}

	existing, err := hermesA2AEntries(out, path)
	if err != nil {
		return nil, err
	}
	if len(o.A2A) > 0 || (o.Refresh && len(existing) > 0) {
		ep, err := readA2A(o)
		if err != nil {
			return nil, err
		}
		var aids []string
		seen := map[string]bool{}
		for _, e := range existing {
			if !seen[e.aid] {
				seen[e.aid] = true
				aids = append(aids, e.aid)
			}
		}
		var added []string
		for _, a := range o.A2A {
			if !seen[a] {
				seen[a] = true
				aids = append(aids, a)
				added = append(added, a)
			}
		}
		before := out
		if out, err = hermesA2A.set(out, path, hermesA2ABody(ep, aids), added); err != nil {
			return nil, err
		}
		if out != before {
			notes = append(notes, fmt.Sprintf("a2a_agents(%d 个,指向 %s)", len(aids), ep.addr))
		}
	}

	var changes []change
	holdsToken := len(a2aEntriesOf(out, path)) > 0
	if out != string(cur) {
		c := change{path: path, before: cur, after: []byte(out), mode: 0o600,
			note: strings.Join(notes, "、") + " → " + short(o, path)}
		if holdsToken {
			c.narrow = 0o600
		}
		changes = append(changes, c)
	} else if holdsToken && fileMode(realPath(path))&0o077 != 0 {
		changes = append(changes, change{path: path, before: cur, chmodOnly: true, narrow: 0o600,
			note: short(o, path) + " 含本机 A2A 令牌,权限收紧为 0600"})
	}
	p, err := planTextBlock(o, hermesSoul(o), hermesPersona, "anet 指引块")
	if err != nil {
		return nil, err
	}
	if p != nil {
		changes = append(changes, *p)
	}
	return changes, nil
}

// a2aEntriesOf is hermesA2AEntries for text already known to parse.
func a2aEntriesOf(src, path string) []a2aEntry {
	e, _ := hermesA2AEntries(src, path)
	return e
}

func (t hermesTool) planUnwire(o *Options) ([]change, []string, error) {
	path := hermesConfig(o)
	cur, err := readMaybe(realPath(path))
	if err != nil {
		return nil, nil, err
	}
	src := string(cur)
	var what []string
	var notes []string
	if len(o.A2A) > 0 {
		// Only the named entries; the MCP entry and the others stay.
		existing, err := hermesA2AEntries(src, path)
		if err != nil {
			return nil, nil, err
		}
		drop := map[string]bool{}
		for _, a := range o.A2A {
			drop[a] = true
		}
		var keep []string
		for _, e := range existing {
			if !drop[e.aid] {
				keep = append(keep, e.aid)
			}
		}
		if len(keep) < len(existing) {
			if len(keep) == 0 {
				src, _, err = hermesA2A.drop(src, path)
			} else {
				// The remaining entries keep their own lines; only the
				// dropped ones go, so nothing needs the token file.
				src, err = removeA2AEntries(src, path, drop)
			}
			if err != nil {
				return nil, nil, err
			}
			what = append(what, fmt.Sprintf("a2a_agents 条目 %d 个", len(existing)-len(keep)))
		} else {
			notes = append(notes, "a2a_agents 里没有 anet 写的这些条目")
		}
	} else {
		var had bool
		if src, had, err = hermesMCP.drop(src, path); err != nil {
			return nil, nil, err
		} else if had {
			what = append(what, "mcp_servers.anet")
		}
		if src, had, err = hermesA2A.drop(src, path); err != nil {
			return nil, nil, err
		} else if had {
			what = append(what, "a2a_agents(含令牌)")
		}
		if k, _, _ := hermesMCP.topKey(splitLines(src)); k >= 0 {
			lines := splitLines(src)
			if line := hasChildKey(lines, k, regionEnd(lines, k), childIndent(lines, k, regionEnd(lines, k)), -1, -1, "anet"); line > 0 {
				notes = append(notes, fmt.Sprintf("%s:%d 的 mcp_servers.anet 不是 anet 写的,未改动", short(o, path), line))
			}
		}
	}
	var changes []change
	if src != string(cur) {
		out := splitLines(src)
		c := change{path: path, before: cur, after: []byte(src), del: onlyBlank(out),
			note: "删除 " + strings.Join(what, "、") + ":" + short(o, path)}
		if len(a2aEntriesOf(src, path)) > 0 {
			c.narrow = 0o600 // the entries left behind still carry the token
		}
		changes = append(changes, c)
	}
	if len(o.A2A) == 0 {
		p, err := planDropTextBlock(o, hermesSoul(o), "anet 指引块")
		if err != nil {
			return nil, nil, err
		}
		if p != nil {
			changes = append(changes, *p)
		}
	}
	return changes, notes, nil
}

// removeA2AEntries deletes the named entries from anet's a2a block, each
// entry being its key line and the more-indented lines under it.
func removeA2AEntries(src, path string, drop map[string]bool) (string, error) {
	lines := splitLines(src)
	b, e, err := hermesA2A.markers.locate(lines, path)
	if err != nil || b < 0 {
		return src, err
	}
	ind := indentOf(lines[b])
	out := append([]string{}, lines[:b+1]...)
	skipping := false
	for _, l := range lines[b+1 : e] {
		if indentOf(l) == ind {
			m := a2aKeyLine.FindStringSubmatch(strings.TrimSpace(l))
			skipping = m != nil && drop[m[1]]
		}
		if !skipping {
			out = append(out, l)
		}
	}
	out = append(out, lines[e:]...)
	return joinLines(out), nil
}

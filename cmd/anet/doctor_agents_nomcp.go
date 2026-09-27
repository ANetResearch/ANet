//go:build no_mcp

package main

// doctor_agents_nomcp.go is doctor's view of the coding tools in a build
// without the MCP server (-tags no_mcp), which links no internal/agentwire:
// the equivalent of its Inspect made by reading each tool's configuration
// file. It finds the same entries and the same Hermes a2a_agents problems
// (0017 Q13: a token that is not a2a_token.txt's, a port that is not
// a2a_addr.txt's), but cannot tell whether an entry is exactly what wire
// would write.

import (
	"encoding/json"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/module"
)

// inspectAgents reads the tools' configuration files.
func inspectAgents(layout daemon.Layout, env doctorEnv) agentsView {
	v := agentsView{tools: probeAgentWiring(env)}
	for _, a := range v.tools {
		if a.Wired {
			v.note = "this binary was built without the MCP server (-tags no_mcp): an anet entry in a coding " +
				"tool reaches anet's tools only if it runs a build that has it"
			break
		}
	}
	if env.hermesHome == "" {
		return v
	}
	v.hermesConfig = filepath.Join(env.hermesHome, "config.yaml")
	b, err := os.ReadFile(v.hermesConfig)
	if err != nil {
		return v
	}
	a2aDir := module.StatePath(layout.Root, module.A2AModuleName)
	addr := ""
	if ab, err := os.ReadFile(filepath.Join(a2aDir, module.A2AAddrFile)); err == nil {
		addr = strings.TrimSpace(string(ab))
	}
	v.a2a = hermesA2AAgents(string(b), addr)
	// Only entries on this node's A2A port are this node's; the token is
	// compared for those (entries of another identity carry its token).
	tok := hermesTokenState(string(b), statFile(filepath.Join(a2aDir, module.A2ATokenFile)))
	for i := range v.a2a {
		v.a2a[i].Token = "unknown"
		if v.a2a[i].Matches {
			v.a2a[i].Token = tok
		}
	}
	return v
}

// probeAgentWiring looks for an anet entry in each coding tool's own
// configuration file, where `anet agents wire` puts it. It reads files
// only; whether the tool can start `anet mcp` is not checked (Handshake
// "not_checked"), and whether the entry is current cannot be told without
// agentwire, so a wired tool counts as current.
func probeAgentWiring(env doctorEnv) []agentWire {
	out := []agentWire{}
	add := func(tool, cfg string, present, wired bool, detail string) {
		out = append(out, agentWire{Tool: tool, Config: cfg, Present: present, Wired: wired, Current: wired,
			Detail: detail, Handshake: "not_checked"})
	}
	if env.home != "" {
		probeHomeTools(env.home, add)
	}

	if env.hermesHome != "" {
		hermes := filepath.Join(env.hermesHome, "config.yaml")
		hp, hw := false, false
		if b, err := os.ReadFile(hermes); err == nil {
			hp, hw = true, yamlHasChild(string(b), "mcp_servers", "anet")
		}
		add("hermes", hermes, hp, hw, "")
	}
	return out
}

// probeHomeTools looks at the tools whose configuration lives under the
// home directory.
func probeHomeTools(h string, add func(tool, cfg string, present, wired bool, detail string)) {
	claude := filepath.Join(h, ".claude.json")
	p, w := jsonHasPath(claude, "mcpServers", "anet")
	detail := ""
	if fileExists(filepath.Join(h, ".claude", "skills", "anet", "SKILL.md")) {
		detail = "skill ~/.claude/skills/anet/SKILL.md present"
	}
	add("claude", claude, p || fileExists(filepath.Join(h, ".claude")), w, detail)

	codex := filepath.Join(h, ".codex", "config.toml")
	cp, cw := false, false
	if b, err := os.ReadFile(codex); err == nil {
		cp = true
		for _, ln := range strings.Split(string(b), "\n") {
			t := strings.ReplaceAll(strings.TrimSpace(ln), " ", "")
			if t == "[mcp_servers.anet]" || t == `[mcp_servers."anet"]` {
				cw = true
			}
		}
	}
	add("codex", codex, cp, cw, "")

	cursor := filepath.Join(h, ".cursor", "mcp.json")
	p, w = jsonHasPath(cursor, "mcpServers", "anet")
	add("cursor", cursor, p || fileExists(filepath.Join(h, ".cursor")), w, "")

	opencode := filepath.Join(h, ".config", "opencode", "opencode.json")
	p, w = jsonHasPath(opencode, "mcp", "anet")
	add("opencode", opencode, p, w, "")
}

// jsonHasPath reports whether a JSON file exists and has the nested key
// path keys.
func jsonHasPath(path string, keys ...string) (present, wired bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return true, false
	}
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return true, false
		}
		if cur, ok = mm[k]; !ok {
			return true, false
		}
	}
	return true, true
}

// yamlHasChild reports whether a top-level YAML mapping key has a child
// key: `parent:` at some indent, then `child:` indented deeper before the
// block ends. A line scan, not a parser; the module has no YAML dependency
// and needs none for this.
func yamlHasChild(text, parent, child string) bool {
	in, indent := false, 0
	for _, ln := range strings.Split(text, "\n") {
		body := ln
		if i := strings.Index(body, " #"); i >= 0 {
			body = body[:i]
		}
		t := strings.TrimSpace(body)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		ind := len(body) - len(strings.TrimLeft(body, " \t"))
		if in {
			if ind <= indent {
				in = false
			} else if strings.HasPrefix(t, child+":") || strings.HasPrefix(t, `"`+child+`":`) {
				return true
			}
		}
		if !in && (t == parent+":" || t == `"`+parent+`":`) {
			in, indent = true, ind
		}
	}
	return false
}

var a2aURLRe = regexp.MustCompile(`https?://[^\s"'<>]+/a2a/v1/agents/[^\s"'<>]+`)

// hermesTokenState compares the tokens of the a2a_agents entries with the
// local A2A token, without reporting either: "current" when the token file's
// token is in the Hermes config, "stale" when it is not, "unknown" when there
// is no token file to compare with. The local A2A token does not expire; it
// goes stale for Hermes when a2a_token.txt is replaced.
func hermesTokenState(text string, token fileState) string {
	if !token.Present {
		return "unknown"
	}
	b, err := os.ReadFile(token.Path)
	tok := strings.TrimSpace(string(b))
	if err != nil || tok == "" {
		return "unknown"
	}
	if strings.Contains(text, tok) {
		return "current"
	}
	return "stale"
}

// hermesA2AAgents finds the a2a_agents URLs that point at an anet local A2A
// interface and compares each one's port with a2aAddr.
func hermesA2AAgents(text, a2aAddr string) []a2aAgentEntry {
	_, wantPort, _ := net.SplitHostPort(a2aAddr)
	out := []a2aAgentEntry{}
	for _, raw := range a2aURLRe.FindAllString(text, -1) {
		// A flow mapping ({url: …, auth: …}) leaves its punctuation on the match.
		raw = strings.TrimRight(raw, ",}]")
		if _, err := url.Parse(raw); err != nil {
			continue
		}
		port := urlPort(raw)
		out = append(out, a2aAgentEntry{URL: raw, Port: port, Matches: wantPort != "" && port == wantPort})
	}
	return out
}

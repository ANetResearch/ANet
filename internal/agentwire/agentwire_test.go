//go:build !no_mcp

package agentwire

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// host is a fake machine: a temporary HOME, a data dir, an environment and
// a PATH that the test controls. Nothing here reads the developer's real
// home or runs a real tool — a test that edited the operator's own Claude
// or Codex settings would be a test nobody runs twice.
type host struct {
	t     *testing.T
	home  string
	data  string
	env   map[string]string
	path  map[string]string // executable name → path
	calls [][]string
	run   func(name string, args []string) ([]byte, error)
	now   time.Time
}

const testBin = "/opt/anet/bin/anet"

func newHost(t *testing.T) *host {
	t.Helper()
	root := t.TempDir()
	h := &host{t: t, home: filepath.Join(root, "home"), data: filepath.Join(root, "home", ".anet"),
		env: map[string]string{}, path: map[string]string{},
		now: time.Date(2026, 9, 27, 10, 11, 12, 0, time.UTC)}
	if err := os.MkdirAll(h.home, 0o700); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *host) opts() Options {
	return Options{
		Bin: testBin, DataDir: h.data, Home: h.home,
		Getenv: func(k string) string { return h.env[k] },
		LookPath: func(n string) (string, error) {
			if p, ok := h.path[n]; ok {
				return p, nil
			}
			return "", errors.New("not on PATH")
		},
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			h.calls = append(h.calls, append([]string{name}, args...))
			if h.run == nil {
				h.t.Fatalf("unexpected command: %s %v", name, args)
			}
			return h.run(name, args)
		},
		Now: func() time.Time { return h.now },
	}
}

func (h *host) p(rel string) string { return filepath.Join(h.home, filepath.FromSlash(rel)) }

func (h *host) write(rel, content string, mode fs.FileMode) {
	h.t.Helper()
	p := h.p(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		h.t.Fatal(err)
	}
}

func (h *host) read(rel string) string {
	h.t.Helper()
	b, err := os.ReadFile(h.p(rel))
	if err != nil {
		h.t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func (h *host) exists(rel string) bool {
	_, err := os.Lstat(h.p(rel))
	return err == nil
}

func (h *host) mode(rel string) fs.FileMode {
	h.t.Helper()
	fi, err := os.Stat(h.p(rel))
	if err != nil {
		h.t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// snapshot is every regular file under HOME except anet's backups.
func (h *host) snapshot() map[string]string {
	h.t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(h.home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(p, ".anet-bak-") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr == nil {
			rel, _ := filepath.Rel(h.home, p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

func (h *host) backups() []string {
	var out []string
	_ = filepath.WalkDir(h.home, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(p, ".anet-bak-") {
			rel, _ := filepath.Rel(h.home, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (h *host) wire(tools ...string) []Result {
	h.t.Helper()
	return h.wireWith(h.opts(), tools...)
}

func (h *host) wireWith(o Options, tools ...string) []Result {
	h.t.Helper()
	rs, err := Wire(o, tools, len(tools) == 0)
	if err != nil {
		h.t.Fatalf("wire %v: %v", tools, err)
	}
	return rs
}

func (h *host) unwire(tools ...string) []Result {
	h.t.Helper()
	return h.unwireWith(h.opts(), tools...)
}

func (h *host) unwireWith(o Options, tools ...string) []Result {
	h.t.Helper()
	rs, err := Unwire(o, tools, len(tools) == 0)
	if err != nil {
		h.t.Fatalf("unwire %v: %v", tools, err)
	}
	return rs
}

func one(t *testing.T, rs []Result, want Status) Result {
	t.Helper()
	if len(rs) != 1 {
		t.Fatalf("want one result, got %d: %+v", len(rs), rs)
	}
	if rs[0].Status != want {
		t.Fatalf("%s: status %s, want %s (err %v, changes %v, notes %v)",
			rs[0].Tool, rs[0].Status, want, rs[0].Err, rs[0].Changes, rs[0].Notes)
	}
	return rs[0]
}

// The operator's existing configuration, per tool, in the format the tool
// itself writes. Round trips start from these so "unwire gives back the
// original" is checked against content anet did not write.
var seeds = map[string]map[string]string{
	ToolClaude: {
		".claude.json": "{\n  \"numStartups\": 3,\n  \"theme\": \"dark\",\n  \"mcpServers\": {\n    \"other\": {\n" +
			"      \"type\": \"stdio\",\n      \"command\": \"npx\",\n      \"args\": [\n        \"-y\",\n        \"x\"\n      ]\n" +
			"    }\n  },\n  \"autoUpdates\": false\n}\n",
		".claude/CLAUDE.md": "# My rules\n\nAlways write tests.\n",
	},
	ToolCodex: {
		".codex/config.toml": "model = \"gpt-5\"\n\n[mcp_servers.docs]\ncommand = \"docs-mcp\"\nargs = []\n",
		".codex/AGENTS.md":   "# Codex notes\n\nBe brief.\n",
	},
	ToolCursor: {
		".cursor/mcp.json": "{\n  \"mcpServers\": {\n    \"other\": {\n      \"command\": \"other-mcp\",\n      \"args\": []\n    }\n  }\n}\n",
	},
	ToolOpenCode: {
		".config/opencode/opencode.json": "{\n  \"$schema\": \"https://opencode.ai/config.json\",\n  \"theme\": \"tokyonight\",\n" +
			"  \"mcp\": {\n    \"other\": {\n      \"type\": \"remote\",\n      \"url\": \"https://example.com/mcp\"\n    }\n  }\n}\n",
		".config/opencode/AGENTS.md": "# opencode rules\n",
	},
	ToolHermes: {
		".hermes/config.yaml": "model:\n  default: claude-sonnet\n\nmcp_servers:\n  filesystem:\n    command: \"npx\"\n" +
			"    args: [\"-y\", \"@modelcontextprotocol/server-filesystem\", \"/tmp\"]\n\ndisplay:\n  compact: true\n",
		".hermes/SOUL.md": "You are a helpful agent.\n",
	},
}

// wiredFiles are the files wire must leave anet's entry in, per tool.
var wiredFiles = map[string][]string{
	ToolClaude:   {".claude.json", ".claude/skills/anet/SKILL.md"},
	ToolCodex:    {".codex/config.toml", ".codex/AGENTS.md"},
	ToolCursor:   {".cursor/mcp.json"},
	ToolOpenCode: {".config/opencode/opencode.json", ".config/opencode/AGENTS.md"},
	ToolHermes:   {".hermes/config.yaml", ".hermes/SOUL.md"},
}

// Every tool, from the operator's existing configuration: wire writes an
// entry the tool can read, a second wire changes nothing, and unwire gives
// back the files as they were — byte for byte.
func TestWireRoundTripsEveryTool(t *testing.T) {
	for _, tool := range Tools() {
		t.Run(tool, func(t *testing.T) {
			h := newHost(t)
			for rel, content := range seeds[tool] {
				h.write(rel, content, 0o644)
			}
			orig := h.snapshot()

			r := one(t, h.wire(tool), Written)
			// One per existing file wire edits: Claude's CLAUDE.md has no
			// old anet block, so only ~/.claude.json is edited there.
			wantBackups := len(seeds[tool])
			if tool == ToolClaude {
				wantBackups = 1
			}
			if len(r.Backups) != wantBackups {
				t.Errorf("backups %v, want %d", r.Backups, wantBackups)
			}
			for _, rel := range wiredFiles[tool] {
				if !h.exists(rel) {
					t.Fatalf("wire did not write %s", rel)
				}
			}
			checkWired(t, h, tool)
			after := h.snapshot()

			one(t, h.wire(tool), UpToDate)
			if got := h.snapshot(); !reflect.DeepEqual(got, after) {
				t.Fatalf("a second wire changed files:\n%s", diffSnap(after, got))
			}

			one(t, h.unwire(tool), Removed)
			if got := h.snapshot(); !reflect.DeepEqual(got, orig) {
				t.Fatalf("unwire did not restore the original files:\n%s", diffSnap(orig, got))
			}
			one(t, h.unwire(tool), NotWired)
		})
	}
}

// From a home where the tool has never written anything: wire creates the
// files, unwire removes them again.
func TestWireFromNothingAndBack(t *testing.T) {
	for _, tool := range Tools() {
		t.Run(tool, func(t *testing.T) {
			h := newHost(t)
			r := one(t, h.wire(tool), Written)
			if len(r.Backups) != 0 {
				t.Errorf("nothing existed, yet wire made backups %v", r.Backups)
			}
			if len(r.Notes) == 0 {
				t.Errorf("an undetected tool wired by name should say it was not detected")
			}
			checkWired(t, h, tool)
			one(t, h.unwire(tool), Removed)
			if got := h.snapshot(); len(got) != 0 {
				t.Fatalf("unwire left files behind: %v", keys(got))
			}
		})
	}
}

// checkWired reads each tool's file the way the tool does and checks that
// anet's entry says what it must: this binary by absolute path, `mcp`, and
// this identity's data dir.
func checkWired(t *testing.T, h *host, tool string) {
	t.Helper()
	switch tool {
	case ToolClaude:
		var f struct {
			MCPServers map[string]claudeEntry `json:"mcpServers"`
		}
		mustJSON(t, h.read(".claude.json"), &f)
		e := f.MCPServers["anet"]
		if e.Type != "stdio" || e.Command != testBin || !sameStrings(e.Args, []string{"mcp"}) || e.Env["ANET_DATA_DIR"] != h.data {
			t.Fatalf("claude entry: %+v", e)
		}
		skill := h.read(".claude/skills/anet/SKILL.md")
		if !strings.HasPrefix(skill, "---\nname: anet\ndescription: ") {
			t.Fatalf("SKILL.md has no frontmatter Claude Code can read:\n%s", skill[:80])
		}
	case ToolCodex:
		cfg := h.read(".codex/config.toml")
		for _, want := range []string{"[mcp_servers.anet]", `command = "` + testBin + `"`, `args = ["mcp"]`,
			`env = { ANET_DATA_DIR = "` + h.data + `" }`} {
			if !strings.Contains(cfg, want) {
				t.Fatalf("config.toml lacks %q:\n%s", want, cfg)
			}
		}
		if strings.Count(cfg, "[mcp_servers.anet]") != 1 {
			t.Fatalf("config.toml defines the table more than once:\n%s", cfg)
		}
		checkPersona(t, h.read(".codex/AGENTS.md"))
	case ToolCursor:
		var f struct {
			MCPServers map[string]stdioEntry `json:"mcpServers"`
		}
		mustJSON(t, h.read(".cursor/mcp.json"), &f)
		e := f.MCPServers["anet"]
		if e.Command != testBin || !sameStrings(e.Args, []string{"mcp"}) || e.Env["ANET_DATA_DIR"] != h.data {
			t.Fatalf("cursor entry: %+v", e)
		}
	case ToolOpenCode:
		var f struct {
			MCP map[string]opencodeEntry `json:"mcp"`
		}
		mustJSON(t, h.read(".config/opencode/opencode.json"), &f)
		e := f.MCP["anet"]
		if e.Type != "local" || !sameStrings(e.Command, []string{testBin, "mcp"}) || e.Enabled == nil || !*e.Enabled ||
			e.Environment["ANET_DATA_DIR"] != h.data {
			t.Fatalf("opencode entry: %+v", e)
		}
		checkPersona(t, h.read(".config/opencode/AGENTS.md"))
	case ToolHermes:
		cfg := h.read(".hermes/config.yaml")
		for _, want := range []string{"  anet:\n", `    command: "` + testBin + `"`, `    args: ["mcp"]`,
			`      ANET_DATA_DIR: "` + h.data + `"`} {
			if !strings.Contains(cfg, want) {
				t.Fatalf("config.yaml lacks %q:\n%s", want, cfg)
			}
		}
		if strings.Contains(cfg, "sh -c") || strings.Contains(cfg, "/bin/sh") {
			t.Fatal("Hermes quarantines MCP entries that start a shell; the entry must run anet directly")
		}
		if strings.Count(cfg, "\nmcp_servers:") > 1 || (strings.HasPrefix(cfg, "mcp_servers:") && strings.Contains(cfg, "\nmcp_servers:")) {
			t.Fatalf("config.yaml has two mcp_servers keys:\n%s", cfg)
		}
		checkPersona(t, h.read(".hermes/SOUL.md"))
	}
}

func checkPersona(t *testing.T, s string) {
	t.Helper()
	if strings.Count(s, "<!-- anet:begin") != 1 || strings.Count(s, "<!-- anet:end -->") != 1 {
		t.Fatalf("want exactly one anet block:\n%s", s)
	}
	if !strings.Contains(s, "AgentNetwork") || !strings.Contains(s, "send_message") || !strings.Contains(s, "wait_task") {
		t.Fatalf("persona does not teach the tools:\n%s", s)
	}
}

func mustJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("not valid JSON (%v):\n%s", err, s)
	}
}

func keys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func diffSnap(want, got map[string]string) string {
	var b strings.Builder
	for _, k := range keys(want) {
		if g, ok := got[k]; !ok {
			fmt.Fprintf(&b, "missing %s\n", k)
		} else if g != want[k] {
			fmt.Fprintf(&b, "--- %s want\n%s--- got\n%s", k, want[k], g)
		}
	}
	for _, k := range keys(got) {
		if _, ok := want[k]; !ok {
			fmt.Fprintf(&b, "extra %s:\n%s", k, got[k])
		}
	}
	return b.String()
}

// --all wires what is installed and says what it skipped.
func TestWireAllSkipsToolsThatAreNotInstalled(t *testing.T) {
	h := newHost(t)
	if err := os.MkdirAll(h.p(".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.path["opencode"] = "/usr/bin/opencode" // on PATH, no config dir yet
	rs := h.wire()
	got := map[string]Status{}
	for _, r := range rs {
		got[r.Tool] = r.Status
	}
	want := map[string]Status{ToolClaude: Skipped, ToolCodex: Written, ToolCursor: Skipped,
		ToolOpenCode: Written, ToolHermes: Skipped}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses %v, want %v", got, want)
	}
	if h.exists(".claude.json") || h.exists(".cursor") || h.exists(".hermes") {
		t.Fatal("--all wrote into a tool that is not installed")
	}
	var order []string
	for _, r := range rs {
		order = append(order, r.Tool)
	}
	if !reflect.DeepEqual(order, Tools()) {
		t.Fatalf("results in order %v, want %v", order, Tools())
	}
}

// A same-named [mcp_servers.anet] anet did not write is a conflict in
// every spelling TOML allows, and nothing — not even AGENTS.md — is
// written.
func TestCodexConflictStopsTheTool(t *testing.T) {
	for name, cfg := range map[string]string{
		"table":         "[mcp_servers.anet]\ncommand = \"anet\"\nargs = [\"mcp\"]\n",
		"quoted table":  "[mcp_servers.\"anet\"]\ncommand = \"x\"\n",
		"spaced table":  "[ mcp_servers . anet ]\ncommand = \"x\"\n",
		"env subtable":  "[mcp_servers.anet.env]\nA = \"b\"\n",
		"dotted root":   "model = \"m\"\nmcp_servers.anet.command = \"x\"\n",
		"under parent":  "[mcp_servers]\nanet = { command = \"x\" }\n",
		"dotted parent": "[mcp_servers]\nanet.command = \"x\"\n",
		"inline":        "mcp_servers = { anet = { command = \"x\" } }\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.write(".codex/config.toml", cfg, 0o644)
			r := one(t, h.wire(ToolCodex), Conflict)
			var ce *ConflictError
			if !errors.As(r.Err, &ce) || ce.Line == 0 {
				t.Fatalf("a conflict must name its line: %v", r.Err)
			}
			if h.read(".codex/config.toml") != cfg {
				t.Fatal("config.toml was changed despite the conflict")
			}
			if h.exists(".codex/AGENTS.md") || len(h.backups()) > 0 {
				t.Fatal("a conflicted tool must be left entirely alone")
			}
			// unwire leaves someone else's table where it is.
			rs := h.unwire(ToolCodex)
			if rs[0].Status != NotWired || len(rs[0].Notes) == 0 || h.read(".codex/config.toml") != cfg {
				t.Fatalf("unwire touched a table anet did not write: %+v", rs[0])
			}
		})
	}
}

// An inline `mcp_servers = { … }` is closed in TOML: appending anet's
// [mcp_servers.anet] after it would make Codex refuse its whole config
// ("Cannot declare ('mcp_servers', 'anet') twice"), so wire stops even when
// the inline table has no anet entry, and unwire has nothing to say about it.
func TestCodexInlineServersTableIsAConflict(t *testing.T) {
	for name, cfg := range map[string]string{
		"inline":       "model = \"m\"\nmcp_servers = { docs = { command = \"docs-mcp\" } }\n",
		"empty inline": "mcp_servers = {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.write(".codex/config.toml", cfg, 0o644)
			r := one(t, h.wire(ToolCodex), Conflict)
			var ce *ConflictError
			if !errors.As(r.Err, &ce) || ce.Line == 0 || !strings.Contains(ce.What, "行内表") {
				t.Fatalf("want a conflict naming the inline table and its line: %v", r.Err)
			}
			if h.read(".codex/config.toml") != cfg || h.exists(".codex/AGENTS.md") || len(h.backups()) > 0 {
				t.Fatal("a conflicted tool must be left entirely alone")
			}
			rs := h.unwire(ToolCodex)
			if rs[0].Status != NotWired || len(rs[0].Notes) != 0 {
				t.Fatalf("unwire: %+v", rs[0])
			}
		})
	}
	// A dotted key under the root is not closed; the block can follow it.
	h := newHost(t)
	cfg := "mcp_servers.docs.command = \"docs-mcp\"\n"
	h.write(".codex/config.toml", cfg, 0o644)
	one(t, h.wire(ToolCodex), Written)
	one(t, h.unwire(ToolCodex), Removed)
	if h.read(".codex/config.toml") != cfg {
		t.Fatalf("round trip changed the file:\n%s", h.read(".codex/config.toml"))
	}
}

// Look-alikes are not conflicts.
func TestCodexLookAlikesAreNotConflicts(t *testing.T) {
	for name, cfg := range map[string]string{
		"other server": "[mcp_servers.anetwork]\ncommand = \"x\"\n",
		"comment":      "# [mcp_servers.anet]\nmodel = \"m\"\n",
		"string":       "instructions = \"\"\"\n[mcp_servers.anet]\n\"\"\"\n",
		"other table":  "[profiles.anet]\nmodel = \"m\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.write(".codex/config.toml", cfg, 0o644)
			one(t, h.wire(ToolCodex), Written)
			one(t, h.unwire(ToolCodex), Removed)
			if h.read(".codex/config.toml") != cfg {
				t.Fatalf("round trip changed the file:\n%s", h.read(".codex/config.toml"))
			}
		})
	}
}

// Claude Code: an entry named anet that runs something else is not
// anet's to replace.
func TestClaudeConflict(t *testing.T) {
	h := newHost(t)
	cfg := "{\n  \"mcpServers\": {\n    \"anet\": {\n      \"command\": \"npx\",\n      \"args\": [\n        \"anet-server\"\n      ]\n    }\n  }\n}\n"
	h.write(".claude.json", cfg, 0o600)
	one(t, h.wire(ToolClaude), Conflict)
	if h.read(".claude.json") != cfg || h.exists(".claude/skills/anet/SKILL.md") {
		t.Fatal("a conflicted tool must be left entirely alone")
	}
}

// Cursor and opencode entries the tutorials told people to write by hand
// (`anet mcp` with no absolute path) are anet's, and wire upgrades them.
func TestHandWrittenAnetEntriesAreUpgraded(t *testing.T) {
	h := newHost(t)
	h.write(".cursor/mcp.json", `{"mcpServers":{"anet":{"command":"anet","args":["mcp"]}}}`, 0o644)
	one(t, h.wire(ToolCursor), Written)
	checkWired(t, h, ToolCursor)

	h.write(".cursor/mcp.json", `{"mcpServers":{"anet":{"command":"uvx","args":["anet"]}}}`, 0o644)
	one(t, h.wire(ToolCursor), Conflict)
}

// With claude on PATH, Claude Code's own CLI does the registration, in
// user scope, with the data dir pinned; a stale anet entry is removed first.
func TestClaudeUsesItsCLIWhenOnPath(t *testing.T) {
	h := newHost(t)
	h.path["claude"] = "/usr/local/bin/claude"
	h.write(".claude.json", "{\n  \"numStartups\": 1\n}\n", 0o600)
	h.run = fakeClaudeCLI(h)

	one(t, h.wire(ToolClaude), Written)
	want := [][]string{{"/usr/local/bin/claude", "mcp", "add", "-s", "user", "anet",
		"-e", "ANET_DATA_DIR=" + h.data, "--", testBin, "mcp"}}
	if !reflect.DeepEqual(h.calls, want) {
		t.Fatalf("calls %q\nwant  %q", h.calls, want)
	}
	checkWired(t, h, ToolClaude)

	h.calls = nil
	one(t, h.wire(ToolClaude), UpToDate)
	if len(h.calls) != 0 {
		t.Fatalf("an up-to-date entry was registered again: %q", h.calls)
	}

	// The binary moved (an upgrade to a new path): remove, then add.
	h.calls = nil
	o := h.opts()
	o.Bin = "/usr/local/bin/anet"
	one(t, h.wireWith(o, ToolClaude), Written)
	if len(h.calls) != 2 || h.calls[0][2] != "remove" || h.calls[1][2] != "add" {
		t.Fatalf("calls %q, want remove then add", h.calls)
	}

	h.calls = nil
	one(t, h.unwireWith(o, ToolClaude), Removed)
	if len(h.calls) != 1 || !reflect.DeepEqual(h.calls[0][1:], []string{"mcp", "remove", "-s", "user", "anet"}) {
		t.Fatalf("unwire calls %q", h.calls)
	}
	if strings.Contains(h.read(".claude.json"), "anet") || h.exists(".claude/skills/anet") {
		t.Fatal("unwire left anet's entry or skill behind")
	}
}

// A claude CLI that says it worked but did not is reported as a failure.
func TestClaudeCLIThatDoesNothingFails(t *testing.T) {
	h := newHost(t)
	h.path["claude"] = "/usr/local/bin/claude"
	h.run = func(string, []string) ([]byte, error) { return []byte("Added"), nil }
	r := one(t, h.wire(ToolClaude), Failed)
	if !strings.Contains(r.Err.Error(), "mcpServers.anet") {
		t.Fatalf("error should say what is missing: %v", r.Err)
	}
}

// fakeClaudeCLI edits ~/.claude.json the way `claude mcp add|remove -s
// user` does.
func fakeClaudeCLI(h *host) func(string, []string) ([]byte, error) {
	return func(_ string, args []string) ([]byte, error) {
		path := h.p(".claude.json")
		b, _ := os.ReadFile(path)
		var root map[string]any
		if len(b) == 0 || json.Unmarshal(b, &root) != nil {
			root = map[string]any{}
		}
		servers, _ := root["mcpServers"].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
		}
		switch args[1] {
		case "add":
			if _, ok := servers["anet"]; ok {
				return []byte("MCP server anet already exists in user config"), errors.New("exit status 1")
			}
			env := map[string]any{}
			i := 5
			for ; args[i] != "--"; i += 2 {
				k, v, _ := strings.Cut(args[i+1], "=")
				env[k] = v
			}
			servers["anet"] = map[string]any{"type": "stdio", "command": args[i+1], "args": args[i+2:], "env": env}
		case "remove":
			delete(servers, "anet")
		}
		root["mcpServers"] = servers
		out, _ := json.MarshalIndent(root, "", "  ")
		return []byte("ok"), os.WriteFile(path, out, 0o600)
	}
}

// The old install's persona is migrated: the block in CLAUDE.md goes, the
// AGENTS.md block is replaced in place, the Cursor rule file is removed.
func TestLegacyPersonaIsMigrated(t *testing.T) {
	h := newHost(t)
	legacy := "<!-- anet:begin (managed by `anet install`) -->\n## AgentNetwork (anet)\n\n" +
		"pricing is display-only text in v0.1 (no settlement yet)\n<!-- anet:end -->\n"
	h.write(".claude/CLAUDE.md", "# Mine\n\n"+legacy, 0o644)
	h.write(".codex/AGENTS.md", "# Codex\n\n"+legacy+"\n## After\n", 0o644)
	h.write(".cursor/rules/agentnetwork-anet.mdc", "---\nalwaysApply: true\n---\n\n## AgentNetwork (anet)\nold\n", 0o644)
	h.write(".cursor/rules/mine.mdc", "## AgentNetwork (anet) is mentioned here too, but this file is not anet's\n", 0o644)

	for _, tool := range []string{ToolClaude, ToolCodex, ToolCursor} {
		one(t, h.wire(tool), Written)
	}
	if got := h.read(".claude/CLAUDE.md"); got != "# Mine\n" {
		t.Fatalf("CLAUDE.md after wire:\n%q", got)
	}
	agents := h.read(".codex/AGENTS.md")
	checkPersona(t, agents)
	if strings.Contains(agents, "display-only") || !strings.HasPrefix(agents, "# Codex\n") || !strings.HasSuffix(agents, "## After\n") {
		t.Fatalf("AGENTS.md block not replaced in place:\n%s", agents)
	}
	if h.exists(".cursor/rules/agentnetwork-anet.mdc") {
		t.Fatal("the old Cursor rule file is still there")
	}
	if !h.exists(".cursor/rules/mine.mdc") {
		t.Fatal("wire deleted a rule file that was not anet's")
	}
}

// The persona block the old `anet install --agent openclaw` appended to
// ~/.openclaw/AGENTS.md is out of date and told the agent to take work from
// anyone. OpenClaw is not wired any more, so --all (either way) and the old
// command itself take the block out; the operator's own text stays.
func TestLegacyOpenClawPersonaIsRemoved(t *testing.T) {
	legacy := "<!-- anet:begin (managed by `anet install`) -->\n## AgentNetwork (anet)\n\n" +
		"- Provide work to others (earn by completing their tasks):\n<!-- anet:end -->\n"
	mine := "# OpenClaw rules\n\nBe careful.\n"

	h := newHost(t)
	rs := h.wire()
	for _, r := range rs {
		if r.Tool == LegacyOpenClaw {
			t.Fatalf("wire --all reported OpenClaw with nothing of anet's there: %+v", r)
		}
	}

	h = newHost(t)
	h.write(".openclaw/AGENTS.md", mine+"\n"+legacy, 0o644)
	rs = h.wire()
	last := rs[len(rs)-1]
	if last.Tool != LegacyOpenClaw || last.Status != Removed || len(last.Backups) != 1 {
		t.Fatalf("wire --all: %+v", last)
	}
	if got := h.read(".openclaw/AGENTS.md"); got != mine {
		t.Fatalf("AGENTS.md after wire --all:\n%q", got)
	}
	// A named wire leaves OpenClaw alone; it is not one of the tools.
	h.write(".openclaw/AGENTS.md", mine+"\n"+legacy, 0o644)
	for _, r := range h.wire(ToolCodex) {
		if r.Tool == LegacyOpenClaw {
			t.Fatal("wire codex touched OpenClaw")
		}
	}
	rs = h.unwire()
	if last := rs[len(rs)-1]; last.Tool != LegacyOpenClaw || last.Status != Removed || h.read(".openclaw/AGENTS.md") != mine {
		t.Fatalf("unwire --all: %+v\n%s", last, h.read(".openclaw/AGENTS.md"))
	}

	// The old command: a file that held only the block is removed.
	h = newHost(t)
	h.write(".openclaw/AGENTS.md", legacy, 0o644)
	r, err := RemoveLegacyOpenClaw(h.opts())
	if err != nil || r.Status != Removed || h.exists(".openclaw/AGENTS.md") {
		t.Fatalf("RemoveLegacyOpenClaw: %+v, %v", r, err)
	}
	if r, err := RemoveLegacyOpenClaw(h.opts()); err != nil || r.Status != NotWired {
		t.Fatalf("second RemoveLegacyOpenClaw: %+v, %v", r, err)
	}
}

// The guide says what this release does, with the §12 tool names, and
// none of what the old persona said that stopped being true.
func TestGuideTextIsCurrent(t *testing.T) {
	for name, text := range map[string]string{"skill": skillMarkdown, "persona": personaMarkdown, "hermes": hermesPersona} {
		for _, stale := range []string{"display-only", "v0.1", "agents_find", "task_delegate", "task_results",
			"task_inbox", "task_message", "task_end", "evidence_read", "credit_balance", "anet install",
			"don't wait for a human", "earn by",
			// docs/notes/0041: free text is matched on this machine (§10.5), never sent to the hub.
			"free-text query is sent to the hub",
			// docs/notes/0041: the default is not this node's value; the model said it was.
			"expect `submit_payment` to be refused"} {
			if strings.Contains(text, stale) {
				t.Errorf("%s still says %q", name, stale)
			}
		}
		for _, want := range []string{"AgentNetwork", "send_message", "wait_task", "anet.effect_status",
			"anet.receipt_verified", "submit_payment", "anet peers allow", "anet pay <task_id>",
			// Red-team F10: failed + UNVERIFIED is "may have happened", and
			// the agent is the one that would send it again.
			"do not send it again as if it had not run"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not mention %q", name, want)
			}
		}
	}
	for _, want := range []string{"list_agents", "get_agent_card", "get_task", "list_tasks", "cancel_task",
		"reply_task", "reject_payment", "inbound_pending", "get_balance", "audit", "node_status",
		"agent_max", "agent_daily_max", "auto_max", "payees.allow", "closed", "anet peers trust",
		"anet inbound policy approve", "llms.txt",
		// docs/notes/0041: message ids that collided across sessions; limits stated as fact.
		"fresh random id", "no tool shows their current", "needs_operator_approval"} {
		if !strings.Contains(skillMarkdown, want) {
			t.Errorf("SKILL.md does not mention %q", want)
		}
	}
	// The frontmatter is YAML: the description is a double-quoted scalar
	// (a plain one containing ": " does not parse), so it may not contain
	// a quote or a backslash of its own.
	front := strings.SplitN(skillMarkdown, "\n---\n", 2)[0]
	var desc string
	for _, l := range strings.Split(front, "\n") {
		if v, ok := strings.CutPrefix(l, "description: "); ok {
			desc = v
		}
	}
	if len(desc) < 2 || desc[0] != '"' || desc[len(desc)-1] != '"' || strings.ContainsAny(desc[1:len(desc)-1], "\"\\") {
		t.Errorf("SKILL.md description is not a clean double-quoted YAML scalar: %s", desc)
	}
	// The persona rides in every Codex/opencode/Hermes session.
	if n := len(personaMarkdown); n > 1600 {
		t.Errorf("persona is %d bytes; it is loaded into every session and should stay short", n)
	}
}

// Backups never overwrite each other: wire then unwire within one second
// keeps a copy of the original and a copy of anet's edit.
func TestBackupsNeverCollide(t *testing.T) {
	h := newHost(t)
	cfg := "model = \"m\"\n"
	h.write(".codex/config.toml", cfg, 0o644)
	one(t, h.wire(ToolCodex), Written)
	one(t, h.unwire(ToolCodex), Removed)
	b := h.backups()
	want := []string{".codex/AGENTS.md.anet-bak-20260927-101112",
		".codex/config.toml.anet-bak-20260927-101112", ".codex/config.toml.anet-bak-20260927-101112-2"}
	if !reflect.DeepEqual(b, want) {
		t.Fatalf("backups %v\nwant %v", b, want)
	}
	if h.read(want[1]) != cfg {
		t.Fatal("the first backup is not the original")
	}
	if !strings.Contains(h.read(want[2]), "[mcp_servers.anet]") {
		t.Fatal("the second backup is not the wired file")
	}
}

// Modes are kept: a file the operator made private stays private, and a
// backup is never more readable than 0600.
func TestModesAreKept(t *testing.T) {
	h := newHost(t)
	h.write(".cursor/mcp.json", "{}\n", 0o640)
	one(t, h.wire(ToolCursor), Written)
	if m := h.mode(".cursor/mcp.json"); m != 0o640 {
		t.Fatalf("mode %o, want 0640 kept", m)
	}
	for _, b := range h.backups() {
		if m := h.mode(b); m&0o077 != 0 {
			t.Fatalf("backup %s is %o", b, m)
		}
	}
	// A file anet creates that may hold credentials starts private.
	one(t, h.wire(ToolClaude), Written)
	if m := h.mode(".claude.json"); m != 0o600 {
		t.Fatalf("new ~/.claude.json is %o, want 0600", m)
	}
}

// A link that points at nothing is neither replaced by a file nor
// followed to create one somewhere else.
func TestDanglingSymlinkIsNotReplaced(t *testing.T) {
	h := newHost(t)
	if err := os.MkdirAll(h.p(".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := h.p("dotfiles/cursor/mcp.json")
	if err := os.Symlink(target, h.p(".cursor/mcp.json")); err != nil {
		t.Fatal(err)
	}
	r := one(t, h.wire(ToolCursor), Failed)
	if !strings.Contains(r.Err.Error(), "符号链接") {
		t.Fatalf("unclear error: %v", r.Err)
	}
	fi, err := os.Lstat(h.p(".cursor/mcp.json"))
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("wire created the link's target")
	}
}

// Two members with one name: the tools (JSON.parse) use the last one, so
// editing the first would report an entry the tool never reads. anet
// refuses instead, and changes nothing.
func TestDuplicateJSONKeysAreRefused(t *testing.T) {
	h := newHost(t)
	cfg := "{\"mcpServers\": {\"a\": {}}, \"mcpServers\": {\"b\": {}}}\n"
	h.write(".cursor/mcp.json", cfg, 0o644)
	r := one(t, h.wire(ToolCursor), Failed)
	if !strings.Contains(r.Err.Error(), "duplicate") || h.read(".cursor/mcp.json") != cfg {
		t.Fatalf("%v\n%s", r.Err, h.read(".cursor/mcp.json"))
	}
}

// A config kept elsewhere and linked into place is edited where it lives;
// the link stays a link.
func TestSymlinkedConfigIsEditedInPlace(t *testing.T) {
	h := newHost(t)
	h.write("dotfiles/codex.toml", "model = \"m\"\n", 0o644)
	if err := os.MkdirAll(h.p(".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.p("dotfiles/codex.toml"), h.p(".codex/config.toml")); err != nil {
		t.Fatal(err)
	}
	one(t, h.wire(ToolCodex), Written)
	fi, err := os.Lstat(h.p(".codex/config.toml"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a file")
	}
	if !strings.Contains(h.read("dotfiles/codex.toml"), "[mcp_servers.anet]") {
		t.Fatal("the link's target was not edited")
	}
}

// JSON members keep their order: a one-entry change is a one-entry diff.
func TestJSONMemberOrderIsKept(t *testing.T) {
	h := newHost(t)
	h.write(".claude.json", "{\n  \"zeta\": 1,\n  \"alpha\": {\n    \"b\": 2,\n    \"a\": 1\n  },\n  \"mid\": [\n    1,\n    2\n  ]\n}\n", 0o600)
	one(t, h.wire(ToolClaude), Written)
	got := h.read(".claude.json")
	if !strings.HasPrefix(got, "{\n  \"zeta\": 1,\n  \"alpha\": {\n    \"b\": 2,\n    \"a\": 1\n  },\n  \"mid\": [\n    1,\n    2\n  ],\n  \"mcpServers\": {") {
		t.Fatalf("member order not kept:\n%s", got)
	}
}

// opencode's JSONC with comments is not rewritten (anet would drop the
// comments); the operator gets the entry to paste.
func TestOpencodeJSONCWithCommentsIsNotRewritten(t *testing.T) {
	h := newHost(t)
	cfg := "{\n  // my settings\n  \"theme\": \"x\"\n}\n"
	h.write(".config/opencode/opencode.jsonc", cfg, 0o644)
	r := one(t, h.wire(ToolOpenCode), Failed)
	if !strings.Contains(r.Err.Error(), `"type": "local"`) {
		t.Fatalf("the error should carry the entry to add by hand: %v", r.Err)
	}
	if h.read(".config/opencode/opencode.jsonc") != cfg {
		t.Fatal("the JSONC file was changed")
	}
}

func TestSelectTools(t *testing.T) {
	h := newHost(t)
	if _, err := Wire(h.opts(), []string{"vscode"}, false); err == nil || !strings.Contains(err.Error(), "vscode") {
		t.Fatalf("an unknown tool must be refused by name: %v", err)
	}
	o := h.opts()
	o.A2A = []string{"bafyreiaaaaaaaa"}
	if _, err := Wire(o, []string{ToolCodex}, false); err == nil {
		t.Fatal("--a2a without hermes must be refused")
	}
	o.A2A = []string{"Not-An-AID"}
	if _, err := Wire(o, []string{ToolHermes}, false); err == nil {
		t.Fatal("an AID outside [a-z0-9] must be refused")
	}
	o = h.opts()
	o.Bin = "anet"
	if _, err := Wire(o, nil, true); err == nil {
		t.Fatal("a relative binary path must be refused: MCP clients do not load the shell's PATH")
	}
}

func TestTOMLString(t *testing.T) {
	for in, want := range map[string]string{
		`/home/u/.local/bin/anet`: `"/home/u/.local/bin/anet"`,
		`C:\Users\u\anet.exe`:     `"C:\\Users\\u\\anet.exe"`,
		"a\"b":                    `"a\"b"`,
		"tab\there":               `"tab\there"`,
		"bell\a":                  `"bell\u0007"`,
		"路径/anet":                 `"路径/anet"`,
	} {
		if got := tomlString(in); got != want {
			t.Errorf("tomlString(%q) = %s, want %s", in, got, want)
		}
	}
}

// Golden files pin the exact text each tool gets for a fixed binary and
// data dir, so a change to what anet writes into other programs' files is
// a visible diff in review. Regenerate with -update.
func TestGoldenEntries(t *testing.T) {
	h := newHost(t)
	o := h.opts()
	o.DataDir = "/home/u/.anet"
	o.A2A = []string{"bafyreiabcdefgh234567"}
	// The a2a state files are read from a temp dir; the data dir written
	// into the entries is a fixed path so the golden text is stable.
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(a2aStatePath(data, A2AAddrFile)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a2aStatePath(data, A2AAddrFile), []byte("127.0.0.1:39900\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a2aStatePath(data, A2ATokenFile), []byte("tok-golden\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ep, err := readA2A(&Options{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.normalize(); err != nil {
		t.Fatal(err)
	}
	cursorWant, _ := marshalIndented(cursorTool{}.server(&o).want)
	opencodeWant, _ := marshalIndented(opencodeTool{}.server(&o).want)
	claudeWant, _ := marshalIndented(claudeWant(&o))
	cases := map[string]string{
		"codex.toml": joinLines(codexBlock(&o)),
		"hermes-mcp.yaml": joinLines(indentLines(append(append([]string{hermesMCP.begin}, hermesMCPBody(&o)...),
			hermesMCP.markers.end), 2)),
		"hermes-a2a.yaml": joinLines(indentLines(append(append([]string{hermesA2A.begin},
			hermesA2ABody(ep, o.A2A)...), hermesA2A.markers.end), 2)),
		"cursor.json":   cursorWant,
		"opencode.json": opencodeWant,
		"claude.json":   claudeWant,
	}
	for name, got := range cases {
		path := filepath.Join("testdata", name+".golden")
		if *update {
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update to create)", err)
		}
		if got != string(want) {
			t.Errorf("%s changed:\n--- want\n%s--- got\n%s", name, want, got)
		}
	}
}

func marshalIndented(v any) (string, error) {
	b, err := marshalNoEscape(v)
	if err != nil {
		return "", err
	}
	obj, err := parseObject(b)
	if err != nil {
		return "", err
	}
	out, err := obj.render(nil)
	return string(out), err
}

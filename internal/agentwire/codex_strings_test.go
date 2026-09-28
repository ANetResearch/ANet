//go:build !no_mcp

package agentwire

import (
	"errors"
	"testing"
)

// A table anet did not write is found behind anything TOML lets a line
// hold (docs/notes/0033, FuzzCodexConfig with the tomllib oracle): a `"""`
// or `”'` inside a one-line string or a comment does not open a
// multi-line string, and a quoted key means what its escapes say. Missed,
// wire appended a second [mcp_servers.anet] and Codex could no longer read
// its configuration at all.
func TestCodexConflictBehindStringsAndEscapes(t *testing.T) {
	for name, cfg := range map[string]string{
		"literal string with triple quotes":    "a = '\"\"\"'\n[mcp_servers.anet]\ncommand = \"x\"\n",
		"comment with triple quotes":           "x = 1 # \"\"\" old note\n[mcp_servers.anet]\ncommand = \"x\"\n",
		"multi-line string closed on the line": "p = \"\"\"a\"\"\" # \"\"\"\n[mcp_servers.anet]\ncommand = \"x\"\n",
		"escaped table key":                    "[mcp_servers.\"an\\u0065t\"]\ncommand = \"x\"\n",
		"escaped dotted key":                   "[mcp_servers]\n\"\\u0061net\".command = \"x\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.write(".codex/config.toml", cfg, 0o644)
			r := one(t, h.wire(ToolCodex), Conflict)
			var ce *ConflictError
			if !errors.As(r.Err, &ce) || ce.Line == 0 {
				t.Fatalf("want a conflict naming its line, got %v", r.Err)
			}
			if h.read(".codex/config.toml") != cfg {
				t.Fatal("config.toml was changed despite the conflict")
			}
		})
	}
	// A multi-line string that really is one still hides what it holds.
	for name, cfg := range map[string]string{
		"basic":             "p = \"\"\"\n[mcp_servers.anet]\n\"\"\"\n",
		"literal":           "p = '''\n[mcp_servers.anet]\n'''\n",
		"escaped delimiter": "p = \"\"\"\nx \\\"\"\"\n[mcp_servers.anet]\n\"\"\"\n",
	} {
		t.Run("inside "+name, func(t *testing.T) {
			h := newHost(t)
			h.write(".codex/config.toml", cfg, 0o644)
			one(t, h.wire(ToolCodex), Written)
		})
	}
}

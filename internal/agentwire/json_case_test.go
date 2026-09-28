//go:build !no_mcp

package agentwire

import "testing"

// A JSON entry is anet's, and up to date, only as the tool reads it:
// member names as written (docs/notes/0033, FuzzJSONConfig). Go's decoder
// ignores their case, so {"Command": "<anet>", ...} was reported up to date
// though Cursor found no command in it, and {"command":"x","Command":"<anet>"}
// was overwritten as anet's own though Cursor ran x.
func TestAnEntryTheToolReadsDifferentlyIsNotAnets(t *testing.T) {
	for name, entry := range map[string]func(data string) string{
		"other case only": func(data string) string {
			return `{"Command":"` + testBin + `","args":["mcp"],"env":{"ANET_DATA_DIR":"` + data + `"}}`
		},
		"both spellings": func(string) string {
			return `{"command":"other-mcp","Command":"` + testBin + `","args":["mcp"]}`
		},
		"args in two cases": func(data string) string {
			return `{"command":"` + testBin + `","args":["x"],"ARGS":["mcp"],"env":{"ANET_DATA_DIR":"` + data + `"}}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			cfg := `{"mcpServers":{"anet":` + entry(h.data) + `}}` + "\n"
			h.write(".cursor/mcp.json", cfg, 0o644)
			one(t, h.wire(ToolCursor), Conflict)
			if h.read(".cursor/mcp.json") != cfg {
				t.Fatalf("wire rewrote an entry that is not anet's as the tool reads it:\n%s", h.read(".cursor/mcp.json"))
			}
		})
	}
	// The entry as wire writes it is still up to date.
	h := newHost(t)
	cfg := `{"mcpServers":{"anet":{"command":"` + testBin + `","args":["mcp"],"env":{"ANET_DATA_DIR":"` + h.data + `"}}}}` + "\n"
	h.write(".cursor/mcp.json", cfg, 0o644)
	one(t, h.wire(ToolCursor), UpToDate)
}

// An omitempty member spelled twice hides from the check above too: Go
// takes the last spelling, and a field that ends at its zero value is left
// out of what it re-encodes, so nothing compared it (the review of
// docs/notes/0033). {"type":"sse", …, "TYPE":""} is a stdio entry to Go and
// an SSE one to Claude Code; it was reported up to date.
func TestAnOmitemptyMemberSpelledTwiceIsNotAnets(t *testing.T) {
	h := newHost(t)
	cfg := `{"mcpServers":{"anet":{"type":"sse","command":"` + testBin + `","args":["mcp"],` +
		`"env":{"ANET_DATA_DIR":"` + h.data + `"},"TYPE":""}}}` + "\n"
	h.write(".claude.json", cfg, 0o600)
	one(t, h.wire(ToolClaude), Conflict)
	if h.read(".claude.json") != cfg {
		t.Fatalf("wire rewrote an entry Claude Code reads as SSE:\n%s", h.read(".claude.json"))
	}
}

//go:build !no_mcp

package agentwire

import (
	"strings"
	"testing"
)

// Where the Hermes config has no mcp_servers key, wire appends one; where
// it has one, wire adds anet's entry among its children. After a document
// that is not a block mapping, after a document end marker, and under a
// key whose value below it is not a mapping, that made the file unreadable
// to Hermes (docs/notes/0033, FuzzHermesConfig with PyYAML as the oracle):
// the first and the last are conflicts now, and the key goes before the
// marker.
func TestHermesKeyIsNotAppendedWhereYAMLCannotTakeIt(t *testing.T) {
	for name, cfg := range map[string]string{
		"scalar":                    "0\n",
		"null":                      "~\n",
		"text":                      "just some text\n",
		"sequence":                  "- a\n- b\n",
		"flow":                      "{model: x}\n",
		"flow seq":                  "[a, b]\n",
		"after a directive and ---": "%YAML 1.1\n---\n- a\n",
		// The key is there, with a value on the lines below it that is
		// not a mapping.
		"servers: a scalar below":   "mcp_servers:\n  0\n",
		"servers: text below":       "mcp_servers:\n  none yet\nmodel: x\n",
		"servers: a flow map below": "mcp_servers:\n  {fs: {command: npx}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.write(".hermes/config.yaml", cfg, 0o600)
			one(t, h.wire(ToolHermes), Conflict)
			if h.read(".hermes/config.yaml") != cfg || h.exists(".hermes/SOUL.md") {
				t.Fatal("a conflicted tool must be left entirely alone")
			}
		})
	}
	for name, cfg := range map[string]string{
		"comments only":             "# nothing yet\n",
		"--- then a mapping":        "---\nmodel: x\n",
		"quoted key first":          "\"model\": x\n",
		"end marker":                "---\nmodel: x\n...\n",
		"end marker, comment after": "model: x\n... # end\n# trailing\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.write(".hermes/config.yaml", cfg, 0o600)
			one(t, h.wire(ToolHermes), Written)
			got := h.read(".hermes/config.yaml")
			if i, j := strings.Index(got, "mcp_servers:"), strings.Index(got, "\n..."); j >= 0 && i > j {
				t.Fatalf("mcp_servers was put after the document end marker:\n%s", got)
			}
			one(t, h.unwire(ToolHermes), Removed)
			if back := h.read(".hermes/config.yaml"); back != cfg {
				t.Fatalf("round trip:\n--- want\n%s--- got\n%s", cfg, back)
			}
		})
	}
}

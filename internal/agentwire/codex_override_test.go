//go:build !no_mcp

package agentwire

import (
	"strings"
	"testing"
)

// Codex reads AGENTS.override.md instead of AGENTS.md; wire says so.
func TestCodexOverrideFileIsReported(t *testing.T) {
	h := newHost(t)
	r := one(t, h.wire(ToolCodex), Written)
	if strings.Contains(strings.Join(r.Notes, "\n"), "AGENTS.override.md") {
		t.Fatalf("no override file, yet a note: %v", r.Notes)
	}
	h.write(".codex/AGENTS.override.md", "mine\n", 0o644)
	r = one(t, h.wire(ToolCodex), UpToDate)
	if !strings.Contains(strings.Join(r.Notes, "\n"), "AGENTS.override.md") {
		t.Fatalf("the override file should be reported: %v", r.Notes)
	}
	if h.read(".codex/AGENTS.override.md") != "mine\n" {
		t.Fatal("the override file was changed")
	}
}


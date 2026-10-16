//go:build no_mcp

package main

import (
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// A -tags no_mcp build has no MCP server to register; wire says so rather
// than writing entries that run a command this binary refuses.
func TestAgentsWireRefusesWithoutMCP(t *testing.T) {
	for name, run := range map[string]func(daemon.Layout, []string) error{"agents": runAgents} {
		err := run(daemon.Layout{Root: t.TempDir()}, []string{"wire", "claude"})
		if err == nil || !strings.Contains(err.Error(), "MCP module (mcp)") && !strings.Contains(err.Error(), "MCP模块（mcp）") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

//go:build no_mcp

package main

import (
	"fmt"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// This build has no MCP northbound. Saying so is better than the command
// not existing: an operator who configured `anet mcp` in a client gets a
// sentence explaining which build they installed, not "unknown command".
func runMCP(daemon.Layout) error {
	return fmt.Errorf("anet mcp: this build was compiled with -tags no_mcp")
}

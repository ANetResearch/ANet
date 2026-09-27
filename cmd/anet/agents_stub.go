//go:build no_mcp

package main

import (
	"fmt"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// This build has no MCP server, so there is nothing to register with a
// coding agent: `anet agents wire` would write entries that run a command
// this binary refuses. Saying which build this is beats writing them.
func runAgents(daemon.Layout, []string) error {
	return fmt.Errorf("anet agents: this build was compiled with -tags no_mcp (no MCP server to register)")
}

func runInstall(daemon.Layout, []string) error {
	return fmt.Errorf("anet install: this build was compiled with -tags no_mcp (no MCP server to register)")
}

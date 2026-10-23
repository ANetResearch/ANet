//go:build no_mcp

package main

import "github.com/ANetResearch/ANet/internal/daemon"

// This build has no MCP server: `anet turn-mcp` answers module_absent, as `anet mcp` does (MOD-F2).
func runTurnMCP() error { return requireModule(daemon.Layout{}, "mcp") }

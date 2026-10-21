//go:build no_mcp

package main

import "github.com/ANetResearch/ANet/internal/daemon"

// This build has no MCP server, so there is nothing to register with a
// coding agent: `anet agents wire` would write entries that run a command
// this binary refuses. It answers module_absent instead.
func runAgents(layout daemon.Layout, _ []string) error { return requireModule(layout, "mcp") }

// runGuide: the guide describes the MCP tools, which this build does not
// have.
func runGuide(_ []string) error { return requireModule(daemon.Layout{}, "mcp") }

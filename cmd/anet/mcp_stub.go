//go:build no_mcp

package main

import "github.com/ANetResearch/ANet/internal/daemon"

// This build has no MCP northbound. The command answers module_absent, the
// one sentence every entrance of an absent module gives (MOD-F2), rather
// than not existing: an operator who configured `anet mcp` in a client gets
// a sentence that says which build they installed and what to do.
func runMCP(layout daemon.Layout, _ bool) error { return requireModule(layout, "mcp") }

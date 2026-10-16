//go:build !no_mcp

package main

// The MCP northbound is compiled in, so say so.
//
// It is not a module.Module — it lives in internal/mcpserv and is reached
// as a CLI subcommand — so it never appears in the module registry, and
// `anet version` reported `modules: service` for a build carrying nine
// working MCP tools. The tag on this file is the same one that governs the
// subsystem, so the declaration cannot outlive what the linker kept.
import "github.com/ANetResearch/ANet/module"

func init() { module.DeclareCompiled("mcp") }

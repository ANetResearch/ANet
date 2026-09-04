//go:build !no_mcp

package main

import (
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// The modules line must name MCP when MCP is compiled in.
//
// MCP is not a module.Module — it is a CLI subcommand over internal/mcpserv
// — so it never reached the registry, and the default build reported
// `modules: service` while carrying nine working MCP tools. The
// documentation tells people to identify their build by reading that line,
// so a line that omits part of the binary is worse than no line.
func TestTheModulesLineNamesMCPWhenItIsCompiledIn(t *testing.T) {
	for _, m := range module.Compiled() {
		if m == "mcp" {
			return
		}
	}
	t.Fatalf("a build without -tags no_mcp does not report mcp: %v", module.Compiled())
}

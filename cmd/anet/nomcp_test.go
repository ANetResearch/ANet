//go:build no_mcp

package main

import (
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// The other direction: a build with -tags no_mcp must not claim mcp.
func TestABuildWithoutMCPDoesNotClaimIt(t *testing.T) {
	for _, m := range module.Compiled() {
		if m == "mcp" {
			t.Fatalf("a -tags no_mcp build reports mcp: %v", module.Compiled())
		}
	}
}

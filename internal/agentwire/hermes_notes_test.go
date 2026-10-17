//go:build !no_mcp

package agentwire

import (
	"strings"
	"testing"
)

// The Hermes guide names the tools as Hermes does, and --a2a reminds the
// operator that the a2a toolset is off by default.
func TestHermesGuideAndA2ANote(t *testing.T) {
	if !strings.Contains(hermesPersona, "mcp_anet_list_agents") {
		t.Fatal("the Hermes guide must give the prefixed tool names")
	}
	h := newHost(t)
	r := one(t, h.wire(ToolHermes), Written)
	if strings.Contains(strings.Join(r.Notes, "\n"), "tools enable a2a") {
		t.Fatalf("a2a note without --a2a: %v", r.Notes)
	}
	if !strings.Contains(h.read(".hermes/SOUL.md"), "mcp_anet_list_agents") {
		t.Fatal("SOUL.md lacks the prefixed names")
	}
}

func TestHermesA2ANoteText(t *testing.T) {
	n := hermesTool{}.wireNotes(&Options{A2A: []string{"bafyreiaaaaaaaa"}}, Written)
	if len(n) != 1 || !strings.Contains(n[0], "hermes tools enable a2a --platform cli") {
		t.Fatalf("notes %v", n)
	}
}

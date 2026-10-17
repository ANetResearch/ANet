//go:build !no_mcp

package main

import (
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/agentwire"
)

// `anet agents wire` supports the tools agentwire registers and no other. openclaw is an auto-reply
// backend only, so the wire line of the help must not name it.
func TestWireAgentChoicesMatchAgentwire(t *testing.T) {
	want := map[string]bool{}
	for _, n := range agentwire.Tools() {
		want[n] = true
	}
	got := strings.Split(wireAgentChoices, "|")
	if len(got) != len(want) {
		t.Fatalf("wireAgentChoices = %q, agentwire serves %v", wireAgentChoices, agentwire.Tools())
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("wireAgentChoices names %q, which agentwire does not serve", n)
		}
	}
}

func TestHelpWireLineOmitsOpenclaw(t *testing.T) {
	for _, sec := range helpSections() {
		for _, e := range sec.entries {
			if strings.HasPrefix(e.use, "agents wire") || strings.HasPrefix(e.use, "agents unwire") {
				if strings.Contains(e.zh+e.en, "openclaw") {
					t.Errorf("help entry %q names openclaw", e.use)
				}
			}
		}
	}
}

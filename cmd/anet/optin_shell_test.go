//go:build shell

package main

import (
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// The other direction. A tag that does not actually link the module would
// leave every deny-case in the suite passing for the wrong reason.
func TestShellTagLinksTheModule(t *testing.T) {
	got := strings.Join(module.Compiled(), ",")
	if !strings.Contains(got, "shell") {
		t.Fatalf("-tags shell did not register the module: %q", got)
	}
}

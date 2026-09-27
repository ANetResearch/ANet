//go:build no_a2a

package main

import (
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// The other direction (SI-8): a -tags no_a2a build does not claim a2a, and
// a config that names it says which tag removed it rather than starting
// without the interface its owner asked for.
func TestABuildWithoutA2ADoesNotClaimIt(t *testing.T) {
	for _, m := range module.Compiled() {
		if m == "a2a" {
			t.Fatalf("a -tags no_a2a build reports a2a: %v", module.Compiled())
		}
	}
	_, err := module.Build(map[string][]byte{"a2a": []byte(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "no_a2a") {
		t.Fatalf("modules.a2a in a no_a2a build: %v", err)
	}
}

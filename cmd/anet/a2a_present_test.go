//go:build !no_a2a

package main

import (
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// The local A2A interface is on by default (A2A-DESIGN §11.1): the module
// is compiled in, and a config without a modules.a2a block still builds it.
func TestTheLocalA2AInterfaceIsOnWithoutConfig(t *testing.T) {
	found := false
	for _, m := range module.Compiled() {
		found = found || m == "a2a"
	}
	if !found {
		t.Fatalf("a build without -tags no_a2a does not report a2a: %v", module.Compiled())
	}
	m, err := module.BuildOne("a2a", nil)
	if err != nil || m == nil {
		t.Fatalf("a2a without a config block: %v, %v", m, err)
	}
}

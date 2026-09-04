//go:build !shell

package main

import (
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// The default build must not contain the modules reached by an additive
// tag. Checked here as well as by symbol count in CI, because this is the
// assertion that fails at the moment somebody adds an untagged import of
// an opt-in module — which is how a default build quietly gains the
// ability to execute commands on its host.
func TestDefaultBuildHasNoOptInModules(t *testing.T) {
	for _, m := range module.Compiled() {
		if m == "shell" {
			t.Fatalf("the default build registered %q; it is reachable only with -tags shell", m)
		}
	}
}

// `anet version` reports what is linked, so an operator can tell two
// binaries at the same commit apart. The list has to come from the
// registry: a build-time stamp says whatever the builder passed, and
// `go build -tags shell` with no -X ldflag would have it report a default
// build that can in fact run commands.
func TestCompiledListIsUsableAsAVariantReport(t *testing.T) {
	got := strings.Join(module.Compiled(), ",")
	if got == "" {
		t.Fatal("a build with no modules at all cannot be told apart from a broken registry")
	}
	if strings.Contains(got, "shell") {
		t.Fatalf("default build reports shell: %q", got)
	}
}

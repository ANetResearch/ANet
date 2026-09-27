//go:build taskboard

package main

import (
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// The other direction of TestTheDefaultBuildHasNoTaskboard. A tag that
// linked nothing would look like a working opt-in, and every default-build
// assertion would pass for the wrong reason.
func TestTheTaskboardTagLinksTheModule(t *testing.T) {
	if got := strings.Join(module.Compiled(), ","); !strings.Contains(got, "taskboard") {
		t.Fatalf("-tags taskboard did not register the module: %q", got)
	}
	mods, err := module.Build(map[string][]byte{"taskboard": []byte(`{}`)})
	if err != nil {
		t.Fatalf("a -tags taskboard build refused a taskboard block: %v", err)
	}
	for _, m := range mods {
		if m.Name() == "taskboard" {
			return
		}
	}
	t.Fatal("a configured taskboard block built no taskboard module")
}

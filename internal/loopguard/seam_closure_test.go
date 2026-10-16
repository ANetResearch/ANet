package loopguard_test

import (
	"slices"
	"testing"
)

// TestSeamClosureHasNoStore (MOD-5; 11-modularity §7.1): the package every
// module imports reaches no store. The state constants live in the leaf
// internal/ixstate, and the projection of a stored interaction in
// internal/a2ashape/stored, which only the kernel imports. Without this a
// preview module could not be held to "previews are never stored" (SI-73)
// by what it imports.
func TestSeamClosureHasNoStore(t *testing.T) {
	deps, ok := depsOf(t, "module", "")
	if !ok {
		t.Fatal("module is not in this tree")
	}
	for _, bad := range []string{
		root + "/internal/runtime/interactions",
		root + "/internal/a2ashape/stored",
		"github.com/ANetResearch/ANetCore/seal",
		"modernc.org/sqlite",
	} {
		if slices.Contains(deps, bad) {
			t.Errorf("go list -deps ./module contains %s", bad)
		}
	}
	// The packages it was cut from still hold what the cut was for.
	a2a, ok := depsOf(t, "internal/a2ashape", "")
	if !ok {
		t.Fatal("internal/a2ashape is not in this tree")
	}
	if slices.Contains(a2a, root+"/internal/runtime/interactions") {
		t.Error("internal/a2ashape imports the interactions store")
	}
	if !slices.Contains(a2a, root+"/internal/ixstate") {
		t.Error("internal/a2ashape does not take its states from internal/ixstate")
	}
}

//go:build !taskboard

package main

import (
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// The default build must not contain the task-board client (A2A-DESIGN
// §16, SI-8). Checked here as well as by symbol count in CI and in
// `build.sh --check`, because this is the assertion that fails the moment
// somebody drops the tag from module_taskboard.go or imports the module
// from an untagged file — which is how a default daemon would start
// offering task.create against a board the default hub no longer serves.
func TestTheDefaultBuildHasNoTaskboard(t *testing.T) {
	for _, m := range module.Compiled() {
		if m == "taskboard" {
			t.Fatal("the default build registered taskboard; it is reachable only with -tags taskboard")
		}
	}
	if strings.Contains(compiledModulesReport(), "taskboard") {
		t.Fatalf("`anet version` would report taskboard: %q", compiledModulesReport())
	}
}

// A node upgraded from a build where the board was in by default may still
// carry a "taskboard" block. The default build refuses to start on it, and
// the refusal must name the flag that exists now. `no_taskboard` is the
// flag that used to govern it; pointing an operator at it sends them to
// remove a tag from a build that never had one.
func TestAConfiguredTaskboardNamesTheTagThatWouldAddIt(t *testing.T) {
	_, err := module.Build(map[string][]byte{"taskboard": []byte(`{}`)})
	if err == nil {
		t.Fatal("a default build accepted a taskboard block it has no code for")
	}
	msg := err.Error()
	if !strings.Contains(msg, "-tags taskboard") {
		t.Errorf("the refusal must name -tags taskboard: %v", err)
	}
	if strings.Contains(msg, "no_taskboard") {
		t.Errorf("the refusal points at the subtractive tag that no longer exists: %v", err)
	}
}

package main

import (
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// Every agent the registry supports has to appear where a user looks for
// the list. Three hand-kept copies of "cursor|claude|codex|openclaw|hermes"
// used to exist, so adding an agent meant remembering three places, and
// the help was one forgotten edit away from denying that a working agent
// works. Two of the three are now derived; this keeps the rest honest.
func TestTheHelpNamesEveryAgentTheRegistrySupports(t *testing.T) {
	help := usageAllText()
	for _, id := range daemon.SupportedExecAgents() {
		if !strings.Contains(help, id) {
			t.Errorf("`anet help --all` never mentions the %q agent, but the registry serves it", id)
		}
	}
}

// And the reverse: the help must not advertise an agent that does not
// exist, which is the failure a hand-kept list produces when an agent is
// removed rather than added.
func TestTheHelpAdvertisesNoAgentTheRegistryLacks(t *testing.T) {
	supported := map[string]bool{}
	for _, id := range daemon.SupportedExecAgents() {
		supported[id] = true
	}
	// The rendered choice list is the one place the help enumerates them.
	for _, id := range strings.Split(agentChoices(), "|") {
		if !supported[id] {
			t.Errorf("the help offers --agent %q, which the registry does not serve", id)
		}
	}
	if len(supported) == 0 {
		t.Fatal("the registry serves no agents at all")
	}
}

// The four tools this release is meant to plug into. Named individually
// rather than counted, because "there are six agents" stays true if one
// is swapped for another.
func TestTheNamedCodingAgentsAreAllSupported(t *testing.T) {
	have := map[string]bool{}
	for _, id := range daemon.SupportedExecAgents() {
		have[id] = true
	}
	for _, want := range []string{"claude", "codex", "cursor", "opencode"} {
		if !have[want] {
			t.Errorf("%q is not a supported agent", want)
		}
	}
}

// An agent id has to be usable on the command line as well as present in
// the registry — the flag checker rejects unknown values, and an agent
// nobody can pass is not integrated.
func TestEveryAgentIdIsAcceptedByTheFlagChecker(t *testing.T) {
	for _, id := range daemon.SupportedExecAgents() {
		for _, cmd := range []struct {
			name string
			rest []string
		}{
			{"install", []string{"--agent", id}},
			{"autoreply", []string{"set", "--backend", "exec", "--agent", id}},
		} {
			if err := checkFlags(cmd.name, cmd.rest); err != nil {
				t.Errorf("anet %s %s: %v", cmd.name, strings.Join(cmd.rest, " "), err)
			}
		}
	}
}

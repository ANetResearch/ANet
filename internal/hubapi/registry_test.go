package hubapi_test

import (
	"reflect"
	"testing"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// The A2A registry entry is read by list_agents and written by the hub in
// another repository; the names are pinned on both sides (ANetHub
// internal/aghub/wirecontract_test.go), as the rest of this package's
// wire is.
func TestTheRegistryFieldNamesArePinned(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  []string
	}{
		{"A2AAgentEntry", hubapi.A2AAgentEntry{}, []string{
			"aid", "avgRating", "card", "cardVerification", "homeHub", "lastSeen", "quiet", "reviewCount", "verifiedAt",
		}},
		{"A2AAgentList", hubapi.A2AAgentList{}, []string{"agents", "nextCursor"}},
	} {
		if got := jsonNames(t, tc.value); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s fields\n got  %v\n want %v", tc.name, got, tc.want)
		}
	}
	for got, want := range map[string]string{
		hubapi.RegistryAgentsPath: "/a2a/v1/agents",
		hubapi.CardVerificationOK: "ok",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
	if hubapi.RegistryMaxLimit != 200 {
		t.Errorf("RegistryMaxLimit %d, want the hub's 200", hubapi.RegistryMaxLimit)
	}
}

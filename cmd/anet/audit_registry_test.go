package main

import (
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/evtypes"
)

// The audit's table of known events is the registry: every registered
// event is known, shown with its label and source, and nothing else is.
func TestAuditKnowsExactlyTheRegisteredEvents(t *testing.T) {
	all := evtypes.All()
	if len(knownEvents) != len(all) {
		t.Fatalf("audit knows %d event types, the registry has %d", len(knownEvents), len(all))
	}
	for _, e := range all {
		ev := classify(daemon.EvidenceRecord{EventType: e.Type, Payload: map[string]any{}})
		if !ev.Known || ev.Source == srcAny || ev.Label == "" {
			t.Errorf("%s: known=%v label=%q source=%q", e.Type, ev.Known, ev.Label, ev.Source)
		}
		// classify refines a few by payload (a settlement, paid or
		// received); the table itself is the registry's.
		if k := knownEvents[e.Type]; k.source != sourceText[e.Source] || k.label != e.Label {
			t.Errorf("%s: table has %q/%q, registry says %q/%q", e.Type, k.label, k.source, e.Label, e.Source)
		}
		if e.AggregatedLabel != "" {
			agg := classify(daemon.EvidenceRecord{EventType: e.Type, Payload: map[string]any{"aggregated": true}})
			if agg.Label != e.AggregatedLabel {
				t.Errorf("%s aggregated: label %q, want %q", e.Type, agg.Label, e.AggregatedLabel)
			}
		}
	}
	if ev := classify(daemon.EvidenceRecord{EventType: "anet.not.registered"}); ev.Known || ev.Source != srcAny {
		t.Errorf("an unregistered event is shown as known: %+v", ev)
	}
}

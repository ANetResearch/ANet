package main

import (
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// A deny that left paid work running names it under skipped_paid (0017
// Q10); `--interaction` and `--peer` find that record as they find the
// interactions a deny canceled.
func TestAuditFindsTheWorkADenyLeftRunning(t *testing.T) {
	layout := writeChain(t, []testEvent{
		{"anet.policy.changed", map[string]any{"field": "peers.deny", "from": nil, "to": "bafyreipeer000001",
			"canceled": []any{"ix_c"}, "skipped_paid": []any{"ix_s"}}},
		{"anet.capability.effect", map[string]any{"interaction_id": "ix_s", "capability": "work.do", "status": "OK"}},
	})
	ch, err := daemon.ReadEvidenceChain(layout)
	if err != nil {
		t.Fatal(err)
	}
	if rep := buildAudit(ch, "", auditFilter{interaction: "ix_s"}); rep.Summary.Events != 2 {
		t.Fatalf("--interaction of paid work a deny skipped: %d events, want 2", rep.Summary.Events)
	}
	if ix := peerInteractions(ch.Records, "bafyreipeer000001"); !ix["ix_c"] || !ix["ix_s"] {
		t.Fatalf("interactions of the denied peer: %v", ix)
	}
}

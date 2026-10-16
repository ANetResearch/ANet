//go:build !no_x402

package x402

import (
	"context"
	"net/http"
	"testing"
)

// A hub that keeps answering the read of a balance proof with an error is
// told to doctor as a failing check, not left looking like a hub that checks out.
func TestAHubThatKeepsFailingTheProofReadIsAFinding(t *testing.T) {
	f := newFakeHub(t)
	d := today()
	f.addEpoch(d-3, 100, 10)
	m, _ := f.hubbed()
	if rep := mustAudit(t, m); !rep.Verified {
		t.Fatalf("baseline: %+v", rep)
	}
	f.addEpoch(d-1, 100, 10)
	f.proofOf = func(*fakeEpoch, string) (int, any) {
		return http.StatusInternalServerError, map[string]any{"reason": "internal", "error": "boom"}
	}
	seen := ""
	for i := 1; i <= auditErrorsBeforeAFinding; i++ {
		if sum, _ := m.AuditSummary().(map[string]any); sum != nil && sum["result"] == "proof_unavailable" {
			t.Fatalf("told after %d failed passes, want %d", i-1, auditErrorsBeforeAFinding)
		}
		seen = m.auditPass(context.Background(), seen)
	}
	sum, _ := m.AuditSummary().(map[string]any)
	if sum == nil || sum["result"] != "proof_unavailable" {
		t.Fatalf("after %d failed passes: %v", auditErrorsBeforeAFinding, sum)
	}
	if fd, _ := sum["finding"].(Finding); fd.Code != "proof_unavailable" || fd.Level != levelFail {
		t.Errorf("finding %+v", sum["finding"])
	}
	// The hub recovers: the next pass checks and clears it.
	f.proofOf = nil
	m.auditPass(context.Background(), "")
	if sum, _ := m.AuditSummary().(map[string]any); sum != nil && sum["result"] == "proof_unavailable" {
		t.Errorf("still told after a pass that checked: %v", sum)
	}
}

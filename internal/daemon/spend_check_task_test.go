//go:build !no_x402

package daemon

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// anet.spend_check on a task that waits on a quote (CUX-S3), through the
// real projection: the fresh-install answer, then the agent tier opened,
// and no key once the quote is no longer waiting. The check is read-only:
// asking for the task twice changes no spending record.
func TestTaskCarriesSpendCheckWhileTheQuoteWaits(t *testing.T) {
	work := &meteredWork{price: 3}
	_, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{ExplicitMax: u64(defaultExplicitMax), DailyMax: u64(defaultDailyMax)})
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req)
	got := getTaskProjection(t, req, id).Metadata[a2ashape.KeySpendCheck]
	want := map[string]any{"who": "operator", "amount": "3", "reason": "over_single_limit", "reason_tier": "agent", "reason_limit": "0"}
	if !equalMaps(got, want) {
		t.Fatalf("fresh install: anet.spend_check %v, want %v", got, want)
	}
	// The a2a interface's view carries the same.
	sc, err := peerScope(prov.AID())
	if err != nil {
		t.Fatal(err)
	}
	if task, err := req.getTask(sc, id, nil); err != nil || !equalMaps(task.Metadata[a2ashape.KeySpendCheck], want) {
		t.Fatalf("a2a view: %v, %v", task.Metadata[a2ashape.KeySpendCheck], err)
	}

	payPolicy(t, req, PaymentsConfig{AgentMax: 10, AgentDailyMax: 10, ExplicitMax: u64(defaultExplicitMax), DailyMax: u64(defaultDailyMax)},
		prov.AID())
	got = getTaskProjection(t, req, id).Metadata[a2ashape.KeySpendCheck]
	want = map[string]any{"who": "agent", "amount": "3", "reason": "over_single_limit", "reason_tier": "auto", "reason_limit": "0"}
	if !equalMaps(got, want) {
		t.Fatalf("agent tier open: anet.spend_check %v, want %v", got, want)
	}
	if st := req.SpendStatus(); st.SpentToday != 0 {
		t.Fatalf("looking at the task spent %d", st.SpentToday)
	}

	// Paid: nothing is waiting any more.
	if _, err := req.PayTask(ctx, PayRequest{TaskID: id, Decision: PayDecisionSubmit, Purpose: "task-agent"}); err != nil {
		t.Fatal(err)
	}
	if _, has := getTaskProjection(t, req, id).Metadata[a2ashape.KeySpendCheck]; has {
		t.Error("anet.spend_check on a task whose quote was paid")
	}
}

func equalMaps(a, b any) bool {
	x, ok1 := a.(map[string]any)
	y, ok2 := b.(map[string]any)
	if !ok1 || !ok2 || len(x) != len(y) {
		return false
	}
	for k, v := range y {
		if x[k] != v {
			return false
		}
	}
	return true
}

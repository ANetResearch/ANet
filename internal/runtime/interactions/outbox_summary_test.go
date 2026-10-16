package interactions_test

import (
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

func failRow(t *testing.T, s *interactions.Store, id int64, lastErr string) {
	t.Helper()
	if err := s.Update(func(tx *interactions.Tx) error {
		return tx.EndOutboxAttempt(id, interactions.OutboxEnd{Attempts: 1, LastError: lastErr})
	}); err != nil {
		t.Fatal(err)
	}
}

// DLV-P-4: a row is stuck when it did not leave for an hour with a network
// failure and the gate is open; a row behind an earlier one, a coded failure
// and a closed gate are not stuck.
func TestOutboxSummaryStuck(t *testing.T) {
	s, err := interactions.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d1 := enqueueOutbox(t, s, interactions.OutboxItem{IX: "ix_a", ToAID: "peer", Type: "anet.delegate/1", Body: []byte("D")})
	t1 := enqueueOutbox(t, s, interactions.OutboxItem{IX: "ix_a", ToAID: "peer", Type: "anet.message/1", Kind: "text", Body: []byte("T")})
	d2 := enqueueOutbox(t, s, interactions.OutboxItem{IX: "ix_b", ToAID: "peer", Type: "anet.delegate/1", Body: []byte("D")})
	failRow(t, s, d1, "-dial: connection refused")
	failRow(t, s, t1, "-dial: connection refused")
	failRow(t, s, d2, "mailbox_full: 507")
	now := time.Now().UnixMilli()

	sum, err := s.OutboxSummary(now, true)
	if err != nil || sum.Stuck != 0 || sum.NotSent != 3 || sum.Awaiting != 0 {
		t.Fatalf("a new queue: %+v %v", sum, err)
	}
	sum, _ = s.OutboxSummary(now+2*3600_000, true)
	if sum.Stuck != 1 || sum.StuckError == "" || sum.StuckOldestMS < 3600_000 {
		t.Errorf("two hours on: %+v (want the delegation only)", sum)
	}
	sum, _ = s.OutboxSummary(now+2*3600_000, false)
	if sum.Stuck != 0 {
		t.Errorf("gate closed: %+v", sum)
	}
}

func TestReadOutboxSummaryMissingStore(t *testing.T) {
	if _, err := interactions.ReadOutboxSummary(t.TempDir(), 0, true); err == nil {
		t.Error("no store: want an error")
	}
}

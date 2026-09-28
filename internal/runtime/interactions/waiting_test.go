package interactions_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The requester's minute sweep for unanswered tasks (A2A-DESIGN §4.2) reads
// idx_ix_waiting alone: state_at sits after goal, request_doc and result in
// the row, megabytes each for a long message, and testing it there would
// walk every waiting task's overflow pages each minute (docs/notes/0035).
func TestWaitingReadsItsIndex(t *testing.T) {
	s := open(t)
	plan, err := s.ExplainWaiting()
	if err != nil {
		t.Fatal(err)
	}
	p := strings.Join(plan, "; ")
	if !strings.Contains(p, "COVERING INDEX idx_ix_waiting") || strings.Contains(p, "TEMP B-TREE") {
		t.Fatalf("plan %q, want the covering index idx_ix_waiting and no sort", p)
	}
}

// Waiting lists the tasks of one role and state whose state was written
// before the cutoff, longest-waiting first; nothing else.
func TestWaitingListsWhatWaitedSinceBeforeTheCutoff(t *testing.T) {
	s := open(t)
	clock := int64(1_000_000)
	s.SetClock(func() int64 { return clock })
	mk := func(id string, role interactions.Role, at int64) {
		t.Helper()
		clock = at
		if err := s.Create(interactions.New{ID: id, Role: role, PeerAID: "peer-a", Goal: "g"}); err != nil {
			t.Fatal(err)
		}
	}
	mk("old", interactions.RoleOutbound, 1000)
	mk("older", interactions.RoleOutbound, 500)
	mk("new", interactions.RoleOutbound, 5000)
	mk("inbound", interactions.RoleInbound, 100)
	mk("working", interactions.RoleOutbound, 200)
	clock = 300
	if _, err := s.SetState("working", interactions.StateWorking); err != nil {
		t.Fatal(err)
	}
	ids := func(ws []interactions.WaitingTask) string {
		var out []string
		for _, w := range ws {
			out = append(out, w.ID)
		}
		return strings.Join(out, ",")
	}
	got, err := s.Waiting(interactions.RoleOutbound, interactions.StateSubmitted, 2000, interactions.WaitingFirst, 10)
	if err != nil {
		t.Fatal(err)
	}
	if ids(got) != "older,old" || got[0].PeerAID != "peer-a" || got[0].StateAt != 500 {
		t.Fatalf("Waiting = %+v, want older, old", got)
	}
	// Paged: the next page starts after the last task of this one.
	first, _ := s.Waiting(interactions.RoleOutbound, interactions.StateSubmitted, 2000, interactions.WaitingFirst, 1)
	if ids(first) != "older" {
		t.Fatalf("Waiting limit 1 = %+v", first)
	}
	if next, _ := s.Waiting(interactions.RoleOutbound, interactions.StateSubmitted, 2000, first[0], 1); ids(next) != "old" {
		t.Fatalf("second page = %+v", next)
	}
	if last, _ := s.Waiting(interactions.RoleOutbound, interactions.StateSubmitted, 2000, got[1], 1); len(last) != 0 {
		t.Fatalf("after the last = %+v", last)
	}
}

// A delegation's delivery moves its task's updated_at, which the
// no-response deadline counts from; any other delivered row, and a task
// that has ended, is not touched.
func TestADeliveredDelegationMarksWhenItLeft(t *testing.T) {
	s := open(t)
	if err := s.Create(interactions.New{ID: "ix", Role: interactions.RoleOutbound, PeerAID: "peer-a", Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get("ix")
	id, err := s.EnqueueOutbox(interactions.OutboxItem{IX: "ix", ToAID: "peer-a", Type: "anet.delegate/1", Body: []byte("b")})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := s.DeleteDeliveredOutbox(id, "ix", false); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Get("ix"); cur.UpdatedAt != before.UpdatedAt {
		t.Fatalf("a row that is not a delegation moved updated_at: %s -> %s", before.UpdatedAt, cur.UpdatedAt)
	}
	id, err = s.EnqueueOutbox(interactions.OutboxItem{IX: "ix", ToAID: "peer-a", Type: "anet.delegate/1", Body: []byte("c")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDeliveredOutbox(id, "ix", true); err != nil {
		t.Fatal(err)
	}
	cur, _ := s.Get("ix")
	if !cur.UpdatedAtTime().After(before.UpdatedAtTime()) {
		t.Fatalf("the delegation's delivery did not move updated_at (%s, before %s)", cur.UpdatedAt, before.UpdatedAt)
	}
	if n, _ := s.OutboxLen(); n != 0 {
		t.Fatalf("%d rows left", n)
	}
	if err := s.SetFailed("ix", nil); err != nil {
		t.Fatal(err)
	}
	ended, _ := s.Get("ix")
	id, _ = s.EnqueueOutbox(interactions.OutboxItem{IX: "ix", ToAID: "peer-a", Type: "anet.delegate/1", Body: []byte("d")})
	time.Sleep(5 * time.Millisecond)
	if err := s.DeleteDeliveredOutbox(id, "ix", true); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Get("ix"); cur.UpdatedAt != ended.UpdatedAt {
		t.Fatal("a delivery touched a task that had ended")
	}
}

package interactions_test

import (
	"fmt"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// A refused delegation is remembered by (sender, message id) until its
// expiry, across a reopen of the store [redteam:F5].
func TestARefusedRowSurvivesAReopenAndExpires(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRefused("peer", []byte("mid-1"), 1000, 5000); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRefused("peer", []byte("mid-1"), 1000, 5000); err != nil {
		t.Fatalf("recording twice: %v", err)
	}
	s.Close()
	if s, err = interactions.Open(dir); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	exact, floor, err := s.Refused("peer", []byte("mid-1"))
	if err != nil || !exact || floor != 0 {
		t.Fatalf("after reopen: exact %v floor %d err %v", exact, floor, err)
	}
	if exact, _, _ := s.Refused("other", []byte("mid-1")); exact {
		t.Fatal("another sender's message id matched")
	}
	if n, err := s.PurgeRefused(5001); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if exact, _, _ := s.Refused("peer", []byte("mid-1")); exact {
		t.Fatal("an expired row is still there")
	}
}

// Past the per-sender bound the sender's oldest rows go and its floor
// rises to cover them; other senders are not affected. Past the total
// bound the oldest rows of anyone go into the global floor. Floors expire
// with the rows they stand for.
func TestRefusedRowsAreBoundedByFloors(t *testing.T) {
	s := open(t)
	s.SetRefusedCaps(interactions.RefusedCaps{PerSender: 3, Total: 5})
	rec := func(from string, i int) {
		t.Helper()
		if err := s.RecordRefused(from, []byte(fmt.Sprintf("mid-%d", i)), uint64(100+i), uint64(10_000+i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 4; i++ {
		rec("a", i)
	}
	// a's oldest (ts 101) went into a's floor.
	if exact, floor, _ := s.Refused("a", []byte("mid-1")); exact || floor != 101 {
		t.Fatalf("a mid-1: exact %v floor %d, want evicted under floor 101", exact, floor)
	}
	if exact, _, _ := s.Refused("a", []byte("mid-4")); !exact {
		t.Fatal("a's newest row went")
	}
	if _, floor, _ := s.Refused("b", []byte("mid-9")); floor != 0 {
		t.Fatalf("b has floor %d from a's eviction", floor)
	}
	// Total 5: a holds 3; b's third row pushes the total to 6, and the
	// oldest row overall (a's ts 102) goes into the global floor.
	rec("b", 5)
	rec("b", 6)
	rec("b", 7)
	rows, floors, err := s.RefusedCount()
	if err != nil || rows != 5 || floors != 2 {
		t.Fatalf("rows %d floors %d err %v", rows, floors, err)
	}
	if _, floor, _ := s.Refused("b", []byte("mid-0")); floor != 102 {
		t.Fatalf("global floor %d, want 102", floor)
	}
	if exact, floor, _ := s.Refused("a", []byte("mid-2")); exact || floor != 102 {
		t.Fatalf("a mid-2: exact %v floor %d", exact, floor)
	}
	// Everything expires.
	if _, err := s.PurgeRefused(20_000); err != nil {
		t.Fatal(err)
	}
	if rows, floors, _ := s.RefusedCount(); rows != 0 || floors != 0 {
		t.Fatalf("after expiry: %d rows, %d floors", rows, floors)
	}
}

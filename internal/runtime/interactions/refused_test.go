package interactions_test

import (
	"errors"
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
	if err := s.RecordRefused("peer", []byte("mid-1"), 1000, 5000, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRefused("peer", []byte("mid-1"), 1000, 5000, ""); err != nil {
		t.Fatalf("recording twice: %v", err)
	}
	s.Close()
	if s, err = interactions.Open(dir); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	exact, _, floor, err := s.Refused("peer", []byte("mid-1"))
	if err != nil || !exact || floor != 0 {
		t.Fatalf("after reopen: exact %v floor %d err %v", exact, floor, err)
	}
	if exact, _, _, _ := s.Refused("other", []byte("mid-1")); exact {
		t.Fatal("another sender's message id matched")
	}
	if n, err := s.PurgeRefused(5001); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if exact, _, _, _ := s.Refused("peer", []byte("mid-1")); exact {
		t.Fatal("an expired row is still there")
	}
}

// Past the per-sender bound the sender's oldest rows go and its floor
// rises to cover them; other senders are not affected. Past the total
// bound the oldest rows of anyone go, each into its own sender's floor:
// there is no floor shared by all senders [redteam:F5]. Floors expire with
// the rows they stand for.
func TestRefusedRowsAreBoundedByFloors(t *testing.T) {
	s := open(t)
	s.SetRefusedCaps(interactions.RefusedCaps{PerSender: 3, Total: 5})
	rec := func(from string, i int) {
		t.Helper()
		if err := s.RecordRefused(from, []byte(fmt.Sprintf("mid-%d", i)), uint64(100+i), uint64(10_000+i), ""); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 4; i++ {
		rec("a", i)
	}
	// a's oldest (ts 101) went into a's floor.
	if exact, _, floor, _ := s.Refused("a", []byte("mid-1")); exact || floor != 101 {
		t.Fatalf("a mid-1: exact %v floor %d, want evicted under floor 101", exact, floor)
	}
	if exact, _, _, _ := s.Refused("a", []byte("mid-4")); !exact {
		t.Fatal("a's newest row went")
	}
	if _, _, floor, _ := s.Refused("b", []byte("mid-9")); floor != 0 {
		t.Fatalf("b has floor %d from a's eviction", floor)
	}
	// Total 5: a holds 3; b's third row pushes the total to 6, and the
	// oldest row overall (a's ts 102) goes into a's floor, not into one b
	// or anybody else is judged by.
	rec("b", 5)
	rec("b", 6)
	rec("b", 7)
	rows, floors, err := s.RefusedCount()
	if err != nil || rows != 5 || floors != 1 {
		t.Fatalf("rows %d floors %d err %v", rows, floors, err)
	}
	if exact, _, floor, _ := s.Refused("a", []byte("mid-2")); exact || floor != 102 {
		t.Fatalf("a mid-2: exact %v floor %d, want evicted under a's floor 102", exact, floor)
	}
	for _, who := range []string{"b", "c"} {
		if _, _, floor, _ := s.Refused(who, []byte("mid-0")); floor != 0 {
			t.Fatalf("%s has floor %d from another sender's eviction", who, floor)
		}
	}
	// Everything expires.
	if _, err := s.PurgeRefused(20_000); err != nil {
		t.Fatal(err)
	}
	if rows, floors, _ := s.RefusedCount(); rows != 0 || floors != 0 {
		t.Fatalf("after expiry: %d rows, %d floors", rows, floors)
	}
}

// The floors are bounded as well: past the bound the floors that expire
// first are forgotten, and the newer ones stay.
func TestRefusedFloorsAreBounded(t *testing.T) {
	s := open(t)
	s.SetRefusedCaps(interactions.RefusedCaps{PerSender: 1, Total: 100, Floors: 2})
	for i, who := range []string{"a", "b", "c"} {
		for j := 0; j < 2; j++ {
			ts := uint64(100 + 10*i + j)
			if err := s.RecordRefused(who, []byte(fmt.Sprintf("mid-%d", j)), ts, ts+10_000, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	if rows, floors, err := s.RefusedCount(); err != nil || rows != 3 || floors != 2 {
		t.Fatalf("rows %d floors %d err %v", rows, floors, err)
	}
	if _, _, floor, _ := s.Refused("a", []byte("mid-0")); floor != 0 {
		t.Fatalf("a's floor %d, want forgotten (it expires first)", floor)
	}
	for who, want := range map[string]uint64{"b": 110, "c": 120} {
		if _, _, floor, _ := s.Refused(who, []byte("mid-0")); floor != want {
			t.Fatalf("%s's floor %d, want %d", who, floor, want)
		}
	}
}

// One eviction over several senders raises each one's own floor to its
// own newest evicted row.
func TestAnEvictionOverSeveralSendersRaisesEachOwnFloor(t *testing.T) {
	s := open(t)
	for i, who := range []string{"a", "b", "a", "c", "d"} {
		ts := uint64(100 + i)
		if err := s.RecordRefused(who, []byte(fmt.Sprintf("mid-%d", i)), ts, ts+10_000, ""); err != nil {
			t.Fatal(err)
		}
	}
	s.SetRefusedCaps(interactions.RefusedCaps{Total: 2})
	if err := s.RecordRefused("e", []byte("mid-5"), 105, 10_105, ""); err != nil {
		t.Fatal(err)
	}
	for who, want := range map[string]uint64{"a": 102, "b": 101, "c": 103, "d": 0, "e": 0} {
		if _, _, floor, err := s.Refused(who, []byte("x")); err != nil || floor != want {
			t.Fatalf("%s: floor %d (%v), want %d", who, floor, err, want)
		}
	}
	if rows, floors, err := s.RefusedCount(); err != nil || rows != 2 || floors != 3 {
		t.Fatalf("rows %d floors %d err %v", rows, floors, err)
	}
}

// A refusal keeps the reason it was told with across a restart (T27, the
// store's part; DLV-ST-8, M25): a replay after the daemon restarted is
// answered with the reason of the first answer, and recording the same
// delegation again does not change it.
func TestRefusedReasonSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRefused("peer", []byte("mid-busy"), 1000, 5000, "quota_caller_per_day"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRefused("peer", []byte("mid-none"), 1000, 5000, ""); err != nil {
		t.Fatal(err)
	}
	// The same delegation again, judged differently now: the first reason
	// is what the requester was told, and stays.
	if err := s.RecordRefused("peer", []byte("mid-busy"), 1000, 5000, "not_accepting"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = interactions.Open(dir); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for mid, want := range map[string]string{"mid-busy": "quota_caller_per_day", "mid-none": ""} {
		exact, reason, _, err := s.Refused("peer", []byte(mid))
		if err != nil || !exact || reason != want {
			t.Errorf("%s after a restart: exact %v reason %q (%v), want %q", mid, exact, reason, err, want)
		}
	}
	// A row that went into the floor took its reason with it.
	if exact, reason, _, err := s.Refused("peer", []byte("mid-unknown")); err != nil || exact || reason != "" {
		t.Errorf("an unrecorded delegation: exact %v reason %q (%v)", exact, reason, err)
	}
}

// Only a reason code is recorded (CORE-K19): anything else is refused and
// nothing is written, so a stored reason is always one a delivery token's
// note can carry.
func TestRecordRefusedTakesOnlyAReasonCode(t *testing.T) {
	s := open(t)
	for _, bad := range []string{"Busy", "busy now", "quota-caller", "_x", "1busy", "anet.reason", "a23456789012345678901234567890x"} {
		err := s.RecordRefused("peer", []byte("mid-"+bad), 1000, 5000, bad)
		if !errors.Is(err, interactions.ErrBadInput) {
			t.Errorf("reason %q: %v, want ErrBadInput", bad, err)
		}
		if exact, _, _, _ := s.Refused("peer", []byte("mid-"+bad)); exact {
			t.Errorf("reason %q: the row was written", bad)
		}
	}
	for _, good := range []string{"", "busy", "a23456789012345678901234567890", "file_type_not_accepted", "x9_"} {
		if err := s.RecordRefused("peer", []byte("mid-"+good), 1000, 5000, good); err != nil {
			t.Errorf("reason %q: %v", good, err)
		}
	}
}

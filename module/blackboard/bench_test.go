package blackboard

import (
	"fmt"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
)

// The board's cost is paid on every contribution from every member, and it
// is dominated by signature verification rather than by the CRDT — which is
// the right shape: merging is cheap, trusting is not.

func benchUnit(b *testing.B, ctrl *identity.Controller, i int) *CogUnit {
	b.Helper()
	u := &CogUnit{
		TaskID: "task-1", Scope: "org", Type: "note",
		Body:  []byte(fmt.Sprintf("contribution %d", i)),
		Stamp: HLC{Wall: time.Now().UnixMilli(), Logical: uint16(i), NodeID: ctrl.AID()},
	}
	if err := u.Sign(ctrl); err != nil {
		b.Fatal(err)
	}
	return u
}

func BenchmarkAddVerified(b *testing.B) {
	ctrl, err := identity.Incept()
	if err != nil {
		b.Fatal(err)
	}
	resolve := func(string) ([]identity.SignedEvent, bool) { return ctrl.KEL(), true }
	board := New("bench")

	units := make([]*CogUnit, 256)
	for i := range units {
		units[i] = benchUnit(b, ctrl, i)
	}
	now := time.Now().UnixMilli()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := board.Add(units[i%len(units)], resolve, now); err != nil {
			b.Fatal(err)
		}
	}
}

// Re-adding is the common case when members sync: the CRDT is add-wins and
// idempotent, so a peer replaying its contributions must be cheap.
func BenchmarkAddIdempotent(b *testing.B) {
	ctrl, err := identity.Incept()
	if err != nil {
		b.Fatal(err)
	}
	resolve := func(string) ([]identity.SignedEvent, bool) { return ctrl.KEL(), true }
	board := New("bench")
	u := benchUnit(b, ctrl, 0)
	now := time.Now().UnixMilli()
	if _, err := board.Add(u, resolve, now); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := board.Add(u, resolve, now); err != nil {
			b.Fatal(err)
		}
	}
}

// Snapshot sorts by causal stamp, so its cost grows with the board and is
// paid by every member that pulls.
func BenchmarkSnapshot(b *testing.B) {
	for _, n := range []int{16, 256, 4096} {
		b.Run(fmt.Sprintf("%dunits", n), func(b *testing.B) {
			ctrl, err := identity.Incept()
			if err != nil {
				b.Fatal(err)
			}
			resolve := func(string) ([]identity.SignedEvent, bool) { return ctrl.KEL(), true }
			board := New("bench")
			now := time.Now().UnixMilli()
			for i := 0; i < n; i++ {
				if _, err := board.Add(benchUnit(b, ctrl, i), resolve, now); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := len(board.Snapshot()); got != n {
					b.Fatalf("snapshot returned %d, want %d", got, n)
				}
			}
		})
	}
}

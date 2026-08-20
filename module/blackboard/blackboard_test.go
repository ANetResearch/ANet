package blackboard_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ANetResearch/ANet/module/blackboard"
	"github.com/ANetResearch/ANetCore/identity"
)

const msgTime = int64(1_000_000)

func kelOf(ctrls ...*identity.Controller) blackboard.KELResolver {
	m := map[string][]identity.SignedEvent{}
	for _, c := range ctrls {
		m[c.AID()] = c.KEL()
	}
	return func(aid string) ([]identity.SignedEvent, bool) { k, ok := m[aid]; return k, ok }
}

func mkUnit(t *testing.T, ctrl *identity.Controller, typ string, stamp blackboard.HLC, body []byte) *blackboard.CogUnit {
	t.Helper()
	u := &blackboard.CogUnit{TaskID: "task1", Scope: "org", Type: typ, Stamp: stamp, Body: body}
	if err := u.Sign(ctrl); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return u
}

func hlc(w int64, l uint16, n string) blackboard.HLC {
	return blackboard.HLC{Wall: w, Logical: l, NodeID: n}
}

// The per-task phase machine gates contributions + sedimentation: a CONCLUDED task freezes (Add
// rejected), transitions are ordered (Active→Concluded→Archived), no-TaskID units are always addable,
// and UnitsForTask scopes to a task.
func TestPhaseMachine(t *testing.T) {
	a, _ := identity.Incept()
	res := kelOf(a)
	bb := blackboard.New("node-a")

	if bb.Phase("task1") != blackboard.PhaseActive {
		t.Fatal("a fresh task must default to Active")
	}
	// add while Active → ok.
	if _, err := bb.Add(mkUnit(t, a, "claim", hlc(1000, 0, "a"), []byte("c1")), res, msgTime); err != nil {
		t.Fatalf("add while active: %v", err)
	}
	// conclude → frozen: a further unit for the task is rejected, and the pre-conclusion cognition stands.
	if err := bb.Conclude("task1"); err != nil {
		t.Fatal(err)
	}
	if bb.Phase("task1") != blackboard.PhaseConcluded {
		t.Fatal("task must be Concluded after Conclude")
	}
	if _, err := bb.Add(mkUnit(t, a, "claim", hlc(1001, 0, "a"), []byte("c2")), res, msgTime); !errors.Is(err, blackboard.ErrTaskNotActive) {
		t.Fatalf("add after conclude = %v, want ErrTaskNotActive", err)
	}
	if u := bb.UnitsForTask("task1"); len(u) != 1 || string(u[0].Body) != "c1" {
		t.Fatalf("UnitsForTask after conclude = %v, want exactly [c1]", u)
	}
	// a no-TaskID (board-global) unit is addable regardless of any task's phase.
	g := &blackboard.CogUnit{Scope: "org", Type: "claim", Stamp: hlc(2000, 0, "a"), Body: []byte("global")}
	if err := g.Sign(a); err != nil {
		t.Fatal(err)
	}
	if _, err := bb.Add(g, res, msgTime); err != nil {
		t.Fatalf("a no-TaskID unit must be addable when a task is concluded: %v", err)
	}
	// ordered transitions: archive ok; archived→concluded rejected; a fresh active task can't skip to archived.
	if err := bb.Archive("task1"); err != nil {
		t.Fatalf("concluded→archived: %v", err)
	}
	if err := bb.Conclude("task1"); !errors.Is(err, blackboard.ErrPhaseTransition) {
		t.Fatalf("archived→concluded = %v, want ErrPhaseTransition", err)
	}
	if err := bb.Archive("task2"); !errors.Is(err, blackboard.ErrPhaseTransition) {
		t.Fatalf("active→archived = %v, want ErrPhaseTransition", err)
	}
}

// Units are returned in causal (HLC) order regardless of add order.
func TestAddSnapshotOrdered(t *testing.T) {
	a, _ := identity.Incept()
	res := kelOf(a)
	bb := blackboard.New("node-a")
	u1 := mkUnit(t, a, "claim", hlc(1000, 0, "a"), []byte("first"))
	u2 := mkUnit(t, a, "claim", hlc(1001, 0, "a"), []byte("second"))
	u3 := mkUnit(t, a, "claim", hlc(1002, 0, "a"), []byte("third"))
	for _, u := range []*blackboard.CogUnit{u3, u1, u2} {
		if _, err := bb.Add(u, res, msgTime); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	snap := bb.Snapshot()
	if len(snap) != 3 || string(snap[0].Body) != "first" || string(snap[2].Body) != "third" {
		t.Fatalf("snapshot not HLC-ordered: %v", snap)
	}
}

// Two blackboards that merge the SAME units in DIFFERENT orders converge to the same view.
func TestConvergence(t *testing.T) {
	a, _ := identity.Incept()
	b, _ := identity.Incept()
	res := kelOf(a, b)
	u1 := mkUnit(t, a, "claim", hlc(1000, 0, "a"), []byte("a1"))
	u2 := mkUnit(t, b, "evidence", hlc(1000, 1, "b"), []byte("b1"))
	u3 := mkUnit(t, a, "conclusion", hlc(1002, 0, "a"), []byte("a2"))

	bb1 := blackboard.New("n1")
	bb2 := blackboard.New("n2")
	if _, err := bb1.Merge([]*blackboard.CogUnit{u1, u2, u3}, res, msgTime); err != nil {
		t.Fatal(err)
	}
	n, err := bb2.Merge([]*blackboard.CogUnit{u3, u1, u2}, res, msgTime)
	if err != nil || n != 3 {
		t.Fatalf("merge added=%d err=%v want 3", n, err)
	}
	s1, s2 := bb1.Snapshot(), bb2.Snapshot()
	if len(s1) != 3 || len(s2) != 3 {
		t.Fatalf("len mismatch: %d %d", len(s1), len(s2))
	}
	for i := range s1 {
		id1, _ := s1[i].ID()
		id2, _ := s2[i].ID()
		if id1 != id2 {
			t.Fatalf("divergent at %d: %s vs %s", i, id1, id2)
		}
	}
}

// Two DISTINCT units with the SAME full HLC stamp sort deterministically (CID tie-break) on
// replicas that ingested them in different orders.
func TestEqualStampDeterministic(t *testing.T) {
	a, _ := identity.Incept()
	res := kelOf(a)
	st := hlc(1000, 0, "a")
	u1 := mkUnit(t, a, "claim", st, []byte("alpha"))
	u2 := mkUnit(t, a, "claim", st, []byte("beta"))
	bb1, bb2 := blackboard.New("n1"), blackboard.New("n2")
	bb1.Add(u1, res, msgTime)
	bb1.Add(u2, res, msgTime)
	bb2.Add(u2, res, msgTime) // reverse order
	bb2.Add(u1, res, msgTime)
	s1, s2 := bb1.Snapshot(), bb2.Snapshot()
	if len(s1) != 2 || len(s2) != 2 {
		t.Fatalf("len: %d %d", len(s1), len(s2))
	}
	for i := range s1 {
		id1, _ := s1[i].ID()
		id2, _ := s2[i].ID()
		if id1 != id2 {
			t.Fatalf("equal-stamp order non-deterministic at %d", i)
		}
	}
}

// A pulled snapshot unit re-verifies on the other side (the envelope survives Marshal/Unmarshal).
func TestSnapshotMarshalRoundTrip(t *testing.T) {
	a, _ := identity.Incept()
	res := kelOf(a)
	bb := blackboard.New("n")
	u := mkUnit(t, a, "claim", hlc(1000, 0, "a"), []byte("payload"))
	if _, err := bb.Add(u, res, msgTime); err != nil {
		t.Fatal(err)
	}
	for _, su := range bb.Snapshot() {
		wire, err := su.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		got, err := blackboard.Unmarshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		if err := got.Verify(a.KEL(), msgTime); err != nil {
			t.Fatalf("pulled unit must re-verify (envelope must survive the wire): %v", err)
		}
		id1, _ := su.ID()
		id2, _ := got.ID()
		if id1 != id2 {
			t.Fatalf("ID changed across marshal: %s vs %s", id1, id2)
		}
	}
}

func TestVerifyRejects(t *testing.T) {
	a, _ := identity.Incept()
	other, _ := identity.Incept()
	bb := blackboard.New("n")
	res := kelOf(a, other)

	// tampered: mutate Body after sign.
	u := mkUnit(t, a, "claim", hlc(1000, 0, "a"), []byte("ok"))
	u.Body = []byte("tampered")
	if _, err := bb.Add(u, res, msgTime); err == nil {
		t.Fatal("tampered unit must fail")
	}
	// author-spoof: signed by a, but claims other as Author (resolver knows other, so it reaches
	// the signer!=author check rather than unknown-author).
	sp := mkUnit(t, a, "claim", hlc(1000, 0, "a"), []byte("x"))
	sp.Author = other.AID()
	if _, err := bb.Add(sp, res, msgTime); !errors.Is(err, blackboard.ErrUnitAuthor) {
		t.Fatalf("author-spoof: want ErrUnitAuthor, got %v", err)
	}
	// unsigned.
	un := &blackboard.CogUnit{Author: a.AID(), Type: "claim", Stamp: hlc(1, 0, "a")}
	if _, err := bb.Add(un, res, msgTime); !errors.Is(err, blackboard.ErrUnitUnsigned) {
		t.Fatalf("unsigned: want ErrUnitUnsigned, got %v", err)
	}
	// unknown author (resolver doesn't know it).
	stranger, _ := identity.Incept()
	us := mkUnit(t, stranger, "claim", hlc(1, 0, "s"), []byte("y"))
	if _, err := bb.Add(us, res, msgTime); !errors.Is(err, blackboard.ErrUnitUnknownAuthor) {
		t.Fatalf("unknown author: want ErrUnitUnknownAuthor, got %v", err)
	}
	// missing type.
	nt := &blackboard.CogUnit{Stamp: hlc(1, 0, "a")}
	if err := nt.Sign(a); err != nil {
		t.Fatal(err)
	}
	if _, err := bb.Add(nt, res, msgTime); !errors.Is(err, blackboard.ErrUnitNoType) {
		t.Fatalf("no type: want ErrUnitNoType, got %v", err)
	}
	// zero stamp.
	zs := &blackboard.CogUnit{Type: "claim"}
	if err := zs.Sign(a); err != nil {
		t.Fatal(err)
	}
	if _, err := bb.Add(zs, res, msgTime); !errors.Is(err, blackboard.ErrUnitBadStamp) {
		t.Fatalf("zero stamp: want ErrUnitBadStamp, got %v", err)
	}
}

func TestIdempotentReAdd(t *testing.T) {
	a, _ := identity.Incept()
	res := kelOf(a)
	bb := blackboard.New("n")
	u := mkUnit(t, a, "claim", hlc(1000, 0, "a"), []byte("x"))
	first, _ := bb.Add(u, res, msgTime)
	if !first {
		t.Fatal("first add must report added=true")
	}
	for i := 0; i < 2; i++ {
		was, err := bb.Add(u, res, msgTime)
		if err != nil || was {
			t.Fatalf("re-add must be added=false no-error: was=%v err=%v", was, err)
		}
	}
	if bb.Len() != 1 {
		t.Fatalf("re-add not idempotent: len=%d", bb.Len())
	}
}

// Concurrent adds of distinct units: correct count + Len, race-free.
func TestConcurrentAdd(t *testing.T) {
	a, _ := identity.Incept()
	res := kelOf(a)
	bb := blackboard.New("n")
	const n = 30
	units := make([]*blackboard.CogUnit, n)
	for i := range units {
		units[i] = mkUnit(t, a, "claim", hlc(int64(1000+i), 0, "a"), []byte{byte(i)})
	}
	var wg sync.WaitGroup
	var added int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(u *blackboard.CogUnit) {
			defer wg.Done()
			was, err := bb.Add(u, res, msgTime)
			if err != nil {
				t.Errorf("add: %v", err)
			}
			if was {
				atomic.AddInt32(&added, 1)
			}
		}(units[i])
	}
	wg.Wait()
	if bb.Len() != n || added != int32(n) {
		t.Fatalf("concurrent add: len=%d added=%d want %d", bb.Len(), added, n)
	}
}

// B5: BodyCID is in the signed preimage, so two units differing only in BodyCID have different IDs.
func TestBodyCIDSigned(t *testing.T) {
	a, _ := identity.Incept()
	res := kelOf(a)
	u1 := &blackboard.CogUnit{Type: "claim", Stamp: hlc(1000, 0, "a"), BodyCID: "bafyAAA"}
	u2 := &blackboard.CogUnit{Type: "claim", Stamp: hlc(1000, 0, "a"), BodyCID: "bafyBBB"}
	u1.Sign(a)
	u2.Sign(a)
	id1, _ := u1.ID()
	id2, _ := u2.ID()
	if id1 == id2 {
		t.Fatal("BodyCID must be signed (distinct BodyCID → distinct ID)")
	}
	u1.BodyCID = "bafyBBB" // tamper after sign
	bb := blackboard.New("n")
	if _, err := bb.Add(u1, res, msgTime); err == nil {
		t.Fatal("tampered BodyCID must fail verify")
	}
}

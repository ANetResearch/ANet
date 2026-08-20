package blackboard

import (
	"errors"
	"sort"
	"sync"

	"github.com/ANetResearch/ANetCore/identity"
)

// KELResolver resolves a CogUnit author's AID to its KEL for signature verification.
type KELResolver func(authorAID string) (kel []identity.SignedEvent, ok bool)

// ErrUnitUnknownAuthor is returned when the author's KEL cannot be resolved (distinct from a
// bad/missing signature — the unit may be well-formed but its author is unknown to this board).
var ErrUnitUnknownAuthor = errors.New("blackboard: cannot resolve author KEL")

// Blackboard is the org's shared-cognition store: an add-wins OR-Set of verified CogUnits keyed
// by unit ID, with an HLC clock that advances past every accepted unit. At org-central it is the
// single authoritative board (members push via bb.add, pull via bb.snapshot); the CRDT makes
// merges order-independent and idempotent. Safe for concurrent use.
//
// Scope: this is the cognition STORE plus a per-TASK phase machine (Active→Concluded→Archived, see
// phase.go) that gates sedimentation. The v1 board-wide phase/LWW, member set, and votes are still not
// here — membership is the O2 credential chain; the phase here is per-task and driven by the card
// lifecycle (org-central concludes a task's board when its card reaches done). Durability is the wiring's
// job (org-store/CAS), not this core.
type Blackboard struct {
	mu     sync.Mutex
	set    *ORSet
	store  map[string]*CogUnit
	clock  *Clock
	phases map[string]Phase // taskID → phase (absent == PhaseActive); gates Add + sedimentation
}

// New builds an empty blackboard whose HLC is tagged with nodeID.
func New(nodeID string) *Blackboard {
	return &Blackboard{set: NewORSet(), store: map[string]*CogUnit{}, clock: NewClock(nodeID), phases: map[string]Phase{}}
}

// NewStamp returns a fresh causal stamp. NOTE: the AUTHOR stamps a unit (before signing — the
// stamp is in the signed preimage); org-central must NOT re-stamp a pulled/pushed unit. This is a
// helper for a node authoring its own units; the central board only merges.
func (b *Blackboard) NewStamp() HLC { return b.clock.Now() }

// Add verifies u (resolving the author's KEL, revocation gate as-of msgTime) and merges it. It
// returns added=true iff the unit was new (not already present). Idempotent: re-adding the same
// unit ID is a no-op overwrite returning added=false.
func (b *Blackboard) Add(u *CogUnit, resolve KELResolver, msgTime int64) (added bool, err error) {
	kel, ok := resolve(u.Author)
	if !ok {
		return false, ErrUnitUnknownAuthor
	}
	if err := u.Verify(kel, msgTime); err != nil {
		return false, err
	}
	id, err := u.ID()
	if err != nil {
		return false, err
	}
	b.clock.Merge(u.Stamp)
	// Phase gate + store write under ONE lock so a concurrent Conclude can't let a unit slip into a
	// just-frozen task: a CONCLUDED/ARCHIVED task's cognition is frozen (its sediment is a stable
	// snapshot). Units with no TaskID are board-global and always accepted.
	b.mu.Lock()
	if u.TaskID != "" {
		if ph := b.phases[u.TaskID]; ph != "" && ph != PhaseActive {
			b.mu.Unlock()
			return false, ErrTaskNotActive
		}
	}
	_, exists := b.store[id]
	b.store[id] = u
	b.mu.Unlock()
	b.set.Add(id, u.Stamp)
	return !exists, nil
}

// Merge folds a batch of units in (e.g. a peer snapshot), returning the count NEWLY added and the
// first error (which stops the batch). Order-independent and idempotent.
func (b *Blackboard) Merge(units []*CogUnit, resolve KELResolver, msgTime int64) (int, error) {
	added := 0
	for _, u := range units {
		was, err := b.Add(u, resolve, msgTime)
		if err != nil {
			return added, err
		}
		if was {
			added++
		}
	}
	return added, nil
}

// Get returns a stored unit by ID. The returned pointer is the board's live unit — treat it as
// read-only (mutating it mutates the board's authoritative copy).
func (b *Blackboard) Get(id string) (*CogUnit, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.store[id]
	return u, ok
}

// Snapshot returns the live units in a TOTAL deterministic order: causal (HLC), then by unit ID
// to break stamp ties — so every replica that ingested the same units returns the identical
// sequence (the converged view). Returned pointers are the board's live units (read-only).
func (b *Blackboard) Snapshot() []*CogUnit {
	ids := b.set.Elements()
	type pair struct {
		id string
		u  *CogUnit
	}
	b.mu.Lock()
	ps := make([]pair, 0, len(ids))
	for _, id := range ids {
		if u, ok := b.store[id]; ok {
			ps = append(ps, pair{id, u})
		}
	}
	b.mu.Unlock()
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].u.Stamp.Less(ps[j].u.Stamp) {
			return true
		}
		if ps[j].u.Stamp.Less(ps[i].u.Stamp) {
			return false
		}
		return ps[i].id < ps[j].id // deterministic CID tie-break for equal stamps
	})
	out := make([]*CogUnit, len(ps))
	for i, p := range ps {
		out[i] = p.u
	}
	return out
}

// Len returns the number of live units.
func (b *Blackboard) Len() int { return b.set.Len() }

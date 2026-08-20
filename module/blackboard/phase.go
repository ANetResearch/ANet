package blackboard

import "errors"

// phase.go is the per-TASK phase machine that gates sedimentation (aet-spec §5.4). The v3 blackboard is
// an org-wide OR-Set of CogUnits tagged by TaskID; "the board for task T" is the subset with TaskID==T.
// Each task progresses Active → Concluded → Archived:
//   - Active:    accepting contributions (the default for any task not yet concluded).
//   - Concluded: the task's card reached done; cognition is FROZEN and the task is eligible to sediment.
//   - Archived:  the task has been sedimented into an AET template (terminal).
//
// org-central drives Conclude from the card lifecycle (card → done) and Archive from Sediment.
type Phase string

const (
	PhaseActive    Phase = "active"
	PhaseConcluded Phase = "concluded"
	PhaseArchived  Phase = "archived"
)

// ErrTaskNotActive is returned by Add when a unit targets a task whose board is no longer Active.
var ErrTaskNotActive = errors.New("blackboard: task board is concluded/archived (cognition frozen)")

// ErrPhaseTransition is returned for an illegal phase transition (e.g. Archived → Concluded).
var ErrPhaseTransition = errors.New("blackboard: illegal phase transition")

// Phase returns a task's current phase (PhaseActive if it has never been transitioned).
func (b *Blackboard) Phase(taskID string) Phase {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ph, ok := b.phases[taskID]; ok {
		return ph
	}
	return PhaseActive
}

// Conclude moves a task Active → Concluded (idempotent if already Concluded); freezing further Add for it.
// Archived → Concluded is rejected (a sedimented task is terminal).
func (b *Blackboard) Conclude(taskID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.phases[taskID] {
	case PhaseArchived:
		return ErrPhaseTransition
	default: // active (absent) or already concluded
		b.phases[taskID] = PhaseConcluded
		return nil
	}
}

// Archive moves a task Concluded → Archived (idempotent if already Archived). A task must be Concluded
// first (Active → Archived is rejected — only a frozen task can be sedimented/archived).
func (b *Blackboard) Archive(taskID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.phases[taskID] {
	case PhaseConcluded, PhaseArchived:
		b.phases[taskID] = PhaseArchived
		return nil
	default: // active/absent
		return ErrPhaseTransition
	}
}

// UnitsForTask returns the live units tagged with taskID, in the board's deterministic converged order.
// It is the sediment input — the task's accumulated cognition.
func (b *Blackboard) UnitsForTask(taskID string) []*CogUnit {
	all := b.Snapshot()
	out := make([]*CogUnit, 0, len(all))
	for _, u := range all {
		if u.TaskID == taskID {
			out = append(out, u)
		}
	}
	return out
}

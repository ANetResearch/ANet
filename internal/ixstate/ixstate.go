// Package ixstate holds the value domains of an interaction's state and
// payment state, apart from the store that keeps them. It is a leaf: it
// imports nothing, so that packages which only need to name a state (the
// A2A task shapes, the module interfaces) do not reach the store, the
// sealing code or the database driver (11-modularity MOD-5,
// TestSeamClosureHasNoStore). internal/runtime/interactions re-exports
// these names as type aliases and constants.
package ixstate

// State is an interaction's task state (A2A-DESIGN §4.1). The strings are
// the A2A TaskState names in lowercase-hyphen form.
type State string

const (
	Submitted     State = "submitted"
	Working       State = "working"
	InputRequired State = "input-required"
	Completed     State = "completed"
	Failed        State = "failed"
	Canceled      State = "canceled"
	Rejected      State = "rejected"
)

// IsTerminal reports whether s is one of the four terminal states. Every
// "has this task ended" check in the daemon goes through it.
func (s State) IsTerminal() bool {
	switch s {
	case Completed, Failed, Canceled, Rejected:
		return true
	}
	return false
}

// Valid reports whether s is one of the seven defined states.
func (s State) Valid() bool {
	switch s {
	case Submitted, Working, InputRequired, Completed, Failed, Canceled, Rejected:
		return true
	}
	return false
}

// Values of the pay_state column (A2A-DESIGN §4.1). Written by the payment
// flow; listed here so the value domain is defined next to the column.
const (
	PayNone      = ""          // no quote
	PayRequired  = "required"  // quoted (provider) or quote received (requester)
	PaySubmitted = "submitted" // provider: payload persisted, awaiting settlement; requester: payment-submitted sent
	PayCompleted = "completed" // settled
	PayFailed    = "failed"    // settlement definitely failed
	PayRejected  = "rejected"  // the requester declined to pay
)

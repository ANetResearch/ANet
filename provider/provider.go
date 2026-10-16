// Package provider defines C1 — the CapabilityProvider contract (the C1 contract):
// the only doorway through which the ANet daemon acquires callable
// capabilities. Native agents, external agent connectors, and physical-world
// runtimes (ANetLink) all enter through this one interface.
//
// Red line (the org lesson): the daemon must never know what stands
// behind a provider — in particular it must never know the concept of a
// "device". A provider declares capabilities, can be invoked, and returns
// effects. Nothing else crosses the boundary.
package provider

import (
	"context"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
)

// Call is one capability invocation crossing C1.
type Call struct {
	// Capability is the exact capability id being invoked (taxonomy form,
	// e.g. "light.onoff"). Resolution to a provider happens before Invoke.
	Capability string
	// Args are CoreDet-encodable invocation arguments.
	Args map[string]any
	// CallID is a caller-chosen idempotency key; providers MAY use it to
	// de-duplicate retries.
	CallID string
	// CallerAID is the verified AID of the calling identity, or "" when the
	// call originates from a local surface (CLI/MCP) before any delegation.
	// It is an authenticated caller only when Via is ViaRelay; see
	// VerifiedCaller.
	CallerAID string
	// Via says which door the call came through: ViaRelay (a delegation
	// whose sender the daemon authenticated). Empty means a local surface.
	// There is no other door: work is paid for inside the task, on the
	// relay door, and the hub's gateway and its vouchers are gone (0.3.0).
	Via string
	// Files are the files a call carries (CUX-F20), in the order of the
	// request's $files. Mime is the type the daemon sniffed from the bytes,
	// never the caller's claim. They are held in memory for the call only:
	// a provider that keeps a file keeps its own copy. Empty for a call
	// without files.
	Files []File
}

// File is one file of a call.
type File struct {
	Name string
	Mime string
	Data []byte
}

// FileReceiver is implemented by a provider that can take the files of a
// call (CUX-F15). The daemon asks per capability: a capability that
// declares files is served only by a provider that answers true for it, and
// the daemon does not look at which provider that is.
type FileReceiver interface {
	AcceptsFiles(capID string) bool
}

// Values of Call.Via.
const (
	ViaRelay = "relay"
)

// VerifiedCaller returns CallerAID when the daemon authenticated it (a call
// that came through the relay, Via == ViaRelay), and "" otherwise.
//
// It is the one caller a provider may pass on: to its log, to its own
// backend, or across C1 to ANetLink as caller.aid (provider/anetlink). Any
// other Via answers "": a local surface has no remote caller at all, and a
// door this version does not know is not one whose caller it vouches for.
// A provider that implements Priced must not read CallerAID itself (the
// structural test in this package checks that; provider/anetlink has its
// own), and no provider may grant anything on the strength of an empty
// answer.
//
// There is no second accessor saying how fresh the caller's witness notice
// was: the daemon makes a relay call only for a message its receive
// pipeline accepted, notice check included, so every relay caller is a
// witnessed one.
func (c Call) VerifiedCaller() string {
	if c.Via != ViaRelay {
		return ""
	}
	return c.CallerAID
}

// RemotePolicy is implemented by a provider whose far side decides, per
// capability, whether a caller that came through the relay may use it
// (ANetLink's c1.remote, which by default allows none).
//
// Optional. The daemon asks before it publishes: a public capability the
// provider refuses to remote callers is left off the card and out of the
// hub registration, since every call to it would be answered
// provider_policy. The daemon learns nothing from the answer but which ids
// to leave out; it does not know what stands behind them.
type RemotePolicy interface {
	// RemoteAllowed reports, for each capability id the provider serves,
	// whether a remote caller may use it. An id it does not list is not
	// allowed, and when it fails none is.
	RemoteAllowed(ctx context.Context) (map[string]bool, error)
}

// RefusalError is an Invoke error for a call the far side refused, or
// failed without an effect to report, for a reason the provider can name in
// the user's words.
//
// Reason is a code of the reason table, the anet.reason the task ends with
// (provider_policy, capability_not_served, args_too_large, unavailable,
// backend_failed, …): never the far side's own code, which belongs to its
// namespace and stays in this node's log (Err). The daemon ends the task as
// that code's row says: rejected, effect UNAVAILABLE, for a call the far
// side certainly did not run; failed, with the row's effect, otherwise. The
// requester is sent the code, not a message: its own node renders the
// sentence.
type RefusalError struct {
	Reason string
	// Verbatim says Reason is a code the far side chose, in the form of
	// CORE-K19, that this node repeats as it is (CUX-F22) even when the
	// reason table has no row for it: the task then ends failed, effect
	// FAILED. Without it a code the table does not know becomes
	// backend_failed.
	Verbatim bool
	// RetryAfterS is how long the far side asks to wait, in seconds,
	// already rounded (seal.RetryBucket); 0 for none. The requester gets
	// it as anet.retry_after_ms.
	RetryAfterS uint32
	// Err is what happened, for this node's log only.
	Err error
}

func (e *RefusalError) Error() string {
	if e.Err == nil {
		return "refused: " + e.Reason
	}
	return "refused (" + e.Reason + "): " + e.Err.Error()
}

func (e *RefusalError) Unwrap() error { return e.Err }

// CapabilityProvider is C1. Implementations must be safe for concurrent use.
type CapabilityProvider interface {
	// ID is the stable provider identifier, unique within one daemon.
	ID() string
	// Capabilities lists the capability ids this provider currently offers.
	Capabilities(ctx context.Context) ([]string, error)
	// Describe returns the CID of the provider's descriptor object in the
	// local CAS ("" when the provider publishes none). The daemon does not
	// interpret it.
	Describe(ctx context.Context) (string, error)
	// Invoke executes one capability call and reports its effect. Transport
	// or execution errors are returned as error; a reachable target whose
	// effect cannot be verified is NOT an error (effect.Unverified). An
	// error after which the effect may or may not have happened is an
	// *OutcomeUnknownError.
	Invoke(ctx context.Context, call Call) (effect.Effect, error)
	// Health reports provider liveness; a non-nil error marks every
	// capability of this provider temporarily unavailable.
	Health(ctx context.Context) error
}

// OutcomeUnknownError is an Invoke error after which nobody can say
// whether the effect happened: the call reached the far end, or may have,
// and its answer did not come back — the call's deadline passed while the
// target worked, or the connection was lost after the request went out.
//
// Neither of the other answers is honest about it. UNAVAILABLE says
// nothing was attempted at the far end, and invites a retry that can run
// the effect a second time; FAILED says it was attempted and did not
// happen. The daemon reports such a call as a failed task whose effect is
// UNVERIFIED, with Reason as anet.reason, as it does a call interrupted by
// a restart (A2A-DESIGN §4.3). A provider returns it only when the call
// may have reached the target; an error that certainly came first (no
// connection, a refused dial) is still UNAVAILABLE.
type OutcomeUnknownError struct {
	// Reason is the anet.reason the task carries: "timeout" when the
	// call's deadline passed, "connection_lost" otherwise.
	Reason string
	// Err is what went wrong, for the message.
	Err error
}

// Reasons an outcome is unknown (OutcomeUnknownError.Reason).
const (
	ReasonTimeout        = "timeout"
	ReasonConnectionLost = "connection_lost"
)

func (e *OutcomeUnknownError) Error() string {
	msg := "the call was sent and its answer was lost"
	if e.Reason == ReasonTimeout {
		msg = "the call was sent and no answer came back in time"
	}
	if e.Err != nil {
		msg += " (" + e.Err.Error() + ")"
	}
	return msg + "; whether its effect happened is not known"
}

func (e *OutcomeUnknownError) Unwrap() error { return e.Err }

// Priced is implemented by a provider whose capabilities cost something.
//
// Optional, and the daemon does the gating rather than the provider. A
// provider knows what its work is worth; whether a particular caller has
// paid is about the interaction, and putting that in every provider would
// mean every author of a capability writing payment code — which is how
// you get seven subtly different ideas of what "paid" means.
type Priced interface {
	// Price reports what a capability costs, in the hub's credit units,
	// and whether it costs anything at all. Free is (0, false), not
	// (0, true): a caller should not have to read a payment requirement
	// to learn there is none.
	Price(capability string) (uint64, bool)
}

// LongRunning is implemented by a provider whose work can legitimately
// take longer than the daemon's default invocation bound.
//
// Optional, and the provider is the only one who can answer it. The
// daemon bounds an invocation so a stuck provider cannot wedge it, but
// the right bound depends entirely on what the capability does: turning
// a camera is milliseconds, building an image on a board is an hour, and
// a single constant cannot be right for both. The constant that was
// there assumed every provider is local and therefore fast — true for
// the device, storage and blackboard providers it was written for, and
// false the moment a provider runs an operator's own command.
//
// Declaring a bound also declares that the call may outlive the mailbox
// poll that delivered it: the daemon runs such calls off the poll loop,
// because a poll loop blocked for an hour is a node that has stopped
// collecting its mail for an hour.
type LongRunning interface {
	// InvokeTimeout reports how long this capability may take, and
	// whether this provider wants to set the bound at all. (0, false)
	// means "use the daemon's default", which is also what a provider
	// that does not implement this interface gets.
	//
	// It is asked per capability because one provider can serve both a
	// health check and a firmware flash.
	InvokeTimeout(capability string) (time.Duration, bool)
}

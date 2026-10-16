package daemon

// identity_check.go is the owner's check of the key history its witness hub
// holds for it (KEL D-27): a pure function of the two histories, so that
// what a hub says can never stop an identity unless the hub holds a key
// history only the holder of its recovery material could have made.

import (
	"errors"

	"github.com/ANetResearch/ANetCore/identity"
)

// HubKELVerdict is classifyHubKEL's conclusion (KEL D-27 rows a–f).
type HubKELVerdict string

const (
	// hubKELInvalid (a): the hub's record does not replay, is another
	// identity's, or cannot be compared with this node's. The identity is
	// not affected; doctor says identity.hub_record_invalid.
	hubKELInvalid HubKELVerdict = "invalid"
	// hubKELAccepted (b): the hub holds this node's KEL with the pending
	// event: the publication was accepted, whatever its answer said.
	hubKELAccepted HubKELVerdict = "accepted"
	// hubKELBehind (c): the hub holds a strict prefix of this node's KEL:
	// push it the longer one.
	hubKELBehind HubKELVerdict = "behind"
	// hubKELSame (d): the hub holds this node's KEL.
	hubKELSame HubKELVerdict = "same"
	// hubKELNewer (e): the hub holds a valid extension of this node's KEL
	// that this node did not make: the identity was rotated elsewhere.
	hubKELNewer HubKELVerdict = "newer"
	// hubKELFork (f): the hub holds a valid KEL of this identity that
	// disagrees with this node's at seq 1 or later.
	hubKELFork HubKELVerdict = "fork"
)

// stops reports whether the verdict stops the identity (rows e and f): the
// only two, and the only ones in which identity.kel's state changes.
func (v HubKELVerdict) stops() bool { return v == hubKELNewer || v == hubKELFork }

// classifyHubKEL decides what the key history hub, which the witness hub
// says it holds for this node, means (KEL D-27). local is the KEL the
// witness accepted (identity file key 1), pending the event being published
// (nil when none is built). The first row that holds, from the top:
//
//	a  hub does not replay; or replays to another AID; or ExtendsKEL(local,
//	   hub) fails for any reason but a fork or a rollback        invalid
//	b  hub is local followed by pending (event preimages)        accepted
//	c  hub is a strict prefix of local                           behind
//	d  hub is local                                              same
//	e  hub extends local                                         newer
//	f  hub forks from local                                      fork
//
// ExtendsKEL replays hub first, so in rows c–f hub is a KEL whose
// signatures hold. Its AID is the CID of its icp, and row a rules out
// another AID, so row f covers only forks at seq 1 or later, every one of
// them signed by a key an earlier event of this identity committed (KEL
// R-3, R-5): only the holder of this node's recovery material — or of its
// data directory, before the witness designation — makes one. A hub can
// forge none of them.
func classifyHubKEL(local []identity.SignedEvent, pending *identity.SignedEvent, hub []identity.SignedEvent) HubKELVerdict {
	if len(local) == 0 {
		return hubKELInvalid
	}
	states, err := identity.Replay(hub)
	if err != nil || len(states) == 0 {
		return hubKELInvalid
	}
	localStates, err := identity.Replay(local)
	if err != nil || len(localStates) == 0 || states[len(states)-1].AID != localStates[len(localStates)-1].AID {
		return hubKELInvalid
	}
	err = identity.ExtendsKEL(local, hub)
	if err != nil && !errors.Is(err, identity.ErrKELFork) && !errors.Is(err, identity.ErrKELRollback) {
		return hubKELInvalid
	}
	if pending != nil && len(hub) == len(local)+1 {
		with := append(append(make([]identity.SignedEvent, 0, len(local)+1), local...), *pending)
		if identity.ExtendsKEL(with, hub) == nil {
			return hubKELAccepted
		}
	}
	switch {
	case errors.Is(err, identity.ErrKELFork):
		return hubKELFork
	case errors.Is(err, identity.ErrKELRollback):
		return hubKELBehind
	case len(hub) == len(local):
		return hubKELSame
	}
	return hubKELNewer
}

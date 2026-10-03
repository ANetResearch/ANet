package interactions

// ixkey.go keeps this node's interaction keys (forward-secrecy §4.5). For each task this node makes a key
// pair; the other side seals the task's later messages to its public half (rk) as a second layer. Deleting
// the private half is what makes those messages unreadable once the task has ended here, so a key goes in
// the transaction of the write that ends the task — never before it commits, never in a step of its own:
//
//   - every state write (SetState, Finish, SetLateResult and the wrappers over them) names a Cause; a
//     write whose target is terminal settles this node's key for the task, as ixKeyFate says, in its own
//     transaction;
//   - CloseIXKey does the same where the end of the task for the key comes without a state write (the
//     provider's terminal message on a task already canceled here);
//   - DeleteAllIXKeys removes every key (emergency rotation, local state rollback, deactivation, stopped);
//   - SweepCloseAfter removes the keys a local end kept for L, once L has passed and the clock gate allows.
//
// These are the only places that delete from ix_key.

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetCore/seal"
)

// Cause is why a state write is made, and so what it does to this node's key for the task (FS §4.5.4,
// engine §5.3). Every state write takes one. The zero value is refused with ErrBadInput, so no path can end
// a task without saying how it ended; a write whose target state is not terminal says CauseNotTerminal.
type Cause string

const (
	// CauseProviderEnd: this node is the provider and the task reaches a terminal state — completed,
	// failed, rejected or canceled, whoever asked for it, startup recovery included. The key is deleted.
	CauseProviderEnd Cause = "provider_end"
	// CausePeerEnded: this node is the requester and the provider has ended the task — its result,
	// terminal status, pending_expired, operator_rejected or closing notice is stored; a result arrives
	// after the task ended here (SetLateResult); a refusal (outcome 2) or task_not_found ends the task.
	// The key is deleted.
	CausePeerEnded Cause = "peer_ended"
	// CauseLocalEnded: this node is the requester and the task ends here while the provider may still
	// answer — a cancel, no_response, delivery_unconfirmed, a delegation given up after it was handed
	// over. The key is kept for L (seal.MaxMessageLifetimeMS) so that a late answer can still be opened:
	// close_after = now + L.
	CauseLocalEnded Cause = "local_ended"
	// CauseNeverSent: this node is the requester and the delegation was given up, or withdrawn, before
	// any of it left this node. Nothing can ever be sealed to the key, so it is deleted.
	CauseNeverSent Cause = "never_sent"
	// CauseRetention: the retention sweep deletes the interaction; a key left on it goes with it.
	CauseRetention Cause = "retention"
	// CauseClosed: the task is closed by an emergency rotation, a local state rollback, a deactivation or
	// the identity stopping (FS-X10, KEL D-29). The key is deleted.
	CauseClosed Cause = "closed"
	// CauseNotTerminal: the write's target state is not terminal. The key is kept.
	CauseNotTerminal Cause = "not_terminal"
)

// Valid reports whether c is one of the seven causes.
func (c Cause) Valid() bool {
	switch c {
	case CauseProviderEnd, CausePeerEnded, CauseLocalEnded, CauseNeverSent, CauseRetention, CauseClosed,
		CauseNotTerminal:
		return true
	}
	return false
}

// When is c for a write to st when st is terminal, and CauseNotTerminal when it is not: the Cause of a write
// whose target state is known only at run time (a status message, a capability result that may be a quote).
func (c Cause) When(st State) Cause {
	if st.IsTerminal() {
		return c
	}
	return CauseNotTerminal
}

// ixFate is what a Cause does to this node's key for the task.
type ixFate uint8

const (
	fateKeep       ixFate = iota + 1 // the key stays
	fateDelete                       // the key is deleted
	fateCloseAfter                   // the key is kept until now + L, then swept
)

// ixKeyFate is the table of FS §4.5.4 (engine §5.3): what cause does to the key of a task in which this
// node has role. A cause that does not belong to the role — the requester naming CauseProviderEnd, the
// provider CausePeerEnded — and an unknown role or cause are ErrBadInput. Pure.
func ixKeyFate(role Role, cause Cause) (ixFate, error) {
	if role != RoleInbound && role != RoleOutbound {
		return 0, fmt.Errorf("%w: role %q", ErrBadInput, role)
	}
	provider := role == RoleInbound
	switch cause {
	case CauseNotTerminal:
		return fateKeep, nil
	case CauseRetention, CauseClosed:
		return fateDelete, nil
	case CauseProviderEnd:
		if provider {
			return fateDelete, nil
		}
	case CausePeerEnded, CauseNeverSent:
		if !provider {
			return fateDelete, nil
		}
	case CauseLocalEnded:
		if !provider {
			return fateCloseAfter, nil
		}
	default:
		return 0, fmt.Errorf("%w: a state write needs a Cause (got %q)", ErrBadInput, cause)
	}
	return 0, fmt.Errorf("%w: cause %s does not apply to the %s side of a task", ErrBadInput, cause, role)
}

// ClockLevel is the level of the deletion clock gate (FS-K14b) as this package's deletions take it: they
// run at ClockFull and ClockLocal and wait at ClockClosed (FS-K15). The zero value is ClockClosed, so a
// caller that does not know the level deletes nothing.
//
// v030-todo(FS-4): FS-4's clockLevel (internal/daemon/epoch_delete.go) is what callers pass here; FS-4
// makes its type this one, or maps its three values onto these.
type ClockLevel uint8

const (
	ClockClosed ClockLevel = iota // the gate is closed: no deletion
	ClockLocal                    // hard-bound deletions only, by the local clock
	ClockFull                     // the clock was just checked with the hub
)

// IXKey is one of this node's interaction keys: the key pair it made for task IX with PeerAID. Priv is
// secret: a caller that read it calls Wipe once it has used it (FS-X8).
type IXKey struct {
	IX      string
	PeerAID string
	// KID is KID(Suite, Pub) (L15), the ikid of a second layer sealed to the key. PutIXKey derives it.
	KID   []byte
	Suite seal.Suite
	Pub   []byte
	Priv  []byte
	// CloseAfter is the unix ms after which SweepCloseAfter deletes the key; 0 is none.
	CloseAfter int64
}

// Wipe zeroes k's private key in memory.
func (k *IXKey) Wipe() {
	if k == nil {
		return
	}
	clear(k.Priv)
}

// RK is the public half of one of this node's interaction keys: what a message it seals on the task
// carries as inner key 15 while it holds the key (FS-X4).
type RK struct {
	Suite seal.Suite
	Pub   []byte
}

// ErrRKMismatch is returned by RecordPeerRK when the interaction already holds a different rk from its
// peer: the message is refused as rk-mismatch (FS-R4).
var ErrRKMismatch = errors.New("interactions: the peer's rk differs from the one recorded for the task")

// ErrIXKeyExists is returned by PutIXKey when the task already has a key. A key is made once per task; a
// second one would not be the key the peer seals to.
var ErrIXKeyExists = errors.New("interactions: the task already has an interaction key")

// RKConflict reports whether a message carrying rk carried conflicts with the rk recorded for its task:
// both are present and differ (FS-R4). An absent recorded rk is not a conflict (the first one is
// recorded), nor is a message that carries none (it declares nothing). The receive path's step 9, the
// transaction of step 10 and the preview frames all decide with this.
func RKConflict(recorded, carried []byte) bool {
	return len(recorded) > 0 && len(carried) > 0 && string(recorded) != string(carried)
}

// errIXKeysNotStored is what the key functions answer until the table exists. v030-todo(FS-6)
var errIXKeysNotStored = errors.New("interactions: interaction keys are not stored yet")

// PutIXKey stores k as this node's key for task k.IX, in the transaction that records the task (FS-X1,
// FS-X2). The task must exist, be open and have k.PeerAID as its peer; k.Suite must be a suite this build
// implements, with a 32-byte public and private key. KID is derived from Suite and Pub (a KID given that
// differs is ErrBadInput). A task that already has a key is ErrIXKeyExists.
func (t *Tx) PutIXKey(k IXKey) error {
	return errIXKeysNotStored // v030-todo(FS-6)
}

// IXKeyByKID returns this node's interaction key whose KID is kid — the key a second layer with ikid kid
// was sealed to (FS-R1) — or (nil, nil) when there is none: the task ended here and its key is gone, or
// the kid was never this node's. A read error is returned as an error (the receive path treats it as
// temporary, ENG-V8). The caller wipes the key once it has opened the layer.
func (s *Store) IXKeyByKID(kid []byte) (*IXKey, error) {
	return nil, errIXKeysNotStored // v030-todo(FS-6)
}

// OwnRK returns the rk this node puts on a message it seals on task ix now — the public half of its key
// for the task — or (nil, nil) when it holds no key for the task (FS-X4: a message sealed after the key is
// deleted carries no rk).
func (s *Store) OwnRK(ix string) (*RK, error) {
	return nil, errIXKeysNotStored // v030-todo(FS-6)
}

// OwnRK is Store.OwnRK inside the transaction.
func (t *Tx) OwnRK(ix string) (*RK, error) {
	return nil, errIXKeysNotStored // v030-todo(FS-6)
}

// RecordPeerRK records rk — coredet(RK), as the peer's message carried it — as the peer's rk for task ix,
// in the transaction of step 10 (FS-R4). The first rk is written; the same one again changes nothing; a
// different one is ErrRKMismatch and the caller rolls the transaction back and refuses the message. An
// absent task is ErrNotFound; an empty rk is ErrBadInput (a message without rk records nothing).
func (t *Tx) RecordPeerRK(ix string, rk []byte) error {
	return errIXKeysNotStored // v030-todo(FS-6)
}

// CloseIXKey settles this node's key for task ix by cause in this transaction, where the end of the task
// for the key comes without a state write: the provider's terminal message stored on a task this node has
// already ended. The task must be terminal (CauseNotTerminal is ErrBadInput: there is nothing to settle).
// The state writes call the same code; nothing else deletes a single task's key.
func (t *Tx) CloseIXKey(ix string, cause Cause) error {
	if !cause.Valid() || cause == CauseNotTerminal {
		return fmt.Errorf("%w: CloseIXKey needs a terminal Cause (got %q)", ErrBadInput, cause)
	}
	return nil // v030-todo(FS-6)
}

// DeleteAllIXKeys deletes every interaction key this node holds, those waiting on close_after included, in
// the transaction that closes the open tasks (closeInteractionsTx: emergency rotation, local state
// rollback, deactivation, stopped; FS-X10). It reports how many it deleted.
func (t *Tx) DeleteAllIXKeys() (int64, error) {
	return 0, errIXKeysNotStored // v030-todo(FS-6)
}

// SweepCloseAfter deletes the interaction keys whose close_after is set and has come (≤ now, unix ms), and
// reports how many (FS-X9). The daemon runs it once per maintenance period S. It deletes at ClockFull and
// ClockLocal; at ClockClosed it deletes nothing and the keys wait for the next run (FS-K15).
func (s *Store) SweepCloseAfter(now int64, level ClockLevel) (int64, error) {
	return 0, errIXKeysNotStored // v030-todo(FS-6)
}

// checkCause checks a state write's cause before anything is written: it must be a state write's cause
// (not CauseRetention), say CauseNotTerminal exactly when the target is not terminal, and belong to this
// node's role in the task. It returns the fate the write gives the key when the task is terminal after it.
// An absent task is ErrNotFound.
func checkCause(e execer, id string, terminal bool, cause Cause) (ixFate, error) {
	if !cause.Valid() || cause == CauseRetention {
		return 0, fmt.Errorf("%w: a state write needs a Cause (got %q)", ErrBadInput, cause)
	}
	if terminal != (cause != CauseNotTerminal) {
		if terminal {
			return 0, fmt.Errorf("%w: a write to a terminal state needs a terminal Cause, not %s", ErrBadInput, cause)
		}
		return 0, fmt.Errorf("%w: cause %s on a write to a state that is not terminal", ErrBadInput, cause)
	}
	var role string
	err := e.QueryRow(`SELECT role FROM interaction WHERE id=?`, id).Scan(&role)
	if err != nil {
		return 0, notFound(err)
	}
	return ixKeyFate(Role(role), cause)
}

// notFound turns "no row" into ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// settleIXKey gives this node's key for task ix the fate a write that left the task terminal decided.
func settleIXKey(e execer, ix string, fate ixFate, nowMS int64) error {
	return nil // v030-todo(FS-6)
}

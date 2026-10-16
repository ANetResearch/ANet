package interactions

// ixkey.go keeps this node's interaction keys (forward-secrecy §4.5). For each task this node makes a key
// pair; the other side seals the task's later messages to its public half (rk) as a second layer. Deleting
// the private half is what makes those messages unreadable once the task has ended here, so a key goes in
// the transaction of the write that ends the task — never before it commits, never in a step of its own:
//
//   - every state write (SetState, Finish, SetLateResult and the wrappers over them) names a Cause; a
//     write whose target is terminal settles this node's key for the task in its own transaction, through
//     closeIXKey, as ixKeyFate says;
//   - CloseIXKey does the same where the end of the task for the key comes without a state write (the
//     provider's terminal message on a task already canceled here);
//   - DeleteAllIXKeys removes every key (emergency rotation, local state rollback, deactivation, stopped);
//   - SweepCloseAfter removes the keys a local end kept for L, once L has passed and the clock gate allows;
//   - PruneTerminal takes a key still kept for a task with the task (CauseRetention).
//
// These are the only places that delete from ix_key. The database is opened with secure_delete, so a
// deleted key's bytes are overwritten in the file; until the next WAL checkpoint they may remain in the
// WAL, which the daemon is to truncate after a key deletion (FS-L8).

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"

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
// caller that does not know the level deletes nothing. The daemon's clockLevel (internal/daemon,
// epoch_delete.go) is this type: the gate it computes is passed here as it is.
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

// checkCause checks a state write's cause before anything is written: it must be a state write's cause
// (not CauseRetention), say CauseNotTerminal exactly when the target is not terminal, and belong to this
// node's role in the task. An absent task is ErrNotFound.
func checkCause(e execer, id string, terminal bool, cause Cause) error {
	if !cause.Valid() || cause == CauseRetention {
		return fmt.Errorf("%w: a state write needs a Cause (got %q)", ErrBadInput, cause)
	}
	if terminal != (cause != CauseNotTerminal) {
		if terminal {
			return fmt.Errorf("%w: a write to a terminal state needs a terminal Cause, not %s", ErrBadInput, cause)
		}
		return fmt.Errorf("%w: cause %s on a write to a state that is not terminal", ErrBadInput, cause)
	}
	var role string
	if err := e.QueryRow(`SELECT role FROM interaction WHERE id=?`, id).Scan(&role); err != nil {
		return notFound(err)
	}
	_, err := ixKeyFate(Role(role), cause)
	return err
}

// closeIXKey is the one place a single task's key is settled (FS §4.5.4, engine §5.3): in transaction e,
// it gives this node's key for task ix the fate ixKeyFate gives cause for this node's role in the task.
// The state writes call it in their own transaction once the task is terminal, and CloseIXKey where no
// state write does; it never runs in a transaction of its own. A task already deleted (retention,
// closing) takes its key with it; for any other cause it is ErrNotFound.
func closeIXKey(e execer, ix string, cause Cause, nowMS int64) error {
	if cause == CauseNotTerminal {
		return nil
	}
	if h := terminalWriteHook.Load(); h != nil {
		(*h)(ix, cause)
	}
	var role string
	err := e.QueryRow(`SELECT role FROM interaction WHERE id=?`, ix).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) && (cause == CauseRetention || cause == CauseClosed) {
		return applyIXKeyFate(e, ix, fateDelete, nowMS)
	}
	if err != nil {
		return notFound(err)
	}
	fate, err := ixKeyFate(Role(role), cause)
	if err != nil {
		return err
	}
	return applyIXKeyFate(e, ix, fate, nowMS)
}

// terminalWriteHook, when set, is called with every terminal write's task
// and Cause, inside its transaction (SetTerminalWriteHook).
var terminalWriteHook atomic.Pointer[func(ix string, cause Cause)]

// SetTerminalWriteHook makes every terminal write — a state write to a
// terminal state, a terminal Finish, a late result, CloseIXKey — call f
// with its task and Cause inside its transaction, before its key is
// settled; nil removes it. Only tests set it: the engine's tests count the
// unified terminal writes of a path (engine E-T31). It returns what to
// restore.
func SetTerminalWriteHook(f func(ix string, cause Cause)) (restore func()) {
	var p *func(ix string, cause Cause)
	if f != nil {
		p = &f
	}
	old := terminalWriteHook.Swap(p)
	return func() { terminalWriteHook.Store(old) }
}

// notFound turns "no row" into ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// PutIXKey stores k as this node's key for task k.IX, in the transaction that records the task (FS-X1,
// FS-X2). The task must exist, be open and have k.PeerAID as its peer; k.Suite must be a suite this build
// implements, with a 32-byte public and private key. KID is derived from Suite and Pub (a KID given that
// differs is ErrBadInput). A task that already has a key is ErrIXKeyExists.
func (t *Tx) PutIXKey(k IXKey) error {
	if k.IX == "" || k.PeerAID == "" {
		return fmt.Errorf("%w: an interaction key needs its task and peer", ErrBadInput)
	}
	if !seal.Supported(k.Suite) || len(k.Pub) != ixKeyLen || len(k.Priv) != ixKeyLen {
		return fmt.Errorf("%w: an interaction key is a suite-1 key pair of %d-byte keys", ErrBadInput, ixKeyLen)
	}
	kid := seal.KID(k.Suite, k.Pub)
	if len(k.KID) != 0 && !bytes.Equal(k.KID, kid) {
		return fmt.Errorf("%w: the key id is not KID(suite, pub)", ErrBadInput)
	}
	var peer, state string
	if err := t.tx.QueryRow(`SELECT peer_aid, state FROM interaction WHERE id=?`, k.IX).Scan(&peer, &state); err != nil {
		return notFound(err)
	}
	if peer != k.PeerAID {
		return fmt.Errorf("%w: the task's peer is not %s", ErrBadInput, k.PeerAID)
	}
	if State(state).IsTerminal() {
		// Its end has passed: nothing would ever delete the key.
		return ErrTerminal
	}
	var one int
	switch err := t.tx.QueryRow(`SELECT 1 FROM ix_key WHERE ix=?`, k.IX).Scan(&one); {
	case err == nil:
		return ErrIXKeyExists
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	_, err := t.tx.Exec(`INSERT INTO ix_key(ix, peer_aid, kid, suite, pub, priv, close_after) VALUES(?,?,?,?,?,?,0)`,
		k.IX, k.PeerAID, kid, int64(k.Suite), k.Pub, k.Priv)
	return err
}

// ixKeyLen is the length of a suite-1 (X25519) public and private key, the only suite this version uses for
// interaction keys (FS-C10).
const ixKeyLen = 32

// IXKeyByKID returns this node's interaction key whose KID is kid — the key a second layer with ikid kid
// was sealed to (FS-R1) — or (nil, nil) when there is none: the task ended here and its key is gone, or
// the kid was never this node's. A read error is returned as an error (the receive path treats it as
// temporary, ENG-V8). The caller wipes the key once it has opened the layer.
func (s *Store) IXKeyByKID(kid []byte) (*IXKey, error) {
	if len(kid) != seal.KIDLen {
		return nil, nil
	}
	var k IXKey
	var suite int64
	err := s.db.QueryRow(`SELECT ix, peer_aid, kid, suite, pub, priv, close_after FROM ix_key WHERE kid=?`, kid).
		Scan(&k.IX, &k.PeerAID, &k.KID, &suite, &k.Pub, &k.Priv, &k.CloseAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k.Suite = seal.Suite(suite)
	return &k, nil
}

// OwnRK returns the rk this node puts on a message it seals on task ix now — the public half of its key
// for the task — or (nil, nil) when it holds no key for the task (FS-X4: a message sealed after the key is
// deleted carries no rk).
func (s *Store) OwnRK(ix string) (*RK, error) { return ownRK(s.db, ix) }

// OwnRK is Store.OwnRK inside the transaction.
func (t *Tx) OwnRK(ix string) (*RK, error) { return ownRK(t.tx, ix) }

func ownRK(e execer, ix string) (*RK, error) {
	var rk RK
	var suite int64
	err := e.QueryRow(`SELECT suite, pub FROM ix_key WHERE ix=?`, ix).Scan(&suite, &rk.Pub)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rk.Suite = seal.Suite(suite)
	return &rk, nil
}

// maxRKBytes bounds a stored rk. A suite-1 coredet(RK) is 38 bytes; the bound only keeps a store from
// taking whatever bytes a caller hands it.
const maxRKBytes = 2048

// RecordPeerRK records rk — coredet(RK), as the peer's message carried it — as the peer's rk for task ix,
// in the transaction of step 10 (FS-R4). The first rk is written; the same one again changes nothing; a
// different one is ErrRKMismatch and the caller rolls the transaction back and refuses the message. An
// absent task is ErrNotFound; an empty rk is ErrBadInput (a message without rk records nothing).
func (t *Tx) RecordPeerRK(ix string, rk []byte) error {
	if ix == "" || len(rk) == 0 || len(rk) > maxRKBytes {
		return fmt.Errorf("%w: RecordPeerRK needs a task and an rk of 1 to %d bytes", ErrBadInput, maxRKBytes)
	}
	var cur []byte
	if err := t.tx.QueryRow(`SELECT peer_rk FROM interaction WHERE id=?`, ix).Scan(&cur); err != nil {
		return notFound(err)
	}
	if len(cur) > 0 {
		if RKConflict(cur, rk) {
			return ErrRKMismatch
		}
		return nil
	}
	_, err := t.tx.Exec(`UPDATE interaction SET peer_rk=? WHERE id=? AND peer_rk IS NULL`, rk, ix)
	return err
}

// CloseIXKey settles this node's key for task ix by cause in this transaction, where the end of the task
// for the key comes without a state write: the provider's terminal message stored on a task this node has
// already ended. The task must be terminal (CauseNotTerminal is ErrBadInput: there is nothing to settle).
// The state writes call the same code; nothing else deletes a single task's key.
func (t *Tx) CloseIXKey(ix string, cause Cause) error {
	if !cause.Valid() || cause == CauseNotTerminal {
		return fmt.Errorf("%w: CloseIXKey needs a terminal Cause (got %q)", ErrBadInput, cause)
	}
	var state string
	switch err := t.tx.QueryRow(`SELECT state FROM interaction WHERE id=?`, ix).Scan(&state); {
	case errors.Is(err, sql.ErrNoRows):
		// Gone: closeIXKey deletes a key left on it for retention or closing, and says ErrNotFound else.
	case err != nil:
		return err
	case !State(state).IsTerminal():
		return fmt.Errorf("%w: task %s is %s, not ended: its key stays", ErrBadInput, ix, state)
	}
	return closeIXKey(t.tx, ix, cause, t.now)
}

// DeleteAllIXKeys deletes every interaction key this node holds, those waiting on close_after included, in
// the transaction that closes the open tasks (closeInteractionsTx: emergency rotation, local state
// rollback, deactivation, stopped; FS-X10). It reports how many it deleted.
func (t *Tx) DeleteAllIXKeys() (int64, error) {
	res, err := t.tx.Exec(`DELETE FROM ix_key`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SweepCloseAfter deletes the interaction keys whose close_after is set and has come (≤ now, unix ms), and
// reports how many (FS-X9). The daemon runs it once per maintenance period S. It deletes at ClockFull and
// ClockLocal; at ClockClosed it deletes nothing and the keys wait for the next run (FS-K15).
func (s *Store) SweepCloseAfter(now int64, level ClockLevel) (int64, error) {
	if level != ClockFull && level != ClockLocal {
		return 0, nil
	}
	var n int64
	err := s.Update(func(t *Tx) error {
		res, err := t.tx.Exec(`DELETE FROM ix_key WHERE close_after != 0 AND close_after <= ?`, now)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// deleteIXKeysOf deletes the keys of the tasks ids, the retention sweep's batch (CauseRetention, FS-L8).
func deleteIXKeysOf(e execer, in string, ids []any) (int64, error) {
	res, err := e.Exec(`DELETE FROM ix_key WHERE ix IN `+in, ids...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// applyIXKeyFate does what fate says to this node's key for task ix: delete it, or keep it until now + L
// (an earlier close_after stays: a second local end does not lengthen the first).
func applyIXKeyFate(e execer, ix string, fate ixFate, nowMS int64) error {
	switch fate {
	case fateDelete:
		_, err := e.Exec(`DELETE FROM ix_key WHERE ix=?`, ix)
		return err
	case fateCloseAfter:
		at := nowMS + int64(seal.MaxMessageLifetimeMS)
		_, err := e.Exec(`UPDATE ix_key SET close_after=? WHERE ix=? AND (close_after = 0 OR close_after > ?)`, at, ix, at)
		return err
	}
	return nil
}

// ixKeySchema is the interaction key table (FS §2.11).
var ixKeySchema = []string{
	`CREATE TABLE ix_key (
	   ix TEXT PRIMARY KEY,
	   peer_aid TEXT NOT NULL,
	   kid BLOB NOT NULL UNIQUE,
	   suite INTEGER NOT NULL,
	   pub BLOB NOT NULL,
	   priv BLOB NOT NULL,
	   close_after INTEGER NOT NULL DEFAULT 0
	 )`,
	// The sweep reads the few keys a local end keeps.
	`CREATE INDEX idx_ix_key_close ON ix_key(close_after) WHERE close_after != 0`,
}

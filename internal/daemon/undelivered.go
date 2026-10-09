package daemon

// undelivered.go ends a task this node is a party to when the outbound
// engine gives up what it sent (engine ENG-X1, ENG-X2; an internal note item).
//
// Every message goes through the outbound queue, so a send succeeds once it
// is recorded, and the peer being out of reach only delays it. When the
// engine gives a row up — its deadline passed, the hub refused it for good,
// the no-response scan ended its delegation — the task waiting on it is
// failed with the reason, rather than left submitted or working forever:
//
//   - a delegation: always;
//   - a text, payment or end_request message of a task this node asked for:
//     the task waits on the answer to it (an end_request of a capability
//     call does not: its result comes whether or not it arrives);
//   - a cancel changes nothing: the task was canceled when it was sent, or
//     waits on a paid provider that decides (§4.2);
//   - a provider's status or result changes nothing on this side.
//
// The effect of a capability call is decided by whether any of it left
// this node and nothing else (ENG-X2, SI-21): never handed over, it did not
// run (UNAVAILABLE); handed over, it may have (UNVERIFIED), whatever the
// hub said after (ENG-H1).
//
// The task is failed in the transaction that deletes the row
// (abandonOutbox), through the unified terminal write (ENG-C4): a delegation
// that never left deletes the task's interaction key at once, any other end
// keeps it for a late answer (ENG-X4).

import (
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// failUndeliveredTx fails the task row leaves waiting with anet.reason
// reason, inside the transaction that gives the row up. handed says whether
// any of the row left this node. It reports whether it failed one: a task
// that ended meanwhile (its answer came first), or one the row does not
// hold up, is left as it is.
func failUndeliveredTx(tx *interactions.Tx, row *interactions.OutboxRow, reason string, handed bool) (bool, error) {
	if row.Type != seal.TypeDelegate && row.Type != seal.TypeMessage {
		return false, nil
	}
	ix, err := tx.Get(row.IX)
	if errors.Is(err, interactions.ErrNotFound) || errors.Is(err, interactions.ErrContentGone) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ix.Role != interactions.RoleOutbound || ix.IsTerminal() {
		return false, nil
	}
	if row.Type == seal.TypeMessage {
		switch row.Kind {
		case rowKindText, rowKindPayment:
		case rowKindEndRequest:
			if ix.IsCapability {
				return false, nil // the result comes whether or not the end request arrives
			}
		default:
			return false, nil
		}
		if _, err := tx.MessageByMsgID(ix.ID, hex.EncodeToString(row.MID)); err != nil {
			if errors.Is(err, interactions.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
	}
	m := map[string]any{a2ashape.KeyState: string(interactions.StateFailed), a2ashape.KeyReason: reason}
	if ix.IsCapability {
		if handed {
			m[a2ashape.KeyEffectStatus] = string(effect.Unverified)
		} else {
			m[a2ashape.KeyEffectStatus] = string(effect.Unavailable)
		}
	}
	meta, _ := json.Marshal(m)
	// A delegation that never left this node can bring no answer; anything
	// else may still be answered (ENG-X4).
	cause := interactions.CauseLocalEnded
	if row.Type == seal.TypeDelegate && !handed {
		cause = interactions.CauseNeverSent
	}
	err = tx.Finish(ix.ID, interactions.Finish{State: interactions.StateFailed,
		Result: []byte(a2ashape.Sentence(reason, a2ashape.English, nil)), Verified: interactions.VerificationUnknown,
		Meta: meta}, cause)
	if errors.Is(err, interactions.ErrTerminal) {
		return false, nil
	}
	return err == nil, err
}

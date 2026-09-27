package daemon

// undelivered.go ends a task this node asked for when the retry queue
// abandons what it sent the provider (retry.go, 0017 Q5).
//
// Every message a requester sends goes through the retry queue, so a send
// succeeds once it is recorded and the provider being unreachable only
// delays it. When the queue gives a row up — its deadline passed, or the
// hub refused it for good — the provider never saw the delegation, or never
// saw the input the task now waits on, and no answer can come. The task is
// then failed with anet.reason=undeliverable, rather than left submitted or
// working forever:
//
//   - the delegation itself: always;
//   - a text message or a payment message: the task waits on the provider's
//     answer to it;
//   - an end request on a text task: the provider completes on it;
//   - a cancel changes nothing here: the task was canceled locally when it
//     was sent, or it waits on a paid provider that decides (§4.2).
//
// A capability call that was never delivered did not run, so its effect
// status is UNAVAILABLE rather than UNVERIFIED.
//
// A row from before rows kept their message id cannot be matched to its
// message and fails nothing but a delegation.
//
// The task is failed in the transaction that deletes the row
// (abandonOutbox): a crash between the two would otherwise leave a task
// waiting forever on a row that is gone.

import (
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// undelivered is what abandoning a row does to the task it left waiting.
type undelivered struct {
	ix *interactions.Interaction
	// firstSeq is the task's first message, whose client message id is
	// taken off when the delegation itself was abandoned; 0 otherwise.
	firstSeq int64
}

// undeliveredTask reports the outbound task an abandoned row leaves
// waiting, if any. It only reads; failUndeliveredTx writes.
func (d *Daemon) undeliveredTask(it *interactions.OutboxItem) (*undelivered, bool) {
	if it.Type != seal.TypeDelegate && it.Type != seal.TypeMessage {
		return nil, false
	}
	ix, err := d.ix.Get(it.IX)
	if err != nil || ix.Role != interactions.RoleOutbound || ix.IsTerminal() {
		return nil, false
	}
	if it.Type == seal.TypeMessage {
		if len(it.MID) == 0 {
			return nil, false
		}
		msg, err := d.ix.MessageByMsgID(ix.ID, hex.EncodeToString(it.MID))
		if err != nil {
			return nil, false
		}
		switch msg.Kind {
		case interactions.MsgText, interactions.MsgPayment:
		case interactions.MsgEndRequest:
			if ix.IsCapability {
				return nil, false // the result comes whether or not the end request arrives
			}
		default:
			return nil, false
		}
		return &undelivered{ix: ix}, true
	}
	u := &undelivered{ix: ix}
	// The client's message id is taken off the first message: the task
	// never reached the provider, and the client's retry of the same
	// message must be a new attempt, not this failed task returned as its
	// duplicate.
	if first, ok := d.firstOwnMessage(ix.ID); ok {
		u.firstSeq = first
	}
	return u, true
}

// failUndeliveredTx fails u's task with anet.reason=undeliverable inside
// the transaction that drops the row. It reports whether it did: a task
// that ended meanwhile (its answer came first) is left as it is.
func failUndeliveredTx(tx *interactions.Tx, u *undelivered, it *interactions.OutboxItem, reason string) (bool, error) {
	m := map[string]any{a2ashape.KeyState: string(interactions.StateFailed), a2ashape.KeyReason: a2ashape.ReasonUndeliverable}
	if u.ix.IsCapability {
		m[a2ashape.KeyEffectStatus] = string(effect.Unavailable)
	}
	meta, _ := json.Marshal(m)
	err := tx.Finish(u.ix.ID, interactions.Finish{State: interactions.StateFailed,
		Result: []byte("not delivered to " + it.ToAID + " (" + reason + ")"), Verified: interactions.VerificationUnknown, Meta: meta})
	if errors.Is(err, interactions.ErrTerminal) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if u.firstSeq > 0 {
		if err := tx.MergeMessageMeta(u.ix.ID, u.firstSeq, map[string]any{a2ashape.KeyMessageID: nil}); err != nil &&
			!errors.Is(err, interactions.ErrNotFound) {
			return false, err
		}
	}
	return true, nil
}

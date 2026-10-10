package daemon

// outbox_seams.go is how the other areas reach the outbound engine (engine
// §1.1): every write to the outbox and its versions is the engine's
// (ENG-G1), so what an emergency rotation, a cancel, the receive pipeline
// or the retention sweep needs done to queued rows goes through the entry
// points below. The ones a later package fills in are stubs until then
// (ENG-G3).

import (
	"context"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// closeCause is why closeInteractionsTx ends every open task (engine
// ENG-K15, ENG-K16): the task's anet.reason.
type closeCause string

const (
	// closeKeyCompromise: an emergency rotation (core §4.7 E1).
	closeKeyCompromise closeCause = "key_compromise"
	// closeLocalStateLost: this node's state went back (FS-B3).
	closeLocalStateLost closeCause = "local_state_lost"
	// closeDeactivated: the identity was deactivated (KEL D-46).
	closeDeactivated closeCause = "identity_deactivated"
	// closeStopped: the identity was rotated or recovered elsewhere (KEL
	// D-29).
	closeStopped closeCause = "identity_stopped"
)

// closeInteractionsTx ends, in tx, every task that is not terminal (engine
// ENG-K15, K16): failed with anet.reason cause, its queued rows given up
// as §2.8 says for a close, every interaction key deleted; for
// key_compromise and local_state_lost a notice is queued to each peer that
// may know the task (a cancel from a requester, a terminal status from a
// provider), to go out under the new keys once the gate opens. KEL-9 calls
// it in E1 and D-29, FS-10 in FS-B3.
//
// v030-todo(ENG-1a): the close.
func (d *Daemon) closeInteractionsTx(tx *interactions.Tx, cause closeCause) error { return nil }

// freezeOrWithdraw is step 2 of CancelTask for a task this node asked for
// (ENG-O4), in tx: the delegation row of ix to `to` that never left and has
// no attempt under way is withdrawn with its whole queue (withdrawn true);
// one that may have left is frozen, and the text and end_request rows
// queued behind it that never left go.
//
// v030-todo(ENG-1a): freeze and withdraw.
func (d *Daemon) freezeOrWithdraw(tx *interactions.Tx, ix, to string) (withdrawn bool, err error) {
	return false, nil
}

// confirmImplicitTx is ENG-C2: a message from the peer of interaction ix
// that this node asked for, committed at receive step 10 in tx, confirms
// the delegation rows of ix to that peer that were handed over.
//
// v030-todo(ENG-1a): the implicit confirmation.
func (d *Daemon) confirmImplicitTx(tx *interactions.Tx, ix, from string) error { return nil }

// hurryAnswerQueue makes every row queued for `to` on ix due now and wakes
// the loop (engine §4.6): a redelivered delegation that was answered says
// its requester did not get the answer, and the answer goes out behind
// whatever was queued ahead of it.
//
// v030-todo(ENG-1a): the hurry.
func (d *Daemon) hurryAnswerQueue(ix, to string) {}

// HasOutboxRowsTx reports whether interaction ix still has a queued row
// (engine ENG-E2): what a deletion of the interaction or of its data key
// checks first, in its own transaction, so that what a row needs to be
// sealed again outlives the row (ENG-S7).
//
// v030-todo(ENG-1a): read the outbox.
func HasOutboxRowsTx(tx *interactions.Tx, ix string) (bool, error) { return false, nil }

// noteDelivery is RC-5's: after a transaction that changes what a task's
// delivery state may say (enqueue, A8, A12, a status answer, a
// confirmation, an abandonment, a withdrawal), the engine calls it with the
// task (engine ENG-P3, DLV-S-36). It writes nothing.
//
// v030-todo(RC-5): compare and publish the delivery event (DLV-P-3).
func (d *Daemon) noteDelivery(ix string) {}

// oneShotKind is what a one-shot envelope carries (engine §2.12): outer key
// 8, no outbox row, no retry, no delivery token.
type oneShotKind uint8

const (
	oneShotPreview oneShotKind = iota + 1 // a stream_preview frame
	oneShotCardGet                        // anet.card.get/1
	oneShotCard                           // anet.card/1
)

// OneShotResult is how a one-shot send ended (ENG-O8b): whether a byte of
// it left this process, and the hub's registered reason for its last
// answer ("" for none or a network failure).
type OneShotResult struct {
	Handed    bool
	HubReason string
}

// oneShotSend seals body once as kind and hands it over once (ENG-O8).
//
// v030-todo(ENG-1b): the one-shot sender; until then nothing is sent.
func (d *Daemon) oneShotSend(ctx context.Context, kind oneShotKind, to, ix string, body []byte) OneShotResult {
	return OneShotResult{}
}

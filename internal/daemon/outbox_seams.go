package daemon

// outbox_seams.go is how the other areas reach the outbound engine (engine
// §1.1): every write to the outbox and its versions is the engine's
// (ENG-G1), so what an emergency rotation, a cancel, the receive pipeline
// or the retention sweep needs done to queued rows goes through the entry
// points below. The ones a later package fills in are stubs until then
// (ENG-G3).

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/evtypes"
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

// valid reports whether c is one of the four.
func (c closeCause) valid() bool {
	switch c {
	case closeKeyCompromise, closeLocalStateLost, closeDeactivated, closeStopped:
		return true
	}
	return false
}

// notifies reports whether closing for c tells each peer that may know the
// task: an emergency rotation and a rollback do; a deactivation and a stop
// send nothing (ENG-K16).
func (c closeCause) notifies() bool { return c == closeKeyCompromise || c == closeLocalStateLost }

// closedSet is what closeInteractionsTx closed, for afterClose once its
// transaction has committed: the evidence and the events.
type closedSet struct {
	cause closeCause
	tasks []string
	rows  []closedRow
}

// closedRow is a row given up by a close, and whether any of it had left.
type closedRow struct {
	row    interactions.OutboxRow
	handed bool
}

// closeInteractionsTx ends, in tx, every task that is not terminal (engine
// ENG-K15, K16):
//
//   - the task fails with anet.reason cause, through the unified terminal
//     write (CauseClosed, which deletes its interaction key); a capability
//     call whose delegation never left is UNAVAILABLE, any other UNVERIFIED;
//   - its queued rows are given up as §2.8's last row says: the evidence
//     reason is undeliverable or delivery_unconfirmed by whether they left;
//   - for key_compromise and local_state_lost, a notice is queued to every
//     peer that may know the task — this node serves it, or its delegation
//     left or was confirmed: a cancel from a requester, a failed status from
//     a provider. It is an ordinary row, sealed after the gate opens with
//     the keys of then (ENG-K15); a delegation that never left is only
//     given up, and its peer is told nothing;
//   - for identity_deactivated and identity_stopped, every queued row goes,
//     those of tasks that had ended too: the gate is closed for good.
//
// Every interaction key goes in the same transaction, those waiting on
// close_after included (FS-X10, §5.3). KEL-9 calls it in E1, D-29 and
// D-46, FS-10 in FS-B3, inside the transaction that records the change;
// afterClose follows the commit.
func (d *Daemon) closeInteractionsTx(tx *interactions.Tx, cause closeCause) (*closedSet, error) {
	if !cause.valid() {
		return nil, fmt.Errorf("anet: no close for %q", cause)
	}
	open, err := tx.OpenInteractions()
	if err != nil {
		return nil, err
	}
	rows, err := tx.AllOutboxRows()
	if err != nil {
		return nil, err
	}
	byIX := map[string][]interactions.OutboxRow{}
	for _, r := range rows {
		byIX[r.IX] = append(byIX[r.IX], r)
	}
	now := d.nowMS()
	c := &closedSet{cause: cause}
	isOpen := map[string]bool{}
	for _, o := range open {
		isOpen[o.ID] = true
		queued, handed := false, false
		for _, r := range byIX[o.ID] {
			if r.Type != seal.TypeDelegate || r.ToAID != o.PeerAID {
				continue
			}
			vers, err := tx.OutboxVersions(r.ID)
			if err != nil {
				return nil, err
			}
			queued, handed = true, versionsHanded(rowView(&r), viewsOf(vers))
		}
		for _, r := range byIX[o.ID] {
			if err := giveUpRowTx(tx, r, c); err != nil {
				return nil, err
			}
		}
		failed, err := closeTaskTx(tx, o, cause, queued && !handed)
		if err != nil {
			return nil, err
		}
		if failed {
			c.tasks = append(c.tasks, o.ID)
		}
		peerKnows := o.Role == interactions.RoleInbound || !queued || handed
		if cause.notifies() && peerKnows {
			if err := queueCloseNoticeTx(tx, o, cause, now); err != nil {
				return nil, err
			}
		}
	}
	if !cause.notifies() {
		for _, r := range rows {
			if !isOpen[r.IX] {
				if err := giveUpRowTx(tx, r, c); err != nil {
					return nil, err
				}
			}
		}
	}
	if _, err := tx.DeleteAllIXKeys(); err != nil {
		return nil, err
	}
	return c, nil
}

// giveUpRowTx gives up row r for a close (§2.8, last row): it goes with its
// versions, and its message is marked unconfirmed.
func giveUpRowTx(tx *interactions.Tx, r interactions.OutboxRow, c *closedSet) error {
	vers, err := tx.OutboxVersions(r.ID)
	if err != nil {
		return err
	}
	handed := versionsHanded(rowView(&r), viewsOf(vers))
	if ok, err := tx.DeleteOutboxRow(r.ID); err != nil || !ok {
		return err
	}
	detail := a2ashape.DeliveryNotSent
	if handed {
		detail = a2ashape.DeliverySent
	}
	err = tx.MergeMessageMetaByID(r.IX, hex.EncodeToString(r.MID),
		map[string]any{metaDelivery: a2ashape.DeliveryUnconfirmed, metaDeliveryDetail: detail})
	if err != nil && !errors.Is(err, interactions.ErrNotFound) && !errors.Is(err, interactions.ErrContentGone) {
		return err
	}
	c.rows = append(c.rows, closedRow{row: r, handed: handed})
	return nil
}

// closeTaskTx fails open task o for cause (ENG-K15): neverSent says its
// delegation never left this node, so the call cannot have run.
func closeTaskTx(tx *interactions.Tx, o interactions.OpenInteraction, cause closeCause, neverSent bool) (bool, error) {
	m := map[string]any{a2ashape.KeyState: string(interactions.StateFailed), a2ashape.KeyReason: string(cause)}
	if o.IsCapability {
		if neverSent {
			m[a2ashape.KeyEffectStatus] = string(effect.Unavailable)
		} else {
			m[a2ashape.KeyEffectStatus] = string(effect.Unverified)
		}
	}
	meta, err := json.Marshal(m)
	if err != nil {
		return false, err
	}
	err = tx.Finish(o.ID, interactions.Finish{State: interactions.StateFailed,
		Result:   []byte(a2ashape.Sentence(string(cause), a2ashape.English, nil)),
		Verified: interactions.VerificationUnknown, Meta: meta}, interactions.CauseClosed)
	if errors.Is(err, interactions.ErrTerminal) {
		return false, nil
	}
	return err == nil, err
}

// queueCloseNoticeTx queues what a close tells o's peer (ENG-K15): a
// cancel from the requester, a failed status saying why from the provider.
func queueCloseNoticeTx(tx *interactions.Tx, o interactions.OpenInteraction, cause closeCause, now uint64) error {
	mid, msgID := newWireMID()
	item := interactions.OutboxItem{IX: o.ID, ToAID: o.PeerAID, MID: mid, TS: now, Exp: now + messageLifetimeMS}
	var err error
	if o.Role == interactions.RoleOutbound {
		item.Type, item.Kind = seal.TypeMessage, rowKindCancel
		item.Body, err = (&delegation.ChatMsg{Kind: delegation.KindCancel, MsgID: msgID}).Marshal()
	} else {
		meta, _ := json.Marshal(map[string]any{a2ashape.KeyReason: string(cause)})
		item.Type, item.Kind = seal.TypeStatus, delegation.StateFailed
		item.Body, err = (&delegation.StatusMsg{State: delegation.StateFailed,
			Text: a2ashape.Sentence(string(cause)+".peer", a2ashape.English, nil), Metadata: meta, At: now}).Marshal()
	}
	if err != nil {
		return err
	}
	_, _, err = tx.EnqueueOutbox(item)
	return err
}

// afterClose is what follows a close's commit: the evidence of every row
// given up (core §2.14), the events, a kick for the notices.
func (d *Daemon) afterClose(c *closedSet) {
	if c == nil {
		return
	}
	for _, r := range c.rows {
		d.outboxMem.forgetRow(r.row.ID)
		reason := deadlineReason(r.handed)
		if d.ledger != nil {
			if _, err := d.ledger.Append(evtypes.DeliveryExpired, map[string]any{
				"interaction_id": r.row.IX, "type": r.row.Type, "to_aid": r.row.ToAID, "attempts": r.row.Attempts,
				"last_error": lastErrorCode(r.row.LastError), "reason": reason, "handed": r.handed,
			}); err != nil {
				log.Printf("anet: delivery expiry evidence: %v", err)
			}
		}
		d.noteDelivery(r.row.IX)
	}
	for _, ix := range c.tasks {
		d.publishResult(ix)
		d.noteDelivery(ix)
	}
	if len(c.rows) > 0 || len(c.tasks) > 0 {
		log.Printf("anet: %d task(s) closed and %d queued message(s) given up (%s)", len(c.tasks), len(c.rows), c.cause)
	}
	d.kickOutbox()
}

// closeLeftAtStart redoes the close of a stopped or deactivated identity at
// start, before anything else is sent (engine §6.3 step 2, KEL D-35): the
// close is idempotent, and one a crash cut short leaves open tasks or rows.
func (d *Daemon) closeLeftAtStart() {
	var cause closeCause
	switch d.identityState().State {
	case IdentityStopped:
		cause = closeStopped
	case IdentityDeactivated:
		cause = closeDeactivated
	default:
		return
	}
	var c *closedSet
	err := d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		c, err = d.closeInteractionsTx(tx, cause)
		return err
	})
	if err != nil {
		log.Printf("anet: close the tasks of a %s identity: %v", cause, err)
		return
	}
	d.afterClose(c)
}

// freezeOrWithdraw is step 2 of CancelTask for a task this node asked for
// (ENG-O4), in tx: the delegation row of ix to `to` that never left and has
// no attempt under way is withdrawn with its whole queue, and returned;
// one that may have left is frozen, and the text and end_request rows
// queued behind it that never left go (nil returned). An attempt under way
// holds the row's lock: the delegation is then frozen, and the attempt's
// A8, which checks frozen in its own transaction, sends nothing more.
func (d *Daemon) freezeOrWithdraw(tx *interactions.Tx, ix, to string) (*interactions.OutboxRow, error) {
	queue, err := tx.OutboxRows(ix)
	if err != nil {
		return nil, err
	}
	var del *interactions.OutboxRow
	for i := range queue {
		if queue[i].Type == seal.TypeDelegate && queue[i].ToAID == to {
			del = &queue[i]
			break
		}
	}
	if del != nil {
		if unlock, ok := d.outboxLocks.tryLock(outboxLockKey(del.ID)); ok {
			defer unlock()
			vers, err := tx.OutboxVersions(del.ID)
			if err != nil {
				return nil, err
			}
			if !del.Attempting && !versionsHanded(rowView(del), viewsOf(vers)) {
				if _, err := tx.DeleteOutboxQueue(ix, to); err != nil {
					return nil, err
				}
				return del, nil
			}
		}
		if _, err := tx.FreezeOutboxDelegation(ix, to); err != nil {
			return nil, err
		}
	}
	_, err = tx.DeleteOutboxFollowUps(ix, to)
	return nil, err
}

// withdrawFrozen is ENG-O5: a frozen delegation that never left and has no
// attempt under way is withdrawn with its whole queue, the cancel behind it
// included — nothing of the task can reach the provider any more, so the
// cancel has nothing to cancel. The task was canceled when it was frozen.
// The caller holds the row's lock.
func (d *Daemon) withdrawFrozen(row *interactions.OutboxRow) error {
	if cur, _, err := d.ix.OutboxState(row.ID); err != nil || !cur.Frozen {
		return nil
	}
	withdrawn := false
	err := d.ix.Update(func(tx *interactions.Tx) error {
		cur, vers, err := tx.OutboxState(row.ID)
		if errors.Is(err, interactions.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !cur.Frozen || cur.Attempting || versionsHanded(rowView(cur), viewsOf(vers)) {
			return nil
		}
		if _, err := tx.DeleteOutboxQueue(cur.IX, cur.ToAID); err != nil {
			return err
		}
		// The canceled task's delegation never left after all: its
		// interaction key goes now, as a withdrawal at the cancel takes it
		// (ENG-X4), not at its close_after.
		if cur.Type == seal.TypeDelegate {
			if err := tx.CloseIXKey(cur.IX, interactions.CauseNeverSent); err != nil &&
				!errors.Is(err, interactions.ErrNotFound) && !errors.Is(err, interactions.ErrBadInput) {
				return err
			}
		}
		withdrawn = true
		return nil
	})
	if err != nil || !withdrawn {
		return err
	}
	d.outboxMem.forgetRow(row.ID)
	d.noteWithdrawn(row)
	return nil
}

// noteWithdrawn is the evidence of a delegation withdrawn (core §2.14:
// reason withdrawn, nothing handed over) and its event.
func (d *Daemon) noteWithdrawn(row *interactions.OutboxRow) {
	if d.ledger != nil {
		if _, err := d.ledger.Append(evtypes.DeliveryExpired, map[string]any{
			"interaction_id": row.IX, "type": row.Type, "to_aid": row.ToAID, "attempts": row.Attempts,
			"last_error": lastErrorCode(row.LastError), "reason": "withdrawn", "handed": false,
		}); err != nil {
			log.Printf("anet: withdrawal evidence: %v", err)
		}
	}
	log.Printf("anet: %s: canceled before it left this node; withdrawn, nothing was sent", row.IX)
	d.noteDelivery(row.IX)
}

// confirmImplicitTx is ENG-C2: a message from the peer of interaction ix
// that this node asked for, committed at receive step 10 in tx, confirms
// the delegation rows of ix to that peer that were handed over — the peer
// would not answer a task it did not get. It applies to delegations only
// (DLV-S-16) and reports how many it confirmed; implicitConfirmed follows
// the commit.
func (d *Daemon) confirmImplicitTx(tx *interactions.Tx, ix, from string) (int, error) {
	if ix == "" || from == "" {
		return 0, nil
	}
	cur, err := tx.Get(ix)
	if errors.Is(err, interactions.ErrNotFound) || errors.Is(err, interactions.ErrContentGone) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if cur.Role != interactions.RoleOutbound || cur.PeerAID != from {
		return 0, nil
	}
	rows, err := tx.OutboxRows(ix)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range rows {
		r := &rows[i]
		if r.Type != seal.TypeDelegate || r.ToAID != from {
			continue
		}
		vers, err := tx.OutboxVersions(r.ID)
		if err != nil {
			return 0, err
		}
		if !versionsHanded(rowView(r), viewsOf(vers)) {
			continue
		}
		if _, err := d.confirmRowTx(tx, r, seal.OutcomeProcessed, seal.Note{}); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// implicitConfirmed follows the commit of confirmImplicitTx's transaction.
func (d *Daemon) implicitConfirmed(ix string, n int) {
	if n == 0 {
		return
	}
	for range n {
		d.count(cntDAckConfirmedImplicit)
	}
	d.noteDelivery(ix)
	d.kickOutbox()
}

// hurryAnswerQueue makes every row queued for `to` on ix due now and
// attempts them in the background, in queue order, whatever their backoff
// (engine §4.6): a redelivered delegation that was answered says its
// requester did not get the answer, and the answer goes out behind
// whatever was queued ahead of it — after its status is asked, so one the
// hub still holds is not sent twice. The caller decides when (engine
// ENG-V18: an answered delegation redelivered while its answer is still
// queued) and limits how often (resendLimiter). It reports whether there
// was anything queued.
func (d *Daemon) hurryAnswerQueue(ix, to string) bool {
	var n int64
	err := d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		n, err = tx.HurryOutbox(ix, to, int64(d.nowMS()))
		return err
	})
	if err != nil {
		log.Printf("anet: %s: hurry the answer: %v", ix, err)
		return false
	}
	if n == 0 {
		return false
	}
	queue, err := d.ix.OutboxQueue(ix, to)
	if err != nil || len(queue) == 0 {
		return err == nil && len(queue) > 0
	}
	d.goBackground(func() {
		for _, r := range queue {
			ctx, cancel := context.WithTimeout(d.ctx, relayCallTimeout)
			_ = d.attemptOutboxAt(ctx, r.ID, true)
			cancel()
		}
	})
	return true
}

// HasOutboxRowsTx reports whether interaction ix still has a queued row
// (engine ENG-E2): what a deletion of the interaction or of its data key
// checks first, in its own transaction, so that what a row needs to be
// sealed again outlives the row (ENG-S7).
func HasOutboxRowsTx(tx *interactions.Tx, ix string) (bool, error) { return tx.HasOutboxRows(ix) }

// noteDelivery is RC-5's: after a transaction that changes what a task's
// delivery state may say (enqueue, A8, A12, a status answer, a
// confirmation, an abandonment, a withdrawal), the engine calls it with the
// task (engine ENG-P3, DLV-S-36). It writes nothing.
//
// v030-todo(RC-5): compare and publish the delivery event (DLV-P-3).
func (d *Daemon) noteDelivery(ix string) {}

// previewCoalescer is how the engine reaches the stream_preview coalescer
// (UX-8, CUX-P6–P8; engine ENG-G3): the provider's frames of a task go out
// only through oneShotSend (ENG-K18), and a round's frames stop once its
// real answer or result is queued (ENG-K20).
type previewCoalescer interface {
	// CancelRound drops the frames of task ix not yet handed over: a real
	// answer or result of the round was queued in the same call, and no
	// frame of that round goes after it (ENG-K20, CUX-P8).
	CancelRound(ix string)
}

// noPreviews is the coalescer until UX-8's (engine ENG-G3): it never sends
// a frame, so there is nothing to cancel.
//
// v030-todo(UX-8): previewCoalescer, the frames and CancelRound.
type noPreviews struct{}

func (noPreviews) CancelRound(string) {}

// previews is this node's preview coalescer.
func (d *Daemon) previews() previewCoalescer {
	if p := d.previewer.Load(); p != nil && *p != nil {
		return *p
	}
	return noPreviews{}
}

// setPreviewer makes c this node's preview coalescer; nil restores the
// stub. It may be called while the daemon runs.
func (d *Daemon) setPreviewer(c previewCoalescer) {
	if c == nil {
		d.previewer.Store(nil)
		return
	}
	d.previewer.Store(&c)
}

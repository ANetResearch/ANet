package daemon

// outbox_scan.go is the engine's part of the no-response sweep (engine
// §2.10, ENG-T1): a delegation this node handed over that its provider has
// not confirmed within no_response_after is given up as
// delivery_unconfirmed, its task fails, and a cancel follows it — the
// provider may still have got it, and must not run it on its own after
// the requester stopped waiting.

import (
	"errors"
	"log"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// scanUnconfirmed is ENG-T1 at now (unix ms) for no_response_after after:
// every delegation row handed over at or before now − after, not frozen,
// of a task this node sent that has not ended, is given up in one
// transaction with the cancel that follows it (DLV-S-27). Candidates are
// read a page at a time and checked again in the write (ENG-T4); a row an
// attempt holds is left for the next sweep. It returns the tasks it
// failed.
func (d *Daemon) scanUnconfirmed(now int64, after time.Duration) []string {
	cutoff := now - after.Milliseconds()
	var cands []interactions.HandedDelegation
	for cur := interactions.HandedDelegationsFirst; ; {
		page, err := d.ix.HandedDelegations(cutoff, cur, noResponseBatch)
		if err != nil {
			if d.ctx.Err() == nil {
				log.Printf("anet: unconfirmed delegations: %v", err)
			}
			return nil
		}
		cands = append(cands, page...)
		if len(page) < noResponseBatch || d.ctx.Err() != nil {
			break
		}
		cur = page[len(page)-1]
	}
	var failed []string
	for _, c := range cands {
		if ok := d.giveUpUnconfirmed(c.ID, cutoff); ok {
			failed = append(failed, c.IX)
		}
	}
	return failed
}

// giveUpUnconfirmed gives up delegation row id when, read again under its
// lock and in the write, it still qualifies (ENG-T1), and queues the
// cancel in the same transaction. It reports whether it did.
func (d *Daemon) giveUpUnconfirmed(id int64, cutoff int64) bool {
	unlock, ok := d.outboxLocks.tryLock(outboxLockKey(id))
	if !ok {
		return false // an attempt is under way; the next sweep looks again
	}
	defer unlock()
	var row *interactions.OutboxRow
	var vers []interactions.OutboxVer
	var failed bool
	var cancelID int64
	err := d.ix.Update(func(tx *interactions.Tx) error {
		r, vs, err := tx.OutboxState(id)
		if errors.Is(err, interactions.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if r.Type != seal.TypeDelegate || r.Frozen || r.HandedAt == 0 || r.HandedAt > cutoff {
			return nil
		}
		ix, err := tx.Get(r.IX)
		if err != nil || ix.Role != interactions.RoleOutbound || ix.IsTerminal() || ix.PeerAID != r.ToAID {
			if errors.Is(err, interactions.ErrNotFound) || errors.Is(err, interactions.ErrContentGone) {
				err = nil
			}
			return err
		}
		why := abandonWhy{reason: reasonDeliveryUnconfirmed, trigger: a2ashape.DeliveryCauseScan, touchTask: true}
		if failed, _, err = d.abandonRowTx(tx, r, vs, why, true); err != nil {
			return err
		}
		row, vers = r, vs
		if !failed {
			return nil
		}
		mid, msgID := newWireMID()
		body, err := (&delegation.ChatMsg{Kind: delegation.KindCancel, MsgID: msgID}).Marshal()
		if err != nil {
			return err
		}
		cancelID, _, err = d.enqueueTx(tx, wireSend{to: r.ToAID, typ: seal.TypeMessage, ix: r.IX, kind: rowKindCancel,
			mid: mid, body: body})
		return err
	})
	if err != nil {
		log.Printf("anet: give up the unconfirmed delegation %d: %v", id, err)
		return false
	}
	if row == nil {
		return false
	}
	_ = vers
	d.outboxMem.forgetRow(id)
	d.afterAbandon(row, reasonDeliveryUnconfirmed, true, failed, 0)
	if !failed {
		return false
	}
	d.noteDelivery(row.IX)
	log.Printf("anet: %s: %s did not confirm the delegation within no_response_after; failed (delivery_unconfirmed), "+
		"a cancel follows (row %d)", row.IX, row.ToAID, cancelID)
	return true
}

package daemon

// retry.go delivers the outbound retry queue (interactions outbox table,
// A2A-DESIGN §3.5 step 4 and §4.2): results, status messages and
// cancellations that must reach their peer.
//
// A row is written in the same transaction as the state change that
// produced it, then attempted at once. A failed attempt is retried with
// exponential backoff from retryBaseDelay up to retryMaxDelay (24 hours).
// The loop reads the table, so a restart resumes where the previous process
// stopped. Every attempt sends the same envelope bytes. Once the envelope's
// exp has passed the row is dropped and the drop recorded as evidence,
// because the recipient would refuse the envelope as expired.
//
// Refusal notices never enter this queue (§2 X2).

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// Backoff bounds of the retry queue.
const (
	retryBaseDelay = 5 * time.Second
	retryMaxDelay  = 24 * time.Hour
	// retryScanInterval is how often the loop looks for due rows when
	// nothing kicks it.
	retryScanInterval = 5 * time.Second
)

// EvDeliveryExpired records a queued message that could not be delivered
// before its envelope expired.
const EvDeliveryExpired = "anet.delivery.expired"

// retryDelay is the wait after the n-th failed attempt (n ≥ 1).
func retryDelay(n int) time.Duration {
	d := retryBaseDelay
	for i := 1; i < n; i++ {
		d *= 2
		if d >= retryMaxDelay {
			return retryMaxDelay
		}
	}
	return d
}

// queueSend seals body to toAID and records it in the outbox inside the
// caller's transaction function, then commits and attempts delivery. write
// runs in the same transaction; if it returns an error nothing is queued.
// The envelope is sealed before the transaction, because sealing may fetch
// the recipient's keys from the hub and a transaction must not wait on the
// network. A sealing failure queues the body instead, and the retry loop
// seals it later.
func (d *Daemon) queueSend(ctx context.Context, toAID, typ, ix string, body []byte,
	write func(tx *interactions.Tx) error) (int64, error) {
	exp := d.nowMS() + messageLifetimeMS
	env, serr := d.sealEnvelope(ctx, toAID, typ, ix, body)
	if serr != nil {
		log.Printf("anet: %s: sealing %s to %s failed (%v); queued for a later attempt", ix, typ, toAID, serr)
	}
	item := interactions.OutboxItem{IX: ix, ToAID: toAID, Type: typ}
	if serr == nil {
		item.Envelope, item.Exp = env, exp
	} else {
		item.Body = body
	}
	var id int64
	err := d.ix.Update(func(tx *interactions.Tx) error {
		if write != nil {
			if err := write(tx); err != nil {
				return err
			}
		}
		var err error
		id, err = tx.EnqueueOutbox(item)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// deliverQueued makes one attempt at an outbox row now. It is called right
// after queueSend by the path that queued the row, so an answer is sent
// without waiting for the loop, and by the loop for rows that are due.
func (d *Daemon) deliverQueued(ctx context.Context, id int64) error {
	unlock := d.outboxLocks.lock(fmt.Sprint(id))
	defer unlock()
	it, err := d.ix.GetOutbox(id)
	if err != nil {
		return nil // delivered or dropped by another attempt
	}
	now := d.nowMS()
	if len(it.Envelope) == 0 {
		exp := now + messageLifetimeMS
		env, serr := d.sealEnvelope(ctx, it.ToAID, it.Type, it.IX, it.Body)
		if serr != nil {
			return d.rescheduleOutbox(it, serr)
		}
		if err := d.ix.SetOutboxEnvelope(it.ID, env, exp); err != nil {
			return err
		}
		it.Envelope, it.Exp = env, exp
	}
	if it.Exp != 0 && now > it.Exp {
		if err := d.ix.DeleteOutbox(it.ID); err != nil {
			return err
		}
		if d.ledger != nil {
			if _, lerr := d.ledger.Append(EvDeliveryExpired, map[string]any{
				"interaction_id": it.IX, "type": it.Type, "to_aid": it.ToAID,
				"attempts": it.Attempts, "last_error": it.LastError,
			}); lerr != nil {
				log.Printf("anet: delivery expiry evidence: %v", lerr)
			}
		}
		log.Printf("anet: %s: %s to %s expired undelivered after %d attempts", it.IX, it.Type, it.ToAID, it.Attempts)
		return nil
	}
	if err := d.deliverEnvelope(ctx, it.ToAID, it.Envelope); err != nil {
		return d.rescheduleOutbox(it, err)
	}
	return d.ix.DeleteOutbox(it.ID)
}

// rescheduleOutbox records a failed attempt with the next backoff.
func (d *Daemon) rescheduleOutbox(it *interactions.OutboxItem, cause error) error {
	n := it.Attempts + 1
	next := int64(d.nowMS()) + retryDelay(n).Milliseconds()
	if err := d.ix.RescheduleOutbox(it.ID, n, next, cause.Error()); err != nil {
		return err
	}
	if n == 1 || n%10 == 0 {
		log.Printf("anet: %s: delivering %s to %s failed (attempt %d, next in %s): %v",
			it.IX, it.Type, it.ToAID, n, retryDelay(n), cause)
	}
	return cause
}

// kickOutbox wakes the retry loop.
func (d *Daemon) kickOutbox() {
	select {
	case d.outboxKick <- struct{}{}:
	default:
	}
}

// outboxLoop delivers due rows until ctx ends. Its first pass, at daemon
// start, attempts every queued row whatever its backoff: the conditions the
// previous process backed off from (a hub down, a peer's keys missing) are
// the ones a restart is likely to follow.
func (d *Daemon) outboxLoop(ctx context.Context) {
	t := time.NewTicker(retryScanInterval)
	defer t.Stop()
	d.flushOutboxAt(ctx, math.MaxInt64)
	for {
		d.flushOutbox(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.outboxKick:
		}
	}
}

// flushOutbox attempts every due row once.
func (d *Daemon) flushOutbox(ctx context.Context) { d.flushOutboxAt(ctx, int64(d.nowMS())) }

// flushOutboxAt attempts once every row due at or before at (unix ms).
func (d *Daemon) flushOutboxAt(ctx context.Context, at int64) {
	due, err := d.ix.DueOutbox(at, 100)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("anet: read outbox: %v", err)
		}
		return
	}
	for _, it := range due {
		if ctx.Err() != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, relayCallTimeout)
		_ = d.deliverQueued(cctx, it.ID)
		cancel()
	}
}

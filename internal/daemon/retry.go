package daemon

// retry.go runs the outbound queue (engine §2): the loop that attempts the
// rows that are due, and deliverQueued, which the path that queued a row
// calls at once (ENG-Q8). What one attempt does is the engine's
// (outbox_step.go, outbox_attempt.go); what a row is, the store's.
//
// The loop reads the table, so a restart resumes where the previous
// process stopped: its first pass attempts every row whatever its backoff
// — the conditions the previous process backed off from (a hub down, a
// peer's keys missing) are the ones a restart is likely to follow — and
// still follows the order of a task's messages, the gate and the schedule
// of status queries.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// EvDeliveryExpired records a queued message that was given up: its
// deadline passed, the hub refused it for good, or (a delegation) its task
// was canceled before it left this node; reason says which and handed
// whether any of it left (core §2.14).
const EvDeliveryExpired = evtypes.DeliveryExpired

// errUndeliverable is what an attempt at a row that was given up returns:
// the message will not be delivered, and nothing is left to retry.
var errUndeliverable = errors.New("not deliverable")

// undeliverableError is errUndeliverable with the reason; maybe is set when
// a byte of the message left this node, so "not delivered" would be more
// than is known ([redteam:F12], engine ENG-A5).
type undeliverableError struct {
	reason string
	maybe  bool
}

func (e *undeliverableError) Error() string {
	if e.maybe {
		return "anet: delivery not confirmed (" + e.reason + "; it may still have reached the recipient)"
	}
	return "anet: not delivered (" + e.reason + ")"
}

func (e *undeliverableError) Is(target error) bool { return target == errUndeliverable }

// outboxWaitingError is an attempt at a row that was not made because an
// earlier row for the same task and peer holds it back (engine ENG-O1).
type outboxWaitingError struct {
	cause error
}

func (e *outboxWaitingError) Error() string {
	return fmt.Sprintf("anet: waiting for an earlier message on this task (%v)", e.cause)
}

func (e *outboxWaitingError) Unwrap() error { return e.cause }

// deliverQueued attempts an outbox row now, after the rows queued before it
// for the same task and peer that have not left yet: a new message is a
// reason to try them. It is called right after queueSend by the path that
// queued the row, so a message goes without waiting for the loop. It
// returns an error wrapping errUndeliverable when the row was given up.
func (d *Daemon) deliverQueued(ctx context.Context, id int64) error {
	row, _, err := d.ix.OutboxState(id)
	if errors.Is(err, interactions.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	queue, err := d.ix.OutboxQueue(row.IX, row.ToAID)
	if err != nil {
		return err
	}
	for i := range queue {
		if a := &queue[i]; a.ID < id && a.HandedAt == 0 {
			// One an attempt is sending now is left to it: the call does
			// not wait for it.
			_, _ = d.tryAttemptOutbox(ctx, a.ID)
		}
	}
	if err := d.attemptOutbox(ctx, id); err != nil {
		return err
	}
	// The loop may have made the first attempt and given the row up: the
	// caller hears it all the same (engine ENG-H3).
	return d.goneAbandoned(row)
}

// goneAbandoned is the undeliverableError of row when it was given up (its
// message says so), and nil while it is queued or once it is confirmed.
func (d *Daemon) goneAbandoned(row *interactions.OutboxRow) error {
	if _, _, err := d.ix.OutboxState(row.ID); !errors.Is(err, interactions.ErrNotFound) {
		return nil
	}
	msg, err := d.ix.MessageByMsgID(row.IX, hex.EncodeToString(row.MID))
	if err != nil {
		return nil
	}
	var meta map[string]any
	if json.Unmarshal([]byte(msg.Metadata), &meta) != nil || meta[metaDelivery] != a2ashape.DeliveryUnconfirmed {
		return nil
	}
	sent := meta[metaDeliveryDetail] == a2ashape.DeliverySent
	reason := reasonUndeliverable
	if sent {
		reason = reasonDeliveryUnconfirmed
	}
	if ix, err := d.ix.Get(row.IX); err == nil && ix.ResultMeta != "" {
		var rm map[string]any
		if json.Unmarshal([]byte(ix.ResultMeta), &rm) == nil {
			if r, ok := rm[a2ashape.KeyReason].(string); ok && r != "" {
				reason = r
			}
		}
	}
	return &undeliverableError{reason: reason, maybe: sent}
}

// kickOutbox wakes the retry loop.
func (d *Daemon) kickOutbox() {
	select {
	case d.outboxKick <- struct{}{}:
	default:
	}
}

// outboxLoop attempts due rows until ctx ends. Its first pass, at daemon
// start, disregards the backoff (ENG-Q8).
func (d *Daemon) outboxLoop(ctx context.Context) {
	t := time.NewTicker(retryScanInterval)
	defer t.Stop()
	d.flushOutboxAt(ctx, math.MaxInt64)
	for {
		if !d.outboxHold.Load() {
			d.flushOutbox(ctx)
		}
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

// outboxFlushRounds bounds how many times one flush reads the due rows
// again after some went: a row that goes frees the next one in its task's
// queue ([redteam:F23]), which is due at once. More than that is left to
// an immediate next flush (kickOutbox).
const outboxFlushRounds = 8

// flushOutboxAt attempts every row due at or before at (unix ms), and the
// rows each frees behind it. A pass at math.MaxInt64 disregards the
// backoff.
func (d *Daemon) flushOutboxAt(ctx context.Context, at int64) {
	noBackoff := at == math.MaxInt64
	for round := 0; round < outboxFlushRounds; round++ {
		due, err := d.ix.DueOutboxIDs(at, 1000)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("anet: read outbox: %v", err)
			}
			return
		}
		progress := false
		for _, id := range due {
			if ctx.Err() != nil {
				return
			}
			before, _, err := d.ix.OutboxState(id)
			if err != nil {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, relayCallTimeout)
			_ = d.attemptOutboxAt(cctx, id, noBackoff)
			cancel()
			after, _, err := d.ix.OutboxState(id)
			if errors.Is(err, interactions.ErrNotFound) || err == nil && before.HandedAt == 0 && after.HandedAt != 0 {
				progress = true // gone or out: whatever queued behind it may be free
			}
		}
		if !progress {
			return
		}
		at = min(at, int64(d.nowMS()))
		noBackoff = false
	}
	d.kickOutbox()
}

// recoverOutboxAttempts is engine §6.3 step 3, before anything is sent: a
// row an attempt marked attempting may have been sent by the process that
// stopped, so its current version counts as handed over now (CR2) — a
// cancel does not withdraw it, and it is not reported never sent.
func (d *Daemon) recoverOutboxAttempts() {
	err := d.ix.Update(func(tx *interactions.Tx) error {
		n, err := tx.RecoverOutboxAttempts(int64(d.nowMS()))
		if err == nil && n > 0 {
			log.Printf("anet: %d message(s) an earlier run may have sent count as handed over", n)
		}
		return err
	})
	if err != nil {
		log.Printf("anet: recover the outbox: %v", err)
	}
}

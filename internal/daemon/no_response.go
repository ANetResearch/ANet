package daemon

// no_response.go ends a task this node asked for when its peer never
// answered at all (A2A-DESIGN §4.2, §4.3; docs/notes/0035 §8.3).
//
// A provider does not always say no. Its refusal notices are rate limited by
// design (§2 X2, inbound.reject_notice: per peer per hour and per minute),
// so a requester that sends more than that gets no answer for the rest: in
// 0035 a burst of 300 calls against a per-caller quota left 64 tasks
// submitted until each client gave up on its own, an hour later for a
// Hermes entry. A provider that is gone for good, or drops a delegation it
// cannot read, is the same from here. Nothing on this side would ever move
// such a task.
//
// So a task this node sent is failed with anet.reason=no_response when all
// of these hold:
//
//   - it is still submitted: the provider has not answered anything, and
//     this node has not sent anything after the delegation (a follow-up or
//     a payment moves it to working);
//   - nothing of it is still queued to go out: a delegation the retry queue
//     is still trying to deliver has not reached the provider yet, and when
//     the queue gives up on it the task fails as undeliverable
//     (undelivered.go);
//   - no status, message or result from the peer is stored for it — a
//     pending_approval notice (submitted, §5.3) is an answer;
//   - no_response_after has passed both since the task was made and since
//     its delegation left this node (DeleteDeliveredOutbox): a delegation
//     that waited out a hub outage gets the whole period after delivery.
//
// The effect is UNVERIFIED (SI-6): the peer may have refused the task, may
// never have read it, or may have run it — a long capability call started
// by an older provider says nothing until it ends. The task is ended here
// only; nothing is sent to the peer. A result that arrives afterwards is
// verified and recorded as any result after a terminal state is (§4.2
// ingestResult: the deliverable, the receipt and the settlement evidence,
// marked after_terminal), without reopening the task. A provider that runs
// a long call now says working when it starts (runCapabilityCall), so its
// requester does not take the wait for silence.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// DefaultNoResponseAfter is no_response_after when the key is left out.
const DefaultNoResponseAfter = 15 * time.Minute

// defaultNoResponseAfterText is how DefaultConfig writes it.
const defaultNoResponseAfterText = "15m"

// noResponseSweepEvery is how often the tasks are looked at; a task fails
// at most this much after its deadline.
const noResponseSweepEvery = time.Minute

// noResponseBatch is how many candidates one read of the index returns; a
// sweep reads page after page until it has seen them all. A variable so
// tests can shorten it.
var noResponseBatch = 256

// EvNoResponse records a task failed because its peer never answered.
const EvNoResponse = "anet.task.no_response"

// noResponseAfter is the effective no_response_after: the configured Go
// duration, DefaultNoResponseAfter when unset, 0 when turned off. A value
// that does not read as a duration of zero or more is an error, which the
// configuration check refuses to start with.
func (c Config) noResponseAfter() (time.Duration, error) {
	s := strings.TrimSpace(c.NoResponseAfter)
	if s == "" {
		return DefaultNoResponseAfter, nil
	}
	v, err := time.ParseDuration(s)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("anet: no_response_after %q is not a duration of zero or more (such as \"15m\"; \"0\" turns it off)", c.NoResponseAfter)
	}
	return v, nil
}

// NoResponseAfterValue is the effective no_response_after (see Config),
// for anet doctor; ok is false when the value does not read.
func (c Config) NoResponseAfterValue() (time.Duration, bool) {
	v, err := c.noResponseAfter()
	return v, err == nil
}

// noResponseLoop fails unanswered tasks every noResponseSweepEvery until
// ctx ends.
func (d *Daemon) noResponseLoop(ctx context.Context) {
	t := time.NewTicker(noResponseSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.failUnanswered(int64(d.nowMS()))
		}
	}
}

// failUnanswered fails, as of now (unix ms), every task this node sent that
// has waited past no_response_after with no answer at all. It returns the
// ids it failed.
func (d *Daemon) failUnanswered(now int64) []string {
	after, err := d.config().noResponseAfter()
	if err != nil || after <= 0 {
		return nil
	}
	cutoff := now - after.Milliseconds()
	// Every candidate is looked at, a page at a time: the ones that do not
	// qualify (a peer that said pending_approval, a delegation still queued)
	// stay submitted and old, and a sweep that read only the first page
	// would never get past them to a newer task nobody answered.
	var candidates []interactions.WaitingTask
	for cursor := interactions.WaitingFirst; ; {
		page, err := d.ix.Waiting(interactions.RoleOutbound, interactions.StateSubmitted, cutoff, cursor, noResponseBatch)
		if err != nil {
			if d.ctx.Err() == nil {
				log.Printf("anet: tasks without an answer: %v", err)
			}
			return nil
		}
		for _, w := range page {
			// The cheap reads first, outside the write lock; the write
			// checks everything again.
			if heard, err := d.ix.HeardFrom(w.ID, w.PeerAID); err != nil || heard {
				continue
			}
			if queued, err := d.ix.OutboxPending(w.ID); err != nil || queued {
				continue
			}
			candidates = append(candidates, w)
		}
		if len(page) < noResponseBatch || d.ctx.Err() != nil {
			break
		}
		cursor = page[len(page)-1]
	}
	var failed []string
	for _, w := range candidates {
		id := w.ID
		ok, peer, err := d.failNoResponse(id, cutoff, after)
		if err != nil {
			log.Printf("anet: %s: fail for no response: %v", id, err)
			continue
		}
		if !ok {
			continue
		}
		failed = append(failed, id)
		if d.ledger != nil {
			if _, lerr := d.ledger.Append(EvNoResponse, map[string]any{
				"interaction_id": id, "peer_aid": peer, "no_response_after_ms": after.Milliseconds(),
			}); lerr != nil {
				log.Printf("anet: no-response evidence: %v", lerr)
			}
		}
		log.Printf("anet: %s: no answer from %s in %s; the task is failed (no_response, effect unverified)", id, peer, after)
		d.publishResult(id)
	}
	return failed
}

// failNoResponse fails task id with anet.reason=no_response when, read again
// inside the write, it still qualifies (see the file comment); cutoff is
// the latest a qualifying task was made or handed off. ok reports whether
// it did.
func (d *Daemon) failNoResponse(id string, cutoff int64, after time.Duration) (ok bool, peer string, err error) {
	err = d.ix.Update(func(tx *interactions.Tx) error {
		cur, err := tx.Get(id)
		if err != nil {
			if errors.Is(err, interactions.ErrNotFound) {
				return nil
			}
			return err
		}
		peer = cur.PeerAID
		if cur.Role != interactions.RoleOutbound || cur.State != interactions.StateSubmitted ||
			len(cur.Receipt) > 0 || cur.StateAt >= cutoff {
			return nil
		}
		if at := cur.UpdatedAtTime(); at.IsZero() || at.UnixMilli() >= cutoff {
			return nil // handed off, or changed, since the cutoff
		}
		if queued, err := tx.OutboxPending(id); err != nil || queued {
			return err
		}
		if heard, err := tx.HeardFrom(id, cur.PeerAID); err != nil || heard {
			return err
		}
		meta, err := json.Marshal(map[string]any{
			a2ashape.KeyState:        string(interactions.StateFailed),
			a2ashape.KeyReason:       a2ashape.ReasonNoResponse,
			a2ashape.KeyEffectStatus: string(effect.Unverified),
		})
		if err != nil {
			return err
		}
		result := fmt.Sprintf("no answer from %s within %s of delivery: it may have refused the task without telling "+
			"(refusal notices are rate limited), not have read it, or be running it — whether it took effect is not known; "+
			"a result that comes later is still recorded", cur.PeerAID, after)
		err = tx.Finish(id, interactions.Finish{State: interactions.StateFailed, Result: []byte(result),
			Verified: interactions.VerificationUnknown, Meta: meta})
		if errors.Is(err, interactions.ErrTerminal) {
			return nil
		}
		if err != nil {
			return err
		}
		ok = true
		return nil
	})
	return ok, peer, err
}
